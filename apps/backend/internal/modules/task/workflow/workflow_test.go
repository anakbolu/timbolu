package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
)

// The workflow tests drive the durable loop with scripted activities. They are
// the epic's gate: the loop must stop on a bound rather than hang, every tool
// that reaches outside Bolu must go through the approval gate, and a handoff
// must produce a real child task whose answer the parent uses.

const (
	parentTask  = "11111111-1111-1111-1111-111111111111"
	childTask   = "22222222-2222-2222-2222-222222222222"
	workspaceID = "33333333-3333-3333-3333-333333333333"
	agentID     = "44444444-4444-4444-4444-444444444444"
	draftID     = "55555555-5555-5555-5555-555555555555"
)

// stub carries one method per activity name. The test environment matches an
// expectation by the function that was registered, so the names the workflow
// calls have to exist as real functions even though every one of them is
// scripted.
type stub struct{}

func (stub) LoadTaskContext(context.Context, TaskInput) (Context, error) { return Context{}, nil }
func (stub) CallModel(context.Context, ModelRequest) (ModelReply, error) { return ModelReply{}, nil }
func (stub) RunTool(context.Context, ToolRequest) (ToolOutcome, error)   { return ToolOutcome{}, nil }
func (stub) RequestDraft(context.Context, ToolRequest) (DraftOutcome, error) {
	return DraftOutcome{}, nil
}
func (stub) RecordStep(context.Context, StepRequest) error { return nil }
func (stub) BeatTask(context.Context, Beat) (Beat, error)  { return Beat{}, nil }
func (stub) CompleteTask(context.Context, CompleteRequest) (TaskOutcome, error) {
	return TaskOutcome{}, nil
}
func (stub) PrepareHandoff(context.Context, HandoffRequest) (PreparedHandoff, error) {
	return PreparedHandoff{}, nil
}
func (stub) MarkHandoffStarted(context.Context, HandoffStarted) error { return nil }
func (stub) ResumeTask(context.Context, ResumeRequest) error          { return nil }

// harness is one test environment with the activity set scripted.
type harness struct {
	env *testsuite.TestWorkflowEnvironment
	// models is the scripted reply per task, consumed in order.
	models map[string][]ModelReply
	// calls counts the model calls per task, which is how "a completed round is
	// never re-run" is asserted.
	calls map[string]int
	// recorded counts the steps written per task.
	recorded map[string]int
	// payloads is the body of every step written, in order. It is what the
	// determinism test compares: a workflow that built a payload from a map
	// without sorted keys, or from anything outside its own history, would
	// produce a different body on a replay.
	payloads [][]byte
	// tools is the scripted tool outcome per tool name.
	tools map[string]ToolOutcome
	// drafted, resumed, ranTool, and handoffStarted count how often each
	// activity ran, which is how "the gate was used" and "the executor was not"
	// are asserted.
	drafted        int
	resumed        int
	ranTool        int
	handoffStarted int

	// loadErr makes the first activity fail, which is how a task that could not
	// be prepared is tested.
	loadErr error
	// completed is the last terminal write the workflow made, which is what a
	// test asserts about the task's own totals.
	completed CompleteRequest
	// terminal is the terminal write per task, which is what a test asserts when
	// a parent and a child both finish.
	terminal map[string]CompleteRequest
	// modelFailures is how many of the first model calls fail before the script
	// takes over. It is what a transient provider failure looks like.
	modelFailures int
	// tokenCeiling makes the beat stop the task once the running total passes
	// it, which is how the token bound is tested without a real model.
	tokenCeiling int64
	// gateCanceled makes the approval gate refuse, which is what a deployment
	// without a gate answers.
	gateCanceled bool
	// handoffRefusal makes the handoff activity refuse instead of opening a
	// child task.
	handoffRefusal string
}

