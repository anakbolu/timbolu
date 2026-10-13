// Package domain defines the task runtime contract: the task a Bolu runs, the
// steps it took, the tools it may call, and the ports the workflow needs for
// everything that is not deterministic.
//
// A task is a durable unit of work. It survives a worker restart, it can wait
// for a human for hours, and every round it takes is recorded — which is what
// lets the office, the activity feed, and the history read the same truth.
package domain

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
)

// Status values, matching the check constraint on `tasks.status`.
const (
	// StatusQueued is a task that exists but has not started.
	StatusQueued = "queued"
	// StatusRunning is a task a workflow is working on.
	StatusRunning = "running"
	// StatusWaitingApproval is a task parked on a human decision. It has not
	// stopped: it is waiting, and the office shows it that way.
	StatusWaitingApproval = "waiting_approval"
	StatusSucceeded       = "succeeded"
	StatusFailed          = "failed"
	StatusCanceled        = "canceled"
)

// Triggers, matching the check constraint on `tasks.trigger`. The trigger is
// what makes "where did this task come from" answerable after the fact.
const (
	TriggerChat    = "chat"
	TriggerRoutine = "routine"
	TriggerWebhook = "webhook"
	TriggerHandoff = "handoff"
)

// Step kinds, matching the check constraint on `task_steps.kind`.
const (
	// StepThink is one round of the model's reasoning, including its tokens.
	StepThink = "think"
	// StepToolCall is one tool the model asked for.
	StepToolCall = "tool_call"
	// StepToolResult is what that tool answered.
	StepToolResult = "tool_result"
	// StepDraft is a draft the task created, which is what a write_external
	// tool becomes.
	StepDraft = "draft"
	// StepFinal is the answer the task settled on.
	StepFinal = "final"
)

// Errors the service and the handlers map onto responses.
var (
	ErrTaskNotFound   = errors.New("task: not found")
	ErrInvalidInput   = errors.New("task: invalid input")
	ErrUnknownTrigger = errors.New("task: unknown trigger")
	ErrUnknownStep    = errors.New("task: unknown step kind")
	// ErrStepLimitReached stops a task that used its whole step allowance.
	ErrStepLimitReached = errors.New("task: step limit reached")
	// ErrTokenLimitReached stops a task that used its whole token allowance.
	ErrTokenLimitReached = errors.New("task: token limit reached")
	// ErrCostLimitReached stops a task the workspace can no longer afford.
	ErrCostLimitReached = errors.New("task: daily cost limit reached")
	// ErrDepthLimitReached refuses a handoff that would nest too deep.
	ErrDepthLimitReached = errors.New("task: handoff chain is too deep")
	// ErrCrossWorkspaceHandoff refuses a handoff to another tenant.
	ErrCrossWorkspaceHandoff = errors.New("task: handoff must stay in the workspace")
	// ErrToolNotAvailable means nothing can execute the tool that was asked for,
	// which is a configuration fact rather than a failure to retry.
	ErrToolNotAvailable = errors.New("task: tool is not available")
	// ErrDraftNoSender means nothing can carry an approved action out, which is a
	// configuration fact rather than a failure to retry.
	ErrDraftNoSender = errors.New("task: no executor can carry this action out")
	// ErrDraftNotApproved means the runtime tried to send a draft nobody approved.
	ErrDraftNotApproved = errors.New("task: the draft is not approved")
	// ErrRevisionLimitReached stops a task whose action keeps being sent back.
	// The message is what the user reads, so it is in their language.
	ErrRevisionLimitReached = errors.New("tugas berhenti: aksi ini terlalu banyak dikembalikan untuk direvisi")
)

