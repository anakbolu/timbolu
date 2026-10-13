package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	llmmocks "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain/mocks"
)

// TestConversationReadsAreScoped keeps the two read paths and the scope check
// honest: a missing workspace is refused before the repository is reached.
func TestConversationReadsAreScoped(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	list, err := h.service.Conversations(context.Background(), h.scope)
	require.NoError(t, err)
	require.Len(t, list, 1)

	one, err := h.service.Conversation(context.Background(), h.scope, conversation.ID)
	require.NoError(t, err)
	require.Equal(t, conversation.ID, one.ID)

	// No workspace, no answer.
	_, err = h.service.Conversations(context.Background(), chatdomain.Scope{})
	require.ErrorIs(t, err, chatdomain.ErrInvalidInput)

	_, err = h.service.Conversation(context.Background(), chatdomain.Scope{}, conversation.ID)
	require.ErrorIs(t, err, chatdomain.ErrInvalidInput)

	// No id, no answer either.
	_, err = h.service.Conversation(context.Background(), h.scope, uuid.Nil)
	require.ErrorIs(t, err, chatdomain.ErrInvalidInput)
}

// TestDirectNeedsAKnownBolu keeps a conversation with another workspace's Bolu
// from being creatable.
func TestDirectNeedsAKnownBolu(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.Direct(context.Background(), h.scope, uuid.Nil)
	require.ErrorIs(t, err, chatdomain.ErrInvalidInput)

	_, err = h.service.Direct(context.Background(), h.scope, uuid.New())
	require.ErrorIs(t, err, chatdomain.ErrAgentNotInConversation)
}

// TestAddParticipantsGuardsTheShape covers the rules a group enforces on the
// add path, which are the same ones it enforces on creation.
func TestAddParticipantsGuardsTheShape(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	// A 1:1 thread does not take new participants.
	_, err := h.service.AddParticipants(context.Background(), h.scope, conversation.ID,
		chatdomain.AddParticipantsRequest{AgentIDs: []uuid.UUID{uuid.New()}})
	require.ErrorIs(t, err, chatdomain.ErrInvalidInput)
	require.Contains(t, err.Error(), "group")

	group, err := h.service.CreateGroup(context.Background(), h.scope, chatdomain.CreateGroupRequest{
		Title:    "Grup",
		AgentIDs: []uuid.UUID{h.agent.ID},
		UserIDs:  []uuid.UUID{h.scope.UserID},
	})
	require.NoError(t, err)

	// Nothing named is a mistake rather than a no-op.
	_, err = h.service.AddParticipants(context.Background(), h.scope, group.ID, chatdomain.AddParticipantsRequest{})
	require.ErrorIs(t, err, chatdomain.ErrInvalidInput)

	// A Bolu that is not in this workspace is refused.
	_, err = h.service.AddParticipants(context.Background(), h.scope, group.ID,
		chatdomain.AddParticipantsRequest{AgentIDs: []uuid.UUID{uuid.New()}})
	require.ErrorIs(t, err, chatdomain.ErrAgentNotInConversation)
}

// TestEventsReplayIsBounded covers the replay read and its page bound.
func TestEventsReplayIsBounded(t *testing.T) {
	h := newHarness(t)

	// Nothing has happened yet.
	events, err := h.service.Events(context.Background(), h.scope, 0, 0)
	require.NoError(t, err)
	require.Empty(t, events)

	_, err = h.service.Events(context.Background(), chatdomain.Scope{}, 0, 0)
	require.ErrorIs(t, err, chatdomain.ErrInvalidInput)

	conversation := h.directConversation(t)
	_, err = h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID, Text: "halo", Reply: false,
	})
	require.NoError(t, err)

	// A limit above the maximum is clamped rather than refused: a client asking
	// for too much gets a page, not an error.
	events, err = h.service.Events(context.Background(), h.scope, 0, EventPageMax*10)
	require.NoError(t, err)
	require.NotEmpty(t, events)

	replay, err := h.service.Events(context.Background(), h.scope, events[len(events)-1].ID, 10)
	require.NoError(t, err)
	require.Empty(t, replay, "nothing happened after the last event")
}

