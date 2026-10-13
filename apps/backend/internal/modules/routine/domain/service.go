package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// View is a routine with the times it will next run at.
//
// The times travel with the routine rather than in a second request per row,
// because "when does this run next" is the first question a routine list answers.
// They are computed on read rather than stored, which is what E11.3 turns on: a
// timezone change moves every future run by itself, because there is no cached time
// that could be stale.
type View struct {
	Routine
	NextRuns []time.Time
}

// Service is what the HTTP handlers, the webhook path, and the worker call.
type Service interface {
	// Routines returns the workspace's routines, newest first, optionally narrowed
	// to one Bolu, each with its next run times.
	Routines(ctx context.Context, scope Scope, filter Filter) ([]View, error)
	// Routine returns one routine with the times it will next run at.
	Routine(ctx context.Context, scope Scope, id uuid.UUID) (View, error)
	// RoutinesFor returns the enabled routines watching one event, which is what a
	// delivery is dispatched to.
	RoutinesFor(ctx context.Context, scope Scope, triggerSource string) ([]Routine, error)

	// Create stores a routine and registers its schedule in one step. A routine
	// that exists without a schedule is one that silently never runs, so the two
	// are written together and a failed registration is reported rather than
	// swallowed.
	//
	// It answers a View, so the caller reads the next run times from the same write
	// that changed them — which is what "a schedule change is visible immediately"
	// means at the API boundary.
	Create(ctx context.Context, scope Scope, request WriteRequest) (View, error)
	// Update stores a change and re-registers the schedule, because the cadence may
	// be what changed.
	Update(ctx context.Context, scope Scope, id uuid.UUID, request WriteRequest) (View, error)
	// SetEnabled pauses or resumes a routine's schedule, keeping its history.
	SetEnabled(ctx context.Context, scope Scope, id uuid.UUID, enabled bool) (View, error)
	// Delete removes the routine and its schedule, and keeps its run history.
	Delete(ctx context.Context, scope Scope, id uuid.UUID) error

	// Runs returns the run history, newest first, optionally for one routine.
	Runs(ctx context.Context, scope Scope, filter RunFilter) ([]Run, error)

	// Preview answers the next run times for a cadence that has not been saved yet,
	// which is what a form shows while it is being filled in.
	Preview(ctx context.Context, scope Scope, request PreviewRequest) ([]time.Time, error)

	// Reschedule re-registers every enabled routine of the workspace. It is what a
	// timezone or working-hours change calls: the next run moves because the times
	// are computed from the workspace's own clock.
	Reschedule(ctx context.Context, scope Scope) error

	// Fire starts the routines watching an event. It is called when an event
	// arrives, which is the only thing that starts a trigger routine.
	Fire(ctx context.Context, scope Scope, triggerSource string) (int, error)

	// RunOnce opens one run of one routine and hands its work to the task runtime.
	// It is what the engine calls on each tick of a schedule.
	RunOnce(ctx context.Context, scope Scope, routineID uuid.UUID) (Run, error)

	// RecordTaskOutcome finishes the run a task belongs to.
	//
	// It is how a run's summary is written: the task runs out of process and its
	// one-line summary is what the history shows, so the task reports its outcome
	// back rather than the routine polling for it. A task a routine did not open
	// answers ErrNotFound, which is most tasks.
	RecordTaskOutcome(ctx context.Context, scope Scope, taskID uuid.UUID, outcome Outcome) error
}

// Outcome is how the task a run opened ended.
//
// The reason travels with it because the history of what asked for a run says why
// it did not happen: a failed run reads as the task's own refusal, and "0 langkah"
// is not that answer.
type Outcome struct {
	// Status is the task's terminal status: succeeded, failed, or canceled.
	Status string
	// Summary is the task's own one line, which is what the history shows.
	Summary string
	// Reason is why a task stopped without succeeding, in the user's words.
	Reason string
}

// Filter narrows a routine read.
type Filter struct {
	AgentID uuid.UUID
	Enabled *bool
}

// RunFilter narrows a run read.
type RunFilter struct {
	RoutineID uuid.UUID
	AgentID   uuid.UUID
	Status    string
	Limit     int
}

// WriteRequest is one routine to store.
type WriteRequest struct {
	AgentID       uuid.UUID
	Name          string
	Kind          string
	Minute        int
	HourStart     int
	HourEnd       int
	DayOfWeek     int
	TriggerSource string
	Instruction   string
	Enabled       bool
}

// PreviewRequest is a cadence a form is asking about.
type PreviewRequest struct {
	Kind      string
	Minute    int
	HourStart int
	HourEnd   int
	DayOfWeek int
}

// Count is how many next runs a preview answers with.
const Count = 5

// RunInput is what one routine run is started with.
//
// It lives in the domain rather than in the workflow package because two callers
// need it and neither may import the other: the scheduler that starts a run (in the
// platform layer) and the workflow that performs it (in this module). Every field is
// an id, so a restart re-reads the current state instead of replaying a snapshot.
type RunInput struct {
	RoutineID   string
	WorkspaceID string
}

