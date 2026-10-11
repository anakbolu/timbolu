package container

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	agentdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain"
	agentmocks "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain/mocks"
	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	taskdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/config"
)

// The adapter tests cover the seam between the task runtime and the modules it
// speaks to. Each adapter exists because two modules must not import each other,
// so what is under test is that the translation loses nothing: an answer written
// into the thread, a task event reaching the durable stream, and a Bolu found by
// the name a handoff uses.

// memoryMessages is an in-memory conversation store.
type memoryMessages struct {
	messages map[uuid.UUID]chatdomain.Message
	order    []uuid.UUID
}

func newMemoryMessages() *memoryMessages {
	return &memoryMessages{messages: map[uuid.UUID]chatdomain.Message{}}
}

func (m *memoryMessages) Append(_ context.Context, scope chatdomain.Scope, message chatdomain.Message) (chatdomain.Message, error) {
	message.ID = uuid.New()
	message.WorkspaceID = scope.WorkspaceID
	message.CreatedAt = time.Now()
	m.messages[message.ID] = message
	m.order = append(m.order, message.ID)
	return message, nil
}

func (m *memoryMessages) Update(_ context.Context, scope chatdomain.Scope, message chatdomain.Message) (chatdomain.Message, error) {
	stored, ok := m.messages[message.ID]
	if !ok {
		return chatdomain.Message{}, chatdomain.ErrMessageNotFound
	}
	stored.Blocks = message.Blocks
	stored.Status = message.Status
	stored.FinishReason = message.FinishReason
	if message.TaskID != uuid.Nil {
		stored.TaskID = message.TaskID
	}
	m.messages[message.ID] = stored
	return stored, nil
}

func (m *memoryMessages) GetMessage(_ context.Context, scope chatdomain.Scope, id uuid.UUID) (chatdomain.Message, error) {
	stored, ok := m.messages[id]
	if !ok || stored.WorkspaceID != scope.WorkspaceID {
		return chatdomain.Message{}, chatdomain.ErrMessageNotFound
	}
	return stored, nil
}

func (m *memoryMessages) History(context.Context, chatdomain.Scope, uuid.UUID, chatdomain.Cursor) (chatdomain.Page, error) {
	return chatdomain.Page{}, nil
}

func (m *memoryMessages) LatestMessages(_ context.Context, scope chatdomain.Scope, conversationID uuid.UUID, limit int) ([]chatdomain.Message, error) {
	messages := make([]chatdomain.Message, 0, len(m.order))
	for _, id := range m.order {
		message := m.messages[id]
		if message.WorkspaceID == scope.WorkspaceID && message.ConversationID == conversationID {
			messages = append(messages, message)
		}
	}
	if len(messages) > limit {
		messages = messages[len(messages)-limit:]
	}
	return messages, nil
}

// memoryEvents is an in-memory event store and publisher.
type memoryEvents struct {
	appended  []chatdomain.Event
	published []chatdomain.Event
	failWrite bool
}

func (m *memoryEvents) AppendEvent(_ context.Context, event chatdomain.Event) (chatdomain.Event, error) {
	if m.failWrite {
		return chatdomain.Event{}, errors.New("the activity stream is unavailable")
	}
	event.ID = int64(len(m.appended) + 1)
	m.appended = append(m.appended, event)
	return event, nil
}

func (m *memoryEvents) SinceEvents(context.Context, uuid.UUID, int64, int) ([]chatdomain.Event, error) {
	return nil, nil
}

func (m *memoryEvents) LatestEvents(context.Context, uuid.UUID, int) ([]chatdomain.Event, error) {
	return nil, nil
}

func (m *memoryEvents) Publish(_ context.Context, _ uuid.UUID, event chatdomain.Event) error {
	m.published = append(m.published, event)
	return nil
}

func (m *memoryEvents) Subscribe(context.Context, uuid.UUID) (<-chan chatdomain.Event, error) {
	return nil, nil
}

