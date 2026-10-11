package domain

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestRenderBlocksSummarisesWhatTheModelCannotRead is the helper both halves of
// the runtime use to turn a stored body back into text.
//
// A block the model cannot read is summarised rather than dropped: a model that
// forgot it already produced a chart would produce it again, and a second chart
// costs a whole round.
func TestRenderBlocksSummarisesWhatTheModelCannotRead(t *testing.T) {
	blocks := []Block{
		{Type: "text", Body: []byte(`{"type":"text","markdown":"Ada 3 pesanan."}`)},
		{Type: "chart", Body: []byte(`{"type":"chart"}`)},
		{Type: "mermaid", Body: []byte(`{"type":"mermaid"}`)},
		{Type: "html", Body: []byte(`{"type":"html"}`)},
		{Type: "table", Body: []byte(`{"type":"table"}`)},
		{Type: "draft", Body: []byte(`{"type":"draft"}`)},
	}

	rendered := RenderBlocks(blocks)
	require.Contains(t, rendered, "Ada 3 pesanan.")
	require.Contains(t, rendered, "[grafik]")
	require.Contains(t, rendered, "[diagram]")
	require.Contains(t, rendered, "[konten interaktif]")
	require.Contains(t, rendered, "[tabel]")
	require.Contains(t, rendered, "[draf menunggu persetujuan]")
}

// TestRenderBlocksDropsABodyItCannotRead covers the honest degradation: a text
// block whose body will not parse contributes nothing rather than failing the
// round the model is about to run.
func TestRenderBlocksDropsABodyItCannotRead(t *testing.T) {
	require.Empty(t, RenderBlocks([]Block{{Type: "text", Body: []byte(`bukan json`)}}))
	require.Empty(t, RenderBlocks(nil))
	require.Empty(t, RenderBlocks([]Block{{Type: "gambar", Body: []byte(`{}`)}}),
		"a block type this version does not know contributes nothing")
}

// TestRenderBlocksKeepsTheOrder is what makes the rendered thread readable: the
// model must read the conversation in the order it happened.
func TestRenderBlocksKeepsTheOrder(t *testing.T) {
	rendered := RenderBlocks([]Block{
		{Type: "text", Body: []byte(`{"type":"text","markdown":"pertama"}`)},
		{Type: "text", Body: []byte(`{"type":"text","markdown":"kedua"}`)},
	})

	require.Equal(t, "pertama\n\nkedua", rendered)
}

// TestWireIDRendersAbsenceAsAnEmptyString is the wire rule the whole API follows:
// encoding/json cannot omit a uuid.UUID — it is an array — so a zero value has to
// be rendered as an empty string or a client reads it as a real id.
func TestWireIDRendersAbsenceAsAnEmptyString(t *testing.T) {
	require.Empty(t, WireID(uuid.Nil))

	id := uuid.New()
	require.Equal(t, id.String(), WireID(id))
}

// TestTaskTotalsAndLiveness is the arithmetic and the liveness rule the bounds
// and the office view both read.
func TestTaskTotalsAndLiveness(t *testing.T) {
	task := Task{InputTokens: 120, OutputTokens: 40}
	require.Equal(t, int64(160), task.TotalTokens())

	for _, status := range []string{StatusQueued, StatusRunning, StatusWaitingApproval} {
		require.True(t, Task{Status: status}.Live(), "%s still occupies a Bolu", status)
	}
	for _, status := range []string{StatusSucceeded, StatusFailed, StatusCanceled, ""} {
		require.False(t, Task{Status: status}.Live(), "%s is finished", status)
	}
}

// TestScopeRequiresAWorkspace is the tenant guard: every task read is scoped to a
// workspace, so a scope without one is unusable however it is otherwise filled.
// Refusing it is what keeps a query from running unscoped.
func TestScopeRequiresAWorkspace(t *testing.T) {
	require.True(t, Scope{}.IsZero())
	require.True(t, Scope{UserID: uuid.New()}.IsZero(),
		"a user without a workspace cannot read a task")
	require.False(t, Scope{WorkspaceID: uuid.New()}.IsZero())
	require.False(t, Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}.IsZero())
}

// TestAgentRefAcceptsWorkUnlessItIsResting is the registry's rule as the runtime
// applies it: only the rest switch takes a Bolu out of service.
func TestAgentRefAcceptsWorkUnlessItIsResting(t *testing.T) {
	require.True(t, AgentRef{Stored: "active"}.Active())
	require.False(t, AgentRef{Stored: Resting}.Active())
	require.True(t, AgentRef{}.Active(), "an unknown switch is not a no: only resting is")
}

// TestMessageFromAgentTellsTheTurnsApart is what lets the model read a thread:
// the role of each turn comes from which author it carries.
func TestMessageFromAgentTellsTheTurnsApart(t *testing.T) {
	require.True(t, Message{AgentID: uuid.New()}.FromAgent())
	require.False(t, Message{UserID: uuid.New()}.FromAgent())
	require.False(t, Message{}.FromAgent())
}

// TestToolApprovalFollowsTheLabel is the product's central rule at its smallest:
// only an action that reaches outside Bolu needs a human.
func TestToolApprovalFollowsTheLabel(t *testing.T) {
	require.False(t, Tool{Label: LabelRead}.NeedsApproval())
	require.False(t, Tool{Label: LabelWriteInternal}.NeedsApproval(),
		"writing inside Bolu needs no approval: nothing leaves the workspace")
	require.True(t, Tool{Label: LabelWriteExternal}.NeedsApproval())
}

// TestUsageAsLLMUsagePricesTheSameNumbers is the seam between the runtime's token
// counts and the price table: the two must describe the same call.
func TestUsageAsLLMUsagePricesTheSameNumbers(t *testing.T) {
	usage := Usage{InputTokens: 120, OutputTokens: 40, CacheReadTokens: 10, CacheWriteTokens: 5}
	converted := usage.AsLLMUsage()

	require.Equal(t, usage.InputTokens, converted.InputTokens)
	require.Equal(t, usage.OutputTokens, converted.OutputTokens)
	require.Equal(t, usage.CacheReadTokens, converted.CacheReadTokens)
	require.Equal(t, usage.CacheWriteTokens, converted.CacheWriteTokens)
}
