// Package activity holds the side-effecting half of the task runtime: everything
// that touches a database, a model, or an integration.
//
// Each method is a plain function the worker registers, so a test registers a
// stub under the same name and the workflow cannot tell the difference. The
// workflow never calls any of this directly: it goes through Temporal, so a
// restart re-runs the activities that did not finish and skips the ones that did.
package activity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/sdk/temporal"

	"github.com/google/uuid"
	"go.uber.org/zap"

	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/workflow"
)

// historyLimit is how much of a conversation a task is given. A task answers the
// recent thread, not the whole archive, and every message costs tokens on every
// round.
const historyLimit = 30

// Deps are the activity dependencies. Every one of them is optional except the
// repositories: a deployment without a model gateway records and parks tasks
// rather than failing, which is what lets the API run while a worker is down.
type Deps struct {
	Tasks    domain.TaskRepository
	Steps    domain.StepRepository
	Agents   domain.AgentDirectory
	Tools    domain.ToolRegistry
	Executor domain.ToolExecutor
	Drafts   domain.DraftGate
	Gateway  domain.Gateway
	Costs    domain.CostTable
	Budget   domain.Budget
	Events   domain.EventSink
	Reply    domain.ReplyWriter
	// Memory is what a Bolu remembers and what a search finds for the task at
	// hand. Optional: without it a task answers from the conversation alone,
	// which is what a deployment with no embedding provider gets.
	Memory domain.MemoryRecall
	// Outcomes is told how a task ended, for the things that open a task and are
	// not a conversation: a routine's history shows the task's own summary, so the
	// task reports back rather than being polled for. Optional, because a deployment
	// with no routine runtime has nothing to tell.
	Outcomes domain.OutcomeSink
	// Namer resolves a Bolu by the name the model knows, which is what a handoff
	// names.
	Namer  domain.AgentNamer
	Limits domain.Limits
	Logger *zap.Logger
	// Tracer starts the spans of one task's steps: its model calls and its tool calls, which
	// are drawn under the activity span the Temporal interceptor opened. Optional: without it
	// the task runs untraced, which is what a deployment with telemetry off does.
	Tracer trace.Tracer
	// Metrics reports what a task did, for the deployment's own charts. Optional: without it
	// the work happens and nothing is charted.
	Metrics domain.Metrics
	// Clock is the source of "how long did this take", so a duration is testable.
	Clock func() time.Time
}

// now is the activity set's clock, which a test pins.
func (a *Activities) now() time.Time {
	if a.deps.Clock != nil {
		return a.deps.Clock()
	}

	return time.Now()
}

// Activities is the set the worker registers.
type Activities struct {
	deps   Deps
	limits domain.Limits
}

// New builds the activity set.
func New(deps Deps) *Activities {
	return &Activities{deps: deps, limits: deps.Limits.Normalise()}
}

