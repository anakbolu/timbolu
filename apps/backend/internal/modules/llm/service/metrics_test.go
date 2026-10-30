package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
)

// recordingMetrics is the port the deployment implements, kept here so a test reads what the gateway
// reported rather than what a mock was told to answer.
type recordingMetrics struct {
	calls []modelCall
}

type modelCall struct {
	provider string
	purpose  string
	fellBack bool
	seconds  float64
}

func (m *recordingMetrics) ModelCall(_ context.Context, provider, purpose string, fellBack bool, seconds float64) {
	m.calls = append(m.calls, modelCall{provider, purpose, fellBack, seconds})
}

// withMetrics installs the recorder on a harness.
func withMetrics(recorder *recordingMetrics) func(*Deps) {
	return func(deps *Deps) { deps.Metrics = recorder }
}

// servePrimary scripts the first provider of a chain to answer.
//
// The model is taken from the fixture rather than written out: the fake factory looks a provider up
// by the model the config resolved, so a test that spelled the name itself would be asserting on a
// copy of the fixture's data instead of on the fixture.
func servePrimary(h *harness, model string) {
	h.provider(model, func(p *fakeProvider) {
		p.chatFn = func(domain.ChatRequest) (domain.ChatResponse, error) {
			return domain.ChatResponse{
				Text:         "Sudah dicatat.",
				Model:        "gpt-4o-mini",
				FinishReason: domain.FinishStop,
				Usage:        domain.Usage{InputTokens: 120, OutputTokens: 30},
			}, nil
		}
	})
}

// TestAProviderCallIsReportedExactlyOnce: a chart that double-counts a call reports a bill nobody
// owes, so the measurement is taken at one point rather than at every place that sees the answer.
func TestAProviderCallIsReportedExactlyOnce(t *testing.T) {
	recorder := &recordingMetrics{}
	h := newHarness(t, withMetrics(recorder))
	scope := workspaceScope()

	chain := twoProviders(scope)
	h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return(chain, nil)
	servePrimary(h, chain[0].Model)
	h.recorder.EXPECT().Record(mock.Anything, mock.Anything).Return(nil).Once()

	_, err := h.service.Chat(context.Background(), scope, chatRequest(domain.RequestMetadata{
		WorkspaceID: scope.WorkspaceID,
		Purpose:     domain.PurposeAgent,
	}))
	require.NoError(t, err)

	require.Len(t, recorder.calls, 1)
	require.Equal(t, chain[0].Adapter, recorder.calls[0].provider, "the provider that actually answered")
	require.Equal(t, domain.PurposeAgent, recorder.calls[0].purpose)
	require.False(t, recorder.calls[0].fellBack, "the first provider answered")
	require.GreaterOrEqual(t, recorder.calls[0].seconds, 0.0)
}

// TestAFallbackIsReportedAsOne is the number that explains an unexpected bill: the call succeeded, on
// the *second* provider, and a chart that showed only the first would have nothing to say about why
// the workspace's spend moved to another model.
func TestAFallbackIsReportedAsOne(t *testing.T) {
	recorder := &recordingMetrics{}
	h := newHarness(t, withMetrics(recorder))
	scope := workspaceScope()

	chain := twoProviders(scope)
	h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return(chain, nil)
	h.provider(chain[0].Model, func(p *fakeProvider) {
		p.chatFn = func(domain.ChatRequest) (domain.ChatResponse, error) {
			return domain.ChatResponse{}, fmt.Errorf("%w: 429", domain.ErrRateLimited)
		}
	})
	h.provider(chain[1].Model, func(p *fakeProvider) {
		p.name = domain.AdapterAnthropic
		p.chatFn = func(domain.ChatRequest) (domain.ChatResponse, error) {
			return domain.ChatResponse{
				Text:  "Dicatat lewat cadangan.",
				Usage: domain.Usage{InputTokens: 90, OutputTokens: 20},
			}, nil
		}
	})
	var recorded domain.UsageEntry
	h.recorder.EXPECT().Record(mock.Anything, mock.Anything).Run(func(_ context.Context, entry domain.UsageEntry) {
		recorded = entry
	}).Return(nil).Once()

	_, err := h.service.Chat(context.Background(), scope, chatRequest(
		domain.RequestMetadata{WorkspaceID: scope.WorkspaceID}))
	require.NoError(t, err)
	require.Equal(t, domain.AdapterAnthropic, recorded.Provider)

	require.Len(t, recorder.calls, 1, "one call reached a provider, however many were tried")
	require.Equal(t, domain.AdapterAnthropic, recorder.calls[0].provider, "and it is the one that answered")
	require.True(t, recorder.calls[0].fellBack)
	require.Equal(t, domain.PurposeAgent, recorder.calls[0].purpose, "an unstated purpose is the agent's turns")
}

