package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
)

type harness struct {
	store     *memoryStore
	publisher *memoryPublisher
	storage   *memoryStorage
	directory *memoryDirectory
	responder *scriptedResponder
	router    *GroupRouter
	gateway   *scriptedGateway
	service   *Service
	scope     chatdomain.Scope
	agent     chatdomain.AgentRef
}

// newHarness builds a service over in-memory ports, with one Bolu in one
// workspace.
func newHarness(t *testing.T, mutate ...func(*Deps)) *harness {
	t.Helper()

	agent := chatdomain.AgentRef{
		ID:      uuid.New(),
		Name:    "Oren",
		Role:    "Penjualan",
		Persona: "Menangani pesanan dan menagih pelanggan dengan ramah.",
		Tone:    "hangat",
		Stored:  "active",
	}

	store := newMemoryStore()
	publisher := newMemoryPublisher()
	storage := newMemoryStorage()
	directory := &memoryDirectory{agents: map[uuid.UUID]chatdomain.AgentRef{agent.ID: agent}}
	responder := &scriptedResponder{}
	gateway := &scriptedGateway{}

	deps := Deps{
		Conversations: store,
		Messages:      store,
		Attachments:   store,
		Events:        store,
		Publisher:     publisher,
		Agents:        directory,
		Responder:     responder,
		Storage:       storage,
		Router: NewGroupRouter(RouterDeps{
			Gateway: gateway,
			Agents:  directory,
		}),
	}
	for _, change := range mutate {
		change(&deps)
	}

	service := New(deps)

	return &harness{
		store:     store,
		publisher: publisher,
		storage:   storage,
		directory: directory,
		responder: responder,
		router:    deps.Router.(*GroupRouter),
		gateway:   gateway,
		service:   service,
		scope:     chatdomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()},
		agent:     agent,
	}
}

// directConversation opens the 1:1 thread with the harness Bolu.
func (h *harness) directConversation(t *testing.T) chatdomain.Conversation {
	t.Helper()

	conversation, err := h.service.Direct(context.Background(), h.scope, h.agent.ID)
	require.NoError(t, err)
	return conversation
}

// waitForMessage waits until the stored message satisfies the predicate, which
// is how a test observes a background reply.
func (h *harness) waitForMessage(t *testing.T, id uuid.UUID, done func(chatdomain.Message) bool) chatdomain.Message {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	var last chatdomain.Message

	for time.Now().Before(deadline) {
		stored, err := h.store.GetMessage(context.Background(), h.scope, id)
		if err == nil {
			last = stored
			if done(stored) {
				return stored
			}
		}
		time.Sleep(5 * time.Millisecond)
	}

	require.Failf(t, "the reply never reached the expected state", "last state: status=%q reason=%q blocks=%d",
		last.Status, last.FinishReason, len(last.Blocks))
	return last
}

// waitForEvent waits until a published event satisfies the predicate, which is
// how a test observes what a client was told rather than what was stored.
func (h *harness) waitForEvent(t *testing.T, eventType string, done func(chatdomain.Event) bool) chatdomain.Event {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, event := range h.store.eventsOfType(eventType) {
			if done(event) {
				return event
			}
		}
		time.Sleep(5 * time.Millisecond)
	}

	require.Failf(t, "the event never arrived", "type: %s", eventType)
	return chatdomain.Event{}
}

func settled(message chatdomain.Message) bool {
	return message.Status != chatdomain.MessageStreaming
}

// TestSendStoresTheMessageAndStartsTheReply is the E5.2 gate: the user message
// and the answer are both stored, with the author marked correctly.
func TestSendStoresTheMessageAndStartsTheReply(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	h.responder.chunks = []chatdomain.StreamChunk{
		{Kind: chatdomain.ChunkText, Text: "Hari ini ada "},
		{Kind: chatdomain.ChunkText, Text: "4 pesanan."},
		{Kind: chatdomain.ChunkDone},
	}

	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "Berapa pesanan hari ini?",
		Reply:          true,
	})
	require.NoError(t, err)
	require.Equal(t, h.scope.UserID, result.Message.AuthorUserID, "the user wrote the message")
	require.Equal(t, h.agent.ID, result.Responder.ID)
	require.False(t, result.Routed, "a direct thread does not need the router")
	require.NotEqual(t, uuid.Nil, result.ReplyMessageID)

	reply := h.waitForMessage(t, result.ReplyMessageID, settled)
	require.Equal(t, chatdomain.MessageComplete, reply.Status)
	require.Equal(t, h.agent.ID, reply.AuthorAgentID, "the Bolu wrote the reply")
	require.Len(t, reply.Blocks, 1)
	require.JSONEq(t, string(jsonTextBlock("Hari ini ada 4 pesanan.")), string(reply.Blocks[0].Body))
}