// LoadTaskContext loads everything one round needs.
//
// It runs once per round rather than once per task: a task that waited for an
// approval for hours must see the persona and the tools as they are now, not as
// they were when it started.
func (a *Activities) LoadTaskContext(ctx context.Context, input workflow.TaskInput) (workflow.Context, error) {
	task, err := a.load(ctx, input)
	if err != nil {
		return workflow.Context{}, err
	}

	scope := scopeOf(task)
	agent, err := a.deps.Agents.Agent(ctx, scope, task.AgentID)
	if err != nil {
		return workflow.Context{}, fmt.Errorf("task: load agent: %w", err)
	}

	// The tools a Bolu may call are the grant's answer, not its wish list: a tool
	// whose integration was never granted must not be offered to the model.
	var tools []domain.Tool
	if a.deps.Tools != nil {
		tools, err = a.deps.Tools.Tools(ctx, scope, task.AgentID)
		if err != nil {
			return workflow.Context{}, fmt.Errorf("task: load tools: %w", err)
		}
	}
	// Handing work to another Bolu is part of the product rather than an
	// integration, so the tool is offered alongside them.
	tools = append(tools, HandoffTool())

	loaded := workflow.Context{
		Agent:  agent,
		Tools:  tools,
		Limits: a.limits,
		Depth:  task.Depth,
	}

	if task.ConversationID != uuid.Nil && a.deps.Reply != nil {
		history, err := a.deps.Reply.History(ctx, scope, task.ConversationID, historyLimit)
		if err != nil {
			return workflow.Context{}, fmt.Errorf("task: load history: %w", err)
		}
		loaded.History = history
	}

	// Memory is loaded once per task rather than per round: the retrieved block
	// then stays identical across the rounds of one task, which is what lets the
	// provider's cache hit on the second round instead of every round paying for
	// a fresh prefix.
	//
	// A failure here is logged rather than fatal. A Bolu that answers without its
	// memory is a worse answer; a Bolu that refuses to answer at all is a broken
	// product.
	if a.deps.Memory != nil {
		recall, err := a.deps.Memory.Recall(ctx, scope, domain.RecallRequest{
			AgentID: task.AgentID,
			TaskID:  task.ID,
			Query:   recallQuery(input, loaded.History),
		})
		if err != nil {
			a.taskLogger(ctx, task).Warn("memory could not be loaded for this task; it will answer without it",
				zap.Error(err),
			)
		} else {
			loaded.Memory = recall
		}
	}

	return loaded, nil
}

// recallQuery is what the memory search runs against: the task's own instruction,
// or the last thing the user said when the task carries no prompt of its own.
func recallQuery(input workflow.TaskInput, history []domain.Message) string {
	if prompt := strings.TrimSpace(input.Prompt); prompt != "" {
		return prompt
	}

	for i := len(history) - 1; i >= 0; i-- {
		if text := strings.TrimSpace(domain.RenderBlocks(history[i].Blocks)); text != "" {
			return text
		}
	}

	return ""
}

// CallModel performs one model call.
func (a *Activities) CallModel(ctx context.Context, request workflow.ModelRequest) (workflow.ModelReply, error) {
	if a.deps.Gateway == nil {
		return workflow.ModelReply{}, fmt.Errorf("task: the model gateway is not configured")
	}

	task, err := a.load(ctx, workflow.TaskInput{TaskID: request.TaskID, WorkspaceID: request.WorkspaceID})
	if err != nil {
		return workflow.ModelReply{}, err
	}
	scope := scopeOf(task)

	// The round's span, with the model call the gateway makes hanging under it. The prompt and
	// the answer are not attributes: the round's size and its cost are what a trace is read for.
	//
	// It covers the budget check and the gateway's refusal as well as the call, so a round that
	// never reached a provider still shows what it was and why it stopped.
	ctx, span := a.span(ctx, "task.model_round", task,
		attribute.Int("history_messages", len(request.Messages)),
		attribute.Int("tool_count", len(request.Tools)),
	)
	defer func() { end(span, err) }()

	// The budget is asked before every call rather than once per task: a workspace
	// that ran out while this task worked must stop spending now.
	if a.deps.Budget != nil {
		if err := a.deps.Budget.Allow(ctx, scope.WorkspaceID); err != nil {
			return workflow.ModelReply{}, err
		}
	}

	response, err := a.deps.Gateway.Chat(ctx, llmdomain.Scope{
		WorkspaceID: scope.WorkspaceID,
		AgentID:     task.AgentID,
	}, a.chatRequest(request, task))
	if err != nil {
		return workflow.ModelReply{}, err
	}

	span.SetAttributes(
		attribute.String("model", response.Model),
		attribute.String("provider", response.Provider),
		attribute.Int("input_tokens", response.Usage.InputTokens),
		attribute.Int("output_tokens", response.Usage.OutputTokens),
		attribute.Int("tool_calls", len(response.ToolCalls)),
	)

	reply := workflow.ModelReply{
		Text:         response.Text,
		Usage:        domain.Usage(response.Usage),
		Model:        response.Model,
		FinishReason: string(response.FinishReason),
	}
	for _, call := range response.ToolCalls {
		reply.ToolCalls = append(reply.ToolCalls, workflow.ToolCall{
			CallID:    call.ID,
			Name:      call.Name,
			Arguments: call.Arguments,
		})
	}

	return reply, nil
}

