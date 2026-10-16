package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
)

// groupOf builds a group with the harness Bolu and every given Bolu in it.
func (h *harness) groupOf(t *testing.T, others ...chatdomain.AgentRef) chatdomain.Conversation {
	t.Helper()

	ids := []uuid.UUID{h.agent.ID}
	for _, agent := range others {
		h.directory.agents[agent.ID] = agent
		ids = append(ids, agent.ID)
	}

	conversation, err := h.service.CreateGroup(context.Background(), h.scope, chatdomain.CreateGroupRequest{
		Title:    "Tim",
		AgentIDs: ids,
		UserIDs:  []uuid.UUID{h.scope.UserID},
	})
	require.NoError(t, err)
	return conversation
}

// TestANamedBoluAnswersInsteadOfTheRouter is the defect this fixes: a group
// message that calls a Bolu by name was answered by whoever the router happened
// to pick, and the Bolu that was called never answered at all.
func TestANamedBoluAnswersInsteadOfTheRouter(t *testing.T) {
	h := newHarness(t)

	biru := chatdomain.AgentRef{
		ID: uuid.New(), Name: "Biru", Role: "Operasional",
		Persona: "Mengurus pengiriman, stok, dan resi.", Stored: "active",
	}
	conversation := h.groupOf(t, biru)

	// The routing model is down, which is when the deterministic fallback decides:
	// a fallback that ignores the name is exactly how a called Bolu stayed silent.
	h.gateway.chatAnswer = ""
	h.responder.chunks = []chatdomain.StreamChunk{
		{Kind: chatdomain.ChunkText, Text: "Stok gudang aman."},
		{Kind: chatdomain.ChunkDone},
	}

	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "@Biru tolong cek ya",
		Reply:          true,
	})
	require.NoError(t, err)

	require.Len(t, result.Responders, 1, "one name, one Bolu answering")
	require.Equal(t, biru.ID, result.Responders[0].ID, "the Bolu the message calls is the one that answers")
	require.False(t, result.Routed, "a name is not a router decision")

	reply := h.waitForMessage(t, result.ReplyMessageIDs[0], settled)
	require.Equal(t, biru.ID, reply.AuthorAgentID)
	require.Equal(t, chatdomain.MessageComplete, reply.Status)
	require.Empty(t, h.store.eventsOfType(chatdomain.EventRouterDecision),
		"the router is not consulted when the message names who it wants")
}

// TestTwoNamedBoluBothAnswer: naming two Bolu is asking two, and each answers in
// its own message.
func TestTwoNamedBoluBothAnswer(t *testing.T) {
	h := newHarness(t)

	biru := chatdomain.AgentRef{ID: uuid.New(), Name: "Biru", Role: "Operasional", Stored: "active"}
	lila := chatdomain.AgentRef{ID: uuid.New(), Name: "Lila", Role: "Keuangan", Stored: "active"}
	conversation := h.groupOf(t, biru, lila)

	h.responder.chunks = []chatdomain.StreamChunk{
		{Kind: chatdomain.ChunkText, Text: "siap"},
		{Kind: chatdomain.ChunkDone},
	}

	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "@Biru cek stok, @Lila siapkan tagihan",
		Reply:          true,
	})
	require.NoError(t, err)

	require.Len(t, result.Responders, 2, "both named Bolu answer")
	require.Equal(t, []uuid.UUID{biru.ID, lila.ID}, []uuid.UUID{result.Responders[0].ID, result.Responders[1].ID},
		"the answers keep the order the names appear in")
	require.Len(t, result.ReplyMessageIDs, 2, "each answer has its own placeholder")

	for i, replyID := range result.ReplyMessageIDs {
		reply := h.waitForMessage(t, replyID, settled)
		require.Equal(t, chatdomain.MessageComplete, reply.Status)
		require.Equal(t, result.Responders[i].ID, reply.AuthorAgentID)
	}
}

// TestAMentionIsNotASubstring: a Bolu called "Biru" must not be called by the
// word "birunya", and a name inside a longer name belongs to the longer one.
func TestAMentionIsNotASubstring(t *testing.T) {
	h := newHarness(t)

	biru := chatdomain.AgentRef{ID: uuid.New(), Name: "Biru", Role: "Operasional", Stored: "active"}
	biruMuda := chatdomain.AgentRef{ID: uuid.New(), Name: "Biru Muda", Role: "Magang", Stored: "active"}
	conversation := h.groupOf(t, biru, biruMuda)

	h.gateway.chatAnswer = "Biru Muda"

	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "@Biru Muda tolong bantu",
		Reply:          true,
	})
	require.NoError(t, err)
	require.Len(t, result.Responders, 1, "the longest name wins, so one Bolu answers")
	require.Equal(t, biruMuda.ID, result.Responders[0].ID)

	// The word alone is not a call: nothing is named, so the router decides.
	result, err = h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "warna birunya bagus",
		Reply:          true,
	})
	require.NoError(t, err)
	require.True(t, result.Routed, "an unaddressed message still goes to the router")
	require.Len(t, result.Responders, 1)
}

// TestAMentionIsCaseInsensitive: people type names however they type them.
func TestAMentionIsCaseInsensitive(t *testing.T) {
	h := newHarness(t)

	biru := chatdomain.AgentRef{ID: uuid.New(), Name: "Biru", Role: "Operasional", Stored: "active"}
	conversation := h.groupOf(t, biru)
	h.responder.chunks = []chatdomain.StreamChunk{{Kind: chatdomain.ChunkDone}}

	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "pagi @BIRU!",
		Reply:          true,
	})
	require.NoError(t, err)
	require.Len(t, result.Responders, 1)
	require.Equal(t, biru.ID, result.Responders[0].ID)

	// A name that ends the message is a name too: the match must not need a
	// character after it.
	result, err = h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "tolong cek ya @Biru",
		Reply:          true,
	})
	require.NoError(t, err)
	require.Len(t, result.Responders, 1)
	require.Equal(t, biru.ID, result.Responders[0].ID)
}

// TestANamedBoluOutsideTheGroupIsNotReached keeps a name in the text from
// summoning a Bolu that is not in the conversation.
func TestANamedBoluOutsideTheGroupIsNotReached(t *testing.T) {
	h := newHarness(t)

	// Registered in the workspace, absent from the group.
	outsider := chatdomain.AgentRef{ID: uuid.New(), Name: "Kunyit", Role: "Pelanggan", Stored: "active"}
	h.directory.agents[outsider.ID] = outsider
	conversation := h.groupOf(t)
	h.gateway.chatAnswer = "Oren"

	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "@Kunyit tolong jawab pelanggan",
		Reply:          true,
	})
	require.NoError(t, err)
	require.Len(t, result.Responders, 1)
	require.Equal(t, h.agent.ID, result.Responders[0].ID,
		"a Bolu outside the group is not a candidate, so the router still decides")
}

// TestADirectThreadIsNotMentionRouted: a DM answers with its one Bolu whatever
// the text says, so a name in it cannot start a second answer.
func TestADirectThreadIsNotMentionRouted(t *testing.T) {
	h := newHarness(t)

	other := chatdomain.AgentRef{ID: uuid.New(), Name: "Biru", Role: "Operasional", Stored: "active"}
	h.directory.agents[other.ID] = other
	conversation := h.directConversation(t)
	h.responder.chunks = []chatdomain.StreamChunk{{Kind: chatdomain.ChunkDone}}

	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "@Biru tolong cek",
		Reply:          true,
	})
	require.NoError(t, err)
	require.Len(t, result.Responders, 1)
	require.Equal(t, h.agent.ID, result.Responders[0].ID, "the DM's own Bolu answers")
}