// Limits bound what one task may spend. Zero means the configured default.
type Limits struct {
	// MaxSteps is how many rounds of the model-and-tools loop a task may take.
	MaxSteps int
	// MaxTokens is how many tokens one task may spend, input and output
	// together.
	MaxTokens int64
	// MaxHandoffDepth is how deep a chain of handoffs may nest.
	MaxHandoffDepth int
	// MaxToolResultBytes truncates a tool's answer. A tool that returns a
	// megabyte would otherwise be re-sent to the model on every later round,
	// which is the costliest way to run out of tokens.
	MaxToolResultBytes int
	// MaxRevisions bounds how many times a person may send one action back.
	// Without it a reviewer and an agent can argue for as long as the task's
	// token allowance lasts, which is a cost nobody agreed to.
	MaxRevisions int
}

// DefaultLimits are the bounds a deployment runs with when the configuration
// says nothing. They are the product's judgement, so they live here rather than
// being duplicated per call site.
var DefaultLimits = Limits{
	MaxSteps:           12,
	MaxTokens:          120_000,
	MaxHandoffDepth:    2,
	MaxToolResultBytes: 16 * 1024,
	MaxRevisions:       3,
}

// Normalise fills the zero fields from the defaults, so a partially configured
// deployment still has bounds.
func (l Limits) Normalise() Limits {
	if l.MaxSteps <= 0 {
		l.MaxSteps = DefaultLimits.MaxSteps
	}
	if l.MaxTokens <= 0 {
		l.MaxTokens = DefaultLimits.MaxTokens
	}
	if l.MaxHandoffDepth < 0 {
		l.MaxHandoffDepth = DefaultLimits.MaxHandoffDepth
	}
	if l.MaxToolResultBytes <= 0 {
		l.MaxToolResultBytes = DefaultLimits.MaxToolResultBytes
	}
	if l.MaxRevisions <= 0 {
		l.MaxRevisions = DefaultLimits.MaxRevisions
	}
	return l
}

// Scope is the tenant identity a query runs as.
type Scope struct {
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
}

// IsZero reports whether the scope carries no identity.
func (s Scope) IsZero() bool { return s.WorkspaceID == uuid.Nil }

