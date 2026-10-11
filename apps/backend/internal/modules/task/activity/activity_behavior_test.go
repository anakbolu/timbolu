package activity

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/workflow"
)

// The activity tests cover what the workflow's own tests script away: the gate
// that turns a write into a draft, the handoff that opens a child task, the
// truncation that bounds a tool's answer, and the resume that un-parks a task.
//
// They run against an in-memory repository rather than a mock, because the
// behaviour under test is a sequence of writes: a mock would assert which calls
// happened and miss what the task's state became.

const (
	workspaceID = "11111111-1111-1111-1111-111111111111"
	agentID     = "22222222-2222-2222-2222-222222222222"
	otherAgent  = "33333333-3333-3333-3333-333333333333"
)

// memoryTasks is an in-memory implementation of the task and step ports, with
// the tenant check and the status guards of the SQL.
type memoryTasks struct {
	tasks map[uuid.UUID]domain.Task
	steps []domain.Step
}

func newMemoryTasks() *memoryTasks {
	return &memoryTasks{tasks: map[uuid.UUID]domain.Task{}}
}

func (m *memoryTasks) Create(_ context.Context, scope domain.Scope, task domain.NewTask) (domain.Task, error) {
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
	}
	m.tasks[created.ID] = created
	return created, nil
}

func (m *memoryTasks) Get(_ context.Context, scope domain.Scope, id uuid.UUID) (domain.Task, error) {
	task, ok := m.tasks[id]
	if !ok || task.WorkspaceID != scope.WorkspaceID {
		return domain.Task{}, domain.ErrTaskNotFound
	}
	return task, nil
}