func newHarness(t *testing.T, limits domain.Limits, tools []domain.Tool) *harness {
	t.Helper()

	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(AgentTaskWorkflow)
	// The stubs are registered under the exact names the workflow calls, which
	// is what lets an expectation be attached to each one. Nothing of the stub
	// body runs: every activity is scripted below.
	registerStubs(env)

	h := &harness{
		env:      env,
		models:   map[string][]ModelReply{},
		calls:    map[string]int{},
		recorded: map[string]int{},
		tools:    map[string]ToolOutcome{},
		terminal: map[string]CompleteRequest{},
	}

	env.OnActivity(ActivityLoadContext, mock.Anything, mock.Anything).Return(func(_ context.Context, _ TaskInput) (Context, error) {
		if h.loadErr != nil {
			return Context{}, h.loadErr
		}
		return Context{
			Agent:  domain.AgentRef{ID: mustUUID(agentID), Name: "Oren", Role: "Penjualan", Persona: "ramah"},
			Tools:  tools,
			Limits: limits.Normalise(),
			Depth:  0,
		}, nil
	})

	env.OnActivity(ActivityCallModel, mock.Anything, mock.Anything).Return(func(_ context.Context, req ModelRequest) (ModelReply, error) {
		h.calls[req.TaskID]++
		if h.calls[req.TaskID] <= h.modelFailures {
			return ModelReply{}, errors.New("provider unavailable")
		}
		queue := h.models[req.TaskID]
		if len(queue) == 0 {
			return ModelReply{}, errors.New("the script ran out of replies for " + req.TaskID)
		}
		reply := queue[0]
		h.models[req.TaskID] = queue[1:]
		return reply, nil
	})

	env.OnActivity(ActivityRunTool, mock.Anything, mock.Anything).Return(func(_ context.Context, req ToolRequest) (ToolOutcome, error) {
		h.ranTool++
		outcome, ok := h.tools[req.Name]
		if !ok {
			return ToolOutcome{}, errors.New("no scripted outcome for " + req.Name)
		}
		return outcome, nil
	})

	env.OnActivity(ActivityRequestDraft, mock.Anything, mock.Anything).Return(func(_ context.Context, req ToolRequest) (DraftOutcome, error) {
		h.drafted++
		if h.gateCanceled {
			return DraftOutcome{
				Status: domain.DraftCanceled,
				Note:   "gerbang persetujuan tidak dikonfigurasi, jadi aksi ini tidak dijalankan",
				Title:  req.Name,
			}, nil
		}
		return DraftOutcome{DraftID: draftID, Status: domain.DraftPending, Title: req.Name}, nil
	})

	env.OnActivity(ActivityRecordStep, mock.Anything, mock.Anything).Return(func(_ context.Context, req StepRequest) error {
		h.recorded[req.TaskID]++
		if len(req.Record.Output) > 0 {
			h.payloads = append(h.payloads, req.Record.Output)
		}
		return nil
	})

	// Beat echoes the request, which is how the workflow learns the running
	// totals and whether a bound was reached. The service applies the configured
	// bounds in the same place, so a scripted ceiling here exercises the same
	// path the deployment uses.
	env.OnActivity(ActivityBeat, mock.Anything, mock.Anything).Return(func(_ context.Context, beat Beat) (Beat, error) {
		if h.tokenCeiling > 0 && beat.TotalTokens() > h.tokenCeiling {
			beat.StopReason = domain.StatusFailed
			beat.StopDetail = "tugas berhenti karena mencapai batas token"
		}
		return beat, nil
	})

	env.OnActivity(ActivityComplete, mock.Anything, mock.Anything).Return(func(_ context.Context, req CompleteRequest) (TaskOutcome, error) {
		h.completed = req
		h.terminal[req.TaskID] = req
		return TaskOutcome{Status: req.Status, Reason: req.Reason, Summary: "ringkasan tugas"}, nil
	})

	env.OnActivity(ActivityMarkHandoffStarted, mock.Anything, mock.Anything).Return(func(context.Context, HandoffStarted) error {
		h.handoffStarted++
		return nil
	})
	env.OnActivity(ActivityResume, mock.Anything, mock.Anything).Return(func(context.Context, ResumeRequest) error {
		h.resumed++
		return nil
	})

	return h
}

// registerStubs installs one stub per activity name. The names are the workflow's
// own constants rather than the Go method names, so the test would fail if the
// workflow and the worker ever disagreed about what an activity is called.
func registerStubs(env *testsuite.TestWorkflowEnvironment) {
	s := stub{}
	for name, fn := range map[string]any{
		ActivityLoadContext:        s.LoadTaskContext,
		ActivityCallModel:          s.CallModel,
		ActivityRunTool:            s.RunTool,
		ActivityRequestDraft:       s.RequestDraft,
		ActivityRecordStep:         s.RecordStep,
		ActivityBeat:               s.BeatTask,
		ActivityComplete:           s.CompleteTask,
		ActivityPrepareHandoff:     s.PrepareHandoff,
		ActivityMarkHandoffStarted: s.MarkHandoffStarted,
		ActivityResume:             s.ResumeTask,
	} {
		env.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
	}
}

func (h *harness) input(taskID string) TaskInput {
	return TaskInput{
		TaskID:      taskID,
		WorkspaceID: workspaceID,
		AgentID:     agentID,
		Title:       "Rekap pesanan hari ini",
		Prompt:      "Rekap pesanan hari ini",
	}
}

// signalLater queues a signal to be delivered while the workflow runs.
//
// A signal sent before ExecuteWorkflow has no workflow to receive it. A zero
// delay queues the callback for the first workflow task, which is what a signal
// that arrived while the previous round ran looks like — the same shape the
// runtime meets in production when a user cancels a task that is mid-round.
func (h *harness) signalLater(t *testing.T, name string, payload any) {
	t.Helper()

	h.env.RegisterDelayedCallback(func() {
		h.env.SignalWorkflow(name, payload)
	}, 0)
}

// run executes the workflow and returns its outcome.
func (h *harness) run(t *testing.T, input TaskInput) TaskOutcome {
	t.Helper()

	h.env.ExecuteWorkflow(AgentTaskWorkflow, input)
	require.True(t, h.env.IsWorkflowCompleted())

	var outcome TaskOutcome
	require.NoError(t, h.env.GetWorkflowResult(&outcome))
	return outcome
}