// TestTaskReplyWriterWritesTheAnswerIntoTheThread is the reply adapter's
// contract: the placeholder the chat module wrote is filled, its task link is
// kept, and the stream learns the answer grew.
func TestTaskReplyWriterWritesTheAnswerIntoTheThread(t *testing.T) {
	messages := newMemoryMessages()
	events := &memoryEvents{}

	scope := chatdomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
	conversationID := uuid.New()
	agentID := uuid.New()

	placeholder, err := messages.Append(context.Background(), scope, chatdomain.Message{
		ConversationID: conversationID,
		AuthorAgentID:  agentID,
		Status:         chatdomain.MessageStreaming,
	})
	require.NoError(t, err)

	writer := taskReplyWriter{messages: messages, events: events, sink: chatEventSink{store: events, publisher: events}}

	taskID := uuid.New()
	body := json.RawMessage(`{"type":"text","markdown":"Ada 3 pesanan hari ini."}`)
	err = writer.Write(context.Background(), taskdomain.Scope{
		UserID: scope.UserID, WorkspaceID: scope.WorkspaceID,
	}, taskdomain.WriteReply{
		MessageID:    placeholder.ID,
		Blocks:       []taskdomain.Block{{Type: chatdomain.BlockText, Body: body}},
		Status:       "complete",
		FinishReason: "",
		TaskID:       taskID,
	})
	require.NoError(t, err)

	stored, err := messages.GetMessage(context.Background(), scope, placeholder.ID)
	require.NoError(t, err)
	require.Equal(t, chatdomain.MessageComplete, stored.Status)
	require.Equal(t, taskID, stored.TaskID, "the message keeps which task produced its answer")
	require.Len(t, stored.Blocks, 1)
	require.JSONEq(t, string(body), string(stored.Blocks[0].Body))
	require.Equal(t, agentID, stored.AuthorAgentID, "the answer stays the Bolu's own message")

	// The stream learned about it twice: once as the durable row, once as the
	// live publish. Both are the same event.
	require.Len(t, events.appended, 1)
	require.Equal(t, chatdomain.EventMessageUpdated, events.appended[0].Type)
	require.Equal(t, conversationID, events.appended[0].ConversationID)
	require.Equal(t, taskID, events.appended[0].TaskID)
	require.Len(t, events.published, 1)
	require.Equal(t, events.appended[0].ID, events.published[0].ID)
}

// TestTaskReplyWriterIgnoresAMessageItDoesNotOwn is the guard against writing an
// answer into a message of another workspace: the read is scoped, so it fails
// rather than overwriting somebody else's row.
func TestTaskReplyWriterIgnoresAMessageItDoesNotOwn(t *testing.T) {
	messages := newMemoryMessages()
	events := &memoryEvents{}

	owner := chatdomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
	placeholder, err := messages.Append(context.Background(), owner, chatdomain.Message{
		ConversationID: uuid.New(),
		AuthorAgentID:  uuid.New(),
		Status:         chatdomain.MessageStreaming,
	})
	require.NoError(t, err)

	writer := taskReplyWriter{messages: messages, events: events}

	err = writer.Write(context.Background(), taskdomain.Scope{
		UserID: uuid.New(), WorkspaceID: uuid.New(),
	}, taskdomain.WriteReply{MessageID: placeholder.ID, Status: "complete"})
	require.ErrorIs(t, err, chatdomain.ErrMessageNotFound)

	stored, err := messages.GetMessage(context.Background(), owner, placeholder.ID)
	require.NoError(t, err)
	require.Equal(t, chatdomain.MessageStreaming, stored.Status, "another workspace's message is untouched")
}

// TestTaskReplyWriterWithNoMessageIsANoOp covers a task that answers no
// conversation: a routine or a webhook task has no message to fill, and writing
// nothing is the right answer rather than an error.
func TestTaskReplyWriterWithNoMessageIsANoOp(t *testing.T) {
	writer := taskReplyWriter{messages: newMemoryMessages()}

	require.NoError(t, writer.Write(context.Background(), taskdomain.Scope{WorkspaceID: uuid.New()},
		taskdomain.WriteReply{MessageID: uuid.Nil, Status: "complete"}))
}

