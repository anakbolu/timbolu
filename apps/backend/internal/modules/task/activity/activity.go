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
	"fmt"
	"strings"

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
	// Namer resolves a Bolu by the name the model knows, which is what a handoff
	// names.
	Namer  domain.AgentNamer
	Limits domain.Limits
	Logger *zap.Logger
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

	return loaded, nil
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
	if a.deps.Executor == nil {
		return workflow.ToolOutcome{}, fmt.Errorf("%w: %s", domain.ErrToolNotAvailable, request.Name)
	}

	task, err := a.load(ctx, workflow.TaskInput{TaskID: request.TaskID, WorkspaceID: request.WorkspaceID})
	if err != nil {
		return workflow.ToolOutcome{}, err
	}
	scope := scopeOf(task)

	result, err := a.deps.Executor.Execute(ctx, scope, domain.ToolCall{
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
		}); err != nil && a.deps.Logger != nil {
			// A task whose answer could not be written is still finished: saying
			// otherwise would leave it live forever.
			a.deps.Logger.Warn("could not write the task's answer into the conversation",
				zap.String("task_id", stored.ID.String()), zap.Error(err))
		}
	}

	a.emit(ctx, scope, stored, workflow.EventTaskFinished)

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

	messages := make([]llmdomain.Message, 0, len(request.Messages))
	for _, message := range request.Messages {
		role := llmdomain.RoleUser
		if message.FromAgent() {
			role = llmdomain.RoleAssistant
		}
		messages = append(messages, llmdomain.Message{Role: role, Text: domain.RenderBlocks(message.Blocks)})
	}

	return llmdomain.ChatRequest{
		System:   request.System,
		Messages: messages,
		Tools:    tools,
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

	if err := a.deps.Events.Emit(ctx, workflow.TaskEvent(scope, task, eventType, payload)); err != nil && a.deps.Logger != nil {
		a.deps.Logger.Warn("task event published only to the store",
			zap.String("type", eventType), zap.Error(err))
	}
}

func scopeOf(task domain.Task) domain.Scope {
	return domain.Scope{WorkspaceID: task.WorkspaceID}
}