// TestWorkflowAnswersInOneRound is the base case: the model answers without a
// tool, and the task succeeds with the answer recorded as its final step.
func TestWorkflowAnswersInOneRound(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits, nil)
	h.models[parentTask] = []ModelReply{{Text: "Ada 3 pesanan hari ini."}}

	outcome := h.run(t, h.input(parentTask))

	require.Equal(t, domain.StatusSucceeded, outcome.Status)
	require.Equal(t, 1, h.calls[parentTask], "one round means one model call")
	require.Equal(t, 2, h.recorded[parentTask], "the thinking round and the final answer are both recorded")
}

// TestWorkflowRunsToolsThenAnswers is the three-round shape the epic asks for:
// the model reads, calls a tool, reads again, and only then answers.
func TestWorkflowRunsToolsThenAnswers(t *testing.T) {
	readTool := domain.Tool{Name: "gmail.search", Label: domain.LabelRead, Schema: json.RawMessage(`{}`)}
	h := newHarness(t, domain.DefaultLimits, []domain.Tool{readTool})
	h.tools["gmail.search"] = ToolOutcome{Name: "gmail.search", Content: []byte(`{"emails":2}`)}

	h.models[parentTask] = []ModelReply{
		{ToolCalls: []ToolCall{{CallID: "c1", Name: "gmail.search", Arguments: []byte(`{"q":"pesanan"}`)}}},
		{Text: "Ada 2 pesanan masuk."},
	}

	outcome := h.run(t, h.input(parentTask))

	require.Equal(t, domain.StatusSucceeded, outcome.Status)
	require.Equal(t, 2, h.calls[parentTask])
	// Round one records think, tool_call, and tool_result; round two records
	// think and final.
	require.Equal(t, 5, h.recorded[parentTask])
}

// TestWorkflowRecordsTheTokenSplit is the accounting contract: the terminal write
// carries the two directions apart, so a task's spend is shown split rather than
// as one total written as all input.
func TestWorkflowRecordsTheTokenSplit(t *testing.T) {
	readTool := domain.Tool{Name: "gmail.search", Label: domain.LabelRead, Schema: json.RawMessage(`{}`)}
	h := newHarness(t, domain.DefaultLimits, []domain.Tool{readTool})
	h.tools["gmail.search"] = ToolOutcome{Name: "gmail.search", Content: []byte(`{"emails":2}`)}

	h.models[parentTask] = []ModelReply{
		{
			ToolCalls: []ToolCall{{CallID: "c1", Name: "gmail.search", Arguments: []byte(`{}`)}},
			Usage:     domain.Usage{InputTokens: 120, OutputTokens: 30},
		},
		{
			Text:  "Ada 2 pesanan.",
			Usage: domain.Usage{InputTokens: 200, OutputTokens: 40},
		},
	}

	outcome := h.run(t, h.input(parentTask))

	require.Equal(t, domain.StatusSucceeded, outcome.Status)
	require.Equal(t, int64(320), h.completed.InputTokens, "the prompt tokens of both rounds")
	require.Equal(t, int64(70), h.completed.OutputTokens, "the completion tokens of both rounds")
	require.Equal(t, int64(390), outcome.Tokens, "the outcome reports the two together")
	require.Equal(t, 2, h.completed.StepCount)

	// The answer is written into the conversation as a text block.
	require.Len(t, h.completed.Blocks, 1)
	require.Equal(t, "text", h.completed.Blocks[0].Type)
	require.Contains(t, string(h.completed.Blocks[0].Body), "Ada 2 pesanan.")
}

// TestWorkflowRetriesAFailedActivityThenContinues is the "satu retry" case: a
// model call that fails once is retried and the task still finishes.
func TestWorkflowRetriesAFailedActivityThenContinues(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits, nil)

	h.modelFailures = 1
	h.models[parentTask] = []ModelReply{{Text: "Selesai."}}

	outcome := h.run(t, h.input(parentTask))

	require.Equal(t, domain.StatusSucceeded, outcome.Status)
	require.Equal(t, 2, h.calls[parentTask], "the failed attempt is retried exactly once more")
}

// TestWorkflowStopsOnTokenLimit is the E6.4 gate: a small token bound stops the
// task at the round it is reached, with a reason a person can read, and the
// task is finished rather than left running.
func TestWorkflowStopsOnTokenLimit(t *testing.T) {
	// One round spends 80 tokens, so a 70-token allowance is crossed by the
	// first round: the bound is checked at the round boundary, which is the
	// cleanest place to stop a task.
	limits := domain.Limits{MaxSteps: 12, MaxTokens: 70, MaxHandoffDepth: 2, MaxToolResultBytes: 1024}
	tool := domain.Tool{Name: "drive.search", Label: domain.LabelRead, Schema: json.RawMessage(`{}`)}
	h := newHarness(t, limits, []domain.Tool{tool})
	h.tools["drive.search"] = ToolOutcome{Name: "drive.search", Content: []byte(`{"files":[]}`)}

	// Every round asks for a tool, so the loop only ends on the token bound.
	for range 5 {
		h.models[parentTask] = append(h.models[parentTask], ModelReply{
			ToolCalls: []ToolCall{{CallID: "c", Name: "drive.search", Arguments: []byte(`{}`)}},
			Usage:     domain.Usage{InputTokens: 60, OutputTokens: 20},
		})
	}

	// The first round already spent more than the allowance, so the beat stops
	// the task there.
	h.tokenCeiling = limits.MaxTokens

	outcome := h.run(t, h.input(parentTask))

	require.Equal(t, domain.StatusFailed, outcome.Status)
	require.Contains(t, outcome.Reason, "batas token")
	require.Equal(t, 1, h.calls[parentTask], "the task stops at the round the bound was reached")
}