// TestStreamedReplyIsPublishedAsItGrows is what makes the text appear gradually
// without a database write per token.
func TestStreamedReplyIsPublishedAsItGrows(t *testing.T) {
	h := newHarness(t)
	h.service.SetClock(func() time.Time { return time.Now().Add(time.Hour) })
	conversation := h.directConversation(t)

	h.responder.chunks = []chatdomain.StreamChunk{
		{Kind: chatdomain.ChunkText, Text: "Hari ini "},
		{Kind: chatdomain.ChunkText, Text: "ada 4 pesanan."},
		{Kind: chatdomain.ChunkDone},
	}

	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "Berapa pesanan?",
		Reply:          true,
	})
	require.NoError(t, err)

	h.waitForEvent(t, chatdomain.EventMessageUpdated, func(event chatdomain.Event) bool {
		var payload chatdomain.MessagePayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return false
		}
		return payload.MessageID == result.ReplyMessageID.String() && payload.Status == chatdomain.MessageComplete
	})

	updates := h.store.eventsOfType(chatdomain.EventMessageUpdated)
	require.NotEmpty(t, updates, "the growing reply must be published")

	// The last update carries the finished body, so a client that joins late
	// renders the same text as one that watched it arrive.
	var payload chatdomain.MessagePayload
	require.NoError(t, json.Unmarshal(updates[len(updates)-1].Payload, &payload))
	require.Equal(t, chatdomain.MessageComplete, payload.Status)
	require.Len(t, payload.Blocks, 1)
	require.Contains(t, string(payload.Blocks[0]), "ada 4 pesanan")

	require.NotEmpty(t, h.store.eventsOfType(chatdomain.EventMessageNew), "the user message is announced too")
}

// TestAFailedStreamLeavesAPartialMessage is the rule the task asks for: a stream
// that dies keeps what arrived, marked, rather than losing a paid-for answer.
func TestAFailedStreamLeavesAPartialMessage(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	h.responder.chunks = []chatdomain.StreamChunk{
		{Kind: chatdomain.ChunkText, Text: "Sebentar, aku cek"},
		{Kind: chatdomain.ChunkError, Err: "penyedia putus di tengah jawaban"},
	}

	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "Cek pesanan",
		Reply:          true,
	})
	require.NoError(t, err)

	reply := h.waitForMessage(t, result.ReplyMessageID, settled)
	require.Equal(t, chatdomain.MessagePartial, reply.Status)
	require.Contains(t, reply.FinishReason, "putus")
	require.Len(t, reply.Blocks, 1, "the text that arrived is kept")
	require.Contains(t, string(reply.Blocks[0].Body), "aku cek")
}

// TestAFailureBeforeAnyTextIsMarkedFailed keeps the distinction: nothing arrived
// is a failure, something arrived is a partial answer.
func TestAFailureBeforeAnyTextIsMarkedFailed(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	h.responder.chunks = []chatdomain.StreamChunk{
		{Kind: chatdomain.ChunkError, Err: "penyedia menolak permintaan"},
	}

	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "Halo",
		Reply:          true,
	})
	require.NoError(t, err)

	reply := h.waitForMessage(t, result.ReplyMessageID, settled)
	require.Equal(t, chatdomain.MessageFailed, reply.Status)
	require.Empty(t, reply.Blocks)
}

// TestAReplyThatCannotBeStartedIsMarkedFailed: a provider that is down when the
// answer starts leaves a visible failure rather than a message stuck on
// "sedang mengetik".
func TestAReplyThatCannotBeStartedIsMarkedFailed(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)
	h.responder.err = errors.New("provider is unreachable")

	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "Halo",
		Reply:          true,
	})
	require.NoError(t, err)

	reply := h.waitForMessage(t, result.ReplyMessageID, settled)
	require.Equal(t, chatdomain.MessageFailed, reply.Status)
	require.Contains(t, reply.FinishReason, "unreachable")
}

// TestARestingBoluDoesNotAnswer: the switch is the user's, and a reply would
// contradict it.
func TestARestingBoluDoesNotAnswer(t *testing.T) {
	h := newHarness(t)
	resting := h.agent
	resting.Stored = "resting"
	h.directory.agents[resting.ID] = resting

	conversation := h.directConversation(t)

	_, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "Halo",
		Reply:          true,
	})
	require.ErrorIs(t, err, chatdomain.ErrNoResponder)
	require.Contains(t, err.Error(), "istirahat")
}

// TestSendingWithoutAReplyStoresOnlyTheMessage is how a client posts a note.
func TestSendingWithoutAReplyStoresOnlyTheMessage(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "Catatan: stok kaos tinggal 3.",
		Reply:          false,
	})
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, result.ReplyMessageID)
	require.Nil(t, result.Responder)

	page, err := h.service.History(context.Background(), h.scope, conversation.ID, chatdomain.Cursor{})
	require.NoError(t, err)
	require.Len(t, page.Messages, 1)
}