// RunTool executes one tool call.
//
// A `write_external` tool never reaches here: the workflow routes it to the draft
// gate, so this only runs what is safe to run unattended.
func (a *Activities) RunTool(ctx context.Context, request workflow.ToolRequest) (workflow.ToolOutcome, error) {
	a.retry(ctx, "RunTool")

	task, err := a.load(ctx, workflow.TaskInput{TaskID: request.TaskID, WorkspaceID: request.WorkspaceID})
	if err != nil {
		return workflow.ToolOutcome{}, err
	}
	scope := scopeOf(task)

	// The tool's span, named for the tool rather than for the call: "the slow one is
	// whatsapp.list_messages" is the question, and a call id answers none of it. The
	// arguments are deliberately not attributes — that is where a customer's data enters the
	// run.
	//
	// It opens before the checks below rather than after them, so a call this deployment
	// could not serve is *visible* as a span that failed: an activity that ran and refused is
	// exactly the thing an incident is looking for, and a refusal that produced no span would
	// be an unexplained gap in the trace.
	ctx, span := a.toolSpan(ctx, task, request.Name)
	// One deferred close for every way out: a failure recorded here and a span ended twice are
	// both worse than a single place that decides.
	defer func() { end(span, err) }()

	started := a.now()

	// The call is recorded however it ends, which is why it is deferred rather than written after
	// the call: the integration travels with it — "which integration is failing" is the question,
	// and a tool name alone answers it only for somebody who knows the registry by heart — and a
	// call this deployment refused is a call that never reached the provider, which is exactly
	// what a failure chart should show.
	var result domain.ToolResult
	defer func() {
		if a.deps.Metrics == nil {
			return
		}

		a.deps.Metrics.ToolCall(ctx, appOfTool(request.Name),
			err != nil || result.IsError, a.now().Sub(started).Seconds())
	}()

	if a.deps.Executor == nil {
		err = fmt.Errorf("%w: %s", domain.ErrToolNotAvailable, request.Name)

		return workflow.ToolOutcome{}, err
	}

	result, err = a.deps.Executor.Execute(ctx, scope, domain.ToolCall{
		TaskID:         task.ID,
		AgentID:        task.AgentID,
		CallID:         request.CallID,
		Name:           request.Name,
		Arguments:      request.Arguments,
		ConversationID: task.ConversationID,
	})
	if err != nil {
		return workflow.ToolOutcome{}, err
	}

	// An error the tool reported is not a failure of the runtime: the model is told what went
	// wrong and decides what to do, so the span carries the fact without failing.
	span.SetAttributes(
		attribute.Int("result_bytes", len(result.Content)),
		attribute.Bool("is_error", result.IsError),
	)
	if result.IsError {
		span.SetStatus(codes.Error, "the tool reported an error")
	}

	content := result.Content
	if len(content) > a.limits.MaxToolResultBytes {
		// A tool that answers with a megabyte would be re-sent to the model on
		// every later round, which is the costliest way to run out of tokens.
		content = truncate(content, a.limits.MaxToolResultBytes)
	}

	return workflow.ToolOutcome{
		Name:    request.Name,
		Content: content,
		IsError: result.IsError,
	}, nil
}