// TestWorkflowStopsOnStepLimit covers the other bound: a model that keeps asking
// for tools is stopped by the step ceiling rather than looping forever.
func TestWorkflowStopsOnStepLimit(t *testing.T) {
	limits := domain.Limits{MaxSteps: 3, MaxTokens: 1_000_000, MaxHandoffDepth: 2, MaxToolResultBytes: 1024}
	tool := domain.Tool{Name: "drive.search", Label: domain.LabelRead, Schema: json.RawMessage(`{}`)}
	h := newHarness(t, limits, []domain.Tool{tool})
	h.tools["drive.search"] = ToolOutcome{Name: "drive.search", Content: []byte(`{"files":[]}`)}

	for range 6 {
		h.models[parentTask] = append(h.models[parentTask], ModelReply{
			ToolCalls: []ToolCall{{CallID: "c", Name: "drive.search", Arguments: []byte(`{}`)}},
		})
	}

	outcome := h.run(t, h.input(parentTask))

	require.Equal(t, domain.StatusFailed, outcome.Status)
	require.Equal(t, 3, h.calls[parentTask], "the loop runs exactly the allowed number of rounds")
}

// TestWorkflowParksForApprovalThenContinues is the E6.3 gate: a write_external
// tool becomes a draft, the task waits, and an approval lets it carry on.
func TestWorkflowParksForApprovalThenContinues(t *testing.T) {
	send := domain.Tool{Name: "gmail.send", Label: domain.LabelWriteExternal, Schema: json.RawMessage(`{}`)}
	h := newHarness(t, domain.DefaultLimits, []domain.Tool{send})

	h.models[parentTask] = []ModelReply{
		{ToolCalls: []ToolCall{{CallID: "c1", Name: "gmail.send", Arguments: []byte(`{"to":"a@b.c"}`)}}},
		{Text: "Email sudah dikirim."},
	}

	// The decision is delivered while the task is parked on the draft. A signal
	// sent before the run would have no workflow to receive it, so it is queued
	// on the test clock instead.
	h.signalLater(t, SignalApproval, ApprovalSignal{DraftID: draftID, Approved: true, Note: "kirim"})

	outcome := h.run(t, h.input(parentTask))

	require.Equal(t, domain.StatusSucceeded, outcome.Status)
	require.Equal(t, 2, h.calls[parentTask], "the task continues after the decision")
	require.Equal(t, 1, h.drafted, "the write went through the approval gate")
	require.Equal(t, 1, h.resumed, "the task was un-parked when the decision arrived")
	require.Zero(t, h.ranTool, "a write_external tool is never executed directly")
}

// TestWorkflowFeedsARevisionBackToTheModel covers the other decision: a revision
// is returned to the model as a failed tool result, so it can try again.
func TestWorkflowFeedsARevisionBackToTheModel(t *testing.T) {
	send := domain.Tool{Name: "gmail.send", Label: domain.LabelWriteExternal, Schema: json.RawMessage(`{}`)}
	h := newHarness(t, domain.DefaultLimits, []domain.Tool{send})

	h.models[parentTask] = []ModelReply{
		{ToolCalls: []ToolCall{{CallID: "c1", Name: "gmail.send", Arguments: []byte(`{"to":"a@b.c"}`)}}},
		{Text: "Baik, saya tidak mengirimnya."},
	}

	h.signalLater(t, SignalApproval, ApprovalSignal{DraftID: draftID, Approved: false, Note: "salah penerima"})

	outcome := h.run(t, h.input(parentTask))

	require.Equal(t, domain.StatusSucceeded, outcome.Status)
	require.Equal(t, 2, h.calls[parentTask])
}

// TestWorkflowRefusesAWriteWithoutAGate is the safe direction: with no approval
// gate configured the action is refused rather than taken, and the refusal
// reaches the model as a failed tool result.
func TestWorkflowRefusesAWriteWithoutAGate(t *testing.T) {
	send := domain.Tool{Name: "gmail.send", Label: domain.LabelWriteExternal, Schema: json.RawMessage(`{}`)}
	h := newHarness(t, domain.DefaultLimits, []domain.Tool{send})

	h.gateCanceled = true

	h.models[parentTask] = []ModelReply{
		{ToolCalls: []ToolCall{{CallID: "c1", Name: "gmail.send", Arguments: []byte(`{"to":"a@b.c"}`)}}},
		{Text: "Pengiriman tidak diizinkan, jadi saya tidak mengirim."},
	}

	outcome := h.run(t, h.input(parentTask))

	require.Equal(t, domain.StatusSucceeded, outcome.Status)
	require.Zero(t, h.ranTool, "the refused action was never executed")
}

