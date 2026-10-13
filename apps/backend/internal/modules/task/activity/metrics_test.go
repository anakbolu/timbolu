package activity

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/workflow"
)

// recordingMetrics is the port the deployment implements, kept here so a test reads what the runtime
// reported rather than what a mock was told to answer.
type recordingMetrics struct {
	dispatched  int
	settled     int
	finished    []finishedCall
	tools       []toolCall
	draftReady  []float64
	decisions   []decisionCall
	retries     []retryCall
	modelCalls  []modelCall
	rateLimited []string
}

type finishedCall struct {
	trigger, status                       string
	seconds                               float64
	inputTokens, outputTokens, costMicros int64
}

type toolCall struct {
	app     string
	failed  bool
	seconds float64
}

type decisionCall struct {
	outcome  string
	revision bool
}

type retryCall struct {
	activity string
	attempt  int
}

type modelCall struct {
	provider, purpose string
	fellBack          bool
	seconds           float64
}

// Implementations of the port.
func (m *recordingMetrics) TaskDispatched(context.Context) { m.dispatched++ }
func (m *recordingMetrics) TaskSettled(context.Context)    { m.settled++ }

func (m *recordingMetrics) TaskFinished(_ context.Context, trigger, status string, seconds float64, in, out, cost int64) {
	m.finished = append(m.finished, finishedCall{trigger, status, seconds, in, out, cost})
}

func (m *recordingMetrics) ToolCall(_ context.Context, app string, failed bool, seconds float64) {
	m.tools = append(m.tools, toolCall{app, failed, seconds})
}

func (m *recordingMetrics) DraftReady(_ context.Context, seconds float64) {
	m.draftReady = append(m.draftReady, seconds)
}

func (m *recordingMetrics) DraftDecided(_ context.Context, outcome string, revision bool) {
	m.decisions = append(m.decisions, decisionCall{outcome, revision})
}

func (m *recordingMetrics) ActivityRetry(_ context.Context, activity string, attempt int) {
	m.retries = append(m.retries, retryCall{activity, attempt})
}

func (m *recordingMetrics) ModelCall(_ context.Context, provider, purpose string, fellBack bool, seconds float64) {
	m.modelCalls = append(m.modelCalls, modelCall{provider, purpose, fellBack, seconds})
}

func (m *recordingMetrics) RateLimited(_ context.Context, surface string) {
	m.rateLimited = append(m.rateLimited, surface)
}

// withMetrics installs the recorder on a harness.
func withMetrics(recorder *recordingMetrics) func(*Deps) {
	return func(deps *Deps) { deps.Metrics = recorder }
}

// TestAFinishedTaskReportsItsOwnNumbers: the tokens, the cost, and the duration are read from the
// stored row rather than recomputed, so what a chart shows is what the ledger holds and what the
// dashboard shows beside it must agree.
func TestAFinishedTaskReportsItsOwnNumbers(t *testing.T) {
	recorder := &recordingMetrics{}
	h := newHarness(t, domain.DefaultLimits, withMetrics(recorder))

	started := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	finished := started.Add(90 * time.Second)
	pinned := started
	h.activities.deps.Clock = func() time.Time { return finished }

	// The task is stored as started, so the duration is the row's own rather than anything this
	// activity made up.
	_, err := h.tasks.mutate(h.scope, h.task.ID, func(task *domain.Task) error {
		task.StartedAt = &pinned

		return nil
	})
	require.NoError(t, err)

	outcome, err := h.activities.CompleteTask(context.Background(), workflow.CompleteRequest{
		TaskID:       h.task.ID.String(),
		WorkspaceID:  h.scope.WorkspaceID.String(),
		Status:       domain.StatusSucceeded,
		InputTokens:  1200,
		OutputTokens: 340,
		CostMicros:   2100,
		StepCount:    3,
	})
	require.NoError(t, err)
	require.Equal(t, domain.StatusSucceeded, outcome.Status)

	require.Equal(t, 1, recorder.settled, "the terminal state brings the in-flight depth down")
	require.Len(t, recorder.finished, 1)

	call := recorder.finished[0]
	require.Equal(t, domain.TriggerChat, call.trigger)
	require.Equal(t, domain.StatusSucceeded, call.status)
	require.InDelta(t, 90, call.seconds, 0.001)
	require.Equal(t, int64(1200), call.inputTokens)
	require.Equal(t, int64(340), call.outputTokens)
	require.Equal(t, int64(2100), call.costMicros)
}

