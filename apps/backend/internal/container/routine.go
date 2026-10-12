package container

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	approvaldomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/domain"
	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/activity"
	routinedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/domain"
	routineservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/service"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/workflow"
	taskdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	workspacerepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/repository"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/config"
	routinetemporal "github.com/robzlabz/employeebot/apps/backend/internal/platform/routine/temporal"
)

// openRoutine builds the routine runtime: the routines a Bolu runs on its own, the
// runs they produced, and the schedules that fire them.
//
// It runs after the task runtime and before the chat and memory modules, because
// those are what a run's work becomes and what an event-driven routine consumes.
func (c *Container) openRoutine(_ context.Context, cfg *config.Config) error {
	if c.Repositories == nil || c.Repositories.Routine == nil {
		c.Logger.Warn("routines are disabled: no database connection")
		return nil
	}

	var engine routinedomain.Engine
	if c.Temporal != nil {
		engine = routinetemporal.NewScheduler(c.Temporal, c.Temporal.TaskQueueAgent(), workflow.WorkflowName)
	} else {
		c.Logger.Warn("routines cannot be scheduled: temporal is not configured")
	}

	var tasks routinedomain.TaskStarter
	if c.Services.Task != nil {
		tasks = routineTaskStarter{service: c.Services.Task}
	}

	deps := routineservice.Deps{
		Repository: c.Repositories.Routine,
		Engine:     engine,
		Tasks:      tasks,
		Queue:      c.routineQueue(),
		Auditor:    c.routineAuditor(),
		Clock:      workspaceClock{workspace: c.Repositories.Workspace},
		Logger:     c.Logger,
	}

	service := routineservice.New(deps)
	c.Routine = service
	c.Services.Routine = service
	c.RoutineTasks = activity.New(activity.Deps{Routines: service, Logger: c.Logger})

	return nil
}

// routineQueue adapts the integration hub's inbound queue to the routine port.
//
// The events are the integration module's to receive, so the routine runtime reads
// them rather than owning a queue of its own: a delivery arriving and a routine
// acting on it are the same fact seen from two sides.
func (c *Container) routineQueue() routinedomain.WorkQueue {
	if c == nil || c.Services == nil || c.Services.Integration == nil {
		return nil
	}
	return routineQueue{integrations: c.Services.Integration}
}

// eventClaimer is the one method of the integration service this adapter uses.
// Naming it separately is what keeps the routine module's queue port from depending
// on the integration module's whole contract: it claims events and reads nothing else.
type eventClaimer interface {
	ClaimPendingEvents(ctx context.Context, scope integrationdomain.Scope, event string, limit int) ([]integrationdomain.QueuedEvent, error)
}

type routineQueue struct {
	integrations eventClaimer
}

// Pending implements routinedomain.WorkQueue.
func (q routineQueue) Pending(ctx context.Context, scope routinedomain.Scope, triggerSource string, limit int) ([]routinedomain.Event, error) {
	events, err := q.integrations.ClaimPendingEvents(ctx, integrationdomain.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
	}, triggerSource, limit)
	if err != nil {
		return nil, err
	}

	out := make([]routinedomain.Event, 0, len(events))
	for _, event := range events {
		out = append(out, routinedomain.Event{
			ID:      event.ID,
			App:     event.App,
			Event:   event.Event,
			Payload: event.Payload,
		})
	}
	return out, nil
}

// routineAuditor adapts the approval repository to the routine audit port, the same
// way the memory module's does: the trail's table belongs to the approval module, so
// that module is the one that writes it.
func (c *Container) routineAuditor() routinedomain.Auditor {
	if c == nil || c.Repositories == nil || c.Repositories.Approval == nil {
		return nil
	}
	return routineAuditor{repository: c.Repositories.Approval}
}

type routineAuditor struct {
	repository auditRecorder
}

// Record implements routinedomain.Auditor.
func (a routineAuditor) Record(ctx context.Context, scope routinedomain.Scope, entry routinedomain.AuditEntry) error {
	return a.repository.RecordAudit(ctx, approvaldomain.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
	}, approvaldomain.AuditEntry{
		Action:      entry.Action,
		ObjectType:  entry.ObjectType,
		ObjectID:    entry.ObjectID,
		ActorUserID: entry.ActorUserID,
		Metadata:    entry.Metadata,
	})
}

// routineTaskStarter opens the task one routine run performs.
type routineTaskStarter struct {
	service taskdomain.Service
}

// Start implements routinedomain.TaskStarter.
func (s routineTaskStarter) Start(ctx context.Context, scope routinedomain.Scope, request routinedomain.StartTask) (uuid.UUID, error) {
	task, err := s.service.Dispatch(ctx, taskdomain.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
	}, taskdomain.DispatchRequest{
		AgentID: request.AgentID,
		// A routine has no conversation: its answer is the task's own summary, which
		// the history shows rather than a thread nobody opened.
		Trigger: taskdomain.TriggerRoutine,
		Title:   request.Title,
		Prompt:  request.Prompt,
	})
	if err != nil {
		return uuid.Nil, err
	}
	return task.ID, nil
}

