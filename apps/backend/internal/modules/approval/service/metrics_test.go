package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/approval/domain"
)

// recordingMetrics is the port the deployment implements, kept here so a test reads what the gate
// reported rather than what a mock was told to answer.
type recordingMetrics struct {
	decisions []decisionCall
}

type decisionCall struct {
	outcome  string
	revision bool
}

func (m *recordingMetrics) DraftDecided(_ context.Context, outcome string, revision bool) {
	m.decisions = append(m.decisions, decisionCall{outcome, revision})
}

// withMetrics installs the recorder on a harness.
func withMetrics(recorder *recordingMetrics) func(*Deps) {
	return func(deps *Deps) { deps.Metrics = recorder }
}

// TestEveryDecisionIsReportedWithTheStatusTheStoreRecorded: the outcome is the module's own status
// rather than a word invented at the call site, and the revision flag is what separates "approved as
// written" from "approved after being corrected" — the difference the product cares about, since
// only the first means the bot produced usable work.
func TestEveryDecisionIsReportedWithTheStatusTheStoreRecorded(t *testing.T) {
	recorder := &recordingMetrics{}
	h := newHarness(t, withMetrics(recorder))
	ctx := context.Background()

	outcome, err := h.service.Request(ctx, h.scope, h.request("gmail.send", `{"to":"a@b.c"}`))
	require.NoError(t, err)

	// A draft nobody has decided yet is not a decision: an empty gate reports nothing, which is
	// what keeps a chart's numerator and denominator the same population.
	require.Empty(t, recorder.decisions, "opening a draft is not a person deciding")

	_, err = h.service.Approve(ctx, h.scope, outcome.DraftID, uuid.MustParse(userID), "kirim")
	require.NoError(t, err)

	require.Len(t, recorder.decisions, 1)
	require.Equal(t, domain.StatusApproved, recorder.decisions[0].outcome)
	require.False(t, recorder.decisions[0].revision, "approved as written")

	// A revision is a decided draft too, and the store's own revision counter says the decision
	// followed a correction.
	revision, err := h.service.Request(ctx, h.scope, h.request("gmail.send", `{"to":"b@c.d"}`))
	require.NoError(t, err)

	_, err = h.service.Revise(ctx, h.scope, revision.DraftID, uuid.MustParse(userID), "nominalnya salah")
	require.NoError(t, err)

	require.Len(t, recorder.decisions, 2)
	require.Equal(t, domain.StatusRevise, recorder.decisions[1].outcome)
	require.True(t, recorder.decisions[1].revision)

	// And a canceled draft is reported as canceled rather than silently dropped.
	canceled, err := h.service.Request(ctx, h.scope, h.request("gmail.send", `{"to":"c@d.e"}`))
	require.NoError(t, err)

	_, err = h.service.Cancel(ctx, h.scope, canceled.DraftID, uuid.MustParse(userID), "batal")
	require.NoError(t, err)

	require.Len(t, recorder.decisions, 3)
	require.Equal(t, domain.StatusCanceled, recorder.decisions[2].outcome)
}

// TestAnUnwiredRecorderIsNotAFailure: a deployment with metrics off passes nothing, and every
// decision still lands.
func TestAnUnwiredRecorderIsNotAFailure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	outcome, err := h.service.Request(ctx, h.scope, h.request("gmail.send", `{"to":"a@b.c"}`))
	require.NoError(t, err)

	_, err = h.service.Approve(ctx, h.scope, outcome.DraftID, uuid.MustParse(userID), "kirim")
	require.NoError(t, err)
}
