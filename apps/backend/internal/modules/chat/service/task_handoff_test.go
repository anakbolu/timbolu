package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
)

// The task handoff is the seam between the conversation and the durable runtime:
// when a runtime is configured, a message becomes a task rather than an
// in-process answer, because only a task survives the request and a restart.

// recordingTasks is a TaskStarter that records what it was asked to open.
type recordingTasks struct {
	requests []chatdomain.StartRequest
	taskID   uuid.UUID
	err      error
}

func (r *recordingTasks) Start(_ context.Context, _ chatdomain.Scope, req chatdomain.StartRequest) (uuid.UUID, error) {
	if r.err != nil {
		return uuid.Nil, r.err
	}
	r.requests = append(r.requests, req)
	if r.taskID == uuid.Nil {
		r.taskID = uuid.New()
	}
	return r.taskID, nil
}

// TestSendOpensATaskWhenTheRuntimeIsConfigured is the E6.1 seam: the message is
// stored, a task is opened for it, and the placeholder the answer fills records
// which task owns it — so a client can follow the task from the message.
func TestSendOpensATaskWhenTheRuntimeIsConfigured(t *testing.T) {
	tasks := &recordingTasks{}
	var captured chatdomain.ReplyRequest
	h := newHarness(t, func(deps *Deps) {
		deps.Tasks = tasks
		deps.Responder.(*scriptedResponder).capture = &captured
	})

	conversation := h.directConversation(t)
	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "Rekap pesanan hari ini",
		Reply:          true,
	})
	require.NoError(t, err)

	require.Len(t, tasks.requests, 1, "a message with a runtime behind it opens exactly one task")
	opened := tasks.requests[0]
	require.Equal(t, conversation.ID, opened.ConversationID)
	require.Equal(t, result.ReplyMessageID, opened.MessageID,
		"the task fills the placeholder the chat module wrote, which is what puts the answer in the thread")
	require.Equal(t, h.agent.ID, opened.AgentID)
	require.Equal(t, chatdomain.TriggerChat, opened.Trigger)
	require.Equal(t, "Rekap pesanan hari ini", opened.Title)

	// The placeholder carries the task id, which is what a client follows.
	stored, err := h.store.GetMessage(context.Background(), h.scope, result.ReplyMessageID)
	require.NoError(t, err)
	require.Equal(t, tasks.taskID, stored.TaskID)
	require.Equal(t, chatdomain.MessageStreaming, stored.Status,
		"the task writes the answer, so the placeholder stays open until it does")

	// The in-process responder is not used: the runtime owns the answer.
	require.Empty(t, captured, "the model must not also answer in process")
}

// TestSendMarksThePlaceholderFailedWhenTheTaskCannotOpen is the honest failure: a
// message whose task could not be opened is marked failed rather than left
// streaming forever, which would read as a Bolu still thinking about it.
func TestSendMarksThePlaceholderFailedWhenTheTaskCannotOpen(t *testing.T) {
	tasks := &recordingTasks{err: errors.New("temporal is unreachable")}
	h := newHarness(t, func(deps *Deps) { deps.Tasks = tasks })

	conversation := h.directConversation(t)
	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "Rekap pesanan hari ini",
		Reply:          true,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "open task")

	stored, err := h.store.GetMessage(context.Background(), h.scope, result.ReplyMessageID)
	require.NoError(t, err)
	require.Equal(t, chatdomain.MessageFailed, stored.Status)
	require.Contains(t, stored.FinishReason, "temporal is unreachable")
}

// TestSendAnswersInProcessWithoutARuntime keeps the fallback honest: a deployment
// without the runtime still answers, it just does not survive a restart.
func TestSendAnswersInProcessWithoutARuntime(t *testing.T) {
	h := newHarness(t)
	h.responder.chunks = []chatdomain.StreamChunk{{Kind: chatdomain.ChunkText, Text: "Ada 3 pesanan."}}

	conversation := h.directConversation(t)
	result, err := h.service.Send(context.Background(), h.scope, chatdomain.SendRequest{
		ConversationID: conversation.ID,
		Text:           "Rekap pesanan hari ini",
		Reply:          true,
	})
	require.NoError(t, err)

	answered := h.waitForMessage(t, result.ReplyMessageID, func(message chatdomain.Message) bool {
		return message.Status == chatdomain.MessageComplete
	})
	require.Equal(t, uuid.Nil, answered.TaskID, "no runtime means no task behind the answer")
}