func (m *memoryTasks) List(_ context.Context, scope domain.Scope, _ domain.Filter) ([]domain.Task, error) {
	var tasks []domain.Task
	for _, task := range m.tasks {
		if task.WorkspaceID == scope.WorkspaceID {
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
}

func (m *memoryTasks) Start(_ context.Context, scope domain.Scope, id uuid.UUID, workflowID, runID string) (domain.Task, error) {
	return m.mutate(scope, id, func(task *domain.Task) error {
		task.Status = domain.StatusRunning
		task.WorkflowID = workflowID
		task.TemporalRunID = runID
		return nil
	})
}

func (m *memoryTasks) Progress(_ context.Context, scope domain.Scope, progress domain.Progress) error {
	_, err := m.mutate(scope, progress.TaskID, func(task *domain.Task) error {
		task.StepCount = progress.StepCount
		task.InputTokens = progress.InputTokens
		task.OutputTokens = progress.OutputTokens
		task.CostMicros = progress.CostMicros
		return nil
	})
	return err
}

func (m *memoryTasks) Park(_ context.Context, scope domain.Scope, id uuid.UUID, reason string, draftID uuid.UUID) (domain.Task, error) {
	return m.mutate(scope, id, func(task *domain.Task) error {
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
		task.Status = finish.Status
		task.Summary = finish.Summary
		task.StoppedReason = finish.StoppedReason
		task.InputTokens = finish.InputTokens
		task.OutputTokens = finish.OutputTokens
		task.StepCount = finish.StepCount
		return nil
	})
}

func (m *memoryTasks) Cancel(_ context.Context, scope domain.Scope, id uuid.UUID, reason string) (domain.Task, error) {
	return m.mutate(scope, id, func(task *domain.Task) error {
		task.Status = domain.StatusCanceled
		task.StoppedReason = reason
		return nil
	})
}

func (m *memoryTasks) Children(_ context.Context, scope domain.Scope, taskID uuid.UUID) ([]domain.Task, error) {
	var tasks []domain.Task
	for _, task := range m.tasks {
		if task.WorkspaceID == scope.WorkspaceID && task.ParentTaskID == taskID {
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
}

func (m *memoryTasks) Append(_ context.Context, _ domain.Scope, step domain.Step) (domain.Step, error) {
	step.Seq = len(m.steps) + 1
	m.steps = append(m.steps, step)
	return step, nil
}

func (m *memoryTasks) ListSteps(_ context.Context, _ domain.Scope, taskID uuid.UUID) ([]domain.Step, error) {
	var steps []domain.Step
	for _, step := range m.steps {
		if step.TaskID == taskID {
			steps = append(steps, step)
		}
	}
	return steps, nil
}

func (m *memoryTasks) mutate(scope domain.Scope, id uuid.UUID, change func(*domain.Task) error) (domain.Task, error) {
	task, ok := m.tasks[id]
	if !ok || task.WorkspaceID != scope.WorkspaceID {
		return domain.Task{}, domain.ErrTaskNotFound
	}
	if err := change(&task); err != nil {
		return domain.Task{}, err
	}
	m.tasks[id] = task
	return task, nil
}

// stubAgents answers the registry questions.
type stubAgents struct{ byName map[string]domain.AgentRef }

func (s stubAgents) Agent(_ context.Context, _ domain.Scope, id uuid.UUID) (domain.AgentRef, error) {
	for _, agent := range s.byName {
		if agent.ID == id {
			return agent, nil
		}
	}
	return domain.AgentRef{}, domain.ErrInvalidInput
}

func (s stubAgents) Active(ref domain.AgentRef) bool { return ref.Active() }

func (s stubAgents) ByName(_ context.Context, _ domain.Scope, name string) (domain.AgentRef, error) {
	agent, ok := s.byName[name]
	if !ok {
		return domain.AgentRef{}, domain.ErrInvalidInput
	}
	return agent, nil
}

// stubDrafts records the proposals it was asked to open.
type stubDrafts struct {
	requests []domain.DraftRequest
	status   string
}

func (s *stubDrafts) RequestDraft(_ context.Context, _ domain.Scope, req domain.DraftRequest) (domain.Draft, error) {
	s.requests = append(s.requests, req)
	status := s.status
	if status == "" {
		status = domain.DraftPending
	}
	return domain.Draft{ID: uuid.New(), Title: req.Title, Status: status}, nil
}

// stubExecutor answers a tool call and records it.
type stubExecutor struct {
	calls   []domain.ToolCall
	content []byte
	err     error
}

func (s *stubExecutor) Execute(_ context.Context, _ domain.Scope, call domain.ToolCall) (domain.ToolResult, error) {
	s.calls = append(s.calls, call)
	if s.err != nil {
		return domain.ToolResult{}, s.err
	}
	return domain.ToolResult{Content: s.content}, nil
}

// countingEvents records the events an activity published.
type countingEvents struct{ types []string }

func (c *countingEvents) Emit(_ context.Context, event domain.Event) error {
	c.types = append(c.types, event.Type)
	return nil
}

// stubReply records the answer written into the conversation.
type stubReply struct {
	written []domain.WriteReply
	history []domain.Message
}

func (s *stubReply) History(context.Context, domain.Scope, uuid.UUID, int) ([]domain.Message, error) {
	return s.history, nil
}

func (s *stubReply) Write(_ context.Context, _ domain.Scope, write domain.WriteReply) error {
	s.written = append(s.written, write)
	return nil
}

// harness is the activity set under test with its fakes.
type harness struct {
	activities *Activities
	tasks      *memoryTasks
	drafts     *stubDrafts
	executor   *stubExecutor
	events     *countingEvents
	reply      *stubReply
	task       domain.Task
	scope      domain.Scope
}

func newHarness(t *testing.T, limits domain.Limits, mutate ...func(*Deps)) *harness {
	t.Helper()

	tasks := newMemoryTasks()
	drafts := &stubDrafts{}
	executor := &stubExecutor{content: json.RawMessage(`{"ok":true}`)}
	events := &countingEvents{}
	reply := &stubReply{}

	scope := domain.Scope{WorkspaceID: uuid.MustParse(workspaceID)}
	task, err := tasks.Create(context.Background(), scope, domain.NewTask{
		AgentID: uuid.MustParse(agentID),
		Trigger: domain.TriggerChat,
		Title:   "Rekap pesanan",
	})
	require.NoError(t, err)

	deps := Deps{
		Tasks:    tasks,
		Steps:    tasks,
		Agents:   stubAgents{byName: map[string]domain.AgentRef{"Oren": {ID: uuid.MustParse(agentID), Name: "Oren", Role: "Penjualan", Stored: "active"}}},
		Tools:    stubTools{},
		Executor: executor,
		Drafts:   drafts,
		Events:   events,
		Reply:    reply,
		Namer:    stubAgents{byName: map[string]domain.AgentRef{"Ijo": {ID: uuid.MustParse(otherAgent), Name: "Ijo", Role: "Gudang", Stored: "active"}}},
		Limits:   limits.Normalise(),
	}
	for _, change := range mutate {
		change(&deps)
	}

	return &harness{
		activities: New(deps),
		tasks:      tasks,
		drafts:     drafts,
		executor:   executor,
		events:     events,
		reply:      reply,
		task:       task,
		scope:      scope,
	}
}

func (h *harness) input() workflow.TaskInput {
	return workflow.TaskInput{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		AgentID:     h.task.AgentID.String(),
		Title:       h.task.Title,
		Prompt:      "Rekap pesanan hari ini",
	}
}

// stubTools offers one reading tool.
type stubTools struct{}

func (stubTools) Tools(context.Context, domain.Scope, uuid.UUID) ([]domain.Tool, error) {
	return []domain.Tool{{
		Name:        "gmail.search",
		Integration: "gmail",
		Description: "Cari email di kotak masuk",
		Schema:      json.RawMessage(`{"type":"object"}`),
		Label:       domain.LabelRead,
	}}, nil
}

// TestLoadTaskContextOffersTheHandoffTool is the registry contract: the tools a
// Bolu was granted are offered, and the handoff tool is offered alongside them
// because it is part of the runtime rather than an integration.
func TestLoadTaskContextOffersTheHandoffTool(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits)

	loaded, err := h.activities.LoadTaskContext(context.Background(), h.input())
	require.NoError(t, err)

	names := make([]string, 0, len(loaded.Tools))
	for _, tool := range loaded.Tools {
		names = append(names, tool.Name)
	}
	require.Contains(t, names, "gmail.search")
	require.Contains(t, names, domain.ToolHandoff)
	require.Equal(t, "Oren", loaded.Agent.Name)
	require.Equal(t, domain.DefaultLimits.MaxSteps, loaded.Limits.MaxSteps)
}

// TestLoadTaskContextRefusesAnUnknownWorkspace is the tenant guard: an activity
// whose workspace is missing or wrong fails rather than reading anything.
func TestLoadTaskContextRefusesAnUnknownWorkspace(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits)

	_, err := h.activities.LoadTaskContext(context.Background(), workflow.TaskInput{TaskID: h.task.ID.String()})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.activities.LoadTaskContext(context.Background(), workflow.TaskInput{
		TaskID:      h.task.ID.String(),
		WorkspaceID: uuid.NewString(),
	})
	require.ErrorIs(t, err, domain.ErrTaskNotFound)
}

// TestCallModelRecordsTheCall is the gateway contract: the request carries the
// task, the Bolu, and the purpose, which is what makes a task's spend
// attributable in the ledger.
func TestCallModelRecordsTheCall(t *testing.T) {
	var captured llmdomain.ChatRequest
	var capturedScope llmdomain.Scope

	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) {
		deps.Gateway = capturingGateway{onChat: func(scope llmdomain.Scope, req llmdomain.ChatRequest) (llmdomain.ChatResponse, error) {
			capturedScope = scope
			captured = req
			return llmdomain.ChatResponse{
				Text: "Ada 2 pesanan.",
				Usage: llmdomain.Usage{
					InputTokens: 100, OutputTokens: 20,
				},
				Model: "test-model", FinishReason: llmdomain.FinishStop,
			}, nil
		}}
	})

	reply, err := h.activities.CallModel(context.Background(), workflow.ModelRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		AgentID:     h.task.AgentID.String(),
		System:      "Kamu Oren.",
		Messages: []domain.Message{{
			UserID: uuid.New(),
			Blocks: []domain.Block{{Type: "text", Body: []byte(`{"type":"text","markdown":"Rekap pesanan"}`)}},
		}},
		Tools: []domain.Tool{{Name: "gmail.search", Description: "Cari email", Schema: json.RawMessage(`{}`), Label: domain.LabelRead}},
	})
	require.NoError(t, err)

	require.Equal(t, "Ada 2 pesanan.", reply.Text)
	require.Equal(t, 100, reply.Usage.InputTokens)
	require.Equal(t, "Kamu Oren.", captured.System)
	require.Len(t, captured.Tools, 1)
	require.Equal(t, "gmail.search", captured.Tools[0].Name)
	require.Equal(t, llmdomain.PurposeAgent, captured.Metadata.Purpose)
	require.Equal(t, h.task.ID, captured.Metadata.TaskID)
	require.Equal(t, h.scope.WorkspaceID, capturedScope.WorkspaceID)
	require.Equal(t, h.task.AgentID, capturedScope.AgentID)

	// The user's turn is rendered as a real text block, which is what the model
	// reads; a message with no text is skipped rather than sent blank.
	require.Len(t, captured.Messages, 1)
	require.Equal(t, llmdomain.RoleUser, captured.Messages[0].Role)
	require.Equal(t, "Rekap pesanan", captured.Messages[0].Text)
}

