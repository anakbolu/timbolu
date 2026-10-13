package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
)

// The E10.4 gate at the chat boundary, and the debounce that makes extraction
// run after a thread stops rather than while it is being written.
//
// The memory double is scripted rather than simulated: what the memory module
// does with the text is its own tests' business. What matters here is that a
// message which asks to remember is answered by the module that stored it, that a
// message which does not goes to the model, and that every message pushes the
// extractor's timer out.

// scriptedMemory records what the chat module asked and answers a script.
type scriptedMemory struct {
	// rememberFn answers the memory question. A nil function means the message is
	// not a memory request, which is what an ordinary message looks like.
	rememberFn func(chatdomain.RememberRequest) (chatdomain.RememberResult, error)
	// rememberErr makes the memory module fail, which is how the degradation path
	// is tested.
	rememberErr error
	// activeErr makes the scheduling call fail.
	activeErr error

	remembered []chatdomain.RememberRequest
	active     []uuid.UUID
}

func (m *scriptedMemory) Remember(_ context.Context, _ chatdomain.Scope, req chatdomain.RememberRequest) (chatdomain.RememberResult, error) {
	if m.rememberErr != nil {
		return chatdomain.RememberResult{}, m.rememberErr
	}
	m.remembered = append(m.remembered, req)
	if m.rememberFn == nil {
		return chatdomain.RememberResult{Handled: false}, nil
	}
	return m.rememberFn(req)
}

func (m *scriptedMemory) ConversationActive(_ context.Context, _ chatdomain.Scope, conversationID, _ uuid.UUID) error {
	m.active = append(m.active, conversationID)
	return m.activeErr
}

// TestAMemoryRequestIsAnsweredByTheModuleThatStoredIt is the E10.4 gate: the
// confirmation names what was stored, and no model call is made for it.
func TestAMemoryRequestIsAnsweredByTheModuleThatStoredIt(t *testing.T) {
	memory := &scriptedMemory{}
	harness := newHarness(t, func(deps *Deps) { deps.Memory = memory })

	memory.rememberFn = func(req chatdomain.RememberRequest) (chatdomain.RememberResult, error) {
		return chatdomain.RememberResult{
			Handled:      true,
			Confirmation: `Siap, aku ingat di memori utama workspace: "jam buka 08.00-21.00"`,
		}, nil
	}

	conversation := harness.directConversation(t)

	result, err := harness.service.Send(context.Background(), harness.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "ingat ya, jam buka 08.00-21.00",
		Reply:          true,
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, result.ReplyMessageID, "the confirmation is the reply the client follows")

	confirmation := harness.waitForMessage(t, result.ReplyMessageID, func(message chatdomain.Message) bool {
		return message.Status == chatdomain.MessageComplete && len(message.Blocks) > 0
	})

	text := ""
	for _, block := range confirmation.Blocks {
		text += string(block.Body)
	}
	require.Contains(t, text, "jam buka 08.00-21.00", "the confirmation names what was stored")
	require.Equal(t, harness.agent.ID, confirmation.AuthorAgentID)

	// The text travels with the message it came from, so the memory can point
	// back at it.
	require.Len(t, memory.remembered, 1)
	require.Equal(t, "ingat ya, jam buka 08.00-21.00", memory.remembered[0].Text)
	require.Equal(t, harness.agent.ID, memory.remembered[0].AgentID)
	require.NotEqual(t, uuid.Nil, memory.remembered[0].MessageID)

	// No model call: the module that stored the row answered, and the streamed
	// reply path (which would have written a placeholder first) never ran.
	require.Empty(t, harness.gateway.recordedCalls())
}