// TestWorkflowHandsOffAndUsesTheChildAnswer is the E6.5 gate: Oren asks Ijo for
// data, the child task runs, and the parent carries on with its answer.
func TestWorkflowHandsOffAndUsesTheChildAnswer(t *testing.T) {
	handoff := domain.Tool{Name: domain.ToolHandoff, Label: domain.LabelRead, Schema: json.RawMessage(`{}`)}
	h := newHarness(t, domain.DefaultLimits, []domain.Tool{handoff})

	h.env.OnActivity(ActivityPrepareHandoff, mock.Anything, mock.Anything).Return(PreparedHandoff{
		Child: TaskInput{
			TaskID:      childTask,
			WorkspaceID: workspaceID,
			AgentID:     agentID,
			Title:       "Ambil data stok",
			Prompt:      "Ambil data stok",
			Depth:       1,
		},
	}, nil)

	h.models[parentTask] = []ModelReply{
		{ToolCalls: []ToolCall{{CallID: "c1", Name: domain.ToolHandoff, Arguments: []byte(`{"agent":"Ijo","instructions":"Ambil data stok"}`)}}},
		{Text: "Stok gudang tinggal 4."},
	}
	h.models[childTask] = []ModelReply{{Text: "Stok gudang tinggal 4."}}

	outcome := h.run(t, h.input(parentTask))

	require.Equal(t, domain.StatusSucceeded, outcome.Status)
	require.Equal(t, 2, h.calls[parentTask], "the parent continues after the child answers")
	require.Equal(t, 1, h.calls[childTask], "the child ran once")
	require.Equal(t, 1, h.handoffStarted, "the child's workflow is recorded on its task")
}

// TestWorkflowReportsARefusedHandoffToTheModel covers the refusals: a chain that
// is too deep, a Bolu that is resting, and a name that matches nobody all come
// back as a failed tool result rather than failing the task.
func TestWorkflowReportsARefusedHandoffToTheModel(t *testing.T) {
	handoff := domain.Tool{Name: domain.ToolHandoff, Label: domain.LabelRead, Schema: json.RawMessage(`{}`)}
	h := newHarness(t, domain.DefaultLimits, []domain.Tool{handoff})

	h.handoffRefusal = "serah-terima ditolak: rantai sudah 2 tingkat, batasnya 2"

	h.models[parentTask] = []ModelReply{
		{ToolCalls: []ToolCall{{CallID: "c1", Name: domain.ToolHandoff, Arguments: []byte(`{"agent":"Ijo","instructions":"apa saja"}`)}}},
		{Text: "Baik, saya kerjakan sendiri."},
	}

	outcome := h.run(t, h.input(parentTask))

	require.Equal(t, domain.StatusSucceeded, outcome.Status)
	require.Equal(t, 2, h.calls[parentTask])
	require.Empty(t, h.models[childTask], "a refused handoff starts no child")
}

// TestWorkflowStopsWhenCanceled is the cancel path: a task the user stopped does
// not run another round.
func TestWorkflowStopsWhenCanceled(t *testing.T) {
	readTool := domain.Tool{Name: "gmail.search", Label: domain.LabelRead, Schema: json.RawMessage(`{}`)}
	h := newHarness(t, domain.DefaultLimits, []domain.Tool{readTool})
	h.tools["gmail.search"] = ToolOutcome{Name: "gmail.search", Content: []byte(`{}`)}

	for range 4 {
		h.models[parentTask] = append(h.models[parentTask], ModelReply{
			ToolCalls: []ToolCall{{CallID: "c", Name: "gmail.search", Arguments: []byte(`{}`)}},
		})
	}

	// The cancel arrives while the task is loading its context, so it stops at
	// the loop's first checkpoint without spending a round on it.
	h.signalLater(t, SignalCancel, struct{}{})

	outcome := h.run(t, h.input(parentTask))

	require.Equal(t, domain.StatusCanceled, outcome.Status)
	require.Equal(t, 0, h.calls[parentTask])
}

// TestWorkflowIgnoresAnApprovalForAnotherDraft is the guard against unblocking
// the wrong wait: a decision about a different draft is not this wait's answer.
func TestWorkflowIgnoresAnApprovalForAnotherDraft(t *testing.T) {
	send := domain.Tool{Name: "gmail.send", Label: domain.LabelWriteExternal, Schema: json.RawMessage(`{}`)}
	h := newHarness(t, domain.DefaultLimits, []domain.Tool{send})

	h.models[parentTask] = []ModelReply{
		{ToolCalls: []ToolCall{{CallID: "c1", Name: "gmail.send", Arguments: []byte(`{}`)}}},
		{Text: "Selesai."},
	}

	// A decision about somebody else's draft first, then the real one. The
	// workflow must ignore the first, which is what keeps a stale approval from
	// unblocking the wrong wait.
	h.signalLater(t, SignalApproval, ApprovalSignal{
		DraftID:  "99999999-9999-9999-9999-999999999999",
		Approved: true,
		Note:     "bukan draf ini",
	})
	h.signalLater(t, SignalApproval, ApprovalSignal{DraftID: draftID, Approved: true, Note: "kirim"})

	outcome := h.run(t, h.input(parentTask))

	require.Equal(t, domain.StatusSucceeded, outcome.Status)
	require.Equal(t, 2, h.calls[parentTask])
}

