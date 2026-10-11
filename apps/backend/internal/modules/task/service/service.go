// Package service implements the task runtime use cases: opening a task from a
// trigger, reading its history and steps, and stopping one that has not
// finished.
//
// The engine is a port. This package decides what a task is — its trigger, its
// bounds — and where it runs is the container's business, which is what makes
// dispatch testable without Temporal.
package service

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
)

// TitleMaxRunes bounds a task title, which is shown in the feed and the office.
const TitleMaxRunes = 160

// TriggerWhitelist is the set a client may open a task with. `handoff` is not in
// it: a chain of handoffs is opened by the runtime, and letting a client claim
// that trigger would make the chain look like something a person asked for.
var TriggerWhitelist = []string{domain.TriggerChat, domain.TriggerRoutine, domain.TriggerWebhook}

// Deps are the service dependencies.
type Deps struct {
	Tasks   domain.TaskRepository
	Steps   domain.StepRepository
	Starter domain.Starter
	// Agents validates the Bolu the task runs as. Optional: without it the
	// foreign key is the only check.
	Agents domain.AgentDirectory
	// Events publishes what a task is doing. Optional: without it the work still
	// happens and only the live views are behind.
	Events domain.EventSink
	Limits domain.Limits
	Logger *zap.Logger
	Clock  func() time.Time
}

// Service is the task runtime implementation.
type Service struct {
	deps   Deps
	limits domain.Limits
	clock  func() time.Time
}

// New builds the service.
func New(deps Deps) *Service {
	clock := deps.Clock
	if clock == nil {
		clock = time.Now
	}

	return &Service{deps: deps, limits: deps.Limits.Normalise(), clock: clock}
}

// Dispatch opens a task for a trigger and hands it to the durable runtime.
func (s *Service) Dispatch(ctx context.Context, scope domain.Scope, req domain.DispatchRequest) (domain.Task, error) {
	if err := requireScope(scope); err != nil {
		return domain.Task{}, err
	}
	if req.AgentID == uuid.Nil {
		return domain.Task{}, fmt.Errorf("%w: agent id is required", domain.ErrInvalidInput)
	}
	if !clientTrigger(req.Trigger) {
		return domain.Task{}, fmt.Errorf("%w: %q", domain.ErrUnknownTrigger, req.Trigger)
	}
	if req.Depth < 0 || req.Depth > s.limits.MaxHandoffDepth {
		return domain.Task{}, fmt.Errorf("%w: depth %d", domain.ErrDepthLimitReached, req.Depth)
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = titleFrom(req.Prompt)
	}
	if len([]rune(title)) > TitleMaxRunes {
		return domain.Task{}, fmt.Errorf("%w: title is longer than %d characters",
			domain.ErrInvalidInput, TitleMaxRunes)
	}

	if s.deps.Agents != nil {
		agent, err := s.deps.Agents.Agent(ctx, scope, req.AgentID)
		if err != nil {
			return domain.Task{}, err
		}
		// A resting Bolu takes no new work. The switch is the user's, and a task
		// that started anyway would contradict it.
		if !s.deps.Agents.Active(agent) {
			return domain.Task{}, fmt.Errorf("%w: %s sedang istirahat", domain.ErrInvalidInput, agent.Name)
		}
	}

	task, err := s.deps.Tasks.Create(ctx, scope, domain.NewTask{
		AgentID:        req.AgentID,
		ParentTaskID:   req.ParentTaskID,
		ConversationID: req.ConversationID,
		ReplyMessageID: req.ReplyMessageID,
		Trigger:        req.Trigger,
		Title:          title,
		Depth:          req.Depth,
	})
	if err != nil {
		return domain.Task{}, err
	}

	if s.deps.Starter == nil {
		// No engine: the task is recorded and left queued rather than reported as
		// started, which is the honest answer for a deployment without Temporal.
		return task, nil
	}

	result, err := s.deps.Starter.Start(ctx, scope, domain.StartInput{
		TaskID:         task.ID,
		AgentID:        req.AgentID,
		ConversationID: req.ConversationID,
		ReplyMessageID: req.ReplyMessageID,
		Title:          title,
		Prompt:         req.Prompt,
		Depth:          req.Depth,
	})
	if err != nil {
		// A task nothing will run must not sit queued forever looking pending, so
		// the failure is recorded on the task itself.
		s.fail(ctx, scope, task, fmt.Sprintf("tidak bisa dijalankan: %v", err))
		return task, fmt.Errorf("task: start workflow: %w", err)
	}

	started, err := s.deps.Tasks.Start(ctx, scope, task.ID, result.WorkflowID, result.RunID)
	if err != nil {
		return task, err
	}

	s.emitTask(ctx, scope, started, "task.started")
	return started, nil
}

// Get returns one task.
func (s *Service) Get(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Task, error) {
	if err := requireScope(scope); err != nil {
		return domain.Task{}, err
	}
	if id == uuid.Nil {
		return domain.Task{}, fmt.Errorf("%w: task id is required", domain.ErrInvalidInput)
	}

	return s.deps.Tasks.Get(ctx, scope, id)
}

