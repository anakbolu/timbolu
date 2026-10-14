// Package domain defines the checklist module's contract: the plan a Bolu writes for itself, the
// tasks and subtasks it is made of, and the ports the service needs to keep it.
//
// Every other module in this codebase is about work somebody else gave a Bolu. This one is about work
// a Bolu gave itself, and three rules are the whole reason it is more than a table definition:
//
//   - **One plan at a time.** A Bolu may hold exactly one active checklist; it writes the next one
//     only after the current one is finished. The rule is what makes the list a goal rather than a
//     scratchpad: a model with three open lists has three things it is "working on" and no answer to
//     "what are you doing". The database enforces it with a partial unique index, so it is a
//     constraint rather than a check somebody remembers to make.
//   - **Completion is derived.** A list is finished when every top-level task is ticked, and a task
//     with an unfinished subtask cannot be ticked at all. Nothing declares a plan done while work
//     underneath it is open, so "selesai" means what it says.
//   - **A subtask is one level deep.** The plan is a list a person scans, not a project tree; the
//     shape is bounded where the rows are written rather than left to the model's judgement.
package domain

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Statuses, matching the check constraint on `checklists.status`.
const (
	// StatusActive is the plan the Bolu is working on. There is at most one per Bolu.
	StatusActive = "active"
	// StatusDone is a plan whose every task is ticked.
	StatusDone = "done"
)

// Errors this module returns.
var (
	// ErrInvalidInput is a request that cannot be honoured: no tasks, a task with no title, more
	// tasks or subtasks than one plan may hold.
	ErrInvalidInput = errors.New("checklist: invalid input")
	// ErrNotFound is a checklist, or an item, that is not in this workspace.
	ErrNotFound = errors.New("checklist: not found")
	// ErrActiveExists is a create while the Bolu still has a plan to finish. It carries the plan's
	// shape in the message the tool answers with, because what the model needs is to see what is
	// still open rather than only that it was refused.
	ErrActiveExists = errors.New("checklist: the Bolu still has a plan to finish")
	// ErrSubtasksOpen is a tick on a task whose subtasks are not all done.
	ErrSubtasksOpen = errors.New("checklist: a task with unfinished subtasks cannot be marked done")
)

// Bounds on one plan. They exist so a model cannot turn its plan into a document: the list is read by
// a person, and a hundred-item plan is one nobody reads.
const (
	// MaxTasks is how many top-level tasks one plan may hold.
	MaxTasks = 20
	// MaxSubtasks is how many subtasks one task may hold.
	MaxSubtasks = 10
	// MaxTitleRunes bounds one title, which is one line in a list rather than a paragraph.
	MaxTitleRunes = 200
	// DefaultLimit bounds a list read when the caller does not say how many.
	DefaultLimit = 50
	// MaxLimit bounds one page.
	MaxLimit = 200
)

// Scope is the tenant a call runs as.
type Scope struct {
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
}

// IsZero reports whether the scope names no workspace.
func (s Scope) IsZero() bool { return s.WorkspaceID == uuid.Nil }

// Item is one line of a plan: a task, or a subtask of one.
type Item struct {
	ID          uuid.UUID
	ChecklistID uuid.UUID
	// ParentID is the task this item is a subtask of, or Nil when it is a task.
	ParentID uuid.UUID
	Title    string
	Done     bool
	// Position is the order the Bolu wrote the items in, which is the order a person reads them in.
	Position int
	// Subtasks are filled on a read of the tree and left empty when the item is read on its own.
	Subtasks  []Item
	CreatedAt time.Time
	UpdatedAt time.Time
	DoneAt    *time.Time
}

// IsSubtask reports whether this item belongs to a task rather than being one.
func (i Item) IsSubtask() bool { return i.ParentID != uuid.Nil }

// OpenSubtasks returns the subtasks that are not ticked yet.
func (i Item) OpenSubtasks() []Item {
	open := make([]Item, 0, len(i.Subtasks))
	for _, subtask := range i.Subtasks {
		if !subtask.Done {
			open = append(open, subtask)
		}
	}

	return open
}

// Checklist is one plan a Bolu wrote for itself.
type Checklist struct {
	ID      uuid.UUID
	AgentID uuid.UUID
	TaskID  uuid.UUID
	Title   string
	Status  string
	// Items are the top-level tasks, in the order they were written, each carrying its subtasks.
	Items     []Item
	CreatedAt time.Time
	UpdatedAt time.Time
	// CompletedAt is when the last task was ticked. Nil while the plan is open.
	CompletedAt *time.Time
}

// IsZero reports whether this is no plan at all, which is what a read answers when a Bolu has not
// written one.
func (c Checklist) IsZero() bool { return c.ID == uuid.Nil }