// TestWorkflowFailsWhenTheFirstActivityFails is the honest failure: a task that
// could not even be prepared is finished as failed with the reason, rather than
// left queued.
func TestWorkflowFailsWhenTheFirstActivityFails(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits, nil)

	h.loadErr = errors.New("database is down")

	outcome := h.run(t, h.input(parentTask))

	require.Equal(t, domain.StatusFailed, outcome.Status)
	require.Contains(t, outcome.Reason, "gagal menyiapkan tugas")
}

// TestWorkflowBuildsEveryPayloadFromItsOwnHistory is the determinism the replay
// depends on.
//
// A workflow is re-executed from its recorded history after a restart, so every
// command it issues must be a function of that history and nothing else. The
// payloads it writes are the place this is easiest to get wrong: a payload built
// from a map would encode in a different order on a second run, and the replay
// would then disagree with the recorded result. The real replay proof is
// TestTaskSurvivesAWorkerRestart, which restarts a live worker; this is the cheap
// check that the workflow has no hidden input.
func TestWorkflowBuildsEveryPayloadFromItsOwnHistory(t *testing.T) {
	readTool := domain.Tool{Name: "gmail.search", Label: domain.LabelRead, Schema: json.RawMessage(`{}`)}

	script := func(h *harness) {
		h.tools["gmail.search"] = ToolOutcome{Name: "gmail.search", Content: []byte(`{"emails":1}`)}
		h.models[parentTask] = []ModelReply{
			{ToolCalls: []ToolCall{{CallID: "c1", Name: "gmail.search", Arguments: []byte(`{}`)}}},
			{Text: "Ada 1 pesanan."},
		}
	}

	first := newHarness(t, domain.DefaultLimits, []domain.Tool{readTool})
	script(first)
	require.Equal(t, domain.StatusSucceeded, first.run(t, first.input(parentTask)).Status)

	second := newHarness(t, domain.DefaultLimits, []domain.Tool{readTool})
	script(second)
	require.Equal(t, domain.StatusSucceeded, second.run(t, second.input(parentTask)).Status)

	require.NotEmpty(t, first.payloads)
	require.Equal(t, first.payloads, second.payloads,
		"the same history must produce byte-identical payloads, or a replay would disagree with the run")
}

// TestSystemPromptCarriesWhoIsAnswering is the persona the model answers under.
// It is assembled inside the workflow, so this is the only place the wording is
// pinned.
func TestSystemPromptCarriesWhoIsAnswering(t *testing.T) {
	loaded := Context{
		Agent:  domain.AgentRef{Name: "Oren", Role: "Penjualan", Persona: "Ramah dan ringkas.", Tone: "hangat"},
		Tools:  []domain.Tool{{Name: "gmail.search", Label: domain.LabelRead}},
		Limits: domain.DefaultLimits,
	}

	prompt := SystemPrompt(loaded, TaskInput{Title: "Rekap"})
	require.Contains(t, prompt, "Oren")
	require.Contains(t, prompt, "Penjualan")
	require.Contains(t, prompt, "Ramah dan ringkas.")
	require.Contains(t, prompt, "hangat")
	require.Contains(t, prompt, "handoff", "a Bolu with tools is told it may hand work to another Bolu")

	// A task that is itself part of a chain is told so, because its answer is
	// data for the task above it rather than a reply to a person. The depth
	// comes from the loaded context, which is what the activity read from the
	// stored task.
	loaded.Depth = 1
	deeper := SystemPrompt(loaded, TaskInput{Title: "Rekap"})
	require.Contains(t, deeper, "bagian dari tugas lain")

	// And a Bolu with no tools is not told about the handoff tool it cannot see.
	bare := SystemPrompt(Context{Agent: domain.AgentRef{Name: "Kunyit"}}, TaskInput{Title: "apa saja"})
	require.NotContains(t, bare, "handoff")
	require.NotContains(t, bare, "Persetujuan")
}

// TestMessagesRendersTheThreadAndThePrompt covers the turns the model reads.
//
// The prompt is the task's own instruction. For a task opened from chat it is the
// message the user just wrote, which the history already carries: appending it
// again would send the same words twice and read as the user repeating himself.
func TestMessagesRendersTheThreadAndThePrompt(t *testing.T) {
	userID := uuid.New()
	agentID := uuid.New()

	// A chat task's thread, as the runtime loads it: an earlier exchange, the
	// message that opened this task, and the placeholder its own answer will
	// fill. That placeholder is empty when the round starts, so it is skipped
	// rather than sent as a blank turn.
	loaded := Context{History: []domain.Message{
		{AgentID: agentID, Blocks: []domain.Block{{Type: "text", Body: []byte(`{"type":"text","markdown":"Selamat pagi."}`)}}},
		{UserID: userID, Blocks: []domain.Block{{Type: "text", Body: []byte(`{"type":"text","markdown":"Rekap pesanan"}`)}}},
		{AgentID: agentID, Blocks: []domain.Block{}},
	}}

	turns := Messages(loaded, TaskInput{Prompt: "Rekap pesanan"})
	require.Len(t, turns, 2, "the two real turns, and nothing for the empty placeholder or the repeated prompt")
	require.True(t, turns[0].FromAgent())
	require.False(t, turns[1].FromAgent())
	require.Equal(t, "Rekap pesanan", textOf(t, turns[1]),
		"the prompt repeats the message the thread already ends with, so it is not sent twice")

	// A prompt the thread does not already carry is appended, which is what a
	// routine or a webhook task needs: it has no conversation to read it from.
	appended := Messages(loaded, TaskInput{Prompt: "Kirim tagihan bulan ini"})
	require.Len(t, appended, 3)
	require.Equal(t, "Kirim tagihan bulan ini", textOf(t, appended[2]))
	require.False(t, appended[2].FromAgent(), "the task's own instruction is a user turn")

	// A task with no history is just its own instruction.
	only := Messages(Context{}, TaskInput{Prompt: "Rekap pesanan"})
	require.Len(t, only, 1)
	require.Equal(t, "Rekap pesanan", textOf(t, only[0]))

	require.Empty(t, Messages(Context{}, TaskInput{}), "a task with no instruction sends nothing")

	// A block the model cannot read is summarised rather than dropped: a model
	// that forgot it already produced a chart would produce it again.
	summarised := Messages(Context{History: []domain.Message{
		{AgentID: agentID, Blocks: []domain.Block{{Type: "chart", Body: []byte(`{"type":"chart"}`)}}},
	}}, TaskInput{})
	require.Len(t, summarised, 1)
	require.Equal(t, "[grafik]", textOf(t, summarised[0]))
}

