package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
)

// twoProviders is the chain most tests use: a workspace primary and one
// fallback, each with its own adapter.
func twoProviders(scope domain.Scope) []domain.StoredProvider {
	primary := storedProvider(func(p *domain.StoredProvider) {
		p.WorkspaceID = scope.WorkspaceID
		p.Name = "Utama"
		p.Model = "gpt-4o-mini"
		p.Priority = 10
		p.IsDefault = true
	})
	fallback := storedProvider(func(p *domain.StoredProvider) {
		p.WorkspaceID = scope.WorkspaceID
		p.ID = uuid.New()
		p.Name = "Cadangan"
		p.Adapter = domain.AdapterAnthropic
		p.Model = "claude-3-5-haiku"
		p.Priority = 20
		p.EncryptedKey = []byte("sealed:sk-live-2")
	})
	return []domain.StoredProvider{primary, fallback}
}

func chatRequest(meta domain.RequestMetadata) domain.ChatRequest {
	return domain.ChatRequest{
		Messages: []domain.Message{{Role: domain.RoleUser, Text: "Catat pesanan baru"}},
		Metadata: meta,
	}
}

// collect drains a stream into a slice.
func collect(t *testing.T, events <-chan domain.StreamEvent) []domain.StreamEvent {
	t.Helper()

	var collected []domain.StreamEvent
	for event := range events {
		collected = append(collected, event)
	}
	return collected
}

// TestChatRecordsOneRowPerCall is the invariant the ledger exists for: a call
// that reached a provider is recorded exactly once, with the model that answered
// and the task that caused it.
func TestChatRecordsOneRowPerCall(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()
	meta := domain.RequestMetadata{
		WorkspaceID: scope.WorkspaceID,
		TaskID:      uuid.New(),
		Purpose:     domain.PurposeAgent,
	}

	h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return(twoProviders(scope), nil)
	h.provider("gpt-4o-mini", func(p *fakeProvider) {
		p.chatFn = func(domain.ChatRequest) (domain.ChatResponse, error) {
			return domain.ChatResponse{
				Text:         "Sudah dicatat.",
				FinishReason: domain.FinishStop,
				Usage:        domain.Usage{InputTokens: 120, OutputTokens: 30},
			}, nil
		}
	})

	h.recorder.EXPECT().Record(mock.Anything, mock.MatchedBy(func(entry domain.UsageEntry) bool {
		return entry.WorkspaceID == scope.WorkspaceID &&
			entry.TaskID == meta.TaskID &&
			entry.Provider == domain.AdapterOpenAI &&
			entry.Model == "gpt-4o-mini" &&
			entry.Purpose == domain.PurposeAgent &&
			entry.Usage.InputTokens == 120
	})).Return(nil).Once()

	response, err := h.service.Chat(context.Background(), scope, chatRequest(meta))
	require.NoError(t, err)
	require.Equal(t, "Sudah dicatat.", response.Text)
	require.Equal(t, domain.AdapterOpenAI, response.Provider)
	require.Equal(t, []string{"gpt-4o-mini"}, h.factory.models(), "the first provider answered, so nothing fell back")
}

// TestChatFallsBackOnARetryableFailure: an outage moves to the next provider.
func TestChatFallsBackOnARetryableFailure(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()

	h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return(twoProviders(scope), nil)
	h.provider("gpt-4o-mini", func(p *fakeProvider) {
		p.chatFn = func(domain.ChatRequest) (domain.ChatResponse, error) {
			return domain.ChatResponse{}, fmt.Errorf("%w: 429", domain.ErrRateLimited)
		}
	})
	h.provider("claude-3-5-haiku", func(p *fakeProvider) {
		p.name = domain.AdapterAnthropic
		p.chatFn = func(domain.ChatRequest) (domain.ChatResponse, error) {
			return domain.ChatResponse{
				Text:  "Dicatat lewat cadangan.",
				Usage: domain.Usage{InputTokens: 90, OutputTokens: 20},
				Model: "claude-3-5-haiku",
			}, nil
		}
	})

	var recorded domain.UsageEntry
	h.recorder.EXPECT().Record(mock.Anything, mock.Anything).Run(func(_ context.Context, entry domain.UsageEntry) {
		recorded = entry
	}).Return(nil).Once()

	response, err := h.service.Chat(context.Background(), scope, chatRequest(domain.RequestMetadata{WorkspaceID: scope.WorkspaceID}))
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-4o-mini", "claude-3-5-haiku"}, h.factory.models())
	require.Equal(t, domain.AdapterAnthropic, response.Provider)
	require.Equal(t, domain.AdapterAnthropic, recorded.Provider, "the ledger names the provider that actually answered")
	require.Equal(t, domain.PurposeAgent, recorded.Purpose)
}