// TestCallModelRefusesWithoutAGateway is the honest failure: a deployment with no
// model configured reports it rather than answering with an empty reply.
func TestCallModelRefusesWithoutAGateway(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits)

	_, err := h.activities.CallModel(context.Background(), workflow.ModelRequest{
		TaskID: h.task.ID.String(), WorkspaceID: h.scope.WorkspaceID.String(),
	})
	require.ErrorContains(t, err, "gateway is not configured")
}

// TestCallModelAsksTheBudgetBeforeSpending is the bound that is checked earliest:
// a workspace that ran out stops before the provider is called, so nothing is
// billed.
func TestCallModelAsksTheBudgetBeforeSpending(t *testing.T) {
	calls := 0
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) {
		deps.Budget = refusingBudget{}
		deps.Gateway = capturingGateway{onChat: func(llmdomain.Scope, llmdomain.ChatRequest) (llmdomain.ChatResponse, error) {
			calls++
			return llmdomain.ChatResponse{Text: "seharusnya tidak sampai"}, nil
		}}
	})

	_, err := h.activities.CallModel(context.Background(), workflow.ModelRequest{
		TaskID: h.task.ID.String(), WorkspaceID: h.scope.WorkspaceID.String(),
	})
	require.ErrorIs(t, err, errBudgetExhausted)
	require.Zero(t, calls, "the provider is never called once the budget is spent")
}

// TestRunToolTruncatesALongAnswer is the E6.3 bound: a tool that answers with
// more than the limit is cut, because a megabyte of result would be re-sent to
// the model on every later round.
func TestRunToolTruncatesALongAnswer(t *testing.T) {
	long := []byte(`{"files":["` + strings.Repeat("x", 5000) + `"]}`)
	h := newHarness(t, domain.Limits{MaxSteps: 5, MaxTokens: 1000, MaxHandoffDepth: 2, MaxToolResultBytes: 256})
	h.executor.content = long

	outcome, err := h.activities.RunTool(context.Background(), workflow.ToolRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		CallID:      "call-1",
		Name:        "gmail.search",
		Arguments:   json.RawMessage(`{"q":"pesanan"}`),
	})
	require.NoError(t, err)

	require.LessOrEqual(t, len(outcome.Content), 512, "the answer is bounded")

	// The truncation is itself valid JSON that says so, so the model is told it
	// was cut rather than being handed a fragment it has to guess about.
	var payload struct {
		Truncated bool   `json:"truncated"`
		Bytes     int    `json:"bytes"`
		Preview   string `json:"preview"`
	}
	require.NoError(t, json.Unmarshal(outcome.Content, &payload))
	require.True(t, payload.Truncated)
	require.Equal(t, len(long), payload.Bytes)
	require.NotEmpty(t, payload.Preview)

	require.Len(t, h.executor.calls, 1)
	require.Equal(t, h.task.ID, h.executor.calls[0].TaskID)
	require.Equal(t, h.task.AgentID, h.executor.calls[0].AgentID)
}

// TestRunToolReportsAMissingExecutor is the configuration fact rather than a
// failure: a tool nothing can run is answered as unavailable, which the model can
// react to.
func TestRunToolReportsAMissingExecutor(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) { deps.Executor = nil })

	_, err := h.activities.RunTool(context.Background(), workflow.ToolRequest{
		TaskID: h.task.ID.String(), WorkspaceID: h.scope.WorkspaceID.String(), Name: "gmail.search",
	})
	require.ErrorIs(t, err, domain.ErrToolNotAvailable)
}

// TestRequestDraftParksTheTask is the product's central guardrail: an action that
// reaches outside Bolu becomes a draft, the task is parked with the reason, and
// the office learns about it from the event.
func TestRequestDraftParksTheTask(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits)

	outcome, err := h.activities.RequestDraft(context.Background(), workflow.ToolRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		CallID:      "call-1",
		Name:        "gmail.send",
		Arguments:   json.RawMessage(`{"subject":"Tagihan Maret","to":"a@b.c"}`),
	})
	require.NoError(t, err)

	require.NotEmpty(t, outcome.DraftID)
	require.Equal(t, domain.DraftPending, outcome.Status)
	require.Equal(t, "Tagihan Maret", outcome.Title, "the draft is named from the action's own subject")

	require.Len(t, h.drafts.requests, 1)
	require.Equal(t, h.task.ID, h.drafts.requests[0].TaskID)
	require.Equal(t, "gmail.send", h.drafts.requests[0].ActionKind)

	stored, err := h.tasks.Get(context.Background(), h.scope, h.task.ID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusWaitingApproval, stored.Status)
	require.Contains(t, stored.WaitingReason, "menunggu persetujuan: Tagihan Maret")
	require.NotEqual(t, uuid.Nil, stored.WaitingDraftID)
	require.Contains(t, h.events.types, workflow.EventTaskWaiting)
}