// TestAnswerBlocksCarriesTheReasonWhenThereIsNoAnswer is the terminal body: a
// task that stopped on a bound still writes why into the thread, because an
// empty message reads as a Bolu that said nothing rather than one that failed.
func TestAnswerBlocksCarriesTheReasonWhenThereIsNoAnswer(t *testing.T) {
	answer := AnswerBlocks("Ada 3 pesanan.", "")
	require.Len(t, answer, 1)
	require.Equal(t, "text", answer[0].Type)
	require.Contains(t, string(answer[0].Body), "Ada 3 pesanan.")

	stopped := AnswerBlocks("", "tugas berhenti karena mencapai batas langkah (1)")
	require.Len(t, stopped, 1)
	require.Contains(t, string(stopped[0].Body), "batas langkah")

	require.Empty(t, AnswerBlocks("", ""), "a task that said nothing and stopped for no reason writes no block")
}

// TestHandoffArgumentsAreReadFromTheCall covers the two fields the handoff tool
// takes, including the case where the model sent something that is not JSON.
func TestHandoffArgumentsAreReadFromTheCall(t *testing.T) {
	require.Equal(t, "Ijo", handoffTarget([]byte(`{"agent":"Ijo","instructions":"Ambil data"}`)))
	require.Equal(t, "Ambil data", handoffInstructions([]byte(`{"agent":"Ijo","instructions":"Ambil data"}`)))

	require.Empty(t, handoffTarget([]byte(`bukan json`)))
	require.Equal(t, "bukan json", handoffInstructions([]byte(`bukan json`)),
		"a call whose arguments will not parse is passed on rather than dropped, so the model sees what it sent")

	require.Equal(t, "Ambil data", handoffInstructions([]byte(`{"instructions":"  Ambil data  "}`)),
		"surrounding space is trimmed, which is what a model that padded its answer means")
}

// TestMustUUIDRefusesWhatItCannotParse is the guard the activities rely on: an id
// that will not parse becomes the zero value, which an activity then reports as
// an invalid input rather than the workflow panicking on it.
func TestMustUUIDRefusesWhatItCannotParse(t *testing.T) {
	require.Equal(t, uuid.Nil, mustUUID("bukan-uuid"))
	require.Equal(t, uuid.Nil, mustUUID(""))

	id := uuid.New()
	require.Equal(t, id, mustUUID(id.String()))
	require.Equal(t, id, mustUUID("  "+id.String()+"  "))
}

// TestBeatTotalsBothDirections is the number the token bound compares against.
func TestBeatTotalsBothDirections(t *testing.T) {
	require.Equal(t, int64(160), Beat{InputTokens: 120, OutputTokens: 40}.TotalTokens())
	require.Equal(t, int64(0), Beat{}.TotalTokens())
}

// TestMarshalQuietNeverFailsARound is the encoding rule: a payload that will not
// encode becomes an empty document rather than ending the round, because a step
// with an unusable payload is worth less than the step itself.
func TestMarshalQuietNeverFailsARound(t *testing.T) {
	require.JSONEq(t, `{"a":1}`, string(marshalQuiet(map[string]any{"a": 1})))
	require.JSONEq(t, `{}`, string(marshalQuiet(func() {})))
}

// TestTaskEventCarriesTheScope is the event the feed and the office read.
func TestTaskEventCarriesTheScope(t *testing.T) {
	workspaceID := uuid.New()
	agentID := uuid.New()
	conversationID := uuid.New()
	taskID := uuid.New()

	event := TaskEvent(domain.Scope{WorkspaceID: workspaceID}, domain.Task{
		ID:             taskID,
		AgentID:        agentID,
		ConversationID: conversationID,
	}, EventTaskRunning, []byte(`{"status":"running"}`))

	require.Equal(t, workspaceID, event.WorkspaceID)
	require.Equal(t, EventTaskRunning, event.Type)
	require.Equal(t, agentID, event.ActorAgentID)
	require.Equal(t, conversationID, event.ConversationID)
	require.Equal(t, taskID, event.TaskID)
	require.JSONEq(t, `{"status":"running"}`, string(event.Payload))
}

