package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	chatdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/chat/domain"
)

// ThreadTitleMaxRunes bounds a thread's title. It is what the thread panel and
// the link in the chat show, so it is a line, not a paragraph.
const ThreadTitleMaxRunes = 120

// OpenThread opens a work thread from a conversation and starts the Bolu on it.
//
// A thread is a conversation of its own so that the work it carries does not
// crowd the room it was asked for in: the Bolu reports there, and a person
// steers it by writing in the thread. The brief is the first message, written
// by the Bolu, so a person opening the thread reads what was asked before what
// was done. The reply that does the work is started the same way a message
// starts one, which is what lets a thread be answered by the durable runtime,
// survive a restart, and wait for an approval like any other answer.
//
// A thread is never opened from a thread: the work it carries belongs to the
// room a person asked in, and a chain of threads would hide it from them.
func (s *Service) OpenThread(ctx context.Context, scope chatdomain.Scope, req chatdomain.OpenThreadRequest) (chatdomain.ThreadResult, error) {
	if err := requireWorkspace(scope); err != nil {
		return chatdomain.ThreadResult{}, err
	}

	title := strings.TrimSpace(req.Title)
	brief := strings.TrimSpace(req.Brief)
	if title == "" || brief == "" {
		return chatdomain.ThreadResult{}, fmt.Errorf("%w: a thread needs a title and a brief", chatdomain.ErrInvalidInput)
	}
	if len([]rune(title)) > ThreadTitleMaxRunes {
		title = string([]rune(title)[:ThreadTitleMaxRunes])
	}
	if len([]rune(brief)) > TextMaxRunes {
		return chatdomain.ThreadResult{}, fmt.Errorf("%w: the brief is longer than %d characters",
			chatdomain.ErrInvalidInput, TextMaxRunes)
	}

	parent, err := s.deps.Conversations.GetConversation(ctx, scope, req.ParentConversationID)
	if err != nil {
		return chatdomain.ThreadResult{}, err
	}
	if parent.Kind == chatdomain.KindThread {
		return chatdomain.ThreadResult{}, chatdomain.ErrThreadInThread
	}

	// The Bolu that carries the thread is one of the room's: a thread is work the
	// room asked for, and a Bolu nobody in it can see is not one it asked.
	var agent chatdomain.AgentRef
	agents, err := s.agentsOf(ctx, scope, parent)
	if err != nil {
		return chatdomain.ThreadResult{}, err
	}
	for _, candidate := range agents {
		if candidate.ID == req.AgentID {
			agent = candidate
		}
	}
	if agent.ID == uuid.Nil {
		return chatdomain.ThreadResult{}, chatdomain.ErrAgentNotInConversation
	}
	if !s.deps.Agents.Active(agent) {
		return chatdomain.ThreadResult{}, fmt.Errorf("%w: %s sedang istirahat", chatdomain.ErrNoResponder, agent.Name)
	}

	// The people of the room are the people of the thread, so whoever could read
	// the request can read the work.
	users := make([]uuid.UUID, 0, len(parent.Participants))
	for _, participant := range parent.Participants {
		if !participant.IsAgent() && participant.UserID != uuid.Nil {
			users = append(users, participant.UserID)
		}
	}

	thread, err := s.deps.Conversations.CreateThread(ctx, scope, parent.ID, title, []uuid.UUID{agent.ID}, users)
	if err != nil {
		return chatdomain.ThreadResult{}, err
	}

	block, err := textBlock("**" + title + "**\n\n" + brief)
	if err != nil {
		return chatdomain.ThreadResult{}, err
	}
	opening, err := s.deps.Messages.Append(ctx, scope, chatdomain.Message{
		ConversationID: thread.ID,
		AuthorAgentID:  agent.ID,
		Blocks:         []chatdomain.Block{block},
		Status:         chatdomain.MessageComplete,
	})
	if err != nil {
		return chatdomain.ThreadResult{}, err
	}
	s.emitMessage(ctx, scope, opening, thread.ID, chatdomain.EventMessageNew)

	result := chatdomain.ThreadResult{Conversation: thread, Brief: opening}

	placeholder, err := s.deps.Messages.Append(ctx, scope, chatdomain.Message{
		ConversationID: thread.ID,
		AuthorAgentID:  agent.ID,
		Blocks:         []chatdomain.Block{},
		Status:         chatdomain.MessageStreaming,
	})
	if err != nil {
		return result, err
	}
	result.ReplyMessageID = placeholder.ID
	s.emitMessage(ctx, scope, placeholder, thread.ID, chatdomain.EventMessageNew)

	// The brief is the Bolu's own words, so it is the history's last turn as the
	// Bolu's. The instruction that sets the work going is a turn of its own, which
	// is why the prompt is worded apart from the brief rather than repeating it.
	prompt := "Kerjakan thread \"" + title + "\" ini sampai selesai, lalu laporkan hasilnya:\n\n" + brief

	if s.deps.Tasks == nil {
		replyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replyTimeout)
		go func() {
			defer cancel()
			s.runReply(replyCtx, scope, thread, agent, opening, placeholder)
		}()
		return result, nil
	}

	taskID, err := s.deps.Tasks.Start(ctx, scope, chatdomain.StartRequest{
		ConversationID: thread.ID,
		MessageID:      placeholder.ID,
		AgentID:        agent.ID,
		Trigger:        chatdomain.TriggerChat,
		Title:          title,
		Prompt:         prompt,
		ParentTaskID:   req.ParentTaskID,
	})
	if err != nil {
		// The thread stays: it is where a person reads why the work did not start,
		// and where they can ask again once it can.
		s.finishReply(ctx, scope, thread, agent, placeholder, nil, chatdomain.MessageFailed, err.Error())
		return result, fmt.Errorf("chat: start thread task: %w", err)
	}
	result.TaskID = taskID

	placeholder.TaskID = taskID
	if stored, err := s.deps.Messages.Update(ctx, scope, placeholder); err == nil {
		placeholder = stored
	}
	s.emitMessage(ctx, scope, placeholder, thread.ID, chatdomain.EventMessageUpdated)
	s.emitAgentState(ctx, scope, agent.ID, chatdomain.StateWorking, chatdomain.AgentStatePayload{
		Reason: "mengerjakan thread: " + title,
		TaskID: chatdomain.WireID(taskID),
	})

	return result, nil
}