// TestRequestDraftWithoutAGateRefusesTheAction is the safe direction: with no
// approval gate configured the action is refused rather than taken, because
// taking it would skip the guardrail the product is built around.
func TestRequestDraftWithoutAGateRefusesTheAction(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) { deps.Drafts = nil })

	outcome, err := h.activities.RequestDraft(context.Background(), workflow.ToolRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		Name:        "gmail.send",
		Arguments:   json.RawMessage(`{"to":"a@b.c"}`),
	})
	require.NoError(t, err)

	require.Equal(t, domain.DraftCanceled, outcome.Status)
	require.Contains(t, outcome.Note, "tidak dikonfigurasi")

	stored, err := h.tasks.Get(context.Background(), h.scope, h.task.ID)
	require.NoError(t, err)
	require.NotEqual(t, domain.StatusWaitingApproval, stored.Status,
		"a refused action must not park the task on an approval that will never come")
}

// TestResumeTaskUnparksAndIsIdempotent covers the decision's arrival: the task
// goes back to running, and a second resume is a no-op rather than a failure —
// a cancel that landed while the decision was in flight already finished it.
func TestResumeTaskUnparksAndIsIdempotent(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits)

	_, err := h.activities.RequestDraft(context.Background(), workflow.ToolRequest{
		TaskID: h.task.ID.String(), WorkspaceID: h.scope.WorkspaceID.String(),
		Name: "gmail.send", Arguments: json.RawMessage(`{}`),
	})
	require.NoError(t, err)

	require.NoError(t, h.activities.ResumeTask(context.Background(), workflow.ResumeRequest{
		TaskID: h.task.ID.String(), WorkspaceID: h.scope.WorkspaceID.String(),
	}))

	resumed, err := h.tasks.Get(context.Background(), h.scope, h.task.ID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusRunning, resumed.Status)
	require.Empty(t, resumed.WaitingReason)

	// A task that is already running is not an error to resume.
	require.NoError(t, h.activities.ResumeTask(context.Background(), workflow.ResumeRequest{
		TaskID: h.task.ID.String(), WorkspaceID: h.scope.WorkspaceID.String(),
	}))
}

// TestPrepareHandoffOpensAChildTask is the E6.5 gate: the parent asks for another
// Bolu by name, a child task is opened in the same workspace, and its input is
// returned for the workflow to start.
func TestPrepareHandoffOpensAChildTask(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits)

	prepared, err := h.activities.PrepareHandoff(context.Background(), workflow.HandoffRequest{
		ParentTaskID: h.task.ID.String(),
		WorkspaceID:  h.scope.WorkspaceID.String(),
		AgentName:    "Ijo",
		Instructions: "Ambil data stok gudang\nlalu rangkum",
		Depth:        0,
	})
	require.NoError(t, err)
	require.Empty(t, prepared.Refusal)

	child := prepared.Child
	require.NotEqual(t, h.task.ID.String(), child.TaskID)
	require.Equal(t, otherAgent, child.AgentID)
	require.Equal(t, h.scope.WorkspaceID.String(), child.WorkspaceID, "a child stays in the same workspace")
	require.Equal(t, 1, child.Depth)
	require.Equal(t, "Ambil data stok gudang", child.Title, "the title is the first line of the instructions")
	require.Equal(t, "Ambil data stok gudang\nlalu rangkum", child.Prompt)

	stored, err := h.tasks.Get(context.Background(), h.scope, uuid.MustParse(child.TaskID))
	require.NoError(t, err)
	require.Equal(t, domain.TriggerHandoff, stored.Trigger)
	require.Equal(t, h.task.ID, stored.ParentTaskID)
	require.Equal(t, domain.StatusQueued, stored.Status)
}

// TestPrepareHandoffRefusesRatherThanFailing is the shape the model needs: a
// refusal is a message it can react to, not an error that ends the task.
func TestPrepareHandoffRefusesRatherThanFailing(t *testing.T) {
	tests := []struct {
		name    string
		request workflow.HandoffRequest
		want    string
	}{
		{
			name: "a chain past the ceiling",
			request: workflow.HandoffRequest{
				AgentName: "Ijo", Instructions: "apa saja", Depth: 2,
			},
			want: "rantai sudah 2 tingkat",
		},
		{
			name: "a name that matches nobody",
			request: workflow.HandoffRequest{
				AgentName: "Siapa Itu", Instructions: "apa saja",
			},
			want: "tidak ada Bolu bernama",
		},
		{
			name: "no name at all",
			request: workflow.HandoffRequest{
				AgentName: "", Instructions: "apa saja",
			},
			want: "tidak ada Bolu bernama",
		},
		{
			name: "empty instructions",
			request: workflow.HandoffRequest{
				AgentName: "Ijo", Instructions: "   ",
			},
			want: "instruksinya kosong",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, domain.DefaultLimits)

			request := test.request
			request.ParentTaskID = h.task.ID.String()
			request.WorkspaceID = h.scope.WorkspaceID.String()

			prepared, err := h.activities.PrepareHandoff(context.Background(), request)
			require.NoError(t, err, "a refusal is not an error")
			require.Contains(t, prepared.Refusal, test.want)
			require.Empty(t, prepared.Child.TaskID, "a refused handoff opens no child")
		})
	}
}

// TestPrepareHandoffRefusesARestingBolu covers the registry's switch at the
// handoff door: a Bolu that is resting takes no new work, so the parent is told
// rather than the child starting anyway.
func TestPrepareHandoffRefusesARestingBolu(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) {
		deps.Namer = stubAgents{byName: map[string]domain.AgentRef{
			"Ijo": {ID: uuid.MustParse(otherAgent), Name: "Ijo", Stored: domain.Resting},
		}}
	})

	prepared, err := h.activities.PrepareHandoff(context.Background(), workflow.HandoffRequest{
		ParentTaskID: h.task.ID.String(),
		WorkspaceID:  h.scope.WorkspaceID.String(),
		AgentName:    "Ijo",
		Instructions: "Ambil data stok",
	})
	require.NoError(t, err)
	require.Contains(t, prepared.Refusal, "Ijo sedang istirahat")
}