// RequestDraft turns a write_external call into a draft a human decides on, and
// parks the task on it.
func (a *Activities) RequestDraft(ctx context.Context, request workflow.ToolRequest) (workflow.DraftOutcome, error) {
	a.retry(ctx, "RequestDraft")

	task, err := a.load(ctx, workflow.TaskInput{TaskID: request.TaskID, WorkspaceID: request.WorkspaceID})
	if err != nil {
		return workflow.DraftOutcome{}, err
	}
	scope := scopeOf(task)

	if a.deps.Drafts == nil {
		// Without an approval gate the only safe answer is to refuse: running the
		// action directly would skip the product's central guardrail.
		return workflow.DraftOutcome{
			Status: domain.DraftCanceled,
			Note:   "gerbang persetujuan tidak dikonfigurasi, jadi aksi ini tidak dijalankan",
			Title:  request.Name,
		}, nil
	}

	draft, err := a.deps.Drafts.RequestDraft(ctx, scope, domain.DraftRequest{
		TaskID:         task.ID,
		AgentID:        task.AgentID,
		ConversationID: task.ConversationID,
		ActionKind:     request.Name,
		Title:          draftTitle(request),
		Payload:        request.Arguments,
	})
	if err != nil {
		return workflow.DraftOutcome{}, err
	}

	// How long the bot took to produce something a person can read, measured from the moment
	// the task opened rather than from this round: the promise is "the work is done, here it
	// is", and a task that thought for three rounds before drafting took all three.
	if a.deps.Metrics != nil && task.StartedAt != nil {
		a.deps.Metrics.DraftReady(ctx, a.now().Sub(*task.StartedAt).Seconds())
	}

	// A policy that allowed the action unattended means there is nobody to wait
	// for: the gate already carried it out, and the task records what happened.
	if draft.Auto {
		a.emit(ctx, scope, task, workflow.EventTaskStep)
		return workflow.DraftOutcome{
			DraftID: draft.ID.String(),
			Status:  draft.Status,
			Note:    draft.Note,
			Title:   draft.Title,
			Auto:    true,
			Content: draft.Content,
		}, nil
	}

	// The task is parked in the store before the workflow waits, so a worker that
	// dies during the wait resumes the wait rather than losing the task.
	parked, err := a.deps.Tasks.Park(ctx, scope, task.ID, "menunggu persetujuan: "+draftTitle(request), draft.ID)
	if err != nil {
		return workflow.DraftOutcome{}, err
	}
	a.emit(ctx, scope, parked, workflow.EventTaskWaiting)

	return workflow.DraftOutcome{
		DraftID: draft.ID.String(),
		Status:  draft.Status,
		Note:    draft.Note,
		Title:   draft.Title,
	}, nil
}

// CarryOutDraft executes an action a person approved.
//
// It reports a hold rather than sending when the workspace is closed, which is
// what lets the workflow wait for the window to open instead of the runtime
// deciding on its own that the action may leave now.
func (a *Activities) CarryOutDraft(ctx context.Context, request workflow.DraftCarryRequest) (workflow.DraftCarryOutcome, error) {
	task, err := a.load(ctx, workflow.TaskInput{TaskID: request.TaskID, WorkspaceID: request.WorkspaceID})
	if err != nil {
		return workflow.DraftCarryOutcome{}, err
	}
	scope := scopeOf(task)

	draftID, err := uuid.Parse(strings.TrimSpace(request.DraftID))
	if err != nil {
		return workflow.DraftCarryOutcome{}, fmt.Errorf("%w: draft id is required", domain.ErrInvalidInput)
	}

	if a.deps.Drafts == nil {
		return workflow.DraftCarryOutcome{}, temporal.NewNonRetryableApplicationError(
			"the approval gate is not configured", "ApprovalMissing", domain.ErrToolNotAvailable)
	}

	result, err := a.deps.Drafts.CarryOut(ctx, scope, draftID)
	if err != nil {
		// A missing executor or a draft nobody approved is a state, not a hiccup:
		// retrying it would spend the retry budget reaching the same answer four
		// times and delay the task's honest failure by seconds.
		if errors.Is(err, domain.ErrDraftNoSender) || errors.Is(err, domain.ErrDraftNotApproved) {
			return workflow.DraftCarryOutcome{}, temporal.NewNonRetryableApplicationError(
				err.Error(), "ApprovalRejected", err)
		}
		return workflow.DraftCarryOutcome{}, err
	}

	if result.Held {
		// A held action is not a failure and not a step: the task is waiting, and
		// it says so in the store a well as on the wire. Without the park its
		// status would stay `running` while the workflow slept for hours, and the
		// office would show it as stuck the moment the heartbeat went stale.
		reason := result.Reason
		if reason == "" {
			reason = "menunggu jam kerja"
		}
		if parked, err := a.deps.Tasks.Park(ctx, scope, task.ID, reason, draftID); err == nil {
			task = parked
		}

		a.emit(ctx, scope, task, workflow.EventTaskWaiting)

		return workflow.DraftCarryOutcome{
			Status:    result.Status,
			Held:      true,
			NotBefore: derefTime(result.NotBefore),
			Reason:    reason,
		}, nil
	}

	a.emit(ctx, scope, task, workflow.EventTaskStep)

	return workflow.DraftCarryOutcome{
		Status:  result.Status,
		Content: result.Content,
	}, nil
}

