package service

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
)

// memoryTasks is an in-memory implementation of the task and step ports.
//
// It is a real implementation rather than a mock because the runtime is
// stateful: a test asserts what a task's status became after a trigger, and a
// mock would only assert which calls happened. The tenant check and the
// status guards of the SQL are reproduced here, so a test that passes against
// the fake is asserting the same contract the database enforces.
type memoryTasks struct {
	mu    sync.Mutex
	tasks map[uuid.UUID]domain.Task
	steps []domain.Step
	// failCreate makes the insert fail, which is how a database error is driven.
	failCreate bool
}

func newMemoryTasks() *memoryTasks {
	return &memoryTasks{tasks: map[uuid.UUID]domain.Task{}}
}

func (m *memoryTasks) Create(_ context.Context, scope domain.Scope, task domain.NewTask) (domain.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failCreate {
		return domain.Task{}, context.DeadlineExceeded
	}
	if scope.WorkspaceID == uuid.Nil {
		return domain.Task{}, domain.ErrInvalidInput
	}

	now := time.Now()
	created := domain.Task{
		ID:             uuid.New(),
		WorkspaceID:    scope.WorkspaceID,
		AgentID:        task.AgentID,
		ParentTaskID:   task.ParentTaskID,
		ConversationID: task.ConversationID,
		ReplyMessageID: task.ReplyMessageID,
		Trigger:        task.Trigger,
		Title:          task.Title,
		Status:         domain.StatusQueued,
		Depth:          task.Depth,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	m.tasks[created.ID] = created
	return created, nil
}

func (m *memoryTasks) Get(_ context.Context, scope domain.Scope, id uuid.UUID) (domain.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	task, ok := m.tasks[id]
	if !ok || task.WorkspaceID != scope.WorkspaceID {
		return domain.Task{}, domain.ErrTaskNotFound
	}
	return task, nil
}

func (m *memoryTasks) List(_ context.Context, scope domain.Scope, filter domain.Filter) ([]domain.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	tasks := make([]domain.Task, 0, len(m.tasks))
	for _, task := range m.tasks {
		if task.WorkspaceID != scope.WorkspaceID {
			continue
		}
		if filter.AgentID != uuid.Nil && task.AgentID != filter.AgentID {
			continue
		}
		if filter.ParentTaskID != uuid.Nil && task.ParentTaskID != filter.ParentTaskID {
			continue
		}
		if filter.Status != "" && task.Status != filter.Status {
			continue
		}
		if filter.Live && !task.Live() {
			continue
		}
		tasks = append(tasks, task)
	}

	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt.After(tasks[j].CreatedAt) })

	limit := filter.Limit
	if limit <= 0 || limit > domain.MaxHistoryLimit {
		limit = domain.DefaultHistoryLimit
	}
	if len(tasks) > limit {
		tasks = tasks[:limit]
	}
	return tasks, nil
}

func (m *memoryTasks) Start(_ context.Context, scope domain.Scope, id uuid.UUID, workflowID, runID string) (domain.Task, error) {
	return m.mutate(scope, id, func(task *domain.Task) error {
		if task.Status != domain.StatusQueued && task.Status != domain.StatusRunning {
			return domain.ErrTaskNotFound
		}
		task.Status = domain.StatusRunning
		task.WorkflowID = workflowID
		task.TemporalRunID = runID
		now := time.Now()
		task.StartedAt = &now
		task.HeartbeatAt = &now
		return nil
	})
}

func (m *memoryTasks) Progress(_ context.Context, scope domain.Scope, progress domain.Progress) error {
	_, err := m.mutate(scope, progress.TaskID, func(task *domain.Task) error {
		if task.Status != domain.StatusQueued && task.Status != domain.StatusRunning {
			return domain.ErrTaskNotFound
		}
		now := time.Now()
		task.HeartbeatAt = &now
		task.InputTokens = progress.InputTokens
		task.OutputTokens = progress.OutputTokens
		task.CostMicros = progress.CostMicros
		task.StepCount = progress.StepCount
		return nil
	})
	return err
}

func (m *memoryTasks) Park(_ context.Context, scope domain.Scope, id uuid.UUID, reason string, draftID uuid.UUID) (domain.Task, error) {
	return m.mutate(scope, id, func(task *domain.Task) error {
		if task.Status != domain.StatusQueued && task.Status != domain.StatusRunning {
			return domain.ErrTaskNotFound
		}
		task.Status = domain.StatusWaitingApproval
		task.WaitingReason = reason
		task.WaitingDraftID = draftID
		return nil
	})
}