// TestMarkHandoffStartedRecordsTheChildWorkflow covers the chain's traceability:
// the child's task records the workflow that runs it, so a chain is readable from
// the parent as well as in Temporal.
func TestMarkHandoffStartedRecordsTheChildWorkflow(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits)

	prepared, err := h.activities.PrepareHandoff(context.Background(), workflow.HandoffRequest{
		ParentTaskID: h.task.ID.String(),
		WorkspaceID:  h.scope.WorkspaceID.String(),
		AgentName:    "Ijo",
		Instructions: "Ambil data stok",
	})
	require.NoError(t, err)

	require.NoError(t, h.activities.MarkHandoffStarted(context.Background(), workflow.HandoffStarted{
		TaskID:      prepared.Child.TaskID,
		WorkspaceID: prepared.Child.WorkspaceID,
		WorkflowID:  "task-" + prepared.Child.TaskID,
		RunID:       "run-1",
	}))

	stored, err := h.tasks.Get(context.Background(), h.scope, uuid.MustParse(prepared.Child.TaskID))
	require.NoError(t, err)
	require.Equal(t, domain.StatusRunning, stored.Status)
	require.Equal(t, "task-"+prepared.Child.TaskID, stored.WorkflowID)
	require.Equal(t, "run-1", stored.TemporalRunID)
	require.Contains(t, h.events.types, workflow.EventTaskStarted)
}

// TestCompleteWritesTheAnswerAndTheSummary is the terminal write: the task's
// state, its one-line summary, and the answer in the conversation it came from.
func TestCompleteWritesTheAnswerAndTheSummary(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits)
	h.reply.written = nil

	// The task answers a conversation, so the reply is written where it was asked
	// for.
	conversationID := uuid.New()
	messageID := uuid.New()
	h.task.ConversationID = conversationID
	h.task.ReplyMessageID = messageID
	h.tasks.tasks[h.task.ID] = h.task

	require.NoError(t, h.activities.RecordStep(context.Background(), workflow.StepRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		Record: workflow.StepRecord{
			Kind:   domain.StepFinal,
			Output: json.RawMessage(`{"text":"Ada 3 pesanan hari ini."}`),
		},
	}))

	outcome, err := h.activities.CompleteTask(context.Background(), workflow.CompleteRequest{
		TaskID:       h.task.ID.String(),
		WorkspaceID:  h.scope.WorkspaceID.String(),
		Status:       domain.StatusSucceeded,
		InputTokens:  500,
		OutputTokens: 120,
		StepCount:    3,
		Blocks:       []domain.Block{{Type: "text", Body: []byte(`{"type":"text","markdown":"Ada 3 pesanan hari ini."}`)}},
	})
	require.NoError(t, err)
	require.Equal(t, domain.StatusSucceeded, outcome.Status)
	require.Equal(t, "Ada 3 pesanan hari ini.", outcome.Summary)

	stored, err := h.tasks.Get(context.Background(), h.scope, h.task.ID)
	require.NoError(t, err)
	require.Equal(t, int64(500), stored.InputTokens)
	require.Equal(t, 3, stored.StepCount)
	require.Contains(t, h.events.types, workflow.EventTaskFinished)

	require.Len(t, h.reply.written, 1)
	require.Equal(t, messageID, h.reply.written[0].MessageID)
	require.Equal(t, workflow.MessageComplete, h.reply.written[0].Status)
	require.Equal(t, h.task.ID, h.reply.written[0].TaskID)
}

// TestCompleteMarksAFailedTaskFailedInTheConversation is the honest partial: a
// task that stopped on a bound writes its reason into the thread rather than
// leaving the placeholder looking like a Bolu that is still thinking.
func TestCompleteMarksAFailedTaskFailedInTheConversation(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits)

	h.task.ConversationID = uuid.New()
	h.task.ReplyMessageID = uuid.New()
	h.tasks.tasks[h.task.ID] = h.task

	_, err := h.activities.CompleteTask(context.Background(), workflow.CompleteRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		Status:      domain.StatusFailed,
		Reason:      "tugas berhenti karena mencapai batas token",
		Blocks:      []domain.Block{{Type: "text", Body: []byte(`{"type":"text","markdown":"tugas berhenti karena mencapai batas token"}`)}},
	})
	require.NoError(t, err)

	require.Len(t, h.reply.written, 1)
	require.Equal(t, workflow.MessageFailed, h.reply.written[0].Status)
	require.Contains(t, h.reply.written[0].FinishReason, "batas token")
}