// TestAttachmentMetadataIsReadable covers the metadata read on its own, which is
// what a client uses to render a link before fetching the bytes.
func TestAttachmentMetadataIsReadable(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	sent, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID, Text: "x", Reply: false,
	})
	require.NoError(t, err)

	attachment, err := h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		MessageID: sent.Message.ID, Filename: "a.txt", ContentType: "text/plain",
	}, []byte("a"))
	require.NoError(t, err)

	metadata, err := h.service.Attachment(context.Background(), h.scope, attachment.ID)
	require.NoError(t, err)
	require.Equal(t, attachment.ID, metadata.ID)

	_, err = h.service.Attachment(context.Background(), h.scope, uuid.New())
	require.ErrorIs(t, err, chatdomain.ErrAttachmentNotFound)
}

// TestNoticesAndBlocksReachTheClient is the responder path a repair produces: a
// notice is shown as text so a client that renders only text still says what is
// happening, and a block ends the text that came before it.
func TestNoticesAndBlocksReachTheClient(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	chart := `{"type":"chart","spec":{"kind":"pie","slices":[{"name":"Kaos","value":10}]}}`
	h.responder.chunks = []chatdomain.StreamChunk{
		{Kind: chatdomain.ChunkText, Text: "Ini rekapnya:"},
		{Kind: chatdomain.ChunkNotice, Text: "memperbaiki render_chart (1 dari 2)"},
		{Kind: chatdomain.ChunkBlock, Block: json.RawMessage(chart)},
		{Kind: chatdomain.ChunkText, Text: "Segitu dulu."},
		{Kind: chatdomain.ChunkDone},
	}

	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID, Text: "Rekap dong", Reply: true,
	})
	require.NoError(t, err)

	reply := h.waitForMessage(t, result.ReplyMessageID, settled)
	require.Equal(t, chatdomain.MessageComplete, reply.Status)
	require.Len(t, reply.Blocks, 3, "text, chart, then the text after it")

	require.Equal(t, chatdomain.BlockText, reply.Blocks[0].Type)
	require.Contains(t, string(reply.Blocks[0].Body), "Ini rekapnya")
	require.Contains(t, string(reply.Blocks[0].Body), "memperbaiki render_chart", "the notice is visible")

	require.Equal(t, chatdomain.BlockChart, reply.Blocks[1].Type)

	require.Equal(t, chatdomain.BlockText, reply.Blocks[2].Type)
	require.Contains(t, string(reply.Blocks[2].Body), "Segitu dulu")

	// The order the model produced is the order the user reads.
	types := []string{reply.Blocks[0].Type, reply.Blocks[1].Type, reply.Blocks[2].Type}
	require.Equal(t, []string{chatdomain.BlockText, chatdomain.BlockChart, chatdomain.BlockText}, types)
}

// TestAnUnreadableBlockChunkIsSkipped keeps one malformed block from ending an
// answer that is otherwise fine.
func TestAnUnreadableBlockChunkIsSkipped(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	h.responder.chunks = []chatdomain.StreamChunk{
		{Kind: chatdomain.ChunkText, Text: "Ini jawabannya."},
		{Kind: chatdomain.ChunkBlock, Block: json.RawMessage(`{bukan json}`)},
		{Kind: chatdomain.ChunkDone},
	}

	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID, Text: "halo", Reply: true,
	})
	require.NoError(t, err)

	reply := h.waitForMessage(t, result.ReplyMessageID, settled)
	require.Equal(t, chatdomain.MessageComplete, reply.Status)
	require.Len(t, reply.Blocks, 1)
	require.Contains(t, string(reply.Blocks[0].Body), "Ini jawabannya")
}

// TestHistoryWithoutAPageLimitUsesTheDefault covers the page size a client gets
// when it does not ask for one.
func TestHistoryWithoutAPageLimitUsesTheDefault(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	for i := range 5 {
		_, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
			ConversationID: conversation.ID, Text: "pesan " + itoa(i), Reply: false,
		})
		require.NoError(t, err)
	}

	page, err := h.service.History(context.Background(), h.scope, conversation.ID, chatdomain.Cursor{})
	require.NoError(t, err)
	require.Len(t, page.Messages, 5)
	require.False(t, page.HasMore)

	// The cursor a page hands back reads the page after it, which for the last
	// page is nothing.
	next, err := h.service.History(context.Background(), h.scope, conversation.ID, page.NextCursor())
	require.NoError(t, err)
	require.Empty(t, next.Messages)
}

// TestTheStreamNeedsAPublisher keeps a deployment without Redis from pretending
// the live path works.
func TestTheStreamNeedsAPublisher(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.Publisher = nil })

	_, err := h.service.Stream(context.Background(), h.scope, 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "live stream")
}