func (m *memoryTasks) Resume(_ context.Context, scope domain.Scope, id uuid.UUID) (domain.Task, error) {
	return m.mutate(scope, id, func(task *domain.Task) error {
		if task.Status != domain.StatusWaitingApproval {
			return domain.ErrTaskNotFound
		}
		task.Status = domain.StatusRunning
		task.WaitingReason = ""
		task.WaitingDraftID = uuid.Nil
		return nil
	})
}

func (m *memoryTasks) Finish(_ context.Context, scope domain.Scope, finish domain.Finish) (domain.Task, error) {
	return m.mutate(scope, finish.TaskID, func(task *domain.Task) error {
		if !task.Live() {
			return domain.ErrTaskNotFound
		}
		task.Status = finish.Status
		task.Summary = finish.Summary
		task.StoppedReason = finish.StoppedReason
		task.InputTokens = finish.InputTokens
		task.OutputTokens = finish.OutputTokens
		task.CostMicros = finish.CostMicros
		task.StepCount = finish.StepCount
		task.WaitingReason = ""
		task.WaitingDraftID = uuid.Nil
		now := time.Now()
		task.FinishedAt = &now
		return nil
	})
}

func (m *memoryTasks) Cancel(_ context.Context, scope domain.Scope, id uuid.UUID, reason string) (domain.Task, error) {
	return m.mutate(scope, id, func(task *domain.Task) error {
		if !task.Live() {
			return domain.ErrTaskNotFound
		}
		task.Status = domain.StatusCanceled
		task.StoppedReason = reason
		now := time.Now()
		task.FinishedAt = &now
		return nil
	})
}

func (m *memoryTasks) Children(_ context.Context, scope domain.Scope, taskID uuid.UUID) ([]domain.Task, error) {
	return m.List(context.Background(), scope, domain.Filter{ParentTaskID: taskID})
}

func (m *memoryTasks) Append(_ context.Context, scope domain.Scope, step domain.Step) (domain.Step, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	seq := 0
	for _, existing := range m.steps {
		if existing.TaskID == step.TaskID && existing.Seq > seq {
			seq = existing.Seq
		}
	}

	step.ID = uuid.New()
	step.Seq = seq + 1
	step.CreatedAt = time.Now()
	m.steps = append(m.steps, step)
	return step, nil
}

func (m *memoryTasks) ListSteps(_ context.Context, scope domain.Scope, taskID uuid.UUID) ([]domain.Step, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	steps := make([]domain.Step, 0, len(m.steps))
	for _, step := range m.steps {
		if step.TaskID == taskID {
			steps = append(steps, step)
		}
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].Seq < steps[j].Seq })
	return steps, nil
}

func (m *memoryTasks) mutate(scope domain.Scope, id uuid.UUID, change func(*domain.Task) error) (domain.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	task, ok := m.tasks[id]
	if !ok || task.WorkspaceID != scope.WorkspaceID {
		return domain.Task{}, domain.ErrTaskNotFound
	}
	if err := change(&task); err != nil {
		return domain.Task{}, err
	}
	task.UpdatedAt = time.Now()
	m.tasks[id] = task
	return task, nil
}

// recordingStarter records what was asked of the engine instead of starting it.
type recordingStarter struct {
	started []domain.StartInput
	signals []string
	// fail makes a start fail, which is how a task that nothing will run is
	// tested.
	fail error
}

func (s *recordingStarter) Start(_ context.Context, _ domain.Scope, input domain.StartInput) (domain.StartResult, error) {
	if s.fail != nil {
		return domain.StartResult{}, s.fail
	}
	s.started = append(s.started, input)
	return domain.StartResult{WorkflowID: "task-" + input.TaskID.String(), RunID: "run-1"}, nil
}

func (s *recordingStarter) Signal(_ context.Context, _ domain.Scope, taskID uuid.UUID, name string, _ any) error {
	s.signals = append(s.signals, name+":"+taskID.String())
	return nil
}

// fakeAgents answers the registry question the service asks.
type fakeAgents struct {
	agent domain.AgentRef
	// resting makes the Bolu refuse new work.
	resting bool
}

func (f fakeAgents) Agent(_ context.Context, _ domain.Scope, _ uuid.UUID) (domain.AgentRef, error) {
	agent := f.agent
	if f.resting {
		agent.Stored = domain.Resting
	}
	return agent, nil
}

func (f fakeAgents) Active(ref domain.AgentRef) bool { return ref.Active() }

// countingEvents records the events a task published.
type countingEvents struct {
	types []string
}

func (c *countingEvents) Emit(_ context.Context, event domain.Event) error {
	c.types = append(c.types, event.Type)
	return nil
}