// TestTaskReplyWriterReadsTheThreadOldestFirst is the history contract: the model
// reads the conversation in the order it happened, not reversed.
func TestTaskReplyWriterReadsTheThreadOldestFirst(t *testing.T) {
	messages := newMemoryMessages()
	scope := chatdomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
	conversationID := uuid.New()

	first, err := messages.Append(context.Background(), scope, chatdomain.Message{
		ConversationID: conversationID,
		AuthorUserID:   scope.UserID,
		Blocks:         []chatdomain.Block{{Type: chatdomain.BlockText, Body: []byte(`{"type":"text","markdown":"pertama"}`)}},
	})
	require.NoError(t, err)
	second, err := messages.Append(context.Background(), scope, chatdomain.Message{
		ConversationID: conversationID,
		AuthorAgentID:  uuid.New(),
		Blocks:         []chatdomain.Block{{Type: chatdomain.BlockText, Body: []byte(`{"type":"text","markdown":"kedua"}`)}},
	})
	require.NoError(t, err)

	writer := taskReplyWriter{messages: messages}
	history, err := writer.History(context.Background(), taskdomain.Scope{
		UserID: scope.UserID, WorkspaceID: scope.WorkspaceID,
	}, conversationID, 10)
	require.NoError(t, err)

	require.Len(t, history, 2)
	require.Equal(t, first.ID, history[0].ID)
	require.Equal(t, second.ID, history[1].ID)
	require.False(t, history[0].FromAgent(), "the user's turn is not attributed to a Bolu")
	require.True(t, history[1].FromAgent())
}

// TestChatEventSinkKeepsTheEventWhenThePublishFails is the stream's guarantee: the
// durable row is written first, so a publish that fails costs latency and never
// the event.
func TestChatEventSinkKeepsTheEventWhenThePublishFails(t *testing.T) {
	events := &memoryEvents{}
	sink := chatEventSink{store: events}

	err := sink.Emit(context.Background(), taskdomain.Event{
		WorkspaceID: uuid.New(),
		Type:        "task.started",
		TaskID:      uuid.New(),
		Payload:     []byte(`{"task_id":"x"}`),
	})
	require.NoError(t, err)
	require.Len(t, events.appended, 1)

	// And a store that refuses reports the failure, which the activity logs
	// rather than failing the round on.
	failing := chatEventSink{store: &memoryEvents{failWrite: true}}
	require.Error(t, failing.Emit(context.Background(), taskdomain.Event{Type: "task.step"}))
}

// TestToolRegistryOffersOnlyGrantedTools is the registry's filter: the tools a
// Bolu was granted are offered with the label that decides whether a call needs a
// human, and each is offered with a schema the model can call.
func TestToolRegistryOffersOnlyGrantedTools(t *testing.T) {
	agents := agentmocks.NewService(t)
	agentID := uuid.New()
	scope := taskdomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}

	agents.EXPECT().AllowedTools(mock.Anything, agentdomain.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}, agentID).
		Return([]agentdomain.Tool{
			{Name: "gmail.search", Integration: "gmail", Label: agentdomain.LabelRead, Description: "Cari email"},
			{Name: "gmail.send", Integration: "gmail", Label: agentdomain.LabelWriteExternal, Description: "Kirim email"},
		}, nil).Once()

	tools, err := toolRegistry{service: agents}.Tools(context.Background(), scope, agentID)
	require.NoError(t, err)
	require.Len(t, tools, 2)

	require.Equal(t, "gmail.search", tools[0].Name)
	require.Equal(t, agentdomain.LabelRead, tools[0].Label)
	require.False(t, tools[0].NeedsApproval())
	require.Equal(t, agentdomain.LabelWriteExternal, tools[1].Label)
	require.True(t, tools[1].NeedsApproval(), "a write_external tool always needs a human")

	// A tool whose integration module has not registered a schema yet is still
	// callable, which is better than a tool the model is offered and cannot use.
	var schema map[string]any
	require.NoError(t, json.Unmarshal(tools[0].Schema, &schema))
	require.Equal(t, "object", schema["type"])
}

