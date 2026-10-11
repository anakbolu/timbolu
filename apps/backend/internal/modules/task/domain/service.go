package domain

import (
	"context"

	"github.com/google/uuid"
)

// Service is the task runtime use case contract.
type Service interface {
	// Dispatch opens a task for a trigger and hands it to the durable runtime.
	// It returns as soon as the workflow owns the task: the work happens out of
	// process, which is what makes it survive the request and a restart.
	Dispatch(ctx context.Context, scope Scope, req DispatchRequest) (Task, error)
	// Get returns one task.
	Get(ctx context.Context, scope Scope, id uuid.UUID) (Task, error)
	// History returns the tasks of the workspace, newest first.
	History(ctx context.Context, scope Scope, filter Filter) ([]Task, error)
	// Steps returns a task's recorded rounds, in order.
	Steps(ctx context.Context, scope Scope, taskID uuid.UUID) ([]Step, error)
	// Children returns the tasks this one handed work to.
	Children(ctx context.Context, scope Scope, taskID uuid.UUID) ([]Task, error)
	// Cancel stops a task that has not finished.
	Cancel(ctx context.Context, scope Scope, id uuid.UUID) (Task, error)
}

// DispatchRequest is one task to open.
type DispatchRequest struct {
	AgentID uuid.UUID
	// ConversationID is the thread the task answers, when it came from chat.
	ConversationID uuid.UUID
	// ReplyMessageID is the placeholder the answer fills. It is set by the chat
	// module, which wrote the placeholder before dispatching.
	ReplyMessageID uuid.UUID
	Trigger        string
	Title          string
	// Prompt is the text the task was asked to act on.
	Prompt string
	// ParentTaskID is set when another task handed this work over.
	ParentTaskID uuid.UUID
	// Depth is how far down a handoff chain this task is.
	Depth int
}

// StartInput is what the workflow needs to begin. Every field is an id: the
// workflow loads the task, the Bolu, and the conversation itself, so a restart
// re-reads the current state rather than replaying a snapshot taken at start.
type StartInput struct {
	TaskID         uuid.UUID
	AgentID        uuid.UUID
	ConversationID uuid.UUID
	ReplyMessageID uuid.UUID
	Title          string
	Prompt         string
	Depth          int
}

// StartResult identifies the running workflow.
type StartResult struct {
	WorkflowID string
	RunID      string
}

// Starter starts the durable workflow that runs a task.
//
// It is a port rather than a direct call because the engine is infrastructure:
// the service decides what a task is, and this decides where it runs. A test
// supplies a starter that records what it was asked to start, which is what
// makes dispatch testable without Temporal.
type Starter interface {
	Start(ctx context.Context, scope Scope, input StartInput) (StartResult, error)
	// Signal delivers a decision to a running task.
	Signal(ctx context.Context, scope Scope, taskID uuid.UUID, name string, payload any) error
}