// TestSendValidatesItsInput keeps an empty message and an over-long one out.
func TestSendValidatesItsInput(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	_, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID, Text: "   ", Reply: true,
	})
	require.ErrorIs(t, err, chatdomain.ErrInvalidInput)

	// An empty message is refused unless it was opened for an attachment, so
	// the plain case keeps its error exactly as before.
	_, err = h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID, Text: "", Reply: false,
	})
	require.ErrorIs(t, err, chatdomain.ErrInvalidInput)

	_, err = h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID, Text: strings.Repeat("a", TextMaxRunes+1), Reply: true,
	})
	require.ErrorIs(t, err, chatdomain.ErrInvalidInput)
	require.Contains(t, err.Error(), "longer than")
}

// TestSendOpensAnEmptyMessageForAnAttachment: an upload may precede the words,
// so a message opened for an attachment is stored with no text and no reply.
func TestSendOpensAnEmptyMessageForAnAttachment(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "",
		Reply:          true,
		ForAttachment:  true,
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, result.Message.ID)
	require.Empty(t, result.Message.Blocks, "the message carries no text")

	// Nothing answers an attachment container: no placeholder, no responder,
	// even though the caller asked for a reply.
	require.Equal(t, uuid.Nil, result.ReplyMessageID)
	require.Nil(t, result.Responder)

	page, err := h.service.History(context.Background(), h.scope, conversation.ID, chatdomain.Cursor{})
	require.NoError(t, err)
	require.Len(t, page.Messages, 1, "no placeholder was written")
	require.Equal(t, result.Message.ID, page.Messages[0].ID)
}

// TestHistoryPagesByCursor is the E5.1 gate: a page boundary is stable while new
// messages arrive, because the cursor names a message rather than an offset.
func TestHistoryPagesByCursor(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	for i := range 25 {
		_, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
			ConversationID: conversation.ID,
			Text:           "pesan " + itoa(i),
			Reply:          false,
		})
		require.NoError(t, err)
	}

	first, err := h.service.History(context.Background(), h.scope, conversation.ID, chatdomain.Cursor{Limit: 10})
	require.NoError(t, err)
	require.Len(t, first.Messages, 10)
	require.True(t, first.HasMore)
	require.Equal(t, "pesan 24", textOf(t, first.Messages[0]), "the newest message comes first")

	second, err := h.service.History(context.Background(), h.scope, conversation.ID, first.NextCursor())
	require.NoError(t, err)
	require.Len(t, second.Messages, 10)
	require.Equal(t, "pesan 14", textOf(t, second.Messages[0]))

	// A message arriving between the two pages must not shift the boundary: the
	// third page still starts where the second ended.
	_, err = h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID, Text: "pesan baru", Reply: false,
	})
	require.NoError(t, err)

	third, err := h.service.History(context.Background(), h.scope, conversation.ID, second.NextCursor())
	require.NoError(t, err)
	require.Len(t, third.Messages, 5)
	require.Equal(t, "pesan 4", textOf(t, third.Messages[0]))
	require.Equal(t, "pesan 0", textOf(t, third.Messages[len(third.Messages)-1]))
	require.False(t, third.HasMore)

	// The message that arrived between the pages is newer than the boundary the
	// client is reading from, so it is neither repeated nor able to shift it.
	for _, message := range append(second.Messages, third.Messages...) {
		require.NotEqual(t, "pesan baru", textOf(t, message))
	}
}

// TestHistoryRejectsAnotherWorkspace: a conversation id from elsewhere answers
// "not found" rather than an empty page.
func TestHistoryRejectsAnotherWorkspace(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	other := chatdomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
	_, err := h.service.History(context.Background(), other, conversation.ID, chatdomain.Cursor{})
	require.ErrorIs(t, err, chatdomain.ErrConversationNotFound)
}

// TestStreamFillsTheGapAfterAReconnect is the E5.7 gate: the backlog and the
// live feed meet without a hole and without a duplicate.
func TestStreamFillsTheGapAfterAReconnect(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	// Three events happen while nobody is listening.
	for i := range 3 {
		_, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
			ConversationID: conversation.ID, Text: "pesan " + itoa(i), Reply: false,
		})
		require.NoError(t, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := h.service.Stream(ctx, h.scope, 0)
	require.NoError(t, err)

	// The backlog arrives first.
	seen := make([]int64, 0, 4)
	for range 3 {
		select {
		case event := <-stream:
			seen = append(seen, event.ID)
		case <-time.After(2 * time.Second):
			require.Fail(t, "the backlog did not arrive")
		}
	}
	require.Equal(t, []int64{1, 2, 3}, seen)

	// Then something new arrives live, and it continues the same sequence.
	_, err = h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID, Text: "pesan 3", Reply: false,
	})
	require.NoError(t, err)

	select {
	case event := <-stream:
		require.Equal(t, int64(4), event.ID, "the live event continues where the backlog stopped")
	case <-time.After(2 * time.Second):
		require.Fail(t, "the live event did not arrive")
	}
}