// derefTime reads a time that may be absent.
func derefTime(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}

// RecordStep writes one round.
func (a *Activities) RecordStep(ctx context.Context, request workflow.StepRequest) error {
	task, err := a.load(ctx, workflow.TaskInput{TaskID: request.TaskID, WorkspaceID: request.WorkspaceID})
	if err != nil {
		return err
	}
	scope := scopeOf(task)

	if _, err := a.deps.Steps.Append(ctx, scope, domain.Step{
		TaskID:       task.ID,
		Kind:         request.Record.Kind,
		ToolName:     request.Record.ToolName,
		ToolLabel:    request.Record.ToolLabel,
		Input:        request.Record.Input,
		Output:       request.Record.Output,
		InputTokens:  request.Record.InputTokens,
		OutputTokens: request.Record.OutputTokens,
	}); err != nil {
		return err
	}

	a.emit(ctx, scope, task, workflow.EventTaskStep)
	return nil
}

// BeatTask records a heartbeat and answers whether the task may keep going.
//
// It is the one place a bound is decided, so "the task stopped because it hit a
// limit" has a single implementation rather than one check per bound, and the
// reason it produces is what the user reads.
func (a *Activities) BeatTask(ctx context.Context, request workflow.Beat) (workflow.Beat, error) {
	task, err := a.load(ctx, workflow.TaskInput{TaskID: request.TaskID, WorkspaceID: request.WorkspaceID})
	if err != nil {
		return workflow.Beat{}, err
	}
	scope := scopeOf(task)

	// A canceled task is discovered here, which is what stops a task whose worker
	// was down when the user pressed cancel.
	if task.Status == domain.StatusCanceled {
		request.StopReason = domain.StatusCanceled
		request.StopDetail = task.StoppedReason
		return request, nil
	}

	if tokens := request.TotalTokens(); tokens > a.limits.MaxTokens {
		request.StopReason = domain.StatusFailed
		request.StopDetail = fmt.Sprintf("tugas berhenti karena mencapai batas token (%d dari %d)",
			tokens, a.limits.MaxTokens)
		return request, nil
	}

	if request.StepCount >= a.limits.MaxSteps {
		request.StopReason = domain.StatusFailed
		request.StopDetail = fmt.Sprintf("tugas berhenti karena mencapai batas langkah (%d)", a.limits.MaxSteps)
		return request, nil
	}

	// The workspace budget is asked here as well as before each model call: a task
	// that spent the allowance mid-round must not start another.
	if a.deps.Budget != nil {
		if err := a.deps.Budget.Allow(ctx, scope.WorkspaceID); err != nil {
			request.StopReason = domain.StatusFailed
			request.StopDetail = "tugas berhenti: " + err.Error()
			//nolint:nilerr // a spent budget is the task's stop reason, not the activity's failure
			return request, nil
		}
	}

	if err := a.deps.Tasks.Progress(ctx, scope, domain.Progress{
		TaskID:       task.ID,
		InputTokens:  request.InputTokens,
		OutputTokens: request.OutputTokens,
		CostMicros:   a.cost(task, request),
		StepCount:    request.StepCount,
	}); err != nil {
		return workflow.Beat{}, err
	}

	a.emit(ctx, scope, task, workflow.EventTaskRunning)
	return request, nil
}

