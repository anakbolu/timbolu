package integration

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	sdkclient "go.temporal.io/sdk/client"
	sdkworker "go.temporal.io/sdk/worker"

	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	taskactivity "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/activity"
	taskdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	taskrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/repository"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/workflow"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/temporal"
)

// This file is the epic's headline gate: a worker is killed in the middle of a
// task, a second worker takes over, and the task finishes without repeating a
// round that was already recorded.
//
// It runs against a real Temporal server and a real Postgres, because that is
// what the claim is about. The model and the tools are stubs: what is under test
// is the durable execution, not the provider.

// TestTaskSurvivesAWorkerRestart kills the worker mid-task and proves the task
// continues on a second worker without re-running a completed round.
func TestTaskSurvivesAWorkerRestart(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	dsn := appDatabase(t)
	temporalHostPort := startTemporal(t, ctx)

	pool := poolFor(t, dsn)
	scope, agentID := taskWorkspace(t, dsn)

	tasks := taskrepo.New(pool)
	task, err := tasks.Create(ctx, scope, taskdomain.NewTask{
		AgentID: agentID,
		Trigger: taskdomain.TriggerChat,
		Title:   "Rekap pesanan hari ini",
	})
	require.NoError(t, err)

	gateway := newBlockingGateway()
	activities := taskactivity.New(taskactivity.Deps{
		Tasks:    tasks,
		Steps:    tasks,
		Agents:   stubAgents{id: agentID},
		Tools:    stubTools{},
		Executor: stubExecutor{},
		Gateway:  gateway,
		Limits:   taskdomain.DefaultLimits,
	})

	queue := "bolu-agent-restart-test"
	client, err := temporal.New(ctx, temporal.Config{
		HostPort:       temporalHostPort,
		Namespace:      "default",
		TaskQueueAgent: queue,
	})
	require.NoError(t, err)
	t.Cleanup(client.Close)

	// The first worker runs the task until the model call of the second round.
	first := startTaskWorker(t, client, queue, activities)

	run, err := client.Client().ExecuteWorkflow(ctx, sdkclient.StartWorkflowOptions{
		ID:        "task-" + task.ID.String(),
		TaskQueue: queue,
	}, workflow.AgentTaskWorkflow, workflow.TaskInput{
		TaskID:      task.ID.String(),
		WorkspaceID: scope.WorkspaceID.String(),
		AgentID:     agentID.String(),
		Title:       task.Title,
		Prompt:      "Rekap pesanan hari ini",
	})
	require.NoError(t, err)

	// Wait until the first round is recorded: the model answered, the tool ran,
	// and both were written. That is the work that must not be repeated.
	require.NoError(t, waitFor(t, 60*time.Second, func() bool {
		steps, err := tasks.ListSteps(ctx, scope, task.ID)
		if err != nil {
			return false
		}
		kinds := map[string]int{}
		for _, step := range steps {
			kinds[step.Kind]++
		}
		return kinds[taskdomain.StepThink] == 1 &&
			kinds[taskdomain.StepToolCall] == 1 &&
			kinds[taskdomain.StepToolResult] == 1
	}), "the first round must be recorded before the worker is killed")

	// The second round's model call is in flight and blocked. Killing the worker
	// is what a deploy, a crash, or an eviction does at exactly this moment.
	require.True(t, gateway.awaitSecondCall(30*time.Second),
		"the second model call must be in flight before the worker is killed")
	first.Stop()

	// A fresh worker picks the task up. The blocked call is retried, and the
	// rounds that already completed are read from history rather than re-run.
	second := startTaskWorker(t, client, queue, activities)
	t.Cleanup(second.Stop)
	gateway.release()

	var outcome workflow.TaskOutcome
	require.NoError(t, run.Get(ctx, &outcome), "the task must finish after the restart")
	require.Equal(t, taskdomain.StatusSucceeded, outcome.Status)

	steps, err := tasks.ListSteps(ctx, scope, task.ID)
	require.NoError(t, err)

	// Two rounds: think, tool_call, tool_result, then think and final. The
	// first round appears exactly once, which is the whole point: the restart
	// did not redo it.
	kinds := map[string]int{}
	seqs := map[int]bool{}
	for _, step := range steps {
		kinds[step.Kind]++
		require.False(t, seqs[step.Seq], "sequence %d was written twice", step.Seq)
		seqs[step.Seq] = true
	}
	require.Equal(t, 2, kinds[taskdomain.StepThink], "one thinking round per model answer, no more")
	require.Equal(t, 1, kinds[taskdomain.StepToolCall])
	require.Equal(t, 1, kinds[taskdomain.StepToolResult])
	require.Equal(t, 1, kinds[taskdomain.StepFinal])
	require.Len(t, steps, 5)

	// The gateway was asked three times: the first round, the call that was
	// killed mid-flight, and its retry. The recorded history proves only two of
	// those became a round.
	require.Equal(t, 3, gateway.calls(), "the in-flight call is retried, the completed one is not")

	stored, err := tasks.Get(ctx, scope, task.ID)
	require.NoError(t, err)
	require.Equal(t, taskdomain.StatusSucceeded, stored.Status)
	require.NotEmpty(t, stored.Summary)
}