// TestStreamResumesFromTheLastEventID: a client that names what it saw gets only
// what it missed.
func TestStreamResumesFromTheLastEventID(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	for i := range 4 {
		_, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
			ConversationID: conversation.ID, Text: "pesan " + itoa(i), Reply: false,
		})
		require.NoError(t, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := h.service.Stream(ctx, h.scope, 2)
	require.NoError(t, err)

	var seen []int64
	for range 2 {
		select {
		case event := <-stream:
			seen = append(seen, event.ID)
		case <-time.After(2 * time.Second):
			require.Fail(t, "the replay did not arrive")
		}
	}
	require.Equal(t, []int64{3, 4}, seen)
}

// TestTheRouterPicksTheBoluTheModelNamed is the E5.8 gate.
func TestTheRouterPicksTheBoluTheModelNamed(t *testing.T) {
	h := newHarness(t)

	biru := chatdomain.AgentRef{
		ID: uuid.New(), Name: "Biru", Role: "Operasional",
		Persona: "Mengurus pengiriman, stok, dan resi.", Stored: "active",
	}
	lila := chatdomain.AgentRef{
		ID: uuid.New(), Name: "Lila", Role: "Keuangan",
		Persona: "Mencatat pemasukan dan menyiapkan tagihan.", Stored: "active",
	}
	h.directory.agents[biru.ID] = biru
	h.directory.agents[lila.ID] = lila

	conversation, err := h.service.CreateGroup(context.Background(), h.scope, chatdomain.CreateGroupRequest{
		Title:    "Grup operasional",
		AgentIDs: []uuid.UUID{h.agent.ID, biru.ID, lila.ID},
		UserIDs:  []uuid.UUID{h.scope.UserID},
	})
	require.NoError(t, err)

	h.responder.chunks = []chatdomain.StreamChunk{
		{Kind: chatdomain.ChunkText, Text: "Siap, aku cek resinya."},
		{Kind: chatdomain.ChunkDone},
	}

	// The router asks the model, which answers with one name.
	h.gateway.chatAnswer = "Biru"

	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "Kak, resi pengiriman sudah ada?",
		Reply:          true,
	})
	require.NoError(t, err)
	require.True(t, result.Routed)
	require.Equal(t, biru.ID, result.Responder.ID, "the model named Biru, and Biru answers")

	decisions := h.store.eventsOfType(chatdomain.EventRouterDecision)
	require.Len(t, decisions, 1)

	var payload struct {
		AgentID string `json:"agent_id"`
		Source  string `json:"source"`
		Reason  string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(decisions[0].Payload, &payload))
	require.Equal(t, biru.ID.String(), payload.AgentID)
	require.Equal(t, chatdomain.RouterSourceModel, payload.Source)
	require.NotEmpty(t, payload.Reason)

	// One message, at most one Bolu answering.
	replies := 0
	for _, message := range h.store.messagesOf(conversation.ID) {
		if message.FromAgent() {
			replies++
		}
	}
	require.Equal(t, 1, replies, "exactly one Bolu replies to a group message")
}

// TestTheRouterOnlyConsidersActiveBolu: a resting Bolu is not a candidate.
func TestTheRouterOnlyConsidersActiveBolu(t *testing.T) {
	h := newHarness(t)

	resting := chatdomain.AgentRef{ID: uuid.New(), Name: "Lila", Role: "Keuangan", Stored: "resting"}
	h.directory.agents[resting.ID] = resting

	conversation, err := h.service.CreateGroup(context.Background(), h.scope, chatdomain.CreateGroupRequest{
		Title:    "Grup kecil",
		AgentIDs: []uuid.UUID{resting.ID, h.agent.ID},
		UserIDs:  []uuid.UUID{h.scope.UserID},
	})
	require.NoError(t, err)

	decision, err := h.router.Pick(context.Background(), h.scope, conversation, chatdomain.Message{})
	require.NoError(t, err)
	require.Equal(t, h.agent.ID, decision.AgentID)
	require.Equal(t, 1, decision.Candidates)
}

// TestTheRouterRefusesAGroupNobodyCanAnswer keeps the failure explicit.
func TestTheRouterRefusesAGroupNobodyCanAnswer(t *testing.T) {
	h := newHarness(t)

	resting := chatdomain.AgentRef{ID: uuid.New(), Name: "Lila", Role: "Keuangan", Stored: "resting"}
	h.directory.agents[resting.ID] = resting

	conversation, err := h.service.CreateGroup(context.Background(), h.scope, chatdomain.CreateGroupRequest{
		Title:    "Grup tidur",
		AgentIDs: []uuid.UUID{resting.ID},
		UserIDs:  []uuid.UUID{h.scope.UserID},
	})
	require.NoError(t, err)

	_, err = h.router.Pick(context.Background(), h.scope, conversation, chatdomain.Message{})
	require.ErrorIs(t, err, chatdomain.ErrNoResponder)
}