// Task is one unit of work a Bolu runs.
type Task struct {
	ID             uuid.UUID
	WorkspaceID    uuid.UUID
	AgentID        uuid.UUID
	ParentTaskID   uuid.UUID
	ConversationID uuid.UUID
	Trigger        string
	Title          string
	Status         string
	// ReplyMessageID is the conversation message this task's answer fills.
	ReplyMessageID uuid.UUID
	WorkflowID     string
	TemporalRunID  string
	// Depth is how far down a chain of handoffs this task is. A task a user
	// started is zero.
	Depth int
	// Summary is what the task did, in one line. It is what survives after the
	// raw steps are pruned.
	Summary string
	// StoppedReason explains a task that stopped without finishing, in the
	// user's words.
	StoppedReason string
	// WaitingReason is what a parked task is waiting for.
	WaitingReason string
	// WaitingDraftID is the approval a parked task is waiting on.
	WaitingDraftID uuid.UUID
	StepCount      int
	InputTokens    int64
	OutputTokens   int64
	CostMicros     int64
	HeartbeatAt    *time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// TotalTokens is what the task spent, which is what the limits compare against.
func (t Task) TotalTokens() int64 { return t.InputTokens + t.OutputTokens }

// Live reports whether the task still occupies a Bolu: queued, running, or
// parked on a decision.
func (t Task) Live() bool {
	switch t.Status {
	case StatusQueued, StatusRunning, StatusWaitingApproval:
		return true
	default:
		return false
	}
}

// Live since is how long a task may go without a heartbeat before it is
// reported as stuck. A worker that dies leaves its task `running` forever, so
// the status alone cannot answer the question.
const stuckAfter = 5 * time.Minute

// Health is how the UI describes a live task.
type Health string

// Health values.
const (
	HealthRunning Health = "running"
	HealthWaiting Health = "waiting"
	HealthStuck   Health = "stuck"
	HealthDone    Health = "done"
)

// Health reports how a task looks right now. It is derived, never stored: the
// heartbeat is the fact, and "stuck" is what a stale heartbeat means.
func (t Task) Health(now time.Time) Health {
	if !t.Live() {
		return HealthDone
	}
	if t.Status == StatusWaitingApproval {
		return HealthWaiting
	}
	if t.HeartbeatAt == nil || now.Sub(*t.HeartbeatAt) > stuckAfter {
		// A queued task has no heartbeat yet, so it is only stuck once it should
		// have started and did not.
		if t.Status == StatusQueued && now.Sub(t.CreatedAt) <= stuckAfter {
			return HealthRunning
		}
		return HealthStuck
	}
	return HealthRunning
}

// Step is one recorded round of a task.
type Step struct {
	ID          uuid.UUID
	TaskID      uuid.UUID
	Seq         int
	Kind        string
	ToolName    string
	ToolLabel   string
	Input       json.RawMessage
	Output      json.RawMessage
	InputTokens int
	// OutputTokens are the model's tokens for this step, which only a think
	// step spends.
	OutputTokens int
	// Pruned marks a step whose raw payload the retention job removed. The row
	// stays so the shape of the task is readable after its contents are gone.
	Pruned    bool
	CreatedAt time.Time
}

// Progress is the heartbeat a running task records.
type Progress struct {
	TaskID       uuid.UUID
	InputTokens  int64
	OutputTokens int64
	CostMicros   int64
	StepCount    int
}

// Finish is the terminal state of a task.
type Finish struct {
	TaskID        uuid.UUID
	Status        string
	Summary       string
	StoppedReason string
	InputTokens   int64
	OutputTokens  int64
	CostMicros    int64
	StepCount     int
}

// Filter narrows a task history read.
type Filter struct {
	// AgentID limits the read to one Bolu.
	AgentID uuid.UUID
	// ParentTaskID limits the read to the children of one task.
	ParentTaskID uuid.UUID
	// Status limits the read to one status; empty means every status.
	Status string
	// Live limits the read to tasks that have not finished.
	Live  bool
	Limit int
}

// DefaultHistoryLimit is the page a client gets when it does not ask for one.
const DefaultHistoryLimit = 50

// MaxHistoryLimit bounds one page.
const MaxHistoryLimit = 200

// TaskRepository is the persistence contract for tasks.
type TaskRepository interface {
	// Create stores a queued task.
	Create(ctx context.Context, scope Scope, task NewTask) (Task, error)
	// Get returns one task.
	Get(ctx context.Context, scope Scope, id uuid.UUID) (Task, error)
	// List returns the history, newest first.
	List(ctx context.Context, scope Scope, filter Filter) ([]Task, error)
	// Start records the workflow that owns the task and marks it running. It is
	// what makes a restart resume rather than start over.
	Start(ctx context.Context, scope Scope, id uuid.UUID, workflowID, runID string) (Task, error)
	// Progress records a heartbeat with the running totals.
	Progress(ctx context.Context, scope Scope, progress Progress) error
	// Park records that the task is waiting for a human decision.
	Park(ctx context.Context, scope Scope, id uuid.UUID, reason string, draftID uuid.UUID) (Task, error)
	// Resume clears the waiting state when the decision arrives.
	Resume(ctx context.Context, scope Scope, id uuid.UUID) (Task, error)
	// Finish records the terminal state. A finished task is never written again.
	Finish(ctx context.Context, scope Scope, finish Finish) (Task, error)
	// Cancel stops a task that has not finished.
	Cancel(ctx context.Context, scope Scope, id uuid.UUID, reason string) (Task, error)
	// Children returns the tasks one task handed work to.
	Children(ctx context.Context, scope Scope, taskID uuid.UUID) ([]Task, error)
}

// StepRepository is the persistence contract for steps.
type StepRepository interface {
	// Append writes one step, assigning its sequence number.
	Append(ctx context.Context, scope Scope, step Step) (Step, error)
	// ListSteps returns a task's steps in order.
	ListSteps(ctx context.Context, scope Scope, taskID uuid.UUID) ([]Step, error)
}

// StepPruner removes the raw payload of steps past their window.
//
// It is a separate interface rather than a method on StepRepository for a reason worth
// stating: running a task and deleting its content are different powers, and nothing that runs
// tasks should hold the second one by construction. The repository implements this; the
// retention job is the only caller.
//
// The rows stay. What a step *was* — its kind, its tool, its sequence, its token counts, when
// it ran — is what the task history and the cost report are built from, and that is not
// content. Only `Input` and `Output` go, and the step is marked, so a reader can tell an
// emptied payload from one that was never there.
type StepPruner interface {
	// PruneContents removes the raw input and output of steps older than a moment, and
	// reports how many it emptied and how many are still waiting.
	PruneContents(ctx context.Context, scope Scope, olderThan time.Time, limit int) (StepPrune, error)
}

// DefaultPruneLimit bounds one bulk prune.
const DefaultPruneLimit = 500

// StepPrune is what one bulk prune did.
type StepPrune struct {
	// Pruned is how many steps were emptied by the call.
	Pruned int
	// Remaining is how many steps were already past the moment and still hold their
	// contents, which is what tells an operator whether one batch kept up.
	Remaining int
}

// NewTask is a task to insert.
type NewTask struct {
	AgentID        uuid.UUID
	ParentTaskID   uuid.UUID
	ConversationID uuid.UUID
	ReplyMessageID uuid.UUID
	Trigger        string
	Title          string
	Depth          int
}

// AgentRef is the minimum a task needs to know about the Bolu running it. The
// fields are the registry's own, so the container adapts one module's type to
// this rather than the two modules importing each other.
type AgentRef struct {
	ID      uuid.UUID
	Name    string
	Role    string
	Persona string
	Tone    string
	// Stored is the registry's own switch and Display is its derived status.
	Stored  string
	Display string
	// Tools is what the Bolu asks for; the registry restricts it to the
	// integrations it was granted.
	Tools        []string
	DefaultModel map[string]any
}

// Resting is the stored status that takes no new work.
const Resting = "resting"

// Active reports whether the Bolu accepts new work.
func (a AgentRef) Active() bool { return a.Stored != Resting }

// AgentDirectory reads the Bolu of a workspace.
type AgentDirectory interface {
	// Agent returns one Bolu, or an error when it is not in the workspace.
	Agent(ctx context.Context, scope Scope, id uuid.UUID) (AgentRef, error)
	// Active reports whether a Bolu accepts new work. A resting Bolu never does.
	Active(ref AgentRef) bool
}

// AgentNamer resolves a Bolu by the name the model knows, which is what a
// handoff names: the model is told the roster, not the ids.
type AgentNamer interface {
	ByName(ctx context.Context, scope Scope, name string) (AgentRef, error)
}

// Message is one turn of the conversation a task answers. It mirrors the chat
// module's message so this package depends on the shape it needs rather than on
// another module's internals.
type Message struct {
	ID             uuid.UUID
	ConversationID uuid.UUID
	AgentID        uuid.UUID
	UserID         uuid.UUID
	Blocks         []Block
}

// Block is one content block of a message.
type Block struct {
	Type string
	Body []byte
}

// Turn is one message of the conversation the agent reads.
//
// It is separate from Message because a model's turn carries more than prose: an
// assistant turn may ask for tools, and the results of those tools are their own
// turn. The runtime has to keep both, or the model would be asked the same
// question every round and never learn what its own tool calls answered.
type Turn struct {
	Role llmdomain.Role
	Text string
	// ToolCalls is what the assistant asked for, on an assistant turn.
	ToolCalls []llmdomain.ToolCall
	// ToolResults is what those calls answered, on a tool turn.
	ToolResults []llmdomain.ToolResult
}

// FromAgent reports whether a Bolu wrote the message.
func (m Message) FromAgent() bool { return m.AgentID != uuid.Nil }

// ReplyWriter writes a task's answer into the conversation it came from, and
// reads the thread the task is answering.
//
// The task runtime never touches the chat tables itself: the answer is a
// message, and the chat module owns what a message is.
type ReplyWriter interface {
	// History returns the conversation so far, oldest first.
	History(ctx context.Context, scope Scope, conversationID uuid.UUID, limit int) ([]Message, error)
	// Write stores the answer. It is the same call the in-process responder
	// makes, so a task's answer and a chat reply are one shape.
	Write(ctx context.Context, scope Scope, write WriteReply) error
}

// WriteReply is the body of an answer at one moment.
type WriteReply struct {
	MessageID    uuid.UUID
	Blocks       []Block
	Status       string
	FinishReason string
	TaskID       uuid.UUID
}

// OutcomeSink is told how a task ended.
//
// It is how a task's result reaches the thing that asked for it when that thing is
// not a conversation. A routine opens a task and its history shows the task's own
// one-line summary, so a task a routine opened reports back through this port
// rather than being polled for; a task opened by chat has no sink entry and says so
// with ErrOutcomeUnclaimed, which the caller ignores because most tasks in the
// product were opened by chat.
type OutcomeSink interface {
	Report(ctx context.Context, scope Scope, report OutcomeReport) error
}

// OutcomeReport is one finished task's outcome.
type OutcomeReport struct {
	TaskID uuid.UUID
	// Status is the task's terminal status: succeeded, failed, or canceled.
	Status string
	// Summary is the task's own one line, which is what a history reads.
	Summary string
	// Reason is why a task stopped without succeeding, in the user's words. It
	// travels with the outcome because the history of what asked for the task says
	// why it did not happen, and "0 langkah" is not that answer.
	Reason string
}

// ErrOutcomeUnclaimed means nothing was waiting on this task, which is the normal
// case for a task a person started.
var ErrOutcomeUnclaimed = errors.New("task: no outcome sink claimed this task")

// Event describes one entry of the activity stream, which is how the feed, the
// office, and the chat see one truth.
//
// It mirrors the chat module's event rather than importing it: the container
// adapts the two, so neither module reaches into the other.
type Event struct {
	WorkspaceID    uuid.UUID
	Type           string
	ActorAgentID   uuid.UUID
	ActorUserID    uuid.UUID
	ConversationID uuid.UUID
	TaskID         uuid.UUID
	DraftID        uuid.UUID
	Payload        json.RawMessage
}

// EventSink publishes what a task is doing.
type EventSink interface {
	Emit(ctx context.Context, event Event) error
}

// Tool is one capability a task may call.
//
// Schema is what the model is offered: a tool without one cannot be called, so
// the registry only reports tools whose schema and executor both exist.
type Tool struct {
	Name        string
	Integration string
	Description string
	Schema      json.RawMessage
	// Label decides whether calling the tool needs a human. `read` never does;
	// `write_external` always does.
	Label string
}

// ToolHandoff is the tool that asks another Bolu to do part of the work. It is
// part of the runtime rather than an integration, so the workflow performs it
// itself instead of an executor.
const ToolHandoff = "handoff"

// Tool labels, matching the check constraint on `tool_catalog.label`.
const (
	LabelRead          = "read"
	LabelWriteInternal = "write_internal"
	LabelWriteExternal = "write_external"
)

// NeedsApproval reports whether a tool reaches outside Bolu, which is what the
// approval policy keys on.
func (t Tool) NeedsApproval() bool { return t.Label == LabelWriteExternal }

// AsLLMTool renders a tool for the model.
func (t Tool) AsLLMTool() llmdomain.Tool {
	return llmdomain.Tool{
		Name:        t.Name,
		Description: t.Description,
		Schema:      t.Schema,
		Label:       t.Label,
	}
}

// ToolRegistry reports the tools an agent may actually call.
//
// Two filters apply and both are real: the grant (which integrations the Bolu
// was given, EPIC 3) and an executor (something that can run the tool, EPIC 8).
// A tool that passes neither is not offered to the model, because offering a
// tool nothing can execute teaches the model to call it.
type ToolRegistry interface {
	Tools(ctx context.Context, scope Scope, agentID uuid.UUID) ([]Tool, error)
}

// ToolExecutor runs one tool call.
//
// It returns the answer as a JSON document, which becomes the tool result the
// model reads. An error it returns is handed back to the model as a failed tool
// result rather than ending the task, so the model can try something else.
type ToolExecutor interface {
	Execute(ctx context.Context, scope Scope, call ToolCall) (ToolResult, error)
}

// ToolCall is one tool the model asked for.
type ToolCall struct {
	TaskID         uuid.UUID
	AgentID        uuid.UUID
	CallID         string
	Name           string
	Arguments      json.RawMessage
	ConversationID uuid.UUID
}

// ToolResult is what a tool answered.
type ToolResult struct {
	// Content is the JSON document the model reads.
	Content json.RawMessage
	// IsError marks an answer that reports a failure, which is how a tool's own
	// error reaches the model as something it can react to.
	IsError bool
}

// MemoryRecall hands one round the memory a Bolu answers with, and records what
// it used.
//
// It is a port rather than a direct call because the memory module owns what a
// memory is: the task runtime asks for a prompt section and receives text, which
// is what keeps this module from learning about embeddings, layers, and layers'
// rules.
type MemoryRecall interface {
	Recall(ctx context.Context, scope Scope, request RecallRequest) (Recall, error)
}

// RecallRequest is what one round asks the memory module for.
type RecallRequest struct {
	AgentID uuid.UUID
	TaskID  uuid.UUID
	// Query is the task at hand, which is what the search runs against.
	Query string
}

// Recall is the memory one round is given.
//
// Stable is the workspace-wide memory: it goes into the prefix a provider caches
// along with the persona and the instructions. Retrieved is what the search
// found for this question — the Bolu's private memory and the document chunks —
// and it is already wrapped in the marker that says it is data.
type Recall struct {
	Stable    string
	Retrieved string
	Used      int
}

// DraftGate turns an action that touches the outside world into a draft a human
// decides on.
//
// It is the product's central guardrail: the task runtime may propose sending an
// email, and this is the only way that proposal becomes an action. EPIC 7 owns
// the policy that decides whether a draft is even needed; this port is how a
// task asks.
type DraftGate interface {
	// RequestDraft stores the proposal and returns the draft a human decides on,
	// or reports that the workspace's policy allowed it to go out unattended.
	RequestDraft(ctx context.Context, scope Scope, request DraftRequest) (Draft, error)
	// CarryOut executes a draft a person approved.
	//
	// It reports a hold rather than sending when the workspace is closed, which is
	// what lets the workflow wait for the window to open instead of the runtime
	// deciding on its own that the action may leave now.
	CarryOut(ctx context.Context, scope Scope, draftID uuid.UUID) (DraftExecution, error)
}

// DraftExecution is what carrying out an approved action produced.
type DraftExecution struct {
	// Status is the draft's status afterwards: sent when it left, approved when
	// it is still held.
	Status string
	// Content is what the action answered.
	Content json.RawMessage
	// Held says the action is waiting for the workspace's working hours, and
	// NotBefore is when it may leave.
	Held      bool
	NotBefore *time.Time
	// Reason explains a hold in the user's words.
	Reason string
}

// DraftRequest is one action awaiting a decision.
type DraftRequest struct {
	TaskID         uuid.UUID
	AgentID        uuid.UUID
	ConversationID uuid.UUID
	ActionKind     string
	Title          string
	Payload        json.RawMessage
}

// Draft is the decision a task waits on.
type Draft struct {
	ID uuid.UUID
	// Title is what the draft is about, which the task shows while it waits.
	Title string
	// Status is pending, approved, revise, sent, or canceled.
	Status string
	// Note is what the person said when they decided.
	Note string
	// Auto says the workspace's policy allowed the action to go out without a
	// person, which is a decision the task records rather than a draft it waits
	// on.
	Auto bool
	// Content is what an action that already ran answered.
	Content json.RawMessage
}

// Draft statuses, matching the check constraint on `drafts.status`.
const (
	DraftPending  = "pending"
	DraftApproved = "approved"
	DraftRevise   = "revise"
	DraftSent     = "sent"
	DraftCanceled = "canceled"
)

// Budget refuses work a workspace can no longer afford.
//
// It is asked before a task starts a round, and the gateway asks the same policy
// before each model call: the round boundary is where a task stops cleanly with a
// reason, and the call boundary is what stops it from crossing the bound in the
// middle of one. One policy answers both, which is what keeps them from
// disagreeing.
//
// The policy knows two bounds — the period's token allowance and the day's cost
// ceiling — and reports which one it hit, because the reason is what the user
// reads.
type Budget interface {
	// Allow returns nil when the workspace may spend, or an error naming why not.
	Allow(ctx context.Context, workspaceID uuid.UUID) error
}

// CostTable turns a model's tokens into a cost, which the task records per step
// so a task's total is its own rather than inferred later. It is the gateway's
// port, so the task and the gateway price a call the same way.
type CostTable = llmdomain.CostTable

// AsLLMUsage renders this module's token counts for the price table.
func (u Usage) AsLLMUsage() llmdomain.Usage {
	return llmdomain.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
	}
}