// TestBeatStopsOnEveryBound is the single place a bound is decided, so each one
// has exactly one implementation and the reason it produces is what the user
// reads.
func TestBeatStopsOnEveryBound(t *testing.T) {
	t.Run("a canceled task is discovered at the checkpoint", func(t *testing.T) {
		h := newHarness(t, domain.DefaultLimits)

		canceled, err := h.tasks.Cancel(context.Background(), h.scope, h.task.ID, "dihentikan oleh pengguna")
		require.NoError(t, err)
		require.Equal(t, domain.StatusCanceled, canceled.Status)

		beat, err := h.activities.BeatTask(context.Background(), workflow.Beat{
			TaskID: h.task.ID.String(), WorkspaceID: h.scope.WorkspaceID.String(), StepCount: 1,
		})
		require.NoError(t, err)
		require.Equal(t, domain.StatusCanceled, beat.StopReason)
		require.Equal(t, "dihentikan oleh pengguna", beat.StopDetail)
	})

	t.Run("the token bound", func(t *testing.T) {
		h := newHarness(t, domain.Limits{MaxSteps: 10, MaxTokens: 100, MaxHandoffDepth: 2, MaxToolResultBytes: 1024})

		beat, err := h.activities.BeatTask(context.Background(), workflow.Beat{
			TaskID: h.task.ID.String(), WorkspaceID: h.scope.WorkspaceID.String(),
			StepCount: 1, InputTokens: 90, OutputTokens: 30,
		})
		require.NoError(t, err)
		require.Equal(t, domain.StatusFailed, beat.StopReason)
		require.Contains(t, beat.StopDetail, "batas token")
		require.Contains(t, beat.StopDetail, "120 dari 100")
	})

	t.Run("the step bound", func(t *testing.T) {
		h := newHarness(t, domain.Limits{MaxSteps: 3, MaxTokens: 1_000_000, MaxHandoffDepth: 2, MaxToolResultBytes: 1024})

		beat, err := h.activities.BeatTask(context.Background(), workflow.Beat{
			TaskID: h.task.ID.String(), WorkspaceID: h.scope.WorkspaceID.String(), StepCount: 3,
		})
		require.NoError(t, err)
		require.Equal(t, domain.StatusFailed, beat.StopReason)
		require.Contains(t, beat.StopDetail, "batas langkah")
	})

	t.Run("a spent budget", func(t *testing.T) {
		h := newHarness(t, domain.DefaultLimits, func(deps *Deps) { deps.Budget = refusingBudget{} })

		beat, err := h.activities.BeatTask(context.Background(), workflow.Beat{
			TaskID: h.task.ID.String(), WorkspaceID: h.scope.WorkspaceID.String(), StepCount: 1,
		})
		require.NoError(t, err)
		require.Equal(t, domain.StatusFailed, beat.StopReason)
		require.Contains(t, beat.StopDetail, "kuota habis")
	})

	t.Run("a task within its bounds keeps going and records a heartbeat", func(t *testing.T) {
		h := newHarness(t, domain.DefaultLimits)

		beat, err := h.activities.BeatTask(context.Background(), workflow.Beat{
			TaskID: h.task.ID.String(), WorkspaceID: h.scope.WorkspaceID.String(),
			StepCount: 2, InputTokens: 300, OutputTokens: 80,
		})
		require.NoError(t, err)
		require.Empty(t, beat.StopReason)
		require.Contains(t, h.events.types, workflow.EventTaskRunning)

		stored, err := h.tasks.Get(context.Background(), h.scope, h.task.ID)
		require.NoError(t, err)
		require.Equal(t, 2, stored.StepCount)
		require.Equal(t, int64(300), stored.InputTokens)
		require.Equal(t, int64(80), stored.OutputTokens)
	})
}

// TestRecordStepAssignsTheSequence covers the history's order: the store assigns
// the sequence, so two activities writing at once cannot pick the same number.
func TestRecordStepAssignsTheSequence(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits)

	for index, kind := range []string{domain.StepThink, domain.StepToolCall, domain.StepToolResult} {
		require.NoError(t, h.activities.RecordStep(context.Background(), workflow.StepRequest{
			TaskID:      h.task.ID.String(),
			WorkspaceID: h.scope.WorkspaceID.String(),
			Record:      workflow.StepRecord{Kind: kind, ToolName: "gmail.search"},
		}))
		require.Equal(t, index+1, h.tasks.steps[index].Seq)
	}

	require.Len(t, h.tasks.steps, 3)
	require.Contains(t, h.events.types, workflow.EventTaskStep)
}

// TestHelpersRenderWhatTheModelReads covers the small pure functions the
// activities lean on.
func TestHelpersRenderWhatTheModelReads(t *testing.T) {
	t.Run("the handoff tool carries a schema the model can call", func(t *testing.T) {
		tool := HandoffTool()
		require.Equal(t, domain.ToolHandoff, tool.Name)
		require.Equal(t, domain.LabelRead, tool.Label, "asking another Bolu is not an action outside Bolu")

		var schema struct {
			Required   []string       `json:"required"`
			Properties map[string]any `json:"properties"`
		}
		require.NoError(t, json.Unmarshal(tool.Schema, &schema))
		require.ElementsMatch(t, []string{"agent", "instructions"}, schema.Required)
	})

	t.Run("a draft is named from its own arguments", func(t *testing.T) {
		require.Equal(t, "Tagihan Maret", draftTitle(workflow.ToolRequest{
			Name: "gmail.send", Arguments: json.RawMessage(`{"subject":"Tagihan Maret"}`),
		}))
		require.Equal(t, "gmail.send", draftTitle(workflow.ToolRequest{
			Name: "gmail.send", Arguments: json.RawMessage(`{"to":"a@b.c"}`),
		}), "an action with no name of its own falls back to the tool")
		require.Equal(t, "gmail.send", draftTitle(workflow.ToolRequest{
			Name: "gmail.send", Arguments: json.RawMessage(`bukan json`),
		}))
	})

	t.Run("a title is the first line, bounded", func(t *testing.T) {
		require.Equal(t, "Rekap pesanan", firstLine("Rekap pesanan\nlalu kirim"))
		require.Equal(t, "", firstLine("   \n  "))
		require.Len(t, []rune(firstLine(strings.Repeat("a", 400))), 120)
	})

	t.Run("a short answer is left alone", func(t *testing.T) {
		short := []byte(`{"ok":true}`)
		require.Equal(t, short, truncate(short, 1024))
	})

	// The render helper lives in the domain, so it is tested where it is
	// defined; what this covers is that the activity reads a body through it
	// rather than sending the raw blocks to the model.
}

// errBudgetExhausted is what a spent workspace budget reports.
var errBudgetExhausted = errors.New("kuota habis")

// refusingBudget refuses every workspace, reporting the reason the policy gave.
type refusingBudget struct {
	reason string
}

func (r refusingBudget) Allow(context.Context, uuid.UUID) error {
	if r.reason != "" {
		return errors.New(r.reason)
	}
	return errBudgetExhausted
}

// capturingGateway hands each call to a function, which is how a test observes
// the request without a mock framework.
type capturingGateway struct {
	onChat func(llmdomain.Scope, llmdomain.ChatRequest) (llmdomain.ChatResponse, error)
}

