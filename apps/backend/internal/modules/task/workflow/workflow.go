// Package workflow holds the durable agent loop: one task is one Temporal
// workflow that calls the model, runs the tools it asks for, records every round,
// and stops on a bound rather than hanging.
//
// This file is deliberately free of I/O. Everything that touches the network, a
// database, or the clock happens in an activity, because a workflow is replayed
// from its history after a worker restart: a non-deterministic step would make
// the replay disagree with the run and the task would be stuck forever.
package workflow

import (
	"errors"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
)

// WorkflowName is what the worker registers and what a start request names.
const WorkflowName = "AgentTaskWorkflow"

// Activity names, referenced by string so a test can mock them and the worker
// can register them without the two sharing a function value.
const (
	ActivityLoadContext        = "LoadTaskContext"
	ActivityCallModel          = "CallModel"
	ActivityRunTool            = "RunTool"
	ActivityRequestDraft       = "RequestDraft"
	ActivityRecordStep         = "RecordStep"
	ActivityBeat               = "BeatTask"
	ActivityComplete           = "CompleteTask"
	ActivityPrepareHandoff     = "PrepareHandoff"
	ActivityMarkHandoffStarted = "MarkHandoffStarted"
	ActivityResume             = "ResumeTask"
)

// Signal names a running task accepts.
const (
	// SignalCancel stops the loop at the next checkpoint.
	SignalCancel = "cancel"
	// SignalApproval tells a parked task what a human decided.
	SignalApproval = "approval"
)

// Activity timeouts. They are per activity rather than one global value because
// the shapes differ: a model call can legitimately take minutes, a step write
// must not.
const (
	contextTimeout  = 30 * time.Second
	modelTimeout    = 5 * time.Minute
	toolTimeout     = 2 * time.Minute
	draftTimeout    = 30 * time.Second
	recordTimeout   = 30 * time.Second
	beatTimeout     = 30 * time.Second
	completeTimeout = 60 * time.Second
	handoffTimeout  = 30 * time.Second
	// childTimeout bounds one child task's whole life, because the parent waits
	// for the answer rather than for the request to be accepted.
	childTimeout = 60 * time.Minute
)

// DefaultRetry is the retry policy every activity gets unless it says otherwise.
//
// It is explicit rather than left to the SDK default: a task that silently
// retried a model call forever would spend a workspace's tokens on one problem.
var DefaultRetry = &temporal.RetryPolicy{
	InitialInterval:    time.Second,
	BackoffCoefficient: 2,
	MaximumInterval:    30 * time.Second,
	MaximumAttempts:    4,
}

// ToolRetry is the policy for running a tool. A tool failure is usually the
// remote service, and a short retry is worth it, but not many times.
var ToolRetry = &temporal.RetryPolicy{
	InitialInterval:    2 * time.Second,
	BackoffCoefficient: 2,
	MaximumInterval:    20 * time.Second,
	MaximumAttempts:    3,
}

// TaskInput is what starts a task. Every field is an id, so a restart re-reads
// the current state instead of replaying a snapshot taken when it was opened.
type TaskInput struct {
	TaskID string
	// WorkspaceID scopes every read an activity makes. It is carried explicitly
	// because the workflow runs out of process and has no request context to
	// derive it from.
	WorkspaceID    string
	AgentID        string
	ConversationID string
	ReplyMessageID string
	Title          string
	Prompt         string
	Depth          int
}

// TaskOutcome is what a finished task reports.
type TaskOutcome struct {
	Status  string
	Summary string
	Reason  string
	Steps   int
	Tokens  int64
}

// Context is one round's input: the Bolu, the tools it may call, and the
// conversation so far. It is loaded fresh each round so a long-running task sees
// edits the user made while it worked.
type Context struct {
	Agent   domain.AgentRef
	Tools   []domain.Tool
	History []domain.Message
	// Limits are the bounds the service configured, so the workflow and the
	// service cannot disagree about what a limit is.
	Limits domain.Limits
	Depth  int
}

// ModelRequest is one model call.
type ModelRequest struct {
	TaskID      string
	WorkspaceID string
	AgentID     string
	System      string
	Messages    []domain.Message
	Tools       []domain.Tool
}

// ModelReply is what the model answered.
type ModelReply struct {
	Text      string
	ToolCalls []ToolCall
	Usage     domain.Usage
	Model     string
	// FinishReason explains why the model stopped, which the summary reads.
	FinishReason string
}