// TestPinningABoluOverridesTheRouter is what an explicit "@Bolu" does.
func TestPinningABoluOverridesTheRouter(t *testing.T) {
	h := newHarness(t)

	lila := chatdomain.AgentRef{ID: uuid.New(), Name: "Lila", Role: "Keuangan", Stored: "active"}
	h.directory.agents[lila.ID] = lila

	conversation, err := h.service.CreateGroup(context.Background(), h.scope, chatdomain.CreateGroupRequest{
		Title:    "Grup",
		AgentIDs: []uuid.UUID{h.agent.ID, lila.ID},
		UserIDs:  []uuid.UUID{h.scope.UserID},
	})
	require.NoError(t, err)

	h.responder.chunks = []chatdomain.StreamChunk{{Kind: chatdomain.ChunkDone}}

	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "@Lila tolong rekap",
		Reply:          true,
		AgentID:        lila.ID,
	})
	require.NoError(t, err)
	require.False(t, result.Routed, "a pinned Bolu is not a router decision")
	require.Equal(t, lila.ID, result.Responder.ID)
	require.Empty(t, h.store.eventsOfType(chatdomain.EventRouterDecision))
}

// TestPinningABoluOutsideTheGroupIsRefused keeps a group from reaching a Bolu
// that is not in it.
func TestPinningABoluOutsideTheGroupIsRefused(t *testing.T) {
	h := newHarness(t)

	lila := chatdomain.AgentRef{ID: uuid.New(), Name: "Lila", Role: "Keuangan", Stored: "active"}
	h.directory.agents[lila.ID] = lila

	conversation, err := h.service.CreateGroup(context.Background(), h.scope, chatdomain.CreateGroupRequest{
		Title:    "Grup",
		AgentIDs: []uuid.UUID{h.agent.ID},
		UserIDs:  []uuid.UUID{h.scope.UserID},
	})
	require.NoError(t, err)

	_, err = h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "@Lila",
		Reply:          true,
		AgentID:        lila.ID,
	})
	require.ErrorIs(t, err, chatdomain.ErrAgentNotInConversation)
}

// TestCreateGroupValidatesItsShape covers the limits a group enforces.
func TestCreateGroupValidatesItsShape(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.CreateGroup(context.Background(), h.scope, chatdomain.CreateGroupRequest{
		AgentIDs: []uuid.UUID{h.agent.ID},
		UserIDs:  []uuid.UUID{h.scope.UserID},
	})
	require.ErrorIs(t, err, chatdomain.ErrInvalidInput, "a group needs a name")

	_, err = h.service.CreateGroup(context.Background(), h.scope, chatdomain.CreateGroupRequest{
		Title:    "Grup",
		AgentIDs: []uuid.UUID{h.agent.ID},
	})
	require.ErrorIs(t, err, chatdomain.ErrInvalidInput, "a group needs two participants")

	_, err = h.service.CreateGroup(context.Background(), h.scope, chatdomain.CreateGroupRequest{
		Title:    "Grup",
		AgentIDs: []uuid.UUID{h.agent.ID},
		UserIDs:  []uuid.UUID{h.scope.UserID, uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()},
	})
	require.ErrorIs(t, err, chatdomain.ErrInvalidInput)
	require.Contains(t, err.Error(), "at most")
}

// TestAttachmentRoundTrip stores a file and reads it back through storage.
func TestAttachmentRoundTrip(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	sent, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID, Text: "Lampiran", Reply: false,
	})
	require.NoError(t, err)

	attachment, err := h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		MessageID:   sent.Message.ID,
		Filename:    "daftar harga oktober.pdf",
		ContentType: "application/pdf",
	}, []byte("harga"))
	require.NoError(t, err)
	require.Equal(t, int64(5), attachment.ByteSize)

	metadata, content, err := h.service.AttachmentContent(context.Background(), h.scope, attachment.ID)
	require.NoError(t, err)
	require.Equal(t, "daftar harga oktober.pdf", metadata.Filename)
	require.Equal(t, "harga", string(content))
}

// TestAttachmentRejectsAnEmptyFile keeps a zero-byte upload out.
func TestAttachmentRejectsAnEmptyFile(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	sent, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID, Text: "x", Reply: false,
	})
	require.NoError(t, err)

	_, err = h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		MessageID: sent.Message.ID, Filename: "kosong.txt",
	}, nil)
	require.ErrorIs(t, err, chatdomain.ErrInvalidInput)
}

// TestAttachmentNeedsStorage is the fail-visible behaviour of a deployment
// without a bucket.
func TestAttachmentNeedsStorage(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.Storage = nil })
	conversation := h.directConversation(t)

	sent, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID, Text: "x", Reply: false,
	})
	require.NoError(t, err)

	_, err = h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		MessageID: sent.Message.ID, Filename: "a.txt",
	}, []byte("a"))
	require.ErrorIs(t, err, chatdomain.ErrStorageUnavailable)
}