// TestChatDoesNotFallBackOnAMalformedRequest: the next provider would fail the
// same way, so the chain stops instead of spending every provider.
func TestChatDoesNotFallBackOnAMalformedRequest(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()

	h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return(twoProviders(scope), nil)
	h.provider("gpt-4o-mini", func(p *fakeProvider) {
		p.chatFn = func(domain.ChatRequest) (domain.ChatResponse, error) {
			return domain.ChatResponse{}, fmt.Errorf("%w: model does not exist", domain.ErrInvalidRequest)
		}
	})

	_, err := h.service.Chat(context.Background(), scope, chatRequest(domain.RequestMetadata{}))
	require.ErrorIs(t, err, domain.ErrInvalidRequest)
	require.Equal(t, []string{"gpt-4o-mini"}, h.factory.models())
}

// TestChatFallsBackWhenTheModelCannotServeTheRequest: a model without tools is
// not a reason to fail the task when another provider has them.
func TestChatFallsBackWhenTheModelCannotServeTheRequest(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()

	h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return(twoProviders(scope), nil)
	h.provider("gpt-4o-mini", func(p *fakeProvider) {
		// A small local model without tool support.
		p.caps = domain.Capabilities{Tools: false, Streaming: true}
	})
	h.provider("claude-3-5-haiku", func(p *fakeProvider) {
		p.name = domain.AdapterAnthropic
		p.chatFn = func(domain.ChatRequest) (domain.ChatResponse, error) {
			return domain.ChatResponse{Text: "ok", Usage: domain.Usage{InputTokens: 10, OutputTokens: 5}}, nil
		}
	})
	h.recorder.EXPECT().Record(mock.Anything, mock.Anything).Return(nil).Once()

	request := chatRequest(domain.RequestMetadata{})
	request.Tools = []domain.Tool{{Name: "gmail.search", Description: "Cari email"}}

	response, err := h.service.Chat(context.Background(), scope, request)
	require.NoError(t, err)
	require.Equal(t, "ok", response.Text)
	require.Equal(t, []string{"gpt-4o-mini", "claude-3-5-haiku"}, h.factory.models())
}

// TestChatStopsBeforeCallingWhenTheQuotaIsSpent: the provider must not be
// reached, because reaching it is what costs money.
func TestChatStopsBeforeCallingWhenTheQuotaIsSpent(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()
	h.withQuota(t, 1000)

	counter := h.withCounter(t)
	counter.EXPECT().Spent(mock.Anything, scope.WorkspaceID).Return(int64(1000), nil)

	_, err := h.service.Chat(context.Background(), scope, chatRequest(domain.RequestMetadata{}))
	require.ErrorIs(t, err, domain.ErrQuotaExceeded)
	require.Empty(t, h.factory.built, "no provider was built, so nothing was billed")
}

// TestChatUsesTheLedgerWhenTheCounterIsUnavailable: a Redis outage must not hand
// out a second allowance.
func TestChatUsesTheLedgerWhenTheCounterIsUnavailable(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()
	h.withQuota(t, 1000)

	counter := h.withCounter(t)
	counter.EXPECT().Spent(mock.Anything, scope.WorkspaceID).Return(int64(0), errors.New("redis is down"))
	h.reader.EXPECT().SumSince(mock.Anything, scope.WorkspaceID, mock.Anything).Return(int64(1200), nil)

	_, err := h.service.Chat(context.Background(), scope, chatRequest(domain.RequestMetadata{}))
	require.ErrorIs(t, err, domain.ErrQuotaExceeded)
	require.Empty(t, h.factory.built)
}

// TestChatReportsACallWhoseCostCouldNotBeWritten: a silent success would hide a
// billed call from the customer's own report.
func TestChatReportsACallWhoseCostCouldNotBeWritten(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()

	h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return(twoProviders(scope), nil)
	h.provider("gpt-4o-mini", func(p *fakeProvider) {
		p.chatFn = func(domain.ChatRequest) (domain.ChatResponse, error) {
			return domain.ChatResponse{Text: "ok", Usage: domain.Usage{InputTokens: 10, OutputTokens: 5}}, nil
		}
	})
	h.recorder.EXPECT().Record(mock.Anything, mock.Anything).Return(errors.New("database is down"))

	_, err := h.service.Chat(context.Background(), scope, chatRequest(domain.RequestMetadata{}))
	require.ErrorIs(t, err, domain.ErrUsageNotRecorded)
	require.False(t, domain.Retryable(err), "replaying a billed call would bill it twice")
}