// ToolCall is one tool the model asked for.
type ToolCall struct {
	CallID    string
	Name      string
	Arguments []byte
}

// ToolRequest is one tool call for an activity to run.
type ToolRequest struct {
	TaskID      string
	WorkspaceID string
	CallID      string
	Name        string
	Arguments   []byte
}

// StepRequest is one round for an activity to record.
type StepRequest struct {
	TaskID      string
	WorkspaceID string
	Record      StepRecord
}

// CompleteRequest is the terminal state for an activity to write.
type CompleteRequest struct {
	TaskID       string
	WorkspaceID  string
	Status       string
	Reason       string
	InputTokens  int64
	OutputTokens int64
	CostMicros   int64
	StepCount    int
	// Blocks is the answer, which the reply writer stores in the conversation.
	Blocks []domain.Block
}

// HandoffStarted records the workflow a child task runs under.
type HandoffStarted struct {
	TaskID      string
	WorkspaceID string
	WorkflowID  string
	RunID       string
}

// ResumeRequest clears a parked task's waiting state.
type ResumeRequest struct {
	TaskID      string
	WorkspaceID string
}

// Event names a task publishes.
const (
	EventTaskStarted  = "task.started"
	EventTaskStep     = "task.step"
	EventTaskRunning  = "task.running"
	EventTaskWaiting  = "task.waiting_approval"
	EventTaskFinished = "task.finished"
)

// Message statuses for the reply the task writes. They are constants here so the
// activity does not import the chat module.
const (
	MessageComplete = "complete"
	MessageFailed   = "failed"
)

// ToolOutcome is what running a tool produced.
type ToolOutcome struct {
	Name    string
	Label   string
	Content []byte
	IsError bool
	// Draft is set when the tool needed approval instead of running, which is
	// what a `write_external` tool becomes.
	Draft *DraftOutcome
}

// DraftOutcome is the approval a write_external tool asked for.
type DraftOutcome struct {
	DraftID string
	Status  string
	Note    string
	Title   string
}

// StepRecord is one round to write to `task_steps`.
type StepRecord struct {
	Kind         string
	ToolName     string
	ToolLabel    string
	Input        []byte
	Output       []byte
	InputTokens  int
	OutputTokens int
}

// Beat is the checkpoint between rounds: it records a heartbeat with the running
// totals and answers whether the task may keep going.
type Beat struct {
	TaskID       string
	WorkspaceID  string
	StepCount    int
	InputTokens  int64
	OutputTokens int64
	// StopReason is set when a bound was reached. The workflow stops on it
	// rather than discovering the problem at the next model call.
	StopReason string
	// StopDetail is the user-facing explanation.
	StopDetail string
}

// TotalTokens is what the task spent across both directions.
func (b Beat) TotalTokens() int64 { return b.InputTokens + b.OutputTokens }

// ApprovalSignal is what a human decided about a draft.
//
// It carries the draft it is about because a task may create more than one draft
// over its life, and an approval for the wrong one must not unblock the wrong
// wait.
type ApprovalSignal struct {
	DraftID  string
	Approved bool
	Note     string
}

// HandoffRequest asks another Bolu to do part of the work.
type HandoffRequest struct {
	ParentTaskID string
	WorkspaceID  string
	AgentName    string
	Instructions string
	Depth        int
}

// PreparedHandoff is the child task a handoff opened, or the reason it was
// refused. It is one value because an activity future carries one result.
type PreparedHandoff struct {
	Child   TaskInput
	Refusal string
}

// HandoffResult is the child task's answer.
type HandoffResult struct {
	ChildTaskID string
	Status      string
	Summary     string
	// Refusal is set when the handoff was refused rather than run, which is
	// returned to the model as a failed tool result.
	Refusal string
}