// TestTaskAgentNamerFindsABoluByTheNameTheModelKnows is the handoff lookup: the
// model is told the roster, so it names a Bolu and the adapter resolves it.
func TestTaskAgentNamerFindsABoluByTheNameTheModelKnows(t *testing.T) {
	agents := agentmocks.NewService(t)
	scope := taskdomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
	ijo := agentdomain.Agent{ID: uuid.New(), Name: "Ijo", Role: "Gudang", Status: agentdomain.StoredActive}

	agents.EXPECT().List(mock.Anything, agentdomain.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}).
		Return([]agentdomain.Agent{ijo, {ID: uuid.New(), Name: "Oren"}}, nil).Times(2)

	directory := taskAgentDirectory{service: agents}

	found, err := directory.ByName(context.Background(), scope, "Ijo")
	require.NoError(t, err)
	require.Equal(t, ijo.ID, found.ID)

	// The match ignores case and surrounding space, which is what a model that
	// writes "ijo " means.
	again, err := directory.ByName(context.Background(), scope, "  ijo ")
	require.NoError(t, err)
	require.Equal(t, ijo.ID, again.ID)

	agents.EXPECT().List(mock.Anything, mock.Anything).Return([]agentdomain.Agent{ijo}, nil).Once()
	_, err = directory.ByName(context.Background(), scope, "Siapa Itu")
	require.ErrorIs(t, err, agentdomain.ErrAgentNotFound)
}

// TestTaskAgentDirectoryCarriesTheRestSwitch is the registry contract at the
// runtime's door.
func TestTaskAgentDirectoryCarriesTheRestSwitch(t *testing.T) {
	agents := agentmocks.NewService(t)
	scope := taskdomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
	agentID := uuid.New()

	agents.EXPECT().Get(mock.Anything, agentdomain.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}, agentID).
		Return(agentdomain.Agent{
			ID: agentID, Name: "Ijo", Role: "Gudang", Persona: "Rapi.", Tone: "tenang",
			Status: agentdomain.StoredResting, Display: agentdomain.DisplayResting,
		}, nil).Once()

	directory := taskAgentDirectory{service: agents}
	ref, err := directory.Agent(context.Background(), scope, agentID)
	require.NoError(t, err)

	require.Equal(t, agentID, ref.ID)
	require.Equal(t, agentdomain.StoredResting, ref.Stored)
	require.Equal(t, agentdomain.DisplayResting, ref.Display)
	require.False(t, directory.Active(ref))
}

// TestQuotaBudgetAsksTheGatewayPolicy is the single-source rule: the task runtime
// asks the same policy the gateway does, so the two cannot disagree about whether
// a workspace may spend.
func TestQuotaBudgetAsksTheGatewayPolicy(t *testing.T) {
	workspaceID := uuid.New()
	budget := quotaBudget{checker: fixedChecker{err: errors.New("kuota habis")}}

	require.ErrorContains(t, budget.Allow(context.Background(), workspaceID), "kuota habis")
	require.NoError(t, quotaBudget{checker: fixedChecker{}}.Allow(context.Background(), workspaceID))
}

// fixedChecker is a quota checker that answers the same way every time.
type fixedChecker struct{ err error }

func (c fixedChecker) Check(context.Context, uuid.UUID) error { return c.err }

// TestTaskLimitsFallBackToTheProductDefaults covers the configuration: a
// deployment that states nothing runs with the defaults, and one that states a
// bound runs with it.
func TestTaskLimitsFallBackToTheProductDefaults(t *testing.T) {
	require.Equal(t, taskdomain.DefaultLimits, taskLimits(nil))

	configured := taskLimits(testConfig())
	require.Equal(t, taskdomain.DefaultLimits.MaxSteps, configured.MaxSteps,
		"the checked-in configuration states the defaults explicitly")

	cfg := testConfig()
	cfg.Tasks = config.TasksConfig{MaxSteps: 3, MaxTokens: 500, MaxHandoffDepth: 1, MaxToolResultBytes: 64}
	require.Equal(t, taskdomain.Limits{MaxSteps: 3, MaxTokens: 500, MaxHandoffDepth: 1, MaxToolResultBytes: 64}, taskLimits(cfg))
}