// TestAFailedCallIsNotReported: nothing reached a provider, so there is nothing to chart — measuring
// it as a call would put a failure on a latency chart where the number has no meaning.
func TestAFailedCallIsNotReported(t *testing.T) {
	recorder := &recordingMetrics{}
	h := newHarness(t, withMetrics(recorder))
	scope := workspaceScope()

	chain := twoProviders(scope)
	h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return(chain, nil)
	h.provider(chain[0].Model, func(p *fakeProvider) {
		p.chatFn = func(domain.ChatRequest) (domain.ChatResponse, error) {
			return domain.ChatResponse{}, domain.ErrInvalidRequest
		}
	})

	_, err := h.service.Chat(context.Background(), scope, chatRequest(domain.RequestMetadata{
		WorkspaceID: scope.WorkspaceID,
	}))
	require.ErrorIs(t, err, domain.ErrInvalidRequest)
	require.Empty(t, recorder.calls, "the request was refused before any provider answered")
}

// TestAnEmbeddingIsReportedUnderItsOwnPurpose: the label is the caller's intent, so an extraction is
// not filed under the agent's turns — which would hide what the fleet spends on each.
func TestAnEmbeddingIsReportedUnderItsOwnPurpose(t *testing.T) {
	recorder := &recordingMetrics{}
	h := newHarness(t, withMetrics(recorder), func(deps *Deps) {
		deps.EmbeddingModel = "text-embedding-3-small"
	})
	scope := workspaceScope()
	h.recorder.EXPECT().Record(mock.Anything, mock.Anything).Return(nil).Once()

	chain := twoProviders(scope)
	h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return(chain, nil)
	h.provider(chain[0].Model, func(p *fakeProvider) {
		// The gateway passes over a provider that advertises no embeddings, so the capability is
		// part of the fixture rather than an implementation detail of the fake.
		p.caps = domain.Capabilities{Embeddings: true}
		p.embedFn = func(domain.EmbedRequest) (domain.EmbedResponse, error) {
			return domain.EmbedResponse{
				Vectors: [][]float32{{0.1, 0.2}},
				Usage:   domain.Usage{InputTokens: 12},
			}, nil
		}
	})

	_, err := h.service.Embed(context.Background(), scope, domain.EmbedRequest{
		Inputs:   []string{"ongkir Bandung 15 ribu"},
		Metadata: domain.RequestMetadata{WorkspaceID: scope.WorkspaceID, Purpose: domain.PurposeEmbedding},
	})
	require.NoError(t, err)

	require.Len(t, recorder.calls, 1)
	require.Equal(t, domain.PurposeEmbedding, recorder.calls[0].purpose)
	require.False(t, recorder.calls[0].fellBack)
}

// TestAnUnwiredRecorderIsNotAFailure: a deployment with metrics off passes nothing, and every call
// still lands.
func TestAnUnwiredRecorderIsNotAFailure(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()

	chain := twoProviders(scope)
	h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return(chain, nil)
	servePrimary(h, chain[0].Model)
	h.recorder.EXPECT().Record(mock.Anything, mock.Anything).Return(nil).Once()

	_, err := h.service.Chat(context.Background(), scope, chatRequest(domain.RequestMetadata{
		WorkspaceID: scope.WorkspaceID,
	}))
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, scope.WorkspaceID)
}