// AgentTaskWorkflow runs one task to its end.
//
// The shape is the document's: a Bolu may think and read as freely as it likes,
// so reading tools run directly; anything that reaches outside Bolu goes through
// a draft a human decides on. Every round is recorded, and the loop stops on a
// bound with a reason a person can read rather than on a timeout.
func AgentTaskWorkflow(ctx workflow.Context, input TaskInput) (TaskOutcome, error) {
	state := &runState{input: input}

	// The signal channels are created before any work, so a cancel or an
	// approval that arrives while the first activity runs is buffered rather
	// than lost.
	cancel := workflow.GetSignalChannel(ctx, SignalCancel)
	approvals := workflow.GetSignalChannel(ctx, SignalApproval)

	loadCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: contextTimeout,
		RetryPolicy:         DefaultRetry,
	})

	var loaded Context
	if err := workflow.ExecuteActivity(loadCtx, ActivityLoadContext, input).Get(loadCtx, &loaded); err != nil {
		return state.fail(ctx, "gagal menyiapkan tugas: "+err.Error())
	}

	for state.steps < loaded.Limits.MaxSteps {
		// The cancel check is here rather than at the top so it covers a signal
		// that arrived while the previous round ran.
		if drainCancel(cancel, &state.canceled) {
			return state.finish(ctx, domain.StatusCanceled, "dihentikan oleh pengguna")
		}

		done, err := state.oneRound(ctx, loaded, approvals, cancel)
		if err != nil {
			return state.fail(ctx, err.Error())
		}

		beat, err := state.beat(ctx)
		if err != nil {
			return state.fail(ctx, err.Error())
		}
		if beat.StopReason != "" {
			return state.finish(ctx, beat.StopReason, beat.StopDetail)
		}
		if done {
			return state.finish(ctx, domain.StatusSucceeded, "")
		}
	}

	// The loop ran its whole allowance without an answer, which is a stop the
	// user must be told about rather than a silent truncation.
	return state.finish(ctx, domain.StatusFailed, "tugas berhenti karena mencapai batas langkah")
}

// runState carries what the workflow accumulated. It is a value the workflow
// owns, never shared with an activity: everything crossing that boundary is a
// serialisable argument.
type runState struct {
	input TaskInput
	steps int
	// The two directions are tracked apart rather than as one total: a task's
	// spend is shown split, and a total that is written as all input would read
	// as a prompt nobody sent.
	inputTokens  int64
	outputTokens int64
	canceled     bool
	answer       string
}

// oneRound asks the model, runs what it asked for, and records the round. It
// reports whether the model produced a final answer.
func (s *runState) oneRound(ctx workflow.Context, loaded Context, approvals, cancel workflow.ReceiveChannel) (bool, error) {
	modelCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: modelTimeout,
		RetryPolicy:         DefaultRetry,
	})
	recordCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: recordTimeout,
		RetryPolicy:         DefaultRetry,
	})

	request := ModelRequest{
		TaskID:      s.input.TaskID,
		WorkspaceID: s.input.WorkspaceID,
		AgentID:     s.input.AgentID,
		System:      SystemPrompt(loaded, s.input),
		Messages:    Messages(loaded, s.input),
		Tools:       loaded.Tools,
	}

	var reply ModelReply
	if err := workflow.ExecuteActivity(modelCtx, ActivityCallModel, request).Get(modelCtx, &reply); err != nil {
		return false, errors.New("model gagal menjawab: " + err.Error())
	}

	s.steps++
	s.inputTokens += int64(reply.Usage.InputTokens)
	s.outputTokens += int64(reply.Usage.OutputTokens)

	// The thinking round is recorded before its tools, so the order in the
	// history is the order the model produced.
	think := StepRecord{
		Kind:         domain.StepThink,
		Input:        marshalQuiet(request.Messages),
		Output:       marshalQuiet(map[string]any{"text": reply.Text, "tool_calls": reply.ToolCalls}),
		InputTokens:  reply.Usage.InputTokens,
		OutputTokens: reply.Usage.OutputTokens,
	}
	if err := s.record(ctx, recordCtx, think); err != nil {
		return false, err
	}

	if len(reply.ToolCalls) == 0 {
		s.answer = reply.Text
		final := StepRecord{
			Kind:   domain.StepFinal,
			Output: marshalQuiet(map[string]any{"text": reply.Text}),
		}
		if err := s.record(ctx, recordCtx, final); err != nil {
			return false, err
		}
		return true, nil
	}

	for _, call := range reply.ToolCalls {
		outcome, err := s.runTool(ctx, loaded, call)
		if err != nil {
			return false, err
		}

		if outcome.Draft != nil {
			// A tool that touches the outside world becomes a draft, and the
			// task parks on it: the workflow waits for the decision rather than
			// holding a worker slot open.
			decision, stopped := waitForApproval(ctx, approvals, cancel, outcome.Draft.DraftID)
			if stopped {
				return false, errors.New("dibatalkan saat menunggu persetujuan")
			}
			if err := s.resume(ctx, outcome.Draft.DraftID, decision); err != nil {
				return false, err
			}
			if !decision.Approved {
				// A revision is fed back to the model as a failed result, which
				// is how the agent learns the human disagreed.
				outcome.Content = marshalQuiet(map[string]any{
					"approved": false,
					"note":     decision.Note,
				})
				outcome.IsError = true
			}
		}

		callStep := StepRecord{
			Kind:      domain.StepToolCall,
			ToolName:  call.Name,
			ToolLabel: outcome.Label,
			Input:     call.Arguments,
		}
		if err := s.record(ctx, recordCtx, callStep); err != nil {
			return false, err
		}

		resultStep := StepRecord{
			Kind:      domain.StepToolResult,
			ToolName:  call.Name,
			ToolLabel: outcome.Label,
			Output:    outcome.Content,
		}
		if outcome.Draft != nil {
			resultStep.Kind = domain.StepDraft
			resultStep.Output = marshalQuiet(map[string]any{
				"draft_id": outcome.Draft.DraftID,
				"status":   outcome.Draft.Status,
			})
		}

		if err := s.record(ctx, recordCtx, resultStep); err != nil {
			return false, err
		}
	}

	return false, nil
}