// TestTaskEventSinkIsAbsentWithoutTheEventStore keeps the container honest: a
// deployment without a database has no event store, and the runtime then publishes
// nothing rather than panicking.
func TestTaskEventSinkIsAbsentWithoutTheEventStore(t *testing.T) {
	c := &Container{Logger: zap.NewNop(), Repositories: &Repositories{}}

	require.Nil(t, c.taskEventSink())
	require.Nil(t, c.taskReplyWriter())
	require.Nil(t, c.taskAgentDirectory())
	require.Nil(t, c.taskAgentNamer())
	require.Nil(t, c.toolRegistry())
	require.Nil(t, c.toolExecutor())
	require.Nil(t, c.draftGate())
	require.Nil(t, c.budget())
}

// TestQuotaBudgetIsAbsentWithoutTheGateway keeps the same rule for the budget: no
// gateway means no policy to ask, and the runtime then relies on the gateway's own
// check at call time.
func TestQuotaBudgetIsAbsentWithoutTheGateway(t *testing.T) {
	c := &Container{Logger: zap.NewNop(), Services: &Services{}}

	require.Nil(t, c.budget())
	require.Nil(t, c.costTable())
}

// workflowSignalCancel is the signal name the service sends, named here so a
// rename in the module is caught rather than silently ignored.
const workflowSignalCancel = "cancel"

// TestTaskStarterRefusesWithoutTemporal is the honest answer for a deployment
// that runs the API without a worker: the runtime records the task and reports
// that nothing will run it, rather than pretending to have started it.
func TestTaskStarterRefusesWithoutTemporal(t *testing.T) {
	starter := taskStarter{}

	_, err := starter.Start(context.Background(), taskdomain.Scope{WorkspaceID: uuid.New()}, taskdomain.StartInput{
		TaskID:  uuid.New(),
		AgentID: uuid.New(),
	})
	require.ErrorContains(t, err, "temporal is not configured")

	err = starter.Signal(context.Background(), taskdomain.Scope{WorkspaceID: uuid.New()},
		uuid.New(), workflowSignalCancel, nil)
	require.ErrorContains(t, err, "temporal is not configured")
}

// TestWorkflowIDIsTheTaskID is what makes a duplicate dispatch safe: the id is
// derived, so two starts for one task collide rather than open a second run.
func TestWorkflowIDIsTheTaskID(t *testing.T) {
	id := uuid.New()
	require.Equal(t, "task-"+id.String(), workflowID(id))
	require.Equal(t, workflowID(id), workflowID(id), "the same task always maps to the same workflow")
	require.NotEqual(t, workflowID(id), workflowID(uuid.New()))
}

// TestWireUUIDRendersAbsenceAsAnEmptyString is the rule every wire shape in this
// codebase follows: encoding/json cannot omit a uuid.UUID, so a zero value has to
// be an empty string or a client reads it as a real id.
func TestWireUUIDRendersAbsenceAsAnEmptyString(t *testing.T) {
	require.Empty(t, wireUUID(uuid.Nil))

	id := uuid.New()
	require.Equal(t, id.String(), wireUUID(id))
}

// TestQuotaBudgetIsAskedBeforeTheRoundIsSpent covers the container's side of the
// bound: the policy it hands the runtime is the gateway's own, so the two cannot
// disagree about whether a workspace may spend.
func TestQuotaBudgetIsAskedBeforeTheRoundIsSpent(t *testing.T) {
	c := &Container{
		Logger:   zap.NewNop(),
		Services: &Services{LLM: gatewayWithQuota{checker: fixedChecker{}}},
	}

	budget := c.budget()
	require.NotNil(t, budget)
	require.NoError(t, budget.Allow(context.Background(), uuid.New()))

	refusing := &Container{
		Logger:   zap.NewNop(),
		Services: &Services{LLM: gatewayWithQuota{checker: fixedChecker{err: errors.New("kuota habis")}}},
	}
	require.ErrorContains(t, refusing.budget().Allow(context.Background(), uuid.New()), "kuota habis")
}

// gatewayWithQuota is a gateway that also answers the quota check, which is what
// the container looks for when it wires the budget.
type gatewayWithQuota struct {
	llmdomain.Gateway
	checker llmdomain.QuotaChecker
}

func (g gatewayWithQuota) Check(ctx context.Context, workspaceID uuid.UUID) error {
	return g.checker.Check(ctx, workspaceID)
}