func (g capturingGateway) Chat(_ context.Context, scope llmdomain.Scope, req llmdomain.ChatRequest) (llmdomain.ChatResponse, error) {
	return g.onChat(scope, req)
}

func (g capturingGateway) Stream(context.Context, llmdomain.Scope, llmdomain.ChatRequest) (<-chan llmdomain.StreamEvent, error) {
	return nil, errors.New("not used")
}

func (g capturingGateway) Providers(context.Context, llmdomain.Scope) ([]llmdomain.Redacted, error) {
	return nil, errors.New("not used")
}

func (g capturingGateway) Upsert(context.Context, llmdomain.Scope, llmdomain.UpsertRequest) (llmdomain.Redacted, error) {
	return llmdomain.Redacted{}, errors.New("not used")
}

func (g capturingGateway) Delete(context.Context, llmdomain.Scope, uuid.UUID) error {
	return errors.New("not used")
}

func (g capturingGateway) Test(context.Context, llmdomain.Scope, uuid.UUID) (llmdomain.Capabilities, error) {
	return llmdomain.Capabilities{}, errors.New("not used")
}

func (g capturingGateway) SetAgentModel(context.Context, llmdomain.Scope, llmdomain.AgentOverride) error {
	return errors.New("not used")
}

func (g capturingGateway) AgentModel(context.Context, llmdomain.Scope) (llmdomain.AgentOverride, error) {
	return llmdomain.AgentOverride{}, errors.New("not used")
}

func (g capturingGateway) Usage(context.Context, llmdomain.Scope, int) ([]llmdomain.UsageDaily, error) {
	return nil, errors.New("not used")
}

// Compile-time checks that the fakes satisfy the ports they stand in for.
var (
	_ domain.TaskRepository = (*memoryTasks)(nil)
	_ domain.StepRepository = (*memoryTasks)(nil)
	_ domain.AgentDirectory = stubAgents{}
	_ domain.AgentNamer     = stubAgents{}
	_ domain.ToolRegistry   = stubTools{}
	_ domain.ToolExecutor   = (*stubExecutor)(nil)
	_ domain.DraftGate      = (*stubDrafts)(nil)
	_ domain.Budget         = refusingBudget{}
	_ domain.EventSink      = (*countingEvents)(nil)
	_ domain.ReplyWriter    = (*stubReply)(nil)
	_ domain.Gateway        = capturingGateway{}
)

// TestEmitSurvivesASinkThatRefuses is the stream's guarantee at the activity's
// door: the event is written to the store first, so a publish that fails costs
// latency and never the event — and never the round.
func TestEmitSurvivesASinkThatRefuses(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) { deps.Events = refusingEvents{} })

	require.NoError(t, h.activities.ResumeTask(context.Background(), workflow.ResumeRequest{
		TaskID: h.task.ID.String(), WorkspaceID: h.scope.WorkspaceID.String(),
	}))
	require.NoError(t, h.activities.RecordStep(context.Background(), workflow.StepRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		Record:      workflow.StepRecord{Kind: domain.StepThink},
	}))
}

// TestEmitIsSkippedWithoutASink keeps the optional dependency optional: a
// deployment with no event store still runs tasks, and only the live views are
// behind.
func TestEmitIsSkippedWithoutASink(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) { deps.Events = nil })

	require.NoError(t, h.activities.RecordStep(context.Background(), workflow.StepRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		Record:      workflow.StepRecord{Kind: domain.StepThink},
	}))
}

// TestCostIsRecordedWhenThePriceTableExists is the accounting seam: a task's own
// total is priced with the same table the ledger uses, so the two cannot
// disagree about what a token cost.
func TestCostIsRecordedWhenThePriceTableExists(t *testing.T) {
	priced := fixedCost{perThousand: 10}
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) { deps.Costs = priced })

	_, err := h.activities.BeatTask(context.Background(), workflow.Beat{
		TaskID: h.task.ID.String(), WorkspaceID: h.scope.WorkspaceID.String(),
		StepCount: 1, InputTokens: 300, OutputTokens: 100,
	})
	require.NoError(t, err)

	stored, err := h.tasks.Get(context.Background(), h.scope, h.task.ID)
	require.NoError(t, err)
	require.Equal(t, int64(4), stored.CostMicros, "the task records what the price table charged")

	// Without a table the task keeps the total it already had, rather than
	// recording a zero that would read as a free call.
	bare := newHarness(t, domain.DefaultLimits)
	_, err = bare.activities.BeatTask(context.Background(), workflow.Beat{
		TaskID: bare.task.ID.String(), WorkspaceID: bare.scope.WorkspaceID.String(), StepCount: 1,
	})
	require.NoError(t, err)
	kept, err := bare.tasks.Get(context.Background(), bare.scope, bare.task.ID)
	require.NoError(t, err)
	require.Equal(t, int64(0), kept.CostMicros)
}

// TestCompleteWithoutAReplyWriterStillFinishes covers the deployment that has no
// conversation behind a task: the state is written, and only the answer into the
// thread is skipped.
func TestCompleteWithoutAReplyWriterStillFinishes(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) { deps.Reply = nil })
	h.task.ConversationID = uuid.New()
	h.tasks.tasks[h.task.ID] = h.task

	outcome, err := h.activities.CompleteTask(context.Background(), workflow.CompleteRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		Status:      domain.StatusSucceeded,
		Blocks:      []domain.Block{{Type: "text", Body: []byte(`{"type":"text","markdown":"selesai"}`)}},
	})
	require.NoError(t, err)
	require.Equal(t, domain.StatusSucceeded, outcome.Status)
}