// TestEmitNeedsAWorkspace keeps an event from being written nowhere.
func TestEmitNeedsAWorkspace(t *testing.T) {
	h := newHarness(t)

	err := h.service.Emit(context.Background(), chatdomain.Event{Type: chatdomain.EventMessageNew})
	require.ErrorIs(t, err, chatdomain.ErrInvalidInput)
}

// TestEmitSurvivesAFailedPublish is the ordering rule: the row is the record, so
// a publish that fails is a warning and never a lost event.
func TestEmitSurvivesAFailedPublish(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.Publisher = failingPublisher{} })

	require.NoError(t, h.service.Emit(context.Background(), chatdomain.Event{
		WorkspaceID: h.scope.WorkspaceID,
		Type:        chatdomain.EventAgentState,
	}))

	require.Len(t, h.store.eventsOfType(chatdomain.EventAgentState), 1, "the row is written even so")
}

// TestEmitWithoutAPublisherIsFine covers a deployment that stores events and
// serves them by replay only.
func TestEmitWithoutAPublisherIsFine(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.Publisher = nil })

	require.NoError(t, h.service.Emit(context.Background(), chatdomain.Event{
		WorkspaceID: h.scope.WorkspaceID,
		Type:        chatdomain.EventAgentState,
	}))
	require.Len(t, h.store.eventsOfType(chatdomain.EventAgentState), 1)
}

// TestAgentStateEventsArePublished is what the dashboard reads: a Bolu's state
// is an event, and no coordinate is stored anywhere.
func TestAgentStateEventsArePublished(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	h.responder.chunks = []chatdomain.StreamChunk{
		{Kind: chatdomain.ChunkText, Text: "siap"},
		{Kind: chatdomain.ChunkDone},
	}

	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID, Text: "halo", Reply: true,
	})
	require.NoError(t, err)
	h.waitForMessage(t, result.ReplyMessageID, settled)

	states := h.store.eventsOfType(chatdomain.EventAgentState)
	require.NotEmpty(t, states, "the dashboard is built from these")

	seen := map[string]bool{}
	for _, event := range states {
		var payload chatdomain.AgentStatePayload
		require.NoError(t, json.Unmarshal(event.Payload, &payload))
		require.Equal(t, h.agent.ID.String(), payload.AgentID)
		require.NotEmpty(t, payload.State)
		require.NotContains(t, string(event.Payload), "x\":", "a coordinate must never be published")
		require.NotContains(t, string(event.Payload), "00000000-0000-0000-0000-000000000000",
			"absence travels as an empty string, never as a zero id")
		seen[payload.State] = true
	}

	require.True(t, seen[chatdomain.StateThinking], "the Bolu is thinking while the answer is produced")
	require.True(t, seen[chatdomain.StateIdle], "and idle once it is done")
}

// TestAReplyThatCannotBeStoredIsReported: a reply the database refuses must not
// look like a reply that was stored.
func TestAReplyThatCannotBeStoredIsReported(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)
	h.store.failUpdate = true

	h.responder.chunks = []chatdomain.StreamChunk{
		{Kind: chatdomain.ChunkText, Text: "siap"},
		{Kind: chatdomain.ChunkDone},
	}

	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID, Text: "halo", Reply: true,
	})
	require.NoError(t, err)

	// The placeholder stays in "streaming": the write failed, so the answer was
	// never recorded, and claiming otherwise would be a lie.
	stored, err := h.store.GetMessage(context.Background(), h.scope, result.ReplyMessageID)
	require.NoError(t, err)
	require.Equal(t, chatdomain.MessageStreaming, stored.Status)
}

// failingPublisher stands in for a Redis outage.
type failingPublisher struct{}

func (failingPublisher) Publish(context.Context, uuid.UUID, chatdomain.Event) error {
	return errors.New("pubsub: redis is down")
}

func (failingPublisher) Subscribe(context.Context, uuid.UUID) (<-chan chatdomain.Event, error) {
	return nil, errors.New("pubsub: redis is down")
}