// record writes one round.
func (s *runState) record(ctx workflow.Context, options workflow.Context, record StepRecord) error {
	return workflow.ExecuteActivity(options, ActivityRecordStep, StepRequest{
		TaskID:      s.input.TaskID,
		WorkspaceID: s.input.WorkspaceID,
		Record:      record,
	}).Get(options, nil)
}

// runTool executes one tool call, or routes it to the approval gate.
func (s *runState) runTool(ctx workflow.Context, loaded Context, call ToolCall) (ToolOutcome, error) {
	// A handoff is the one tool the workflow performs itself: it opens a child
	// task and waits for the answer, which is what makes a chain traceable
	// rather than a conversation. It never reaches the executor, because asking
	// an integration to run a tool it does not own would be handing it the
	// runtime's own job.
	if call.Name == domain.ToolHandoff {
		return s.handoffTool(ctx, loaded, call)
	}

	tool, known := findTool(loaded.Tools, call.Name)
	if !known {
		// Nothing can run it, so the model is told rather than the task failing:
		// the model can pick another tool.
		return ToolOutcome{
			Name:    call.Name,
			Content: marshalQuiet(map[string]any{"error": "alat tidak tersedia untuk Bolu ini"}),
			IsError: true,
		}, nil
	}

	request := ToolRequest{
		TaskID:      s.input.TaskID,
		WorkspaceID: s.input.WorkspaceID,
		CallID:      call.CallID,
		Name:        call.Name,
		Arguments:   call.Arguments,
	}

	if tool.NeedsApproval() {
		return s.requestDraft(ctx, tool, request)
	}

	toolCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: toolTimeout,
		RetryPolicy:         ToolRetry,
	})

	var outcome ToolOutcome
	if err := workflow.ExecuteActivity(toolCtx, ActivityRunTool, request).Get(toolCtx, &outcome); err != nil {
		// A tool that could not run at all is the model's problem to solve, not
		// the task's: it is reported back as a failed result.
		//nolint:nilerr // the tool's failure is handed to the model, which can try something else
		return ToolOutcome{
			Name:    call.Name,
			Label:   tool.Label,
			Content: marshalQuiet(map[string]any{"error": err.Error()}),
			IsError: true,
		}, nil
	}

	outcome.Name = call.Name
	outcome.Label = tool.Label
	return outcome, nil
}

// requestDraft turns a write_external tool into a draft and parks the task.
func (s *runState) requestDraft(ctx workflow.Context, tool domain.Tool, request ToolRequest) (ToolOutcome, error) {
	draftCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: draftTimeout,
		RetryPolicy:         DefaultRetry,
	})

	var draft DraftOutcome
	if err := workflow.ExecuteActivity(draftCtx, ActivityRequestDraft, request).Get(draftCtx, &draft); err != nil {
		//nolint:nilerr // a gate that could not be reached is a refused action, which the model reads
		return ToolOutcome{
			Name:    request.Name,
			Label:   tool.Label,
			Content: marshalQuiet(map[string]any{"error": err.Error()}),
			IsError: true,
		}, nil
	}

	content := marshalQuiet(map[string]any{"queued_for_approval": true})
	if draft.Status == domain.DraftCanceled {
		// No gate is configured, so the action was refused rather than queued.
		// Telling the model that is what keeps it from waiting for an approval
		// that will never come.
		return ToolOutcome{
			Name:    request.Name,
			Label:   tool.Label,
			Content: marshalQuiet(map[string]any{"error": draft.Note}),
			IsError: true,
		}, nil
	}

	return ToolOutcome{
		Name:    request.Name,
		Label:   tool.Label,
		Content: content,
		Draft:   &draft,
	}, nil
}