// TestStreamFallsBackBeforeTheFirstByte: an error before any content is the one
// moment another provider can take over without duplicating an answer.
func TestStreamFallsBackBeforeTheFirstByte(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()

	h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return(twoProviders(scope), nil)
	h.provider("gpt-4o-mini", func(p *fakeProvider) {
		p.streamFn = func(domain.ChatRequest) (<-chan domain.StreamEvent, error) {
			events := make(chan domain.StreamEvent, 1)
			events <- domain.StreamEvent{Type: domain.EventError, Err: fmt.Errorf("%w: 503", domain.ErrProviderUnavailable)}
			close(events)
			return events, nil
		}
	})
	h.provider("claude-3-5-haiku", func(p *fakeProvider) {
		p.name = domain.AdapterAnthropic
		p.streamFn = func(domain.ChatRequest) (<-chan domain.StreamEvent, error) {
			return scriptedStream(
				domain.StreamEvent{Type: domain.EventText, Text: "Halo"},
				domain.StreamEvent{Type: domain.EventUsage, Usage: &domain.Usage{InputTokens: 12, OutputTokens: 4}},
				domain.StreamEvent{Type: domain.EventDone, FinishReason: domain.FinishStop},
			), nil
		}
	})
	h.recorder.EXPECT().Record(mock.Anything, mock.MatchedBy(func(entry domain.UsageEntry) bool {
		return entry.Provider == domain.AdapterAnthropic && entry.Usage.InputTokens == 12
	})).Return(nil).Once()

	events, err := h.service.Stream(context.Background(), scope, chatRequest(domain.RequestMetadata{}))
	require.NoError(t, err)

	collected := collect(t, events)
	require.Len(t, collected, 3)
	require.Equal(t, domain.EventText, collected[0].Type)
	require.Equal(t, "Halo", collected[0].Text)
	require.Equal(t, domain.EventDone, collected[2].Type)
	require.Equal(t, []string{"gpt-4o-mini", "claude-3-5-haiku"}, h.factory.models())
}

// TestStreamDoesNotRestartAfterContent: restarting mid-answer would duplicate
// what the caller already rendered, so the failure is reported instead.
func TestStreamDoesNotRestartAfterContent(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()

	h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return(twoProviders(scope), nil)
	h.provider("gpt-4o-mini", func(p *fakeProvider) {
		p.streamFn = func(domain.ChatRequest) (<-chan domain.StreamEvent, error) {
			return scriptedStream(
				domain.StreamEvent{Type: domain.EventText, Text: "Halo"},
				domain.StreamEvent{Type: domain.EventError, Err: fmt.Errorf("%w: dropped", domain.ErrProviderUnavailable)},
			), nil
		}
	})
	h.provider("claude-3-5-haiku", func(p *fakeProvider) {
		p.name = domain.AdapterAnthropic
	})
	h.recorder.EXPECT().Record(mock.Anything, mock.Anything).Return(nil).Once()

	events, err := h.service.Stream(context.Background(), scope, chatRequest(domain.RequestMetadata{}))
	require.NoError(t, err)

	collected := collect(t, events)
	require.Len(t, collected, 2)
	require.Equal(t, domain.EventText, collected[0].Type)
	require.Equal(t, domain.EventError, collected[1].Type)
	require.Equal(t, []string{"gpt-4o-mini"}, h.factory.models(), "the fallback was never built")
}

// TestStreamEndsWithAnErrorWhenTheProviderStopsSilently: a truncated answer must
// not be reported as finished.
func TestStreamEndsWithAnErrorWhenTheProviderStopsSilently(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()

	h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return(twoProviders(scope), nil)
	h.provider("gpt-4o-mini", func(p *fakeProvider) {
		p.streamFn = func(domain.ChatRequest) (<-chan domain.StreamEvent, error) {
			return scriptedStream(domain.StreamEvent{Type: domain.EventText, Text: "separuh"}), nil
		}
	})
	h.recorder.EXPECT().Record(mock.Anything, mock.Anything).Return(nil).Once()

	events, err := h.service.Stream(context.Background(), scope, chatRequest(domain.RequestMetadata{}))
	require.NoError(t, err)

	collected := collect(t, events)
	require.Len(t, collected, 2)
	require.Equal(t, domain.EventError, collected[1].Type)
	require.ErrorIs(t, collected[1].Err, domain.ErrProviderUnavailable)
}