// CompleteTask writes the terminal state and the answer.
func (a *Activities) CompleteTask(ctx context.Context, request workflow.CompleteRequest) (workflow.TaskOutcome, error) {
	a.retry(ctx, "CompleteTask")

	task, err := a.load(ctx, workflow.TaskInput{TaskID: request.TaskID, WorkspaceID: request.WorkspaceID})
	if err != nil {
		return workflow.TaskOutcome{}, err
	}
	scope := scopeOf(task)

	stored, err := a.deps.Tasks.Finish(ctx, scope, domain.Finish{
		TaskID:        task.ID,
		Status:        request.Status,
		Summary:       a.summary(ctx, scope, task),
		StoppedReason: request.Reason,
		InputTokens:   request.InputTokens,
		OutputTokens:  request.OutputTokens,
		CostMicros:    request.CostMicros,
		StepCount:     request.StepCount,
	})
	if err != nil {
		return workflow.TaskOutcome{}, err
	}

	// The answer is written into the conversation, which is what makes a task
	// started from chat visible where it was asked for.
	if a.deps.Reply != nil && task.ConversationID != uuid.Nil {
		status := workflow.MessageComplete
		if stored.Status != domain.StatusSucceeded {
			status = workflow.MessageFailed
		}
		if err := a.deps.Reply.Write(ctx, scope, domain.WriteReply{
			MessageID:    task.ReplyMessageID,
			Blocks:       request.Blocks,
			Status:       status,
			FinishReason: stored.StoppedReason,
			TaskID:       stored.ID,
		}); err != nil {
			// A task whose answer could not be written is still finished: saying
			// otherwise would leave it live forever.
			a.taskLogger(ctx, stored).Warn("could not write the task's answer into the conversation",
				zap.Error(err))
		}
	}

	// The task's own numbers, which is what the depth comes back down from and what "tokens and
	// cost per task" is read from. They are taken from the stored row rather than from the
	// request, so a task whose tokens the runtime recomputed is charted as it was stored — the
	// same numbers the ledger holds and the dashboard shows.
	if a.deps.Metrics != nil {
		a.deps.Metrics.TaskSettled(ctx)
		a.deps.Metrics.TaskFinished(ctx, stored.Trigger, stored.Status,
			durationOf(stored, a.now()), stored.InputTokens, stored.OutputTokens, stored.CostMicros)
	}

	a.emit(ctx, scope, stored, workflow.EventTaskFinished)

	// Whoever opened this task is told how it ended. A routine's run is finished
	// with the task's own one-line summary, which is what its history shows; a task
	// opened by chat has no claimant and that is not a failure.
	if a.deps.Outcomes != nil {
		if err := a.deps.Outcomes.Report(ctx, scope, domain.OutcomeReport{
			TaskID:  stored.ID,
			Status:  stored.Status,
			Summary: stored.Summary,
			Reason:  stored.StoppedReason,
		}); err != nil && !errors.Is(err, domain.ErrOutcomeUnclaimed) {
			// A task whose outcome could not be reported is still finished: saying
			// otherwise would leave it live forever.
			a.taskLogger(ctx, stored).Warn("could not report the task's outcome", zap.Error(err))
		}
	}

	return workflow.TaskOutcome{
		Status:  stored.Status,
		Summary: stored.Summary,
		Reason:  stored.StoppedReason,
	}, nil
}