// TestAToolCallIsReportedByItsIntegration: the label an operator acts on is the integration, and it
// is derived from the tool's own name — which the registry keeps to `<app>.<operation>`.
func TestAToolCallIsReportedByItsIntegration(t *testing.T) {
	recorder := &recordingMetrics{}
	h := newHarness(t, domain.DefaultLimits, withMetrics(recorder))
	h.executor.content = json.RawMessage(`{"ok":true}`)

	_, err := h.activities.RunTool(context.Background(), workflow.ToolRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		Name:        "whatsapp.list_messages",
		Arguments:   json.RawMessage(`{}`),
	})
	require.NoError(t, err)
	require.Len(t, recorder.tools, 1)
	require.Equal(t, "whatsapp", recorder.tools[0].app)
	require.False(t, recorder.tools[0].failed)

	// A call this deployment could not serve is reported too, and as a failure: it is a call that
	// did not reach the provider, which is exactly what the chart exists to show.
	h.activities.deps.Executor = nil
	_, err = h.activities.RunTool(context.Background(), workflow.ToolRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		Name:        "gmail.search",
		Arguments:   json.RawMessage(`{}`),
	})
	require.ErrorIs(t, err, domain.ErrToolNotAvailable)

	require.Len(t, recorder.tools, 2)
	require.Equal(t, "gmail", recorder.tools[1].app)
	require.True(t, recorder.tools[1].failed)
}

// TestAToolWithAnUnexpectedNameFallsBackToTheWholeName: a name that does not follow the convention
// is still bounded by the registry, and the alternative — cutting it at the first dot — would file
// unrelated tools under one invented integration.
func TestAToolWithAnUnexpectedNameFallsBackToTheWholeName(t *testing.T) {
	require.Equal(t, "gmail", appOfTool("gmail.search"))
	require.Equal(t, "list_messages", appOfTool("list_messages"))
	require.Equal(t, "", appOfTool(""))
}

// TestADraftsReadinessIsMeasuredFromTheTaskOpening: the promise is "the work is done, here it is", so
// a task that thought for three rounds before drafting took all three.
func TestADraftsReadinessIsMeasuredFromTheTaskOpening(t *testing.T) {
	recorder := &recordingMetrics{}

	opened := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	now := opened.Add(4 * time.Minute)

	h := newHarness(t, domain.DefaultLimits, withMetrics(recorder))
	h.activities.deps.Clock = func() time.Time { return now }

	_, err := h.tasks.mutate(h.scope, h.task.ID, func(task *domain.Task) error {
		task.StartedAt = &opened

		return nil
	})
	require.NoError(t, err)

	send := domain.Tool{Name: "gmail.send", Label: domain.LabelWriteExternal, Schema: json.RawMessage(`{}`)}
	h.activities.deps.Tools = stubTools{tools: []domain.Tool{send}}

	_, err = h.activities.RequestDraft(context.Background(), workflow.ToolRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		Name:        "gmail.send",
		Arguments:   json.RawMessage(`{"to":"a@b.c"}`),
	})
	require.NoError(t, err)

	require.Len(t, recorder.draftReady, 1)
	require.InDelta(t, 240, recorder.draftReady[0], 0.001, "four minutes from the task opening")
}

// TestNoRetryIsReportedOutsideAnActivity: the attempt number comes from Temporal, so a call that is
// not running as an activity has none — and asking for it would panic rather than report.
func TestNoRetryIsReportedOutsideAnActivity(t *testing.T) {
	recorder := &recordingMetrics{}
	h := newHarness(t, domain.DefaultLimits, withMetrics(recorder))

	require.NotPanics(t, func() { h.activities.retry(context.Background(), "RunTool") })
	require.Empty(t, recorder.retries, "a direct call is not a retry")
}

// TestNoMetricsIsNotAFailure: a deployment with metrics off passes nothing, and every activity still
// runs — which is what makes the modules call through unconditionally.
func TestNoMetricsIsNotAFailure(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits)
	h.executor.content = json.RawMessage(`{"ok":true}`)

	_, err := h.activities.RunTool(context.Background(), workflow.ToolRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		Name:        "gmail.search",
		Arguments:   json.RawMessage(`{}`),
	})
	require.NoError(t, err)

	_, err = h.activities.CompleteTask(context.Background(), workflow.CompleteRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		Status:      domain.StatusSucceeded,
	})
	require.NoError(t, err)
}