// taskOutcomeSink adapts the routine service to the task runtime's port, which is how
// a finished task's summary reaches the run it belongs to.
//
// It resolves the service when it is called rather than when it is installed, and
// that is the whole point of the type: the task runtime is assembled before the
// routine runtime — a run's work becomes a task, so the task side must exist first —
// so a sink captured at assembly time would hold a nil service forever, and every
// finished task would report its outcome to nobody.
type taskOutcomeSink struct {
	container *Container
}

// Report implements taskdomain.OutcomeSink.
func (s taskOutcomeSink) Report(ctx context.Context, scope taskdomain.Scope, report taskdomain.OutcomeReport) error {
	if s.container == nil || s.container.Routine == nil {
		return taskdomain.ErrOutcomeUnclaimed
	}

	return routineOutcomeSink{routines: s.container.Routine}.Report(ctx, scope, report)
}

type routineOutcomeSink struct {
	routines routinedomain.Service
}

// Report implements taskdomain.OutcomeSink.
func (s routineOutcomeSink) Report(ctx context.Context, scope taskdomain.Scope, report taskdomain.OutcomeReport) error {
	return s.routines.RecordTaskOutcome(ctx, routinedomain.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
	}, report.TaskID, routinedomain.Outcome{
		Status:  report.Status,
		Summary: report.Summary,
		Reason:  report.Reason,
	})
}

// workspaceHoursReader is the one method of the workspace repository this adapter
// uses, named so the routine runtime's clock port does not depend on the workspace
// module's whole repository.
type workspaceHoursReader interface {
	WorkingHours(ctx context.Context, workspaceID uuid.UUID) (workspacerepo.WorkingHours, error)
}

// workspaceClock reads the zone and window a routine is scheduled against.
type workspaceClock struct {
	workspace workspaceHoursReader
}

// Location implements routinedomain.WorkspaceClock.
func (c workspaceClock) Location(ctx context.Context, workspaceID uuid.UUID) (*time.Location, error) {
	if c.workspace == nil {
		return time.UTC, nil
	}

	window, err := c.workspace.WorkingHours(ctx, workspaceID)
	if err != nil {
		return nil, err
	}

	location, err := time.LoadLocation(window.Timezone)
	if err != nil {
		// A zone this build cannot load is reported rather than guessed at. The
		// workspace service refuses such a write, so this only happens to a row
		// changed outside the app — and scheduling it in UTC would fire every
		// routine of that workspace at a wall-clock time nobody chose while the
		// settings screen still claimed the real zone.
		return nil, fmt.Errorf("routine: the workspace timezone %q is not usable: %w", window.Timezone, err)
	}
	return location, nil
}

// WorkingHours implements routinedomain.WorkspaceClock.
func (c workspaceClock) WorkingHours(ctx context.Context, workspaceID uuid.UUID) (routinedomain.WorkingHours, error) {
	if c.workspace == nil {
		return routinedomain.WorkingHours{}, errors.New("routine: the workspace repository is not configured")
	}

	window, err := c.workspace.WorkingHours(ctx, workspaceID)
	if err != nil {
		return routinedomain.WorkingHours{}, err
	}

	return routinedomain.WorkingHours{
		Start:    window.Start,
		End:      window.End,
		Timezone: window.Timezone,
		WorkDays: window.Days,
	}, nil
}

// RescheduleRoutines re-registers every routine of a workspace.
//
// It is what the workspace update calls: a changed timezone moves every future run,
// and the engine is told rather than left to fire at the old wall-clock time.
func (c *Container) RescheduleRoutines(ctx context.Context, workspaceID uuid.UUID) {
	if c == nil || c.Routine == nil {
		return
	}

	if err := c.Routine.Reschedule(ctx, routinedomain.Scope{
		// A workspace-level reschedule runs with no user: the change that triggered
		// it belongs to whoever made it, and the routines are the workspace's.
		UserID:      uuid.Nil,
		WorkspaceID: workspaceID,
	}); err != nil && c.Logger != nil {
		c.Logger.Warn("could not reschedule this workspace's routines after its clock changed",
			zap.String("workspace_id", workspaceID.String()), zap.Error(err))
	}
}

// RegisterRoutineTasks installs the routine workflow and its activity on the agent
// worker.
func RegisterRoutineTasks(w workerish, c *Container) {
	if c == nil || c.RoutineTasks == nil {
		return
	}

	w.RegisterWorkflow(workflow.RoutineRunWorkflow)
	w.RegisterActivity(c.RoutineTasks)
}

// Compile-time checks that every adapter satisfies the port it is wired to.
var (
	_ routinedomain.WorkQueue      = routineQueue{}
	_ routinedomain.Auditor        = routineAuditor{}
	_ routinedomain.TaskStarter    = routineTaskStarter{}
	_ routinedomain.WorkspaceClock = workspaceClock{}
	_ taskdomain.OutcomeSink       = taskOutcomeSink{}
	_ taskdomain.OutcomeSink       = routineOutcomeSink{}
)