// PrepareHandoff opens a child task for another Bolu and returns the input its
// workflow starts with.
//
// It is a separate step from starting the child because the workflow does the
// starting: a Temporal child workflow is what makes the chain visible in the UI
// and lets the parent wait for the answer without holding an activity open for
// half an hour.
//
// A refusal is returned as a message rather than an error, because the model is
// the one that has to react to it.
func (a *Activities) PrepareHandoff(ctx context.Context, request workflow.HandoffRequest) (workflow.PreparedHandoff, error) {
	parent, err := a.load(ctx, workflow.TaskInput{TaskID: request.ParentTaskID, WorkspaceID: request.WorkspaceID})
	if err != nil {
		return workflow.PreparedHandoff{}, err
	}
	scope := scopeOf(parent)

	refuse := func(reason string) (workflow.PreparedHandoff, error) {
		return workflow.PreparedHandoff{Refusal: reason}, nil
	}

	if request.Depth >= a.limits.MaxHandoffDepth {
		return refuse(fmt.Sprintf(
			"serah-terima ditolak: rantai sudah %d tingkat, batasnya %d",
			request.Depth, a.limits.MaxHandoffDepth))
	}

	if a.deps.Namer == nil {
		return refuse("serah-terima ditolak: registry tidak bisa mencari Bolu berdasarkan nama")
	}
	// The lookup is by name because that is what the model knows: it is told the
	// roster, not the ids.
	target, err := a.deps.Namer.ByName(ctx, scope, request.AgentName)
	if err != nil {
		return refuse(fmt.Sprintf("serah-terima ditolak: tidak ada Bolu bernama %q", request.AgentName))
	}
	if !a.deps.Agents.Active(target) {
		return refuse(fmt.Sprintf("serah-terima ditolak: %s sedang istirahat", target.Name))
	}

	instructions := strings.TrimSpace(request.Instructions)
	if instructions == "" {
		return refuse("serah-terima ditolak: instruksinya kosong")
	}

	child, err := a.deps.Tasks.Create(ctx, scope, domain.NewTask{
		AgentID:        target.ID,
		ParentTaskID:   parent.ID,
		ConversationID: parent.ConversationID,
		Trigger:        domain.TriggerHandoff,
		Title:          firstLine(instructions),
		Depth:          request.Depth + 1,
	})
	if err != nil {
		return workflow.PreparedHandoff{}, err
	}

	return workflow.PreparedHandoff{Child: workflow.TaskInput{
		TaskID:         child.ID.String(),
		WorkspaceID:    scope.WorkspaceID.String(),
		AgentID:        target.ID.String(),
		ConversationID: domain.WireID(parent.ConversationID),
		Title:          child.Title,
		Prompt:         instructions,
		Depth:          child.Depth,
	}}, nil
}

// ResumeTask clears a parked task's waiting state once the decision is in.
func (a *Activities) ResumeTask(ctx context.Context, request workflow.ResumeRequest) error {
	task, err := a.load(ctx, workflow.TaskInput{TaskID: request.TaskID, WorkspaceID: request.WorkspaceID})
	if err != nil {
		return err
	}

	if task.Status != domain.StatusWaitingApproval {
		// The task is no longer parked, which is not a failure: a cancel that
		// landed while the decision was in flight already finished it.
		return nil
	}

	resumed, err := a.deps.Tasks.Resume(ctx, scopeOf(task), task.ID)
	if err != nil {
		return err
	}

	a.emit(ctx, scopeOf(task), resumed, workflow.EventTaskRunning)
	return nil
}

// MarkHandoffStarted records the workflow a child task runs under, so the chain
// is traceable from the parent as well as in Temporal.
func (a *Activities) MarkHandoffStarted(ctx context.Context, request workflow.HandoffStarted) error {
	child, err := a.load(ctx, workflow.TaskInput{TaskID: request.TaskID, WorkspaceID: request.WorkspaceID})
	if err != nil {
		return err
	}
	scope := scopeOf(child)

	started, err := a.deps.Tasks.Start(ctx, scope, child.ID, request.WorkflowID, request.RunID)
	if err != nil {
		return err
	}

	a.emit(ctx, scope, started, workflow.EventTaskStarted)
	return nil
}