// TestTheGroupRouterAsksTheModel covers the routing call itself: the model is
// asked once, with the roster and the message, and its answer picks the Bolu.
func TestTheGroupRouterAsksTheModel(t *testing.T) {
	gateway := llmmocks.NewGateway(t)
	directory := &memoryDirectory{agents: map[uuid.UUID]chatdomain.AgentRef{}}

	oren := chatdomain.AgentRef{ID: uuid.New(), Name: "Oren", Role: "Penjualan", Persona: "Pesanan dan tagihan.", Stored: "active"}
	biru := chatdomain.AgentRef{ID: uuid.New(), Name: "Biru", Role: "Operasional", Persona: "Pengiriman dan stok.", Stored: "active"}
	directory.agents[oren.ID] = oren
	directory.agents[biru.ID] = biru

	gateway.EXPECT().Chat(mock.Anything, mock.Anything, mock.MatchedBy(func(req llmdomain.ChatRequest) bool {
		// A routing call is its own purpose, so its cost is attributable.
		return req.Metadata.Purpose == llmdomain.PurposeRouting &&
			strings.Contains(req.Messages[0].Text, "Biru") &&
			strings.Contains(req.Messages[0].Text, "resi pengiriman") &&
			req.MaxTokens > 0 && req.MaxTokens <= 32
	})).Return(llmdomain.ChatResponse{Text: "Biru"}, nil).Once()

	router := NewGroupRouter(RouterDeps{Gateway: gateway, Agents: directory})

	conversation := chatdomain.Conversation{
		Kind: chatdomain.KindGroup,
		Participants: []chatdomain.Participant{
			{AgentID: oren.ID}, {AgentID: biru.ID},
		},
	}
	message := chatdomain.Message{Blocks: []chatdomain.Block{{
		Type: chatdomain.BlockText,
		Body: json.RawMessage(`{"type":"text","markdown":"Kak, resi pengiriman sudah ada?"}`),
	}}}

	decision, err := router.Pick(context.Background(), chatdomain.Scope{WorkspaceID: uuid.New()}, conversation, message)
	require.NoError(t, err)
	require.Equal(t, biru.ID, decision.AgentID)
	require.Equal(t, chatdomain.RouterSourceModel, decision.Source)
	require.Equal(t, 2, decision.Candidates)
	require.NotEmpty(t, decision.Reason)
}

// TestTheRouterFallsBackWhenTheModelNamesNobody keeps a vague answer from
// failing the message.
func TestTheRouterFallsBackWhenTheModelNamesNobody(t *testing.T) {
	gateway := llmmocks.NewGateway(t)
	directory := &memoryDirectory{agents: map[uuid.UUID]chatdomain.AgentRef{}}

	oren := chatdomain.AgentRef{ID: uuid.New(), Name: "Oren", Role: "Penjualan", Persona: "Pesanan.", Stored: "active"}
	biru := chatdomain.AgentRef{ID: uuid.New(), Name: "Biru", Role: "Operasional", Persona: "Pengiriman.", Stored: "active"}
	directory.agents[oren.ID] = oren
	directory.agents[biru.ID] = biru

	gateway.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything).
		Return(llmdomain.ChatResponse{Text: "Sepertinya salah satu dari mereka bisa."}, nil).Once()

	router := NewGroupRouter(RouterDeps{Gateway: gateway, Agents: directory})

	conversation := chatdomain.Conversation{
		Kind:         chatdomain.KindGroup,
		Participants: []chatdomain.Participant{{AgentID: oren.ID}, {AgentID: biru.ID}},
	}
	message := chatdomain.Message{Blocks: []chatdomain.Block{{
		Type: chatdomain.BlockText,
		Body: json.RawMessage(`{"type":"text","markdown":"Tolong urus pengiriman ini"}`),
	}}}

	decision, err := router.Pick(context.Background(), chatdomain.Scope{WorkspaceID: uuid.New()}, conversation, message)
	require.NoError(t, err)
	require.Equal(t, chatdomain.RouterSourceFallback, decision.Source)
	require.Contains(t, []uuid.UUID{oren.ID, biru.ID}, decision.AgentID)
}