// TestLoadTaskContextReadsTheThreadItAnswers covers the history the model reads:
// a task opened from chat is given the conversation, oldest first.
func TestLoadTaskContextReadsTheThreadItAnswers(t *testing.T) {
	conversationID := uuid.New()
	h := newHarness(t, domain.DefaultLimits)
	h.reply.history = []domain.Message{
		{UserID: uuid.New(), Blocks: []domain.Block{{Type: "text", Body: []byte(`{"type":"text","markdown":"Rekap pesanan"}`)}}},
	}
	h.task.ConversationID = conversationID
	h.tasks.tasks[h.task.ID] = h.task

	loaded, err := h.activities.LoadTaskContext(context.Background(), h.input())
	require.NoError(t, err)
	require.Len(t, loaded.History, 1)
	require.Equal(t, domain.RenderBlocks(h.reply.history[0].Blocks),
		domain.RenderBlocks(loaded.History[0].Blocks),
		"the thread reaches the activity as it was stored, so the workflow can render it")
}

// TestLoadTaskContextReportsAFailingHistoryRead is the honest failure: a task
// that cannot read its thread is not started with a partial one, because the
// model would then answer without the question.
func TestLoadTaskContextReportsAFailingHistoryRead(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) {
		deps.Reply = failingReply{err: errors.New("the conversation store is down")}
	})
	h.task.ConversationID = uuid.New()
	h.tasks.tasks[h.task.ID] = h.task

	_, err := h.activities.LoadTaskContext(context.Background(), h.input())
	require.ErrorContains(t, err, "load history")
}

// TestRunToolReportsAFailingExecutor is the tool's own failure, which the model
// reads as a failed result rather than the task ending on it.
func TestRunToolReportsAFailingExecutor(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) {
		deps.Executor = &stubExecutor{err: errors.New("the provider is unreachable")}
	})

	_, err := h.activities.RunTool(context.Background(), workflow.ToolRequest{
		TaskID: h.task.ID.String(), WorkspaceID: h.scope.WorkspaceID.String(), Name: "gmail.search",
	})
	require.ErrorContains(t, err, "unreachable")
}

// TestDraftTitleFallsBackToTheSubject covers the naming of a draft, which is what
// the office shows while the task waits.
func TestDraftTitleFallsBackToTheSubject(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits)

	_, err := h.activities.RequestDraft(context.Background(), workflow.ToolRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		Name:        "gmail.send",
		Arguments:   json.RawMessage(`{"summary":"Tagihan Maret"}`),
	})
	require.NoError(t, err)
	require.Len(t, h.drafts.requests, 1)
	require.Equal(t, "Tagihan Maret", h.drafts.requests[0].Title)
}

// TestCompleteKeepsTheSummaryWhenTheTaskHasNoFinalStep covers a task that stopped
// before it answered: the summary is still written, so the history reads as a
// task that did something rather than an empty row.
func TestCompleteKeepsTheSummaryWhenTheTaskHasNoFinalStep(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits)

	require.NoError(t, h.activities.RecordStep(context.Background(), workflow.StepRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		Record:      workflow.StepRecord{Kind: domain.StepToolCall, ToolName: "gmail.search"},
	}))

	outcome, err := h.activities.CompleteTask(context.Background(), workflow.CompleteRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		Status:      domain.StatusFailed,
		Reason:      "tugas berhenti karena mencapai batas langkah (1)",
	})
	require.NoError(t, err)
	require.Contains(t, outcome.Summary, "1 langkah")
}

// refusingEvents is an event sink that always fails, which is what a Redis
// outage looks like from an activity.
type refusingEvents struct{}

func (refusingEvents) Emit(context.Context, domain.Event) error {
	return errors.New("the activity stream is unavailable")
}

// failingReply is a reply writer whose history read fails.
type failingReply struct {
	err error
}

func (f failingReply) History(context.Context, domain.Scope, uuid.UUID, int) ([]domain.Message, error) {
	return nil, f.err
}

func (f failingReply) Write(context.Context, domain.Scope, domain.WriteReply) error { return nil }

// fixedCost is a price table that charges the same for every call.
type fixedCost struct {
	perThousand int64
	cost        int64
}

func (f fixedCost) Cost(_ string, usage llmdomain.Usage) int64 {
	f.cost = int64(usage.InputTokens+usage.OutputTokens) * f.perThousand / 1000
	return f.cost
}

// TestBeatStopsOnASpentDailyBudget is the reason the two bounds are separate: a
// workspace with tokens left for the month still stops once the day is spent,
// and the reason it produces names the day rather than the month.
func TestBeatStopsOnASpentDailyBudget(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) {
		deps.Budget = refusingBudget{reason: "kuota_exceeded: 5000 dari 5000 micro-rupiah terpakai hari ini"}
	})

	beat, err := h.activities.BeatTask(context.Background(), workflow.Beat{
		TaskID: h.task.ID.String(), WorkspaceID: h.scope.WorkspaceID.String(), StepCount: 1,
	})
	require.NoError(t, err)

	require.Equal(t, domain.StatusFailed, beat.StopReason)
	require.Contains(t, beat.StopDetail, "micro-rupiah")
	require.Contains(t, beat.StopDetail, "hari ini")

	// The beat reports the stop and writes nothing terminal: finishing the task
	// is CompleteTask's job, which is what keeps the terminal write in one place.
	stored, err := h.tasks.Get(context.Background(), h.scope, h.task.ID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusQueued, stored.Status, "the beat never writes a terminal status")
}

// TestCallModelStopsWhenTheDayIsSpent is the call-boundary half of the same
// bound: a round already in flight stops rather than crossing the ceiling.
func TestCallModelStopsWhenTheDayIsSpent(t *testing.T) {
	calls := 0
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) {
		deps.Budget = refusingBudget{reason: "kuota_exceeded: 5000 dari 5000 micro-rupiah terpakai hari ini"}
		deps.Gateway = capturingGateway{onChat: func(llmdomain.Scope, llmdomain.ChatRequest) (llmdomain.ChatResponse, error) {
			calls++
			return llmdomain.ChatResponse{Text: "seharusnya tidak sampai"}, nil
		}}
	})

	_, err := h.activities.CallModel(context.Background(), workflow.ModelRequest{
		TaskID: h.task.ID.String(), WorkspaceID: h.scope.WorkspaceID.String(),
	})
	require.ErrorContains(t, err, "micro-rupiah")
	require.Zero(t, calls, "the provider is never called once the day is spent")
}