// TestTestCallProvesTheKeyAndIsRecorded: the connectivity check is a real,
// billed call, so it lands in the ledger like any other.
func TestTestCallProvesTheKeyAndIsRecorded(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()
	provider := storedProvider(func(p *domain.StoredProvider) { p.WorkspaceID = scope.WorkspaceID })

	h.store.EXPECT().Get(mock.Anything, scope.WorkspaceID, provider.ID).Return(provider, nil)
	h.provider("gpt-4o-mini", func(p *fakeProvider) {
		p.chatFn = func(req domain.ChatRequest) (domain.ChatResponse, error) {
			require.Len(t, req.Messages, 1)
			require.Equal(t, 16, req.MaxTokens)
			return domain.ChatResponse{Text: "siap", Usage: domain.Usage{InputTokens: 8, OutputTokens: 2}}, nil
		}
	})
	h.recorder.EXPECT().Record(mock.Anything, mock.MatchedBy(func(entry domain.UsageEntry) bool {
		return entry.Purpose == domain.PurposeRouting
	})).Return(nil).Once()

	capabilities, err := h.service.Test(context.Background(), scope, provider.ID)
	require.NoError(t, err)
	require.True(t, capabilities.Tools)
}

// TestChatNeverAsksAModelThatCannotReadPictures is issue #237 at the chain: the
// picture is routed on, so a text-only provider is skipped before it is called
// rather than after it answers 400. A refusal costs a round trip and, with a
// provider that bills the attempt, money.
func TestChatNeverAsksAModelThatCannotReadPictures(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()

	chain := twoProviders(scope)
	h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return(chain, nil)

	asked := 0
	h.provider("gpt-4o-mini", func(p *fakeProvider) {
		p.caps.Vision = false
		p.chatFn = func(domain.ChatRequest) (domain.ChatResponse, error) {
			asked++
			return domain.ChatResponse{}, errors.New("a text-only model must never be asked to read a picture")
		}
	})
	h.provider(chain[1].Model, func(p *fakeProvider) {
		p.name = domain.AdapterAnthropic
		p.chatFn = func(req domain.ChatRequest) (domain.ChatResponse, error) {
			require.Len(t, req.Messages[0].Images, 1, "the provider that can see is the one that gets the picture")
			return domain.ChatResponse{Text: "Stok tinggal 3.", Usage: domain.Usage{InputTokens: 10, OutputTokens: 5}}, nil
		}
	})
	h.recorder.EXPECT().Record(mock.Anything, mock.Anything).Return(nil).Once()

	request := chatRequest(domain.RequestMetadata{})
	request.Messages[0].Images = []domain.Image{{MediaType: "image/png", Data: "aGVsbG8="}}

	response, err := h.service.Chat(context.Background(), scope, request)
	require.NoError(t, err)
	require.Equal(t, "Stok tinggal 3.", response.Text)
	require.Zero(t, asked, "the text-only provider was never asked")
	require.Equal(t, []string{"gpt-4o-mini", chain[1].Model}, h.factory.models())
}

// TestStreamRefusesAPictureBeforeCallingATextOnlyModel is the shape the chat
// responder sees: a chain whose only model is text-only fails with
// ErrUnsupported, which is what makes the reply ask again without the picture
// instead of ending as failed.
func TestStreamRefusesAPictureBeforeCallingATextOnlyModel(t *testing.T) {
	h := newHarness(t)
	scope := workspaceScope()

	h.store.EXPECT().List(mock.Anything, scope.WorkspaceID).Return(
		[]domain.StoredProvider{storedProvider(func(p *domain.StoredProvider) {
			p.WorkspaceID = scope.WorkspaceID
			p.Model = "gpt-4o-mini"
		})}, nil)

	asked := 0
	h.provider("gpt-4o-mini", func(p *fakeProvider) {
		p.caps.Vision = false
		p.streamFn = func(domain.ChatRequest) (<-chan domain.StreamEvent, error) {
			asked++
			return scriptedStream(domain.StreamEvent{Type: domain.EventDone, FinishReason: domain.FinishStop}), nil
		}
	})

	request := chatRequest(domain.RequestMetadata{})
	request.Messages[0].Images = []domain.Image{{MediaType: "image/png", Data: "aGVsbG8="}}

	_, err := h.service.Stream(context.Background(), scope, request)
	require.ErrorIs(t, err, domain.ErrUnsupported,
		"the responder retries without the pictures on exactly this error")
	require.Zero(t, asked, "the stream never started, so the provider was never called")
	require.False(t, domain.Retryable(err), "the next provider is tried by the chain, not by the caller")
}

// scriptedStream builds a closed channel that yields the events in order.
func scriptedStream(events ...domain.StreamEvent) <-chan domain.StreamEvent {
	out := make(chan domain.StreamEvent, len(events))
	for _, event := range events {
		out <- event
	}
	close(out)
	return out
}