// startTemporal runs a Temporal dev server and returns its host:port.
func startTemporal(t *testing.T, ctx context.Context) string {
	t.Helper()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "temporalio/temporal:latest",
			ExposedPorts: []string{"7233/tcp"},
			Cmd:          []string{"server", "start-dev", "--ip", "0.0.0.0", "--port", "7233"},
			WaitingFor:   wait.ForListeningPort("7233/tcp").WithStartupTimeout(3 * time.Minute),
		},
		Started: true,
	})
	require.NoError(t, err, "start temporal dev server")

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cleanupCancel()
		require.NoError(t, container.Terminate(cleanupCtx))
	})

	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "7233")
	require.NoError(t, err)

	return host + ":" + port.Port()
}

// startTaskWorker runs one worker process for the task workflow.
func startTaskWorker(t *testing.T, client *temporal.Client, queue string, activities *taskactivity.Activities) sdkworker.Worker {
	t.Helper()

	w := sdkworker.New(client.Client(), queue, sdkworker.Options{})
	w.RegisterWorkflow(workflow.AgentTaskWorkflow)
	w.RegisterActivity(activities)
	require.NoError(t, w.Start())

	return w
}

// waitFor polls until the condition holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, condition func() bool) error {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("condition was not met before the deadline")
}

// blockingGateway answers the first model call, blocks on the second, and
// answers every call after that.
//
// It is what makes the restart observable: the moment the second call is
// blocked is the moment the worker is killed, and a retry of that call is
// indistinguishable from a first attempt as far as the workflow is concerned.
type blockingGateway struct {
	mu        sync.Mutex
	callCount int
	blocked   chan struct{}
	released  chan struct{}
}

func newBlockingGateway() *blockingGateway {
	return &blockingGateway{
		blocked:  make(chan struct{}),
		released: make(chan struct{}),
	}
}

func (g *blockingGateway) calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.callCount
}

// awaitSecondCall waits until the second call has started and is blocked.
func (g *blockingGateway) awaitSecondCall(timeout time.Duration) bool {
	select {
	case <-g.blocked:
		return true
	case <-time.After(timeout):
		return false
	}
}

// release lets every blocked call finish.
func (g *blockingGateway) release() { close(g.released) }

func (g *blockingGateway) Chat(ctx context.Context, _ llmdomain.Scope, _ llmdomain.ChatRequest) (llmdomain.ChatResponse, error) {
	g.mu.Lock()
	g.callCount++
	call := g.callCount
	g.mu.Unlock()

	if call == 1 {
		return llmdomain.ChatResponse{
			Text: "Saya cek dulu pesanannya.",
			ToolCalls: []llmdomain.ToolCall{{
				ID:        "call-1",
				Name:      "gmail.search",
				Arguments: json.RawMessage(`{"q":"pesanan"}`),
			}},
			Usage:        llmdomain.Usage{InputTokens: 120, OutputTokens: 30},
			Model:        "stub",
			FinishReason: llmdomain.FinishToolCalls,
		}, nil
	}

	if call == 2 {
		// This is the call the worker is killed on.
		close(g.blocked)
		select {
		case <-g.released:
		case <-ctx.Done():
			return llmdomain.ChatResponse{}, ctx.Err()
		}
	}

	return llmdomain.ChatResponse{
		Text:         "Ada 2 pesanan masuk hari ini.",
		Usage:        llmdomain.Usage{InputTokens: 200, OutputTokens: 40},
		Model:        "stub",
		FinishReason: llmdomain.FinishStop,
	}, nil
}