// TestContentIsServedByItsReference is the E5.6 read path: the reference names a
// content object and nothing else.
func TestContentIsServedByItsReference(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	key := chatdomain.ObjectKey(chatdomain.ContentRefPrefix, h.scope.WorkspaceID.String(), uuid.NewString(), "index.html")
	_, err := h.storage.Put(context.Background(), key, "text/html", []byte("<h1>halo</h1>"))
	require.NoError(t, err)

	document, err := h.service.Content(context.Background(), chatdomain.EncodeContentRef(key))
	require.NoError(t, err)
	require.Equal(t, "<h1>halo</h1>", string(document))

	// An attachment is not a content document, so its key is refused.
	attachmentKey := chatdomain.ObjectKey("attachments", h.scope.WorkspaceID.String(), uuid.NewString(), "a.pdf")
	_, err = h.storage.Put(context.Background(), attachmentKey, "application/pdf", []byte("x"))
	require.NoError(t, err)

	_, err = h.service.Content(context.Background(), chatdomain.EncodeContentRef(attachmentKey))
	require.ErrorIs(t, err, chatdomain.ErrInvalidBlock)

	_ = conversation
}

// TestResponderStreamsTextAndBlocks is the E5.4 gate at the responder level: the
// model calls render_chart, the block is validated, and it reaches the client.
func TestResponderStreamsTextAndBlocks(t *testing.T) {
	storage := newMemoryStorage()
	gateway := &scriptedGateway{rounds: [][]llmdomain.StreamEvent{{
		{Type: llmdomain.EventText, Text: "Ini rekap mingguanmu."},
		{
			Type: llmdomain.EventToolCall,
			ToolCall: &llmdomain.ToolCall{
				ID:   "call_1",
				Name: ToolRenderChart,
				Arguments: json.RawMessage(`{
					"title": "Penjualan mingguan",
					"spec": {"kind": "bar", "categories": ["Sen", "Sel"], "series": [{"name": "Kaos", "data": [10, 12]}]}
				}`),
			},
		},
		{Type: llmdomain.EventDone, FinishReason: llmdomain.FinishStop},
	}}}

	responder := NewModelResponder(ResponderDeps{Gateway: gateway, Storage: storage})

	chunks, err := responder.Reply(context.Background(), chatdomain.Scope{WorkspaceID: uuid.New()}, chatdomain.ReplyRequest{
		Agent:        chatdomain.AgentRef{ID: uuid.New(), Name: "Ijo", Role: "Data"},
		RepairBudget: 2,
	})
	require.NoError(t, err)

	var (
		text   strings.Builder
		blocks []json.RawMessage
		done   bool
	)
	for chunk := range chunks {
		switch chunk.Kind {
		case chatdomain.ChunkText:
			text.WriteString(chunk.Text)
		case chatdomain.ChunkBlock:
			blocks = append(blocks, chunk.Block)
		case chatdomain.ChunkDone:
			done = true
		case chatdomain.ChunkError:
			require.Failf(t, "the responder failed", "%s", chunk.Err)
		}
	}

	require.True(t, done)
	require.Equal(t, "Ini rekap mingguanmu.", text.String())
	require.Len(t, blocks, 1)

	var chart struct {
		Type  string `json:"type"`
		Title string `json:"title"`
		Spec  struct {
			Kind string `json:"kind"`
		} `json:"spec"`
	}
	require.NoError(t, json.Unmarshal(blocks[0], &chart))
	require.Equal(t, chatdomain.BlockChart, chart.Type)
	require.Equal(t, "bar", chart.Spec.Kind)
	require.Equal(t, "Penjualan mingguan", chart.Title)

	// One round was enough, so the model was asked once.
	require.Len(t, gateway.recordedCalls(), 1)
}

// TestResponderRepairsAnInvalidBlock is the E5.4 repair loop: the validator's
// message goes back to the model, which corrects the block, and the loop is
// bounded.
func TestResponderRepairsAnInvalidBlock(t *testing.T) {
	storage := newMemoryStorage()

	invalid := `{"kind":"bar","categories":["Sen","Sel"],"series":[{"name":"Kaos","data":[10]}]}`
	valid := `{"kind":"bar","categories":["Sen","Sel"],"series":[{"name":"Kaos","data":[10,12]}]}`

	gateway := &scriptedGateway{rounds: [][]llmdomain.StreamEvent{
		{
			{
				Type: llmdomain.EventToolCall,
				ToolCall: &llmdomain.ToolCall{
					ID: "call_1", Name: ToolRenderChart,
					Arguments: json.RawMessage(`{"spec": ` + invalid + `}`),
				},
			},
			{Type: llmdomain.EventDone, FinishReason: llmdomain.FinishStop},
		},
		{
			{
				Type: llmdomain.EventToolCall,
				ToolCall: &llmdomain.ToolCall{
					ID: "call_2", Name: ToolRenderChart,
					Arguments: json.RawMessage(`{"spec": ` + valid + `}`),
				},
			},
			{Type: llmdomain.EventDone, FinishReason: llmdomain.FinishStop},
		},
	}}

	responder := NewModelResponder(ResponderDeps{Gateway: gateway, Storage: storage})

	chunks, err := responder.Reply(context.Background(), chatdomain.Scope{WorkspaceID: uuid.New()}, chatdomain.ReplyRequest{
		Agent:        chatdomain.AgentRef{ID: uuid.New(), Name: "Ijo", Role: "Data"},
		RepairBudget: 2,
	})
	require.NoError(t, err)

	var (
		blocks  []json.RawMessage
		notices []string
	)
	for chunk := range chunks {
		switch chunk.Kind {
		case chatdomain.ChunkBlock:
			blocks = append(blocks, chunk.Block)
		case chatdomain.ChunkNotice:
			notices = append(notices, chunk.Text)
		case chatdomain.ChunkError:
			require.Failf(t, "the responder failed", "%s", chunk.Err)
		}
	}

	require.Len(t, blocks, 1, "only the repaired block is sent")
	require.Len(t, notices, 1, "the repair is visible to the user")
	require.Contains(t, notices[0], "memperbaiki")
	require.Contains(t, notices[0], "1 dari 2")

	// The second round was told what was wrong, which is what let the model fix
	// the field instead of guessing again.
	calls := gateway.recordedCalls()
	require.Len(t, calls, 2)
	require.Len(t, calls[1].Messages, 2)
	require.Len(t, calls[1].Messages[1].ToolResults, 1)
	require.True(t, calls[1].Messages[1].ToolResults[0].IsError)
	require.Contains(t, calls[1].Messages[1].ToolResults[0].Content, "categories")
}