// Gateway is the model the workflow calls.
type Gateway = llmdomain.Gateway

// Metrics is where the task runtime reports what it did, for the deployment's own charts.
//
// It is a port rather than a direct call because the module owns no telemetry: the container
// implements it over whatever the deployment configured, and a deployment with metrics off is
// handed nothing. That shape is what lets this package stay free of an SDK — and it is why every
// method here takes plain values rather than an instrument, so the names and the label vocabularies
// live in one place instead of being spelled out at each call site.
//
// Nothing here names a workspace, a Bolu, a task, or a workflow: a label with an unbounded value is
// a time series per value, so the identity is left to `usage_ledger` and `tasks`, where the rows
// already are and where a per-workspace question is answered exactly.
type Metrics interface {
	// TaskDispatched reports work this deployment accepted.
	TaskDispatched(ctx context.Context)
	// TaskSettled reports work that reached a terminal state, which is what brings the depth
	// back down.
	TaskSettled(ctx context.Context)
	// TaskFinished reports a finished task with what it spent. The duration is how long it ran,
	// in seconds.
	TaskFinished(ctx context.Context, trigger, status string, seconds float64, inputTokens, outputTokens, costMicros int64)
	// ToolCall reports one tool call. app is the integration the tool belongs to, which is what
	// makes "which integration is failing" answerable — the tool's own name and its label are
	// not here because a call site does not know them and because the step rows answer both
	// exactly when the question is per-task rather than per-fleet.
	ToolCall(ctx context.Context, app string, failed bool, seconds float64)
	// DraftReady reports how long a draft took to be ready for a person, in seconds.
	DraftReady(ctx context.Context, seconds float64)
	// DraftDecided reports what a person did with a draft. revision is whether the decision
	// followed a revision, which is the difference between "approved" and "approved as written".
	DraftDecided(ctx context.Context, outcome string, revision bool)
	// ActivityRetry reports an activity attempt past the first.
	ActivityRetry(ctx context.Context, activity string, attempt int)
}

// UsageRecorder persists what one model call cost, which is how a task's spend
// reaches the same ledger the rest of the product reads.
type UsageRecorder = llmdomain.UsageRecorder
