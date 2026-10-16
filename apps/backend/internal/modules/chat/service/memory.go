package service

import (
	"context"

	"github.com/google/uuid"
	"go.uber.org/zap"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
)

// scheduleExtraction tells the memory module that this conversation moved.
//
// It runs on every message rather than once per thread, because the point is the
// quiet period: each message pushes the extraction further out, so a thread is
// read after it stops rather than while it is being written.
func (s *Service) scheduleExtraction(ctx context.Context, scope chatdomain.Scope, conversation chatdomain.Conversation, addressed uuid.UUID) {
	if s.deps.Memory == nil {
		return
	}

	agentID := memoryBolu(conversation, addressed)

	if err := s.deps.Memory.ConversationActive(ctx, scope, conversation.ID, agentID); err != nil && s.deps.Logger != nil {
		// A conversation that cannot be scheduled is simply never extracted
		// automatically. That is worth a warning and not worth failing the
		// message the user just sent.
		s.deps.Logger.Warn("could not schedule memory extraction for this conversation",
			zap.String("conversation_id", conversation.ID.String()),
			zap.Error(err),
		)
	}
}

// memoryBolu decides which Bolu a conversation's extraction is attributed to.
//
// The message's own address wins, because that is who the fact is about. A direct
// thread has exactly one Bolu, so it is that one. A group message nobody addressed
// has no answer, and the extraction then writes only to the main memory: a
// role-specific fact has no private layer to go to, and the workspace-wide layer
// is the wrong place for it.
func memoryBolu(conversation chatdomain.Conversation, addressed uuid.UUID) uuid.UUID {
	if addressed != uuid.Nil {
		return addressed
	}

	for _, participant := range conversation.Participants {
		if participant.AgentID != uuid.Nil {
			return participant.AgentID
		}
	}

	return uuid.Nil
}

// remember stores what a message asked to be remembered and writes the answer.
//
// It reports whether it took the message over. When it did, the reply is already
// in the thread and no task is opened: what the user asked for is a note in the
// Bolu's memory, and the confirmation of that is a cheaper and more accurate
// answer than a model call.
func (s *Service) remember(
	ctx context.Context,
	scope chatdomain.Scope,
	conversation chatdomain.Conversation,
	responder chatdomain.AgentRef,
	userMessage chatdomain.Message,
	text string,
	result *chatdomain.SendResult,
) (bool, error) {
	outcome, err := s.deps.Memory.Remember(ctx, scope, chatdomain.RememberRequest{
		Text:      text,
		AgentID:   responder.ID,
		MessageID: userMessage.ID,
	})
	if err != nil {
		// The message is not lost: the ordinary reply runs, so a memory module
		// that is down degrades to a Bolu that answers like before.
		if s.deps.Logger != nil {
			s.deps.Logger.Warn("a memory request could not be handled; answering normally",
				zap.String("conversation_id", conversation.ID.String()),
				zap.Error(err),
			)
		}
		return false, nil
	}

	if !outcome.Handled {
		return false, nil
	}

	block, err := textBlock(outcome.Confirmation)
	if err != nil {
		return false, err
	}

	confirmation, err := s.deps.Messages.Append(ctx, scope, chatdomain.Message{
		ConversationID: conversation.ID,
		AuthorAgentID:  responder.ID,
		Blocks:         []chatdomain.Block{block},
		Status:         chatdomain.MessageComplete,
	})
	if err != nil {
		return false, err
	}

	s.emitMessage(ctx, scope, confirmation, conversation.ID, chatdomain.EventMessageNew)

	result.ReplyMessageIDs = []uuid.UUID{confirmation.ID}
	return true, nil
}