// History returns the tasks of the workspace, newest first.
func (s *Service) History(ctx context.Context, scope domain.Scope, filter domain.Filter) ([]domain.Task, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	if filter.Status != "" && !knownStatus(filter.Status) {
		return nil, fmt.Errorf("%w: %q", domain.ErrInvalidInput, filter.Status)
	}

	return s.deps.Tasks.List(ctx, scope, filter)
}

// Steps returns a task's recorded rounds, in order.
func (s *Service) Steps(ctx context.Context, scope domain.Scope, taskID uuid.UUID) ([]domain.Step, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}

	// Reading the task first confirms the caller may see it: a task id from
	// another workspace answers "not found" rather than an empty list.
	if _, err := s.deps.Tasks.Get(ctx, scope, taskID); err != nil {
		return nil, err
	}

	return s.deps.Steps.ListSteps(ctx, scope, taskID)
}

// Children returns the tasks this one handed work to.
func (s *Service) Children(ctx context.Context, scope domain.Scope, taskID uuid.UUID) ([]domain.Task, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}

	return s.deps.Tasks.Children(ctx, scope, taskID)
}

// Cancel stops a task that has not finished.
//
// The status is written first and the running workflow is told second. A worker
// that is down therefore cannot leave a task running: the workflow reads its own
// status after every round and stops, and the signal only makes it stop sooner.
func (s *Service) Cancel(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Task, error) {
	if err := requireScope(scope); err != nil {
		return domain.Task{}, err
	}

	task, err := s.deps.Tasks.Get(ctx, scope, id)
	if err != nil {
		return domain.Task{}, err
	}
	if !task.Live() {
		return task, fmt.Errorf("%w: tugas sudah selesai", domain.ErrInvalidInput)
	}

	canceled, err := s.deps.Tasks.Cancel(ctx, scope, id, "dihentikan oleh pengguna")
	if err != nil {
		return domain.Task{}, err
	}

	if s.deps.Starter != nil && task.WorkflowID != "" {
		if err := s.deps.Starter.Signal(ctx, scope, id, SignalCancel, nil); err != nil && s.deps.Logger != nil {
			s.deps.Logger.Warn("could not tell the running task it was canceled; its status will stop it",
				zap.String("task_id", id.String()), zap.Error(err))
		}
	}

	s.emitTask(ctx, scope, canceled, EventTaskFinished)
	return canceled, nil
}

// SignalCancel is the signal name that stops a running task.
const SignalCancel = "cancel"

// Event types the feed, the office, and the chat read.
const (
	EventTaskStarted  = "task.started"
	EventTaskStep     = "task.step"
	EventTaskFinished = "task.finished"
)

// fail records a task that could not be dispatched.
func (s *Service) fail(ctx context.Context, scope domain.Scope, task domain.Task, reason string) {
	failed, err := s.deps.Tasks.Finish(ctx, scope, domain.Finish{
		TaskID:        task.ID,
		Status:        domain.StatusFailed,
		StoppedReason: reason,
	})
	if err != nil {
		if s.deps.Logger != nil {
			s.deps.Logger.Error("could not record a task that failed to start",
				zap.String("task_id", task.ID.String()), zap.Error(err))
		}
		return
	}

	s.emitTask(ctx, scope, failed, EventTaskFinished)
}

// emitTask publishes a task's state, which is how the feed and the office learn
// about it without polling.
func (s *Service) emitTask(ctx context.Context, scope domain.Scope, task domain.Task, eventType string) {
	if s.deps.Events == nil {
		return
	}

	payload := Payload(task, s.clock())
	if err := s.deps.Events.Emit(ctx, taskEvent(scope, task, eventType, payload)); err != nil && s.deps.Logger != nil {
		// Not fatal: the event is written to the store first, so a publish that
		// fails costs the live views latency and never the event.
		s.deps.Logger.Warn("task event published only to the store",
			zap.String("type", eventType), zap.Error(err))
	}
}

func requireScope(scope domain.Scope) error {
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}
	return nil
}

// clientTrigger reports whether a caller may open a task with this trigger.
//
// `handoff` is deliberately not in the set: a chain of handoffs is opened by the
// runtime, through the repository, and a client that could claim the trigger
// would make the chain look like something a person asked for.
func clientTrigger(trigger string) bool {
	return slices.Contains(TriggerWhitelist, trigger)
}

func knownStatus(status string) bool {
	switch status {
	case domain.StatusQueued, domain.StatusRunning, domain.StatusWaitingApproval,
		domain.StatusSucceeded, domain.StatusFailed, domain.StatusCanceled:
		return true
	default:
		return false
	}
}

// titleFrom takes a task's title from the text it was asked to act on, which is
// what a user recognises in the feed.
func titleFrom(prompt string) string {
	trimmed := strings.TrimSpace(prompt)
	if trimmed == "" {
		return "Tugas baru"
	}

	line := trimmed
	if index := strings.IndexByte(trimmed, '\n'); index >= 0 {
		line = trimmed[:index]
	}

	title := strings.TrimSpace(line)
	if len([]rune(title)) <= TitleMaxRunes {
		return title
	}
	return string([]rune(title)[:TitleMaxRunes-1]) + "…"
}