// TestTheRouterFallsBackWhenTheModelIsDown is the case that matters during an
// outage: the group still gets an answer.
func TestTheRouterFallsBackWhenTheModelIsDown(t *testing.T) {
	gateway := llmmocks.NewGateway(t)
	directory := &memoryDirectory{agents: map[uuid.UUID]chatdomain.AgentRef{}}

	oren := chatdomain.AgentRef{
		ID: uuid.New(), Name: "Oren", Role: "Penjualan",
		Persona: "Menangani pesanan dan menagih pelanggan.", Stored: "active",
	}
	biru := chatdomain.AgentRef{
		ID: uuid.New(), Name: "Biru", Role: "Operasional",
		Persona: "Mengurus pengiriman, stok, dan ongkos kirim.", Stored: "active",
	}
	directory.agents[oren.ID] = oren
	directory.agents[biru.ID] = biru

	gateway.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything).
		Return(llmdomain.ChatResponse{}, llmdomain.ErrProviderUnavailable).Once()

	router := NewGroupRouter(RouterDeps{Gateway: gateway, Agents: directory})

	conversation := chatdomain.Conversation{
		Kind:         chatdomain.KindGroup,
		Participants: []chatdomain.Participant{{AgentID: oren.ID}, {AgentID: biru.ID}},
	}
	message := chatdomain.Message{Blocks: []chatdomain.Block{{
		Type: chatdomain.BlockText,
		Body: json.RawMessage(`{"type":"text","markdown":"Kapan pengiriman barangnya?"}`),
	}}}

	decision, err := router.Pick(context.Background(), chatdomain.Scope{WorkspaceID: uuid.New()}, conversation, message)
	require.NoError(t, err)
	require.Equal(t, chatdomain.RouterSourceFallback, decision.Source)
	require.Equal(t, biru.ID, decision.AgentID, "the operational Bolu scores highest on a shipping question")
}

// TestTheRouterWithoutAGatewayStillPicks covers a deployment that stores and
// serves conversations but has no model configured for routing.
func TestTheRouterWithoutAGatewayStillPicks(t *testing.T) {
	directory := &memoryDirectory{agents: map[uuid.UUID]chatdomain.AgentRef{}}

	oren := chatdomain.AgentRef{ID: uuid.New(), Name: "Oren", Role: "Penjualan", Stored: "active"}
	biru := chatdomain.AgentRef{ID: uuid.New(), Name: "Biru", Role: "Operasional", Stored: "active"}
	directory.agents[oren.ID] = oren
	directory.agents[biru.ID] = biru

	router := NewGroupRouter(RouterDeps{Agents: directory})

	conversation := chatdomain.Conversation{
		Kind:         chatdomain.KindGroup,
		Participants: []chatdomain.Participant{{AgentID: oren.ID}, {AgentID: biru.ID}},
	}

	decision, err := router.Pick(context.Background(), chatdomain.Scope{WorkspaceID: uuid.New()}, conversation, chatdomain.Message{})
	require.NoError(t, err)
	require.Equal(t, chatdomain.RouterSourceFallback, decision.Source)
}

// TestTheRouterRefusesAGroupWithNoBolu keeps the failure explicit rather than
// picking nobody.
func TestTheRouterRefusesAGroupWithNoBolu(t *testing.T) {
	router := NewGroupRouter(RouterDeps{Agents: &memoryDirectory{agents: map[uuid.UUID]chatdomain.AgentRef{}}})

	_, err := router.Pick(context.Background(), chatdomain.Scope{WorkspaceID: uuid.New()},
		chatdomain.Conversation{Kind: chatdomain.KindGroup}, chatdomain.Message{})
	require.ErrorIs(t, err, chatdomain.ErrNoResponder)
}

// TestOneBoluNeedsNoRoutingCall keeps a group of one from spending a model call.
func TestOneBoluNeedsNoRoutingCall(t *testing.T) {
	gateway := llmmocks.NewGateway(t)
	directory := &memoryDirectory{agents: map[uuid.UUID]chatdomain.AgentRef{}}

	oren := chatdomain.AgentRef{ID: uuid.New(), Name: "Oren", Role: "Penjualan", Stored: "active"}
	directory.agents[oren.ID] = oren

	router := NewGroupRouter(RouterDeps{Gateway: gateway, Agents: directory})

	decision, err := router.Pick(context.Background(), chatdomain.Scope{WorkspaceID: uuid.New()},
		chatdomain.Conversation{Kind: chatdomain.KindGroup, Participants: []chatdomain.Participant{{AgentID: oren.ID}}},
		chatdomain.Message{})
	require.NoError(t, err)
	require.Equal(t, oren.ID, decision.AgentID)
	require.Contains(t, decision.Reason, "Oren")

	// No call was made, which the mock would have failed on.
	gateway.AssertNotCalled(t, "Chat", mock.Anything, mock.Anything, mock.Anything)
}