// resume clears the parked state once the decision is in.
func (s *runState) resume(ctx workflow.Context, draftID string, decision ApprovalSignal) error {
	resumeCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: recordTimeout,
		RetryPolicy:         DefaultRetry,
	})

	if err := workflow.ExecuteActivity(resumeCtx, ActivityResume, ResumeRequest{
		TaskID:      s.input.TaskID,
		WorkspaceID: s.input.WorkspaceID,
	}).Get(resumeCtx, nil); err != nil {
		return errors.New("gagal melanjutkan tugas setelah keputusan: " + err.Error())
	}
	return nil
}

// handoffTool opens a child task for another Bolu and waits for its answer,
// returning it in the shape every other tool answers in.
//
// A refusal is a failed tool result rather than an error: the model is the one
// that has to react to it, and ending the task would take that decision away.
func (s *runState) handoffTool(ctx workflow.Context, loaded Context, call ToolCall) (ToolOutcome, error) {
	result, err := s.handoff(ctx, loaded, call)
	if err != nil {
		return ToolOutcome{}, err
	}

	if result.Refusal != "" {
		return ToolOutcome{
			Name:    call.Name,
			Label:   domain.LabelRead,
			Content: marshalQuiet(map[string]any{"error": result.Refusal}),
			IsError: true,
		}, nil
	}

	return ToolOutcome{
		Name:  call.Name,
		Label: domain.LabelRead,
		Content: marshalQuiet(map[string]any{
			"child_task_id": result.ChildTaskID,
			"status":        result.Status,
			"summary":       result.Summary,
		}),
		IsError: result.Status == domain.StatusFailed,
	}, nil
}

// handoff opens a child task for another Bolu and waits for its answer.
//
// The child is a Temporal child workflow rather than an activity, so the chain
// is visible in the Temporal UI, the parent's wait survives a restart, and a
// child that runs long does not hold the parent's activity slot.
func (s *runState) handoff(ctx workflow.Context, loaded Context, call ToolCall) (HandoffResult, error) {
	prepareCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: handoffTimeout,
		RetryPolicy:         DefaultRetry,
	})

	var prepared PreparedHandoff
	request := HandoffRequest{
		ParentTaskID: s.input.TaskID,
		WorkspaceID:  s.input.WorkspaceID,
		AgentName:    handoffTarget(call.Arguments),
		Instructions: handoffInstructions(call.Arguments),
		Depth:        loaded.Depth,
	}
	if err := workflow.ExecuteActivity(prepareCtx, ActivityPrepareHandoff, request).Get(prepareCtx, &prepared); err != nil {
		return HandoffResult{}, errors.New("serah-terima gagal: " + err.Error())
	}
	if prepared.Refusal != "" {
		return HandoffResult{Refusal: prepared.Refusal}, nil
	}
	child := prepared.Child

	childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		// The child runs under its own task id, which is what makes the chain
		// readable from Temporal without a lookup table.
		WorkflowID:               "task-" + child.TaskID,
		WorkflowExecutionTimeout: childTimeout,
	})

	future := workflow.ExecuteChildWorkflow(childCtx, AgentTaskWorkflow, child)

	// The execution is recorded before the wait, so a restart mid-child still
	// shows which run the parent is waiting on.
	var execution workflow.Execution
	if err := future.GetChildWorkflowExecution().Get(childCtx, &execution); err != nil {
		return HandoffResult{}, errors.New("serah-terima gagal dimulai: " + err.Error())
	}
	markCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: recordTimeout,
		RetryPolicy:         DefaultRetry,
	})
	if err := workflow.ExecuteActivity(markCtx, ActivityMarkHandoffStarted, HandoffStarted{
		TaskID:      child.TaskID,
		WorkspaceID: child.WorkspaceID,
		WorkflowID:  execution.ID,
		RunID:       execution.RunID,
	}).Get(markCtx, nil); err != nil {
		return HandoffResult{}, err
	}

	var outcome TaskOutcome
	if err := future.Get(childCtx, &outcome); err != nil {
		// A child that failed is reported as a failed tool result, so the parent
		// can carry on with what it has rather than failing with it.
		//nolint:nilerr // a child that failed is the parent's data, not the parent's failure
		return HandoffResult{
			ChildTaskID: child.TaskID,
			Status:      domain.StatusFailed,
			Summary:     err.Error(),
		}, nil
	}

	return HandoffResult{
		ChildTaskID: child.TaskID,
		Status:      outcome.Status,
		Summary:     outcome.Summary,
	}, nil
}