// Repository persists routines and their runs.
type Repository interface {
	Create(ctx context.Context, scope Scope, routine Routine) (Routine, error)
	Get(ctx context.Context, scope Scope, id uuid.UUID) (Routine, error)
	List(ctx context.Context, scope Scope, filter Filter) ([]Routine, error)
	// ListByTrigger finds the routines watching one event. It is how a delivery
	// reaches the routines that asked for it.
	ListByTrigger(ctx context.Context, scope Scope, triggerSource string) ([]Routine, error)
	// ListEnabled returns every enabled routine of the workspace, which is what a
	// reschedule walks.
	ListEnabled(ctx context.Context, scope Scope) ([]Routine, error)
	Update(ctx context.Context, scope Scope, routine Routine) (Routine, error)
	// SetScheduleID records the engine's id for a routine.
	SetScheduleID(ctx context.Context, scope Scope, id uuid.UUID, scheduleID string) error
	// Delete removes the routine. Its runs survive: the link is dropped and the name
	// was snapshotted when they were written.
	Delete(ctx context.Context, scope Scope, id uuid.UUID) error

	StartRun(ctx context.Context, scope Scope, run Run) (Run, error)
	// FinishRun records how a run ended, and is idempotent: a workflow that
	// re-runs an activity after a restart must not open a second outcome on a run
	// that already has one.
	FinishRun(ctx context.Context, scope Scope, finish FinishRun) (Run, error)
	// LatestRun returns the newest run of a routine, which is what a re-fired
	// routine checks before starting another.
	LatestRun(ctx context.Context, scope Scope, routineID uuid.UUID) (Run, error)
	// AttachTask records the task a run opened, which is what lets the task's own
	// outcome find its way back to the run.
	AttachTask(ctx context.Context, scope Scope, runID, taskID uuid.UUID) (Run, error)
	// RunByTask returns the run a task belongs to.
	RunByTask(ctx context.Context, scope Scope, taskID uuid.UUID) (Run, error)
	ListRuns(ctx context.Context, scope Scope, filter RunFilter) ([]Run, error)
	// Agents returns the Bolu of the workspace with the name a routine is labelled
	// with. The rows belong to the agent module; this reads one column.
	Agents(ctx context.Context, scope Scope) ([]AgentInfo, error)
}

// FinishRun is one run's outcome.
type FinishRun struct {
	RunID      uuid.UUID
	Status     string
	Summary    string
	TaskID     uuid.UUID
	FinishedAt time.Time
}

// AgentInfo is the per-Bolu part a routine's label needs: the name and the rest
// switch.
type AgentInfo struct {
	ID      uuid.UUID
	Name    string
	Resting bool
}

// Engine registers, updates, pauses, and removes the schedules a routine is fired
// by.
//
// It is a port rather than a direct Temporal call because the routine module must
// stay runnable without a Temporal server: the API answers its reads and its
// history, and only the writes that need a schedule report that none can be
// registered.
type Engine interface {
	// Register creates or replaces one routine's schedule, idempotently: the same
	// routine registered twice leaves exactly one schedule.
	Register(ctx context.Context, routine Routine, location *time.Location) (string, error)
	// SetPaused pauses or resumes a schedule without removing it.
	SetPaused(ctx context.Context, scheduleID string, paused bool, reason string) error
	// Remove deletes a schedule. A schedule that is already gone is not an error:
	// the routine's row is what the caller is deleting.
	Remove(ctx context.Context, scheduleID string) error
}

// TaskStarter opens the task a run performs.
//
// It is a port for the same reason the chat module's is: the routine runtime does
// not know what a task is. A run's work is a task because that is what gives it a
// durable workflow, a recorded round, and an approval gate.
type TaskStarter interface {
	// Start opens a task for a routine run and returns its id.
	Start(ctx context.Context, scope Scope, request StartTask) (uuid.UUID, error)
}

// StartTask is one run's work.
type StartTask struct {
	AgentID uuid.UUID
	Title   string
	Prompt  string
}

// WorkQueue reports the unconsumed events a routine watches.
//
// It is the cheap check E11.5 is built on: a routine started with nothing waiting
// finishes without calling a model, and the saving is real rather than claimed.
// The port reads the integration module's queue rather than owning one, because
// the events are that module's to receive.
type WorkQueue interface {
	// Pending returns the oldest unclaimed events of one kind, and claims them in
	// the same step, so two routines watching one event cannot both act on it.
	Pending(ctx context.Context, scope Scope, triggerSource string, limit int) ([]Event, error)
}

// Event is one queued delivery a routine acts on.
type Event struct {
	ID      uuid.UUID
	App     string
	Event   string
	Payload []byte
}

// Auditor records what a person did to a routine, for the trail the product
// answers "who turned this off" with.
type Auditor interface {
	Record(ctx context.Context, scope Scope, entry AuditEntry) error
}

// AuditEntry is one thing that happened, for the record.
type AuditEntry struct {
	Action      string
	ObjectType  string
	ObjectID    string
	ActorUserID uuid.UUID
	Metadata    map[string]any
}

// Audit actions this module records.
const (
	ActionCreated = "routine.created"
	ActionUpdated = "routine.updated"
	ActionPaused  = "routine.paused"
	ActionResumed = "routine.resumed"
	ActionDeleted = "routine.deleted"
)

// WorkspaceClock reads the zone a routine is scheduled in.
//
// It is the zone and nothing else: a routine carries its own hours, and whether it
// may act outside the workspace's working window is the approval gate's question
// (Epic 7), not this module's. Asking for the window here would invite a second
// answer to a question that already has one.
type WorkspaceClock interface {
	// Location returns the workspace's timezone.
	Location(ctx context.Context, workspaceID uuid.UUID) (*time.Location, error)
}