// TestResponderStopsAfterTheRepairBudget: a block that never validates must not
// spend tokens forever.
func TestResponderStopsAfterTheRepairBudget(t *testing.T) {
	storage := newMemoryStorage()
	invalid := `{"kind":"bar","categories":["Sen"],"series":[{"name":"Kaos","data":[10,12]}]}`

	round := []llmdomain.StreamEvent{
		{
			Type: llmdomain.EventToolCall,
			ToolCall: &llmdomain.ToolCall{
				ID: "call", Name: ToolRenderChart, Arguments: json.RawMessage(`{"spec": ` + invalid + `}`),
			},
		},
		{Type: llmdomain.EventDone, FinishReason: llmdomain.FinishStop},
	}
	gateway := &scriptedGateway{rounds: [][]llmdomain.StreamEvent{round, round, round, round}}

	responder := NewModelResponder(ResponderDeps{Gateway: gateway, Storage: storage})

	chunks, err := responder.Reply(context.Background(), chatdomain.Scope{WorkspaceID: uuid.New()}, chatdomain.ReplyRequest{
		Agent:        chatdomain.AgentRef{ID: uuid.New(), Name: "Ijo", Role: "Data"},
		RepairBudget: 2,
	})
	require.NoError(t, err)

	var (
		blocks  int
		notices []string
	)
	for chunk := range chunks {
		switch chunk.Kind {
		case chatdomain.ChunkBlock:
			blocks++
		case chatdomain.ChunkNotice:
			notices = append(notices, chunk.Text)
		}
	}

	require.Zero(t, blocks, "an invalid block is never sent")
	require.Len(t, notices, 3, "the two repair attempts plus the final notice")
	require.Contains(t, notices[len(notices)-1], "belum valid")
	require.Len(t, gateway.recordedCalls(), 3, "the initial round plus two repairs")
}

// TestResponderStoresSandboxedHTML is the E5.6 write path: the document goes to
// storage and the block carries a reference, not the document.
func TestResponderStoresSandboxedHTML(t *testing.T) {
	storage := newMemoryStorage()
	gateway := &scriptedGateway{rounds: [][]llmdomain.StreamEvent{{
		{
			Type: llmdomain.EventToolCall,
			ToolCall: &llmdomain.ToolCall{
				ID:   "call_1",
				Name: ToolRenderHTML,
				Arguments: json.RawMessage(`{
					"title": "Simulasi diskon",
					"html": "<html><body><button onclick=\"document.title='x'\">Hitung</button><script>document.cookie</script></body></html>"
				}`),
			},
		},
		{Type: llmdomain.EventDone, FinishReason: llmdomain.FinishStop},
	}}}

	responder := NewModelResponder(ResponderDeps{Gateway: gateway, Storage: storage})
	scope := chatdomain.Scope{WorkspaceID: uuid.New()}

	chunks, err := responder.Reply(context.Background(), scope, chatdomain.ReplyRequest{
		Agent:        chatdomain.AgentRef{ID: uuid.New(), Name: "Pinky", Role: "Pemasaran"},
		RepairBudget: 2,
	})
	require.NoError(t, err)

	var blocks []json.RawMessage
	for chunk := range chunks {
		if chunk.Kind == chatdomain.ChunkBlock {
			blocks = append(blocks, chunk.Block)
		}
	}
	require.Len(t, blocks, 1)

	var document struct {
		Type       string `json:"type"`
		ContentRef string `json:"content_ref"`
		ByteSize   int64  `json:"byte_size"`
	}
	require.NoError(t, json.Unmarshal(blocks[0], &document))
	require.Equal(t, chatdomain.BlockHTML, document.Type)
	require.NotContains(t, string(blocks[0]), "<script>", "the document is stored, not inlined")
	require.Positive(t, document.ByteSize)

	// The stored document is exactly what the agent wrote, and it is reachable
	// only through the content prefix.
	key, err := chatdomain.DecodeContentRef(document.ContentRef)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(key, chatdomain.ContentRefPrefix+"/"))
	require.Contains(t, string(storage.objects[key]), "document.cookie")
}