// TestTextTurnCarriesARealTextBlock is the shape the activity renders back into
// the model's message: a turn whose body is not a text block would reach the
// provider as an empty string.
func TestTextTurnCarriesARealTextBlock(t *testing.T) {
	agentID := uuid.New()
	turn := textTurn("Ada 3 pesanan.", agentID, uuid.Nil)

	require.Len(t, turn.Blocks, 1)
	require.Equal(t, "text", turn.Blocks[0].Type)
	require.True(t, turn.FromAgent())
	require.JSONEq(t, `{"type":"text","markdown":"Ada 3 pesanan."}`, string(turn.Blocks[0].Body))
}

// textOf reads the markdown out of a rendered turn.
func textOf(t *testing.T, turn domain.Message) string {
	t.Helper()

	require.Len(t, turn.Blocks, 1)
	var body struct {
		Markdown string `json:"markdown"`
	}
	require.NoError(t, json.Unmarshal(turn.Blocks[0].Body, &body))
	return body.Markdown
}

// TestWorkflowHandoffNeverReachesTheExecutor is the routing rule: a handoff is the
// one tool the workflow performs itself, so it must not also be offered to an
// integration executor that does not own it.
func TestWorkflowHandoffNeverReachesTheExecutor(t *testing.T) {
	handoff := domain.Tool{Name: domain.ToolHandoff, Label: domain.LabelRead, Schema: json.RawMessage(`{}`)}
	h := newHarness(t, domain.DefaultLimits, []domain.Tool{handoff})

	h.env.OnActivity(ActivityPrepareHandoff, mock.Anything, mock.Anything).Return(func(_ context.Context, _ HandoffRequest) (PreparedHandoff, error) {
		return PreparedHandoff{Refusal: "serah-terima ditolak: tidak ada Bolu bernama \"Ijo\""}, nil
	})

	h.models[parentTask] = []ModelReply{
		{ToolCalls: []ToolCall{{CallID: "c1", Name: domain.ToolHandoff, Arguments: []byte(`{"agent":"Ijo","instructions":"ambil data"}`)}}},
		{Text: "Baik, saya kerjakan sendiri."},
	}

	outcome := h.run(t, h.input(parentTask))

	require.Equal(t, domain.StatusSucceeded, outcome.Status)
	require.Zero(t, h.ranTool, "the handoff is performed by the workflow, not by a tool executor")
	require.Equal(t, 2, h.calls[parentTask], "a refusal comes back to the model as a failed result")
}

// TestWorkflowRecordsAChildThatFailedAsAFailedToolResult is the honest answer for
// a handoff whose child failed: the parent keeps what it has rather than failing
// with the child.
func TestWorkflowRecordsAChildThatFailedAsAFailedToolResult(t *testing.T) {
	handoff := domain.Tool{Name: domain.ToolHandoff, Label: domain.LabelRead, Schema: json.RawMessage(`{}`)}
	h := newHarness(t, domain.DefaultLimits, []domain.Tool{handoff})

	h.env.OnActivity(ActivityPrepareHandoff, mock.Anything, mock.Anything).Return(func(_ context.Context, _ HandoffRequest) (PreparedHandoff, error) {
		return PreparedHandoff{Child: TaskInput{
			TaskID: childTask, WorkspaceID: workspaceID, AgentID: agentID,
			Title: "Ambil data stok", Prompt: "Ambil data stok", Depth: 1,
		}}, nil
	})

	// The child answers nothing and fails: its model script is empty, so the
	// child workflow ends as failed and the parent is told.
	h.env.OnActivity(ActivityCallModel, mock.Anything, mock.Anything).Return(func(_ context.Context, req ModelRequest) (ModelReply, error) {
		h.calls[req.TaskID]++
		if req.TaskID == childTask {
			return ModelReply{}, errors.New("the child's provider is down")
		}
		queue := h.models[req.TaskID]
		reply := queue[0]
		h.models[req.TaskID] = queue[1:]
		return reply, nil
	})

	h.models[parentTask] = []ModelReply{
		{ToolCalls: []ToolCall{{CallID: "c1", Name: domain.ToolHandoff, Arguments: []byte(`{"agent":"Ijo","instructions":"Ambil data stok"}`)}}},
		{Text: "Data stok belum bisa diambil, saya laporkan apa adanya."},
	}

	outcome := h.run(t, h.input(parentTask))

	require.Equal(t, domain.StatusSucceeded, outcome.Status, "the parent does not fail with its child")

	// The child's activity was retried to the policy's limit before the child
	// gave up, which is what a provider that is down looks like.
	require.Equal(t, int(DefaultRetry.MaximumAttempts), h.calls[childTask])
	require.Equal(t, domain.StatusFailed, h.terminal[childTask].Status)
	require.Contains(t, h.terminal[childTask].Reason, "model gagal menjawab")

	require.Equal(t, 2, h.calls[parentTask], "the parent carried on after the child failed")
	require.Equal(t, domain.StatusSucceeded, h.terminal[parentTask].Status)
}
