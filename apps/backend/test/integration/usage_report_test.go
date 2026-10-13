package integration

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	llmrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/repository"
	taskdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	taskrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/repository"
)

// EPIC 13's E13.5 gate: the numbers the usage screen reads are the rows the ledger holds, read three
// ways over one window.
//
// It runs against a real Postgres because the three reads are SQL — a total, a per-Bolu aggregation,
// and a per-task one — and the property under test is that they agree with the rows rather than with
// each other: an aggregate that silently dropped the rows made outside a task, or that joined the
// agent's name away when the Bolu was deleted, would pass a mocked test and be wrong on an invoice.

// TestTheUsageReportAgreesWithTheLedger inserts a known set of provider calls and reads the report
// back, asserting the whole of it rather than a spot check.
func TestTheUsageReportAgreesWithTheLedger(t *testing.T) {
	ctx := t.Context()
	pool := poolFor(t, appDatabase(t))

	user, err := createVerifiedUser(t, pool)
	require.NoError(t, err)

	workspace, err := onboardWorkspace(t, pool, user)
	require.NoError(t, err)

	// Two Bolu, so the per-Bolu read has something to group.
	created := seedAgents(t, pool, workspace, "Oren", "Ijo")
	agents := []uuid.UUID{created[0].ID, created[1].ID}

	// The task the calls are attributed to, created the way the runtime creates one.
	tasks := taskrepo.New(pool)
	createdTask, err := tasks.Create(ctx, taskdomain.Scope{UserID: user, WorkspaceID: workspace}, taskdomain.NewTask{
		AgentID: agents[0],
		Trigger: taskdomain.TriggerChat,
		Title:   "Rekap pesanan",
	})
	require.NoError(t, err)
	task := createdTask.ID

	ledger := llmrepo.New(pool)
	since := time.Now().Add(-time.Hour)

	// Three calls: two for Oren on the task, and one embedding with no task and no Bolu — which is
	// what a memory extraction looks like, and which the per-task rows must not claim.
	for _, entry := range []llmdomain.UsageEntry{
		{WorkspaceID: workspace, AgentID: agents[0], TaskID: task, Provider: "openai", Model: "gpt-4o-mini",
			Purpose: llmdomain.PurposeAgent, Usage: llmdomain.Usage{InputTokens: 1000, OutputTokens: 300}, CostMicros: 1_500_000},
		{WorkspaceID: workspace, AgentID: agents[0], TaskID: task, Provider: "openai", Model: "gpt-4o-mini",
			Purpose: llmdomain.PurposeAgent, Usage: llmdomain.Usage{InputTokens: 500, OutputTokens: 200}, CostMicros: 900_000},
		{WorkspaceID: workspace, AgentID: agents[1], Provider: "openai", Model: "text-embedding-3-small",
			Purpose: llmdomain.PurposeEmbedding, Usage: llmdomain.Usage{InputTokens: 120}, CostMicros: 10_000},
	} {
		require.NoError(t, ledger.Record(ctx, entry))
	}

	report, err := ledger.Report(ctx, workspace, since, 10)
	require.NoError(t, err)

	// The total is every call, including the one outside a task.
	require.Equal(t, int64(3), report.Totals.Calls)
	require.Equal(t, int64(1620), report.Totals.InputTokens)
	require.Equal(t, int64(500), report.Totals.OutputTokens)
	require.Equal(t, int64(2_410_000), report.Totals.CostMicros)

	// The per-Bolu read groups by the bot that made the call, dearest first, and names it.
	require.Len(t, report.ByAgent, 2)
	require.Equal(t, agents[0], report.ByAgent[0].AgentID)
	require.Equal(t, "Oren", report.ByAgent[0].AgentName)
	require.Equal(t, int64(2), report.ByAgent[0].Calls)
	require.Equal(t, int64(2_400_000), report.ByAgent[0].CostMicros)
	require.Equal(t, agents[1], report.ByAgent[1].AgentID)
	require.Equal(t, int64(1), report.ByAgent[1].Calls)

	// The per-task read groups by the work that caused it, and the calls outside a task are absent —
	// which is why the totals and the sum of these rows differ, and why both are on the report.
	require.Len(t, report.ByTask, 1)
	require.Equal(t, task, report.ByTask[0].TaskID)
	require.Equal(t, "Rekap pesanan", report.ByTask[0].TaskTitle)
	require.Equal(t, int64(2), report.ByTask[0].Calls)
	require.Equal(t, int64(1500), report.ByTask[0].InputTokens)
	require.Equal(t, int64(2_400_000), report.ByTask[0].CostMicros)

	// And the rows do not claim the total: the embedding is the difference, which is a fact about the
	// product rather than a rounding error.
	perTask := int64(0)
	for _, row := range report.ByTask {
		perTask += row.CostMicros
	}
	require.Equal(t, report.Totals.CostMicros-report.ByAgent[1].CostMicros, perTask)
}