// TestResponderRefusesAnOversizedDocument is the size limit the task asks for.
func TestResponderRefusesAnOversizedDocument(t *testing.T) {
	storage := newMemoryStorage()
	huge := strings.Repeat("x", chatdomain.HTMLMaxBytes+1)

	gateway := &scriptedGateway{rounds: [][]llmdomain.StreamEvent{
		{{
			Type: llmdomain.EventToolCall,
			ToolCall: &llmdomain.ToolCall{
				ID: "call", Name: ToolRenderHTML,
				Arguments: json.RawMessage(`{"html": "` + huge + `"}`),
			},
		}, {Type: llmdomain.EventDone}},
		{{Type: llmdomain.EventDone}},
		{{Type: llmdomain.EventDone}},
	}}

	responder := NewModelResponder(ResponderDeps{Gateway: gateway, Storage: storage})

	chunks, err := responder.Reply(context.Background(), chatdomain.Scope{WorkspaceID: uuid.New()}, chatdomain.ReplyRequest{
		Agent:        chatdomain.AgentRef{ID: uuid.New(), Name: "Pinky"},
		RepairBudget: 1,
	})
	require.NoError(t, err)

	for chunk := range chunks {
		require.NotEqual(t, chatdomain.ChunkBlock, chunk.Kind, "an oversized document is never stored")
	}
}

// TestResponderPromptCarriesThePersonaAndTheGroup keeps the instructions the
// model answers under explicit.
func TestResponderPromptCarriesThePersonaAndTheGroup(t *testing.T) {
	storage := newMemoryStorage()
	gateway := &scriptedGateway{rounds: [][]llmdomain.StreamEvent{{{Type: llmdomain.EventDone}}}}

	responder := NewModelResponder(ResponderDeps{Gateway: gateway, Storage: storage})

	chunks, err := responder.Reply(context.Background(), chatdomain.Scope{WorkspaceID: uuid.New()}, chatdomain.ReplyRequest{
		Conversation: chatdomain.Conversation{
			Kind: chatdomain.KindGroup,
			Participants: []chatdomain.Participant{
				{AgentID: uuid.New(), DisplayName: "Oren", Role: "Penjualan"},
				{AgentID: uuid.New(), DisplayName: "Biru", Role: "Operasional"},
			},
		},
		Agent: chatdomain.AgentRef{
			ID: uuid.New(), Name: "Oren", Role: "Penjualan",
			Persona: "Menangani pesanan dengan ramah.", Tone: "hangat",
		},
		History: []chatdomain.Message{
			{AuthorUserID: uuid.New(), Blocks: blocksOf(t, `{"type":"text","markdown":"Ada pesanan baru?"}`)},
			{AuthorAgentID: uuid.New(), Blocks: blocksOf(t, `{"type":"chart","spec":{"kind":"bar","categories":["Sen"],"series":[{"name":"Kaos","data":[3]}]}}`)},
		},
		RepairBudget: 1,
	})
	require.NoError(t, err)
	for range chunks {
	}

	calls := gateway.recordedCalls()
	require.Len(t, calls, 1)

	system := calls[0].System
	require.Contains(t, system, "Oren")
	require.Contains(t, system, "Penjualan")
	require.Contains(t, system, "Menangani pesanan dengan ramah.")
	require.Contains(t, system, "grup")
	require.Contains(t, system, "Biru")

	// The history reaches the model in order, and a block it cannot read is
	// summarised rather than dropped: a model that forgot it drew a chart would
	// draw it again.
	require.Len(t, calls[0].Messages, 2)
	require.Equal(t, llmdomain.RoleUser, calls[0].Messages[0].Role)
	require.Equal(t, llmdomain.RoleAssistant, calls[0].Messages[1].Role)
	require.Contains(t, calls[0].Messages[1].Text, "grafik bar")

	// The three block tools are offered.
	require.Len(t, calls[0].Tools, 3)
}

func blocksOf(t *testing.T, documents ...string) []chatdomain.Block {
	t.Helper()

	blocks := make([]chatdomain.Block, 0, len(documents))
	for _, document := range documents {
		var header struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal([]byte(document), &header))
		blocks = append(blocks, chatdomain.Block{Type: header.Type, Body: json.RawMessage(document)})
	}
	return blocks
}

// itoa renders a small integer, which keeps a test message readable without
// pulling in strconv for one call.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

func textOf(t *testing.T, message chatdomain.Message) string {
	t.Helper()

	for _, block := range message.Blocks {
		if block.Type != chatdomain.BlockText {
			continue
		}
		var text struct {
			Markdown string `json:"markdown"`
		}
		require.NoError(t, json.Unmarshal(block.Body, &text))
		return text.Markdown
	}
	return ""
}