// TestAnOrdinaryMessageGoesToTheTaskRuntime is the other half: a message that is
// not a memory request must not be swallowed by the memory module.
func TestAnOrdinaryMessageGoesToTheTaskRuntime(t *testing.T) {
	memory := &scriptedMemory{}
	starter := &scriptedTaskStarter{}
	harness := newHarness(t, func(deps *Deps) {
		deps.Memory = memory
		deps.Tasks = starter
	})

	conversation := harness.directConversation(t)

	result, err := harness.service.Send(context.Background(), harness.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "berapa ongkir ke Bandung?",
		Reply:          true,
	})
	require.NoError(t, err)

	require.Len(t, memory.remembered, 1, "the module is asked about every message")
	require.Len(t, starter.started, 1, "and a message that is not a request still opens a task")
	require.NotEqual(t, uuid.Nil, result.ReplyMessageID)
}

// TestEveryMessagePushesTheExtractionOut: the debounce is what makes extraction
// run after a thread stops.
func TestEveryMessagePushesTheExtractionOut(t *testing.T) {
	memory := &scriptedMemory{}
	harness := newHarness(t, func(deps *Deps) { deps.Memory = memory })

	conversation := harness.directConversation(t)

	// Three messages from the Bolu, so no reply path is involved.
	for _, text := range []string{"halo", "ongkir Bandung berapa?", "oke terima kasih"} {
		_, err := harness.service.Send(context.Background(), harness.scope, chatdomain.SendRequest{
			ConversationID: conversation.ID,
			Text:           text,
			Reply:          false,
		})
		require.NoError(t, err)
	}

	require.Len(t, memory.active, 3, "each message resets the quiet period")
	for _, id := range memory.active {
		require.Equal(t, conversation.ID, id)
	}
}

// TestAMemoryModuleThatIsDownDoesNotBreakTheMessage: the degradation path. A
// memory module that cannot answer leaves the message to the ordinary reply,
// which is a Bolu that answers like it did before rather than one that fails.
func TestAMemoryModuleThatIsDownDoesNotBreakTheMessage(t *testing.T) {
	memory := &scriptedMemory{rememberErr: errors.New("memory is down")}
	starter := &scriptedTaskStarter{}
	harness := newHarness(t, func(deps *Deps) {
		deps.Memory = memory
		deps.Tasks = starter
	})

	conversation := harness.directConversation(t)

	_, err := harness.service.Send(context.Background(), harness.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "ingat ya, jam buka 08.00-21.00",
		Reply:          true,
	})
	require.NoError(t, err)
	require.Len(t, starter.started, 1, "the message is not lost when the memory module is")
}

// TestSchedulingFailureDoesNotBreakTheMessage: extraction is best-effort, and a
// message must not fail because a schedule could not be started.
func TestSchedulingFailureDoesNotBreakTheMessage(t *testing.T) {
	memory := &scriptedMemory{activeErr: errors.New("temporal is down")}
	harness := newHarness(t, func(deps *Deps) { deps.Memory = memory })

	conversation := harness.directConversation(t)

	_, err := harness.service.Send(context.Background(), harness.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "berapa ongkir ke Bandung?",
		Reply:          false,
	})
	require.NoError(t, err)
	require.Len(t, memory.active, 1)
}

// TestNoMemoryModuleChangesNothing: a deployment without the module behaves
// exactly as it did before, which is what makes it optional.
func TestNoMemoryModuleChangesNothing(t *testing.T) {
	starter := &scriptedTaskStarter{}
	harness := newHarness(t, func(deps *Deps) { deps.Tasks = starter })

	conversation := harness.directConversation(t)

	_, err := harness.service.Send(context.Background(), harness.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "ingat ya, jam buka 08.00-21.00",
		Reply:          true,
	})
	require.NoError(t, err)
	require.Len(t, starter.started, 1)
}

// scriptedTaskStarter records the tasks the chat module opens.
type scriptedTaskStarter struct {
	started []chatdomain.StartRequest
}

func (s *scriptedTaskStarter) Start(_ context.Context, _ chatdomain.Scope, req chatdomain.StartRequest) (uuid.UUID, error) {
	s.started = append(s.started, req)
	return uuid.New(), nil
}