// Active reports whether this plan is the one the Bolu is working on.
func (c Checklist) Active() bool { return c.Status == StatusActive }

// Counts is how much of a plan is done, counted over tasks and subtasks together — the number a
// screen shows and the number a model reads to know how far along it is.
func (c Checklist) Counts() (done int, total int) {
	for _, item := range c.Items {
		total++
		if item.Done {
			done++
		}

		for _, subtask := range item.Subtasks {
			total++
			if subtask.Done {
				done++
			}
		}
	}

	return done, total
}

// Find returns the item with this id, task or subtask.
func (c Checklist) Find(id uuid.UUID) (Item, bool) {
	for _, item := range c.Items {
		if item.ID == id {
			return item, true
		}

		for _, subtask := range item.Subtasks {
			if subtask.ID == id {
				return subtask, true
			}
		}
	}

	return Item{}, false
}

// NewItem is one task to write down, with the subtasks it starts with.
type NewItem struct {
	Title    string
	Subtasks []string
}

// NewChecklist is a plan to write.
type NewChecklist struct {
	AgentID uuid.UUID
	// TaskID is the task the Bolu was working on when it wrote the plan down. It is attribution
	// rather than identity: the plan outlives the task.
	TaskID uuid.UUID
	Title  string
	// Tasks are the top-level tasks, in the order the Bolu wrote them; the order is stored so a
	// person reads the plan the way it was meant.
	Tasks []NewItem
}

// Filter narrows a list read.
type Filter struct {
	// AgentID limits the read to one Bolu.
	AgentID uuid.UUID
	// Status limits it to `active` or `done`; empty means both.
	Status string
	Limit  int
}

// Normalised returns the filter with its bounds applied, so every caller reads the same page size.
func (f Filter) Normalised() Filter {
	switch {
	case f.Limit <= 0:
		f.Limit = DefaultLimit
	case f.Limit > MaxLimit:
		f.Limit = MaxLimit
	}

	return f
}

// Repository is the persistence contract.
//
// The methods are about whole plans rather than about rows, because that is the unit the product
// talks about: a plan is read whole, and a tick answers with the plan it changed. Everything that
// decides whether a tick is allowed lives above this line; what lives below it is the two facts the
// database owns — one active plan per Bolu, and a finished plan when nothing is left open.
type Repository interface {
	// Active returns the Bolu's open plan, or ErrNotFound when it has none.
	Active(ctx context.Context, scope Scope, agentID uuid.UUID) (Checklist, error)
	// Create writes a plan and its items in one transaction, and answers with the tree it stored.
	Create(ctx context.Context, scope Scope, plan NewChecklist) (Checklist, error)
	// MarkDone ticks one item and answers with the plan as it now is. When that tick was the last
	// open task, the plan is finished in the same transaction.
	MarkDone(ctx context.Context, scope Scope, checklistID, itemID uuid.UUID) (Checklist, error)
	// RemoveItem deletes one item, and with it the subtasks underneath it, and answers with the plan
	// as it now is — which may be a finished one, if that item was the last thing open.
	RemoveItem(ctx context.Context, scope Scope, checklistID, itemID uuid.UUID) (Checklist, error)
	// Clear deletes a plan and everything in it.
	Clear(ctx context.Context, scope Scope, checklistID uuid.UUID) error
	// List returns plans, newest first, filtered by the caller.
	List(ctx context.Context, scope Scope, filter Filter) ([]Checklist, error)
}

// Service is what the tool adapter and the HTTP handler call.
//
// Every write names the Bolu rather than the plan, because a Bolu has one plan and the tools it calls
// are about "my list" rather than about an id it has to remember.
type Service interface {
	// Read returns the Bolu's open plan, or ErrNotFound when it has not written one.
	Read(ctx context.Context, scope Scope, agentID uuid.UUID) (Checklist, error)
	// Create writes the Bolu's next plan. It is refused while one is still open.
	Create(ctx context.Context, scope Scope, plan NewChecklist) (Checklist, error)
	// Check ticks one item of the open plan and answers with the plan as it now is.
	Check(ctx context.Context, scope Scope, agentID, itemID uuid.UUID) (Checklist, error)
	// Remove deletes one item of the open plan, subtasks and all, and answers with what is left.
	Remove(ctx context.Context, scope Scope, agentID, itemID uuid.UUID) (Checklist, error)
	// Clear throws the open plan away, which is what frees the Bolu to write another.
	Clear(ctx context.Context, scope Scope, agentID uuid.UUID) error
	// List returns plans for a screen: the workspace's, newest first.
	List(ctx context.Context, scope Scope, filter Filter) ([]Checklist, error)
}
