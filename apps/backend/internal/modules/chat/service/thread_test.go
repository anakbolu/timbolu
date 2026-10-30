package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	chatdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/chat/domain"
)

// A work thread is a conversation a Bolu opens from the chat it was asked in. The
// tests below are the contract the thread panel relies on: the thread names the
// room it came from, starts with the brief, and is answered by the durable runtime
// with an instruction of its own rather than the Bolu's own words.

// TestOpenThreadStartsTheWorkUnderTheParent: the thread is linked to its room, its
// first message is the brief, and the task that does the work fills a placeholder
// in the thread and names the task that opened it.
func TestOpenThreadStartsTheWorkUnderTheParent(t *testing.T) {
	tasks := &recordingTasks{}
	h := newHarness(t, func(deps *Deps) { deps.Tasks = tasks })
	parent := h.directConversation(t)
	parentTask := uuid.New()

	result, err := h.service.OpenThread(context.Background(), h.scope, chatdomain.OpenThreadRequest{
		ParentConversationID: parent.ID,
		AgentID:              h.agent.ID,
		Title:                "Publish carousel IG",
		Brief:                "Siapkan carousel No 43 dan masukkan ke antrean.",
		ParentTaskID:         parentTask,
	})
	require.NoError(t, err)

	thread := result.Conversation
	require.Equal(t, chatdomain.KindThread, thread.Kind)
	require.Equal(t, parent.ID, thread.ParentConversationID, "the thread names the room it was opened from")
	require.Equal(t, "Publish carousel IG", thread.Title)

	brief, err := h.store.GetMessage(context.Background(), h.scope, result.Brief.ID)
	require.NoError(t, err)
	require.Equal(t, thread.ID, brief.ConversationID)
	require.Equal(t, h.agent.ID, brief.AuthorAgentID, "the brief is the Bolu's own words, not the person's")

	require.Len(t, tasks.requests, 1)
	opened := tasks.requests[0]
	require.Equal(t, thread.ID, opened.ConversationID)
	require.Equal(t, result.ReplyMessageID, opened.MessageID)
	require.Equal(t, parentTask, opened.ParentTaskID)
	require.Contains(t, opened.Prompt, "Siapkan carousel No 43")
	require.NotEqual(t, "**Publish carousel IG**\n\nSiapkan carousel No 43 dan masukkan ke antrean.", opened.Prompt,
		"the instruction is worded apart from the brief, or the model would read only its own turn")

	placeholder, err := h.store.GetMessage(context.Background(), h.scope, result.ReplyMessageID)
	require.NoError(t, err)
	require.Equal(t, tasks.taskID, placeholder.TaskID)
	require.Equal(t, chatdomain.MessageStreaming, placeholder.Status)
}

// TestOpenThreadRefusesAThreadInAThread: the work belongs to the room a person
// asked in, so a thread cannot open another one.
func TestOpenThreadRefusesAThreadInAThread(t *testing.T) {
	h := newHarness(t, func(deps *Deps) { deps.Tasks = &recordingTasks{} })
	parent := h.directConversation(t)

	first, err := h.service.OpenThread(context.Background(), h.scope, chatdomain.OpenThreadRequest{
		ParentConversationID: parent.ID, AgentID: h.agent.ID, Title: "Riset", Brief: "Cari sumber.",
	})
	require.NoError(t, err)

	_, err = h.service.OpenThread(context.Background(), h.scope, chatdomain.OpenThreadRequest{
		ParentConversationID: first.Conversation.ID, AgentID: h.agent.ID, Title: "Lagi", Brief: "Cari lagi.",
	})
	require.ErrorIs(t, err, chatdomain.ErrThreadInThread)
}

// TestOpenThreadNeedsABoluFromTheRoom: a Bolu that is not in the conversation was
// not asked, so it cannot carry the room's work.
func TestOpenThreadNeedsABoluFromTheRoom(t *testing.T) {
	h := newHarness(t, func(deps *Deps) { deps.Tasks = &recordingTasks{} })
	parent := h.directConversation(t)

	_, err := h.service.OpenThread(context.Background(), h.scope, chatdomain.OpenThreadRequest{
		ParentConversationID: parent.ID, AgentID: uuid.New(), Title: "Riset", Brief: "Cari sumber.",
	})
	require.ErrorIs(t, err, chatdomain.ErrAgentNotInConversation)
}

// TestOpenThreadNeedsATitleAndABrief: an empty thread is nothing a person could
// steer, so it is refused before anything is written.
func TestOpenThreadNeedsATitleAndABrief(t *testing.T) {
	h := newHarness(t)
	parent := h.directConversation(t)

	_, err := h.service.OpenThread(context.Background(), h.scope, chatdomain.OpenThreadRequest{
		ParentConversationID: parent.ID, AgentID: h.agent.ID, Title: "Riset", Brief: "  ",
	})
	require.ErrorIs(t, err, chatdomain.ErrInvalidInput)
}

// TestAMessageInAThreadIsAnsweredByItsBolu: a person steers a thread by writing in
// it, and the one Bolu it carries answers — the router is not asked.
func TestAMessageInAThreadIsAnsweredByItsBolu(t *testing.T) {
	tasks := &recordingTasks{}
	h := newHarness(t, func(deps *Deps) { deps.Tasks = tasks })
	parent := h.directConversation(t)

	opened, err := h.service.OpenThread(context.Background(), h.scope, chatdomain.OpenThreadRequest{
		ParentConversationID: parent.ID, AgentID: h.agent.ID, Title: "Riset", Brief: "Cari sumber.",
	})
	require.NoError(t, err)

	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: opened.Conversation.ID,
		Text:           "Pakai sumber resmi saja",
		Reply:          true,
	})
	require.NoError(t, err)
	require.Len(t, result.Responders, 1)
	require.Equal(t, h.agent.ID, result.Responders[0].ID)
	require.False(t, result.Routed, "a thread is answered like a direct conversation")
}