// load reads one task inside its workspace scope.
func (a *Activities) load(ctx context.Context, input workflow.TaskInput) (domain.Task, error) {
	workspaceID, err := uuid.Parse(strings.TrimSpace(input.WorkspaceID))
	if err != nil {
		return domain.Task{}, fmt.Errorf("%w: workspace id is required", domain.ErrInvalidInput)
	}
	taskID, err := uuid.Parse(strings.TrimSpace(input.TaskID))
	if err != nil {
		return domain.Task{}, fmt.Errorf("%w: task id is required", domain.ErrInvalidInput)
	}

	return a.deps.Tasks.Get(ctx, domain.Scope{WorkspaceID: workspaceID}, taskID)
}

// chatRequest renders one model call.
func (a *Activities) chatRequest(request workflow.ModelRequest, task domain.Task) llmdomain.ChatRequest {
	tools := make([]llmdomain.Tool, 0, len(request.Tools))
	for _, tool := range request.Tools {
		tools = append(tools, tool.AsLLMTool())
	}

	// The turns map one to one: the workflow keeps the conversation in the shape
	// the model reads, so a tool call and its result reach the provider as the
	// pair they are rather than as loose prose.
	messages := make([]llmdomain.Message, 0, len(request.Messages))
	for _, turn := range request.Messages {
		messages = append(messages, llmdomain.Message{
			Role:        turn.Role,
			Text:        turn.Text,
			ToolCalls:   turn.ToolCalls,
			ToolResults: turn.ToolResults,
		})
	}

	return llmdomain.ChatRequest{
		System:    request.System,
		Retrieved: request.Retrieved,
		Messages:  messages,
		Tools:     tools,
		// The model decides: some tasks are prose, some need a tool.
		ToolChoice: llmdomain.ToolChoiceAuto,
		Metadata: llmdomain.RequestMetadata{
			WorkspaceID: task.WorkspaceID,
			AgentID:     task.AgentID,
			TaskID:      task.ID,
			Purpose:     llmdomain.PurposeAgent,
		},
	}
}

// summary writes the one line that survives the raw steps.
func (a *Activities) summary(ctx context.Context, scope domain.Scope, task domain.Task) string {
	steps, err := a.deps.Steps.ListSteps(ctx, scope, task.ID)
	if err != nil {
		return ""
	}

	tools := 0
	var last string
	for _, step := range steps {
		switch step.Kind {
		case domain.StepToolCall:
			tools++
		case domain.StepFinal:
			var payload struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(step.Output, &payload); err == nil {
				last = strings.TrimSpace(payload.Text)
			}
		}
	}

	switch {
	case last != "":
		return firstLine(last)
	case tools > 0:
		return fmt.Sprintf("%d langkah, %d alat dipakai", len(steps), tools)
	default:
		return fmt.Sprintf("%d langkah", len(steps))
	}
}

// cost prices what the task spent, so its own total is recorded rather than
// inferred later from the ledger.
func (a *Activities) cost(task domain.Task, beat workflow.Beat) int64 {
	if a.deps.Costs == nil {
		return task.CostMicros
	}
	return a.deps.Costs.Cost(task.Title, domain.Usage{
		InputTokens:  int(beat.InputTokens),
		OutputTokens: int(beat.OutputTokens),
	}.AsLLMUsage())
}

// emit publishes a task event, which is how the feed and the office see it.
func (a *Activities) emit(ctx context.Context, scope domain.Scope, task domain.Task, eventType string) {
	if a.deps.Events == nil {
		return
	}

	payload, err := json.Marshal(map[string]any{
		"task_id":  task.ID.String(),
		"agent_id": domain.WireID(task.AgentID),
		"status":   task.Status,
		"trigger":  task.Trigger,
		"title":    task.Title,
	})
	if err != nil {
		return
	}

	if err := a.deps.Events.Emit(ctx, workflow.TaskEvent(scope, task, eventType, payload)); err != nil {
		a.taskLogger(ctx, task).Warn("task event published only to the store",
			zap.String("type", eventType), zap.Error(err))
	}
}

func scopeOf(task domain.Task) domain.Scope {
	return domain.Scope{WorkspaceID: task.WorkspaceID}
}