// The rest of the Gateway interface is not exercised by the runtime: the task
// workflow only ever calls Chat. Each method answers "not used" rather than
// pretending to work.

func (g *blockingGateway) Stream(context.Context, llmdomain.Scope, llmdomain.ChatRequest) (<-chan llmdomain.StreamEvent, error) {
	return nil, errors.New("not used")
}

func (g *blockingGateway) Providers(context.Context, llmdomain.Scope) ([]llmdomain.Redacted, error) {
	return nil, errors.New("not used")
}

func (g *blockingGateway) Upsert(context.Context, llmdomain.Scope, llmdomain.UpsertRequest) (llmdomain.Redacted, error) {
	return llmdomain.Redacted{}, errors.New("not used")
}

func (g *blockingGateway) Delete(context.Context, llmdomain.Scope, uuid.UUID) error {
	return errors.New("not used")
}

func (g *blockingGateway) Test(context.Context, llmdomain.Scope, uuid.UUID) (llmdomain.Capabilities, error) {
	return llmdomain.Capabilities{}, errors.New("not used")
}

func (g *blockingGateway) SetAgentModel(context.Context, llmdomain.Scope, llmdomain.AgentOverride) error {
	return errors.New("not used")
}

func (g *blockingGateway) AgentModel(context.Context, llmdomain.Scope) (llmdomain.AgentOverride, error) {
	return llmdomain.AgentOverride{}, errors.New("not used")
}

func (g *blockingGateway) Usage(context.Context, llmdomain.Scope, int) ([]llmdomain.UsageDaily, error) {
	return nil, errors.New("not used")
}

// stubAgents answers the registry questions the runtime asks.
type stubAgents struct{ id uuid.UUID }

func (s stubAgents) Agent(context.Context, taskdomain.Scope, uuid.UUID) (taskdomain.AgentRef, error) {
	return taskdomain.AgentRef{ID: s.id, Name: "Oren", Role: "Penjualan", Stored: "active"}, nil
}

func (s stubAgents) Active(taskdomain.AgentRef) bool { return true }

func (s stubAgents) ByName(_ context.Context, _ taskdomain.Scope, name string) (taskdomain.AgentRef, error) {
	if name == "Oren" {
		return taskdomain.AgentRef{ID: s.id, Name: "Oren", Stored: "active"}, nil
	}
	return taskdomain.AgentRef{}, errors.New("no such Bolu")
}

// stubTools offers one reading tool.
type stubTools struct{}

func (stubTools) Tools(context.Context, taskdomain.Scope, uuid.UUID) ([]taskdomain.Tool, error) {
	return []taskdomain.Tool{{
		Name:        "gmail.search",
		Integration: "gmail",
		Description: "Cari email di kotak masuk",
		Schema:      json.RawMessage(`{"type":"object"}`),
		Label:       taskdomain.LabelRead,
	}}, nil
}

// stubExecutor answers a tool call without a remote service.
type stubExecutor struct{}

func (stubExecutor) Execute(context.Context, taskdomain.Scope, taskdomain.ToolCall) (taskdomain.ToolResult, error) {
	return taskdomain.ToolResult{Content: json.RawMessage(`{"emails":2}`)}, nil
}

// taskWorkspace onboards one workspace and returns the scope and the Bolu the
// task runs as.
func taskWorkspace(t *testing.T, dsn string) (taskdomain.Scope, uuid.UUID) {
	t.Helper()

	fixture := newTaskFixture(t, dsn)
	return fixture.scope, fixture.agentID
}