// beat records a heartbeat and asks whether the task may continue.
func (s *runState) beat(ctx workflow.Context) (Beat, error) {
	beatCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: beatTimeout,
		RetryPolicy:         DefaultRetry,
	})

	var beat Beat
	if err := workflow.ExecuteActivity(beatCtx, ActivityBeat, Beat{
		TaskID:       s.input.TaskID,
		WorkspaceID:  s.input.WorkspaceID,
		StepCount:    s.steps,
		InputTokens:  s.inputTokens,
		OutputTokens: s.outputTokens,
	}).Get(beatCtx, &beat); err != nil {
		return Beat{}, err
	}
	return beat, nil
}

// finish writes the terminal state of the task.
func (s *runState) finish(ctx workflow.Context, status, reason string) (TaskOutcome, error) {
	completeCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: completeTimeout,
		RetryPolicy:         DefaultRetry,
	})

	outcome := TaskOutcome{
		Status: status,
		Reason: reason,
		Steps:  s.steps,
		Tokens: s.inputTokens + s.outputTokens,
	}
	if reason != "" {
		outcome.Summary = reason
	}

	var stored TaskOutcome
	if err := workflow.ExecuteActivity(completeCtx, ActivityComplete, CompleteRequest{
		TaskID:       s.input.TaskID,
		WorkspaceID:  s.input.WorkspaceID,
		Status:       status,
		Reason:       reason,
		InputTokens:  s.inputTokens,
		OutputTokens: s.outputTokens,
		StepCount:    s.steps,
		Blocks:       AnswerBlocks(s.answer, status),
	}).Get(completeCtx, &stored); err != nil {
		// The task's own state could not be written, so the workflow returns the
		// failure rather than pretending it finished.
		return outcome, err
	}

	stored.Steps = s.steps
	stored.Tokens = s.inputTokens + s.outputTokens
	if stored.Status == "" {
		stored.Status = status
	}
	if stored.Reason == "" {
		stored.Reason = reason
	}
	return stored, nil
}

// fail ends the task as failed, keeping the message the user needs.
func (s *runState) fail(ctx workflow.Context, reason string) (TaskOutcome, error) {
	return s.finish(ctx, domain.StatusFailed, reason)
}

// waitForApproval parks the workflow until a human decides.
//
// The wait has no timeout: a person may take hours, and that is the point of a
// durable task. What keeps it safe is that the task is parked in the store, so a
// restart resumes the wait rather than losing the task.
func waitForApproval(ctx workflow.Context, approvals, cancel workflow.ReceiveChannel, draftID string) (ApprovalSignal, bool) {
	var (
		stopped  bool
		decision ApprovalSignal
	)

	selector := workflow.NewSelector(ctx)
	selector.AddReceive(approvals, func(channel workflow.ReceiveChannel, _ bool) {
		var signal ApprovalSignal
		channel.Receive(ctx, &signal)
		// A decision about another draft is not this wait's answer, so it is
		// kept for the round that asked for it.
		if signal.DraftID != draftID {
			return
		}
		decision = signal
	})
	selector.AddReceive(cancel, func(channel workflow.ReceiveChannel, _ bool) {
		var ignored struct{}
		channel.Receive(ctx, &ignored)
		stopped = true
	})

	// A signal for a different draft leaves decision zero, so the loop selects
	// again rather than returning an answer nobody gave.
	for !stopped && decision.DraftID != draftID {
		selector.Select(ctx)
	}

	return decision, stopped
}

// drainCancel reports whether a cancel signal is waiting, without blocking.
func drainCancel(cancel workflow.ReceiveChannel, seen *bool) bool {
	if *seen {
		return true
	}
	var ignored struct{}
	if cancel.ReceiveAsync(&ignored) {
		*seen = true
	}
	return *seen
}

func findTool(tools []domain.Tool, name string) (domain.Tool, bool) {
	for _, tool := range tools {
		if tool.Name == name {
			return tool, true
		}
	}
	return domain.Tool{}, false
}
