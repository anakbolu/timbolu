package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
)

// Send stores the user's message and starts the reply.
//
// It returns as soon as the message is stored. The answer is produced by a
// background worker and delivered on the event stream, which is what makes
// closing the tab harmless: the reply is written whether or not anyone is
// watching, because the tokens it costs are already paid for.
func (s *Service) Send(ctx context.Context, scope chatdomain.Scope, req chatdomain.SendRequest) (chatdomain.SendResult, error) {
	if err := requireWorkspace(scope); err != nil {
		return chatdomain.SendResult{}, err
	}

	text := strings.TrimSpace(req.Text)
	if text == "" && len(req.AttachmentIDs) == 0 {
		return chatdomain.SendResult{}, fmt.Errorf("%w: write something first", chatdomain.ErrInvalidInput)
	}
	if len([]rune(text)) > TextMaxRunes {
		return chatdomain.SendResult{}, fmt.Errorf("%w: the message is longer than %d characters",
			chatdomain.ErrInvalidInput, TextMaxRunes)
	}

	conversation, err := s.deps.Conversations.GetConversation(ctx, scope, req.ConversationID)
	if err != nil {
		return chatdomain.SendResult{}, err
	}

	attachments, err := s.resolveAttachments(ctx, scope, req.AttachmentIDs)
	if err != nil {
		return chatdomain.SendResult{}, err
	}

	blocks := make([]chatdomain.Block, 0, 1)
	if text != "" {
		block, err := textBlock(text)
		if err != nil {
			return chatdomain.SendResult{}, err
		}
		blocks = append(blocks, block)
	}

	userMessage, err := s.deps.Messages.Append(ctx, scope, chatdomain.Message{
		ConversationID: conversation.ID,
		AuthorUserID:   scope.UserID,
		Blocks:         blocks,
		Attachments:    attachments,
		Status:         chatdomain.MessageComplete,
	})
	if err != nil {
		return chatdomain.SendResult{}, err
	}

	s.emitMessage(ctx, scope, userMessage, conversation.ID, chatdomain.EventMessageNew)

	result := chatdomain.SendResult{Message: userMessage}
	if !req.Reply {
		return result, nil
	}

	responder, decision, routed, err := s.pickResponder(ctx, scope, conversation, userMessage, req.AgentID)
	if err != nil {
		return result, err
	}
	result.Responder = &responder
	result.Routed = routed

	// The placeholder is written before the answer starts, so a client can show
	// the Bolu thinking and follow that one message rather than every message of
	// the thread. It also means a crash mid-answer leaves a visible partial
	// rather than nothing.
	placeholder, err := s.deps.Messages.Append(ctx, scope, chatdomain.Message{
		ConversationID: conversation.ID,
		AuthorAgentID:  responder.ID,
		Blocks:         []chatdomain.Block{},
		Status:         chatdomain.MessageStreaming,
	})
	if err != nil {
		return result, err
	}
	result.ReplyMessageID = placeholder.ID

	s.emitMessage(ctx, scope, placeholder, conversation.ID, chatdomain.EventMessageNew)
	if routed {
		s.emitRouterDecision(ctx, scope, conversation, userMessage, decision)
	}

	// The durable runtime owns the answer when it is configured. A task survives
	// the request, a restart, and an approval that takes hours; the in-process
	// reply below does not, which is why it is the fallback rather than the
	// default.
	if s.deps.Tasks != nil {
		return s.dispatchTask(ctx, scope, conversation, responder, userMessage, placeholder, text, result)
	}

	// The reply runs on a context of its own: the request that started it ends
	// as soon as the message is stored, and the answer must not end with it.
	replyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replyTimeout)

	go func() {
		defer cancel()
		s.runReply(replyCtx, scope, conversation, responder, userMessage, placeholder)
	}()

	return result, nil
}

// dispatchTask hands the message to the durable runtime and records the task on
// the placeholder, so a client can follow the task from the message it answers.
//
// A dispatch that fails marks the placeholder failed rather than leaving it
// streaming forever: the user asked a question and must not be shown a Bolu that
// is still thinking about it.
func (s *Service) dispatchTask(
	ctx context.Context,
	scope chatdomain.Scope,
	conversation chatdomain.Conversation,
	responder chatdomain.AgentRef,
	userMessage, placeholder chatdomain.Message,
	prompt string,
	result chatdomain.SendResult,
) (chatdomain.SendResult, error) {
	taskID, err := s.deps.Tasks.Start(ctx, scope, chatdomain.StartRequest{
		ConversationID: conversation.ID,
		MessageID:      placeholder.ID,
		AgentID:        responder.ID,
		Trigger:        chatdomain.TriggerChat,
		Title:          prompt,
	})
	if err != nil {
		s.finishReply(ctx, scope, conversation, responder, placeholder, nil, chatdomain.MessageFailed, err.Error())
		return result, fmt.Errorf("chat: open task: %w", err)
	}

	placeholder.TaskID = taskID
	if stored, err := s.deps.Messages.Update(ctx, scope, placeholder); err == nil {
		placeholder = stored
		result.Message = userMessage
	}

	s.emitMessage(ctx, scope, placeholder, conversation.ID, chatdomain.EventMessageUpdated)
	s.emitAgentState(ctx, scope, responder.ID, chatdomain.StateWorking, chatdomain.AgentStatePayload{
		Reason: "mengerjakan tugas",
		TaskID: chatdomain.WireID(taskID),
	})

	return result, nil
}

// resolveAttachments loads the metadata of the files named in a message, so the
// stored message carries them and the client renders them without a fetch.
func (s *Service) resolveAttachments(ctx context.Context, scope chatdomain.Scope, ids []uuid.UUID) ([]chatdomain.Attachment, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	attachments := make([]chatdomain.Attachment, 0, len(ids))
	for _, id := range dedupe(ids) {
		attachment, err := s.deps.Attachments.Open(ctx, scope, id)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, attachment)
	}
	return attachments, nil
}

// pickResponder decides which Bolu answers.
//
// A pinned Bolu wins, which is what an explicit "@Bolu" in a group is. A direct
// thread answers with the one Bolu it is with. A group goes to the router.
func (s *Service) pickResponder(ctx context.Context, scope chatdomain.Scope, conversation chatdomain.Conversation, message chatdomain.Message, pinned uuid.UUID) (chatdomain.AgentRef, chatdomain.RouterDecision, bool, error) {
	agents, err := s.agentsOf(ctx, scope, conversation)
	if err != nil {
		return chatdomain.AgentRef{}, chatdomain.RouterDecision{}, false, err
	}
	if len(agents) == 0 {
		return chatdomain.AgentRef{}, chatdomain.RouterDecision{}, false, chatdomain.ErrNoResponder
	}

	if pinned != uuid.Nil {
		for _, agent := range agents {
			if agent.ID == pinned {
				return agent, chatdomain.RouterDecision{AgentID: pinned, Reason: "diminta langsung", Source: chatdomain.RouterSourceFallback}, false, nil
			}
		}
		return chatdomain.AgentRef{}, chatdomain.RouterDecision{}, false, chatdomain.ErrAgentNotInConversation
	}

	if conversation.Kind == chatdomain.KindDirect {
		agent := agents[0]
		if !s.deps.Agents.Active(agent) {
			// A resting Bolu is left alone rather than woken: the switch is the
			// user's, and a reply would contradict it.
			return chatdomain.AgentRef{}, chatdomain.RouterDecision{}, false, fmt.Errorf("%w: %s sedang istirahat", chatdomain.ErrNoResponder, agent.Name)
		}
		return agent, chatdomain.RouterDecision{AgentID: agent.ID, Reason: "obrolan langsung", Source: chatdomain.RouterSourceFallback, Candidates: 1}, false, nil
	}

	if s.deps.Router == nil {
		return chatdomain.AgentRef{}, chatdomain.RouterDecision{}, false, chatdomain.ErrNoResponder
	}

	decision, err := s.deps.Router.Pick(ctx, scope, conversation, message)
	if err != nil {
		return chatdomain.AgentRef{}, chatdomain.RouterDecision{}, false, err
	}
	for _, agent := range agents {
		if agent.ID == decision.AgentID {
			return agent, decision, true, nil
		}
	}

	return chatdomain.AgentRef{}, chatdomain.RouterDecision{}, false, chatdomain.ErrNoResponder
}

// agentsOf returns the Bolu of a conversation, in the order the participants are
// listed.
func (s *Service) agentsOf(ctx context.Context, scope chatdomain.Scope, conversation chatdomain.Conversation) ([]chatdomain.AgentRef, error) {
	ids := make([]uuid.UUID, 0, len(conversation.Participants))
	for _, participant := range conversation.Participants {
		if participant.IsAgent() {
			ids = append(ids, participant.AgentID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}

	return s.deps.Agents.Agents(ctx, scope, ids)
}

// runReply produces one answer and keeps the stored message in step with it.
//
// The message is the record, so it is updated as the answer grows and finished
// in every outcome: complete, partial, or failed. A stream that dies therefore
// leaves the tokens it produced behind, marked for what they are, rather than
// losing a paid-for answer or presenting a truncated one as whole.
func (s *Service) runReply(ctx context.Context, scope chatdomain.Scope, conversation chatdomain.Conversation, agent chatdomain.AgentRef, prompt, placeholder chatdomain.Message) {
	history, err := s.deps.Messages.LatestMessages(ctx, scope, conversation.ID, historyLimit)
	if err != nil {
		s.finishReply(ctx, scope, conversation, agent, placeholder, nil, chatdomain.MessageFailed, err.Error())
		return
	}

	// The placeholder is part of the history it must not be part of: a model
	// given its own empty answer would continue it instead of answering.
	history = withoutMessage(history, placeholder.ID)

	s.emitAgentState(ctx, scope, agent.ID, chatdomain.StateThinking, chatdomain.AgentStatePayload{
		Reason: "membaca pesanmu",
		TaskID: chatdomain.WireID(placeholder.TaskID),
	})

	events, err := s.deps.Responder.Reply(ctx, scope, chatdomain.ReplyRequest{
		Conversation: conversation,
		Agent:        agent,
		Message:      prompt,
		History:      history,
		RepairBudget: s.settings.RepairBudget,
	})
	if err != nil {
		s.finishReply(ctx, scope, conversation, agent, placeholder, nil, chatdomain.MessageFailed, err.Error())
		return
	}

	collector := &replyCollector{
		blocks:   []chatdomain.Block{},
		lastText: "",
		flushAt:  s.clock(),
	}

	for chunk := range events {
		if ctx.Err() != nil {
			// The deadline passed while the answer was arriving. What arrived is
			// kept and marked, rather than discarded.
			collector.finish(chatdomain.MessagePartial, "waktu habis")
			s.finishReply(ctx, scope, conversation, agent, placeholder, collector, chatdomain.MessagePartial, "waktu habis")
			return
		}

		switch chunk.Kind {
		case chatdomain.ChunkText:
			collector.appendText(chunk.Text)
			s.flushIfDue(ctx, scope, conversation, placeholder, collector)

		case chatdomain.ChunkNotice:
			collector.appendNotice(chunk.Text)

		case chatdomain.ChunkBlock:
			block, err := decodeBlock(chunk.Block)
			if err != nil {
				continue
			}
			collector.appendBlock(block)
			s.flushIfDue(ctx, scope, conversation, placeholder, collector, true)

		case chatdomain.ChunkError:
			reason := chunk.Err
			if reason == "" {
				reason = "penyedia gagal menjawab"
			}
			status := chatdomain.MessagePartial
			if collector.empty() {
				status = chatdomain.MessageFailed
			}
			collector.finish(status, reason)
			s.finishReply(ctx, scope, conversation, agent, placeholder, collector, status, reason)
			return

		case chatdomain.ChunkDone:
			// The answer ended; the loop below finishes it so a provider that
			// sends Done and then closes does not finish twice.
		}
	}

	status := chatdomain.MessageComplete
	reason := collector.reason
	if collector.err != nil {
		status = chatdomain.MessagePartial
		if collector.empty() {
			status = chatdomain.MessageFailed
		}
		reason = collector.err.Error()
	}
	collector.finish(status, reason)

	s.finishReply(ctx, scope, conversation, agent, placeholder, collector, status, reason)
}

// flushIfDue writes and publishes the growing message on a timer, so a token
// costs neither a database write nor a broadcast.
func (s *Service) flushIfDue(ctx context.Context, scope chatdomain.Scope, conversation chatdomain.Conversation, placeholder chatdomain.Message, collector *replyCollector, force ...bool) {
	if len(force) == 0 || !force[0] {
		if s.clock().Sub(collector.flushAt) < s.settings.StreamFlush {
			return
		}
	}

	collector.flushAt = s.clock()
	s.updateMessage(ctx, scope, conversation, placeholder, collector, chatdomain.MessageStreaming, "")
}

// finishReply writes the final state of the answer and tells the clients.
func (s *Service) finishReply(ctx context.Context, scope chatdomain.Scope, conversation chatdomain.Conversation, agent chatdomain.AgentRef, placeholder chatdomain.Message, collector *replyCollector, status, reason string) {
	// The context may already be over, which is exactly when the last write
	// matters most; a short grace period is enough for one statement.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	stored := s.updateMessage(writeCtx, scope, conversation, placeholder, collector, status, reason)

	if status != chatdomain.MessageComplete {
		s.emitAgentState(writeCtx, scope, agent.ID, chatdomain.StateIdle, chatdomain.AgentStatePayload{
			Reason: reason,
		})
		return
	}

	s.emitAgentState(writeCtx, scope, agent.ID, chatdomain.StateIdle, chatdomain.AgentStatePayload{
		Reason: "menunggu pesan berikutnya",
	})
	_ = stored
}

// updateMessage stores the current body and publishes it.
func (s *Service) updateMessage(ctx context.Context, scope chatdomain.Scope, conversation chatdomain.Conversation, placeholder chatdomain.Message, collector *replyCollector, status, reason string) chatdomain.Message {
	blocks := []chatdomain.Block{}
	if collector != nil {
		blocks = collector.blocks
	}

	stored, err := s.deps.Messages.Update(ctx, scope, chatdomain.Message{
		ID:           placeholder.ID,
		Blocks:       blocks,
		Status:       status,
		FinishReason: reason,
		TaskID:       placeholder.TaskID,
	})
	if err != nil {
		if s.deps.Logger != nil {
			s.deps.Logger.Error("could not store the reply",
				zap.String("message_id", placeholder.ID.String()), zap.Error(err))
		}
		return placeholder
	}

	// An update, never a new message: the placeholder already exists on the
	// client, and announcing it again would show it twice.
	s.emitMessage(ctx, scope, stored, conversation.ID, chatdomain.EventMessageUpdated)
	return stored
}

// emitMessage publishes a message event.
//
// The event type is passed in rather than derived from the status: a reply that
// finishes is an update to a message the client already has, and calling it new
// would make it appear twice.
func (s *Service) emitMessage(ctx context.Context, scope chatdomain.Scope, message chatdomain.Message, conversationID uuid.UUID, eventType string) {
	payload := chatdomain.WireMessage(message)
	payload.ConversationID = conversationID.String()

	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}

	_ = s.Emit(ctx, chatdomain.Event{
		WorkspaceID:    scope.WorkspaceID,
		Type:           eventType,
		ActorAgentID:   message.AuthorAgentID,
		ActorUserID:    message.AuthorUserID,
		ConversationID: conversationID,
		TaskID:         message.TaskID,
		Payload:        encoded,
	})
}

// emitRouterDecision records who the router picked and why.
func (s *Service) emitRouterDecision(ctx context.Context, scope chatdomain.Scope, conversation chatdomain.Conversation, message chatdomain.Message, decision chatdomain.RouterDecision) {
	encoded, err := json.Marshal(map[string]any{
		"agent_id":   decision.AgentID,
		"reason":     decision.Reason,
		"source":     decision.Source,
		"candidates": decision.Candidates,
	})
	if err != nil {
		return
	}

	_ = s.Emit(ctx, chatdomain.Event{
		WorkspaceID:    scope.WorkspaceID,
		Type:           chatdomain.EventRouterDecision,
		ActorAgentID:   decision.AgentID,
		ConversationID: conversation.ID,
		TaskID:         message.TaskID,
		Payload:        encoded,
	})
}

// replyCollector accumulates one answer.
//
// It is the only writer of the message body while the answer arrives, so the
// stored body and the published one cannot disagree: both come from here.
type replyCollector struct {
	mu       sync.Mutex
	blocks   []chatdomain.Block
	lastText string
	reason   string
	err      error
	flushAt  time.Time
}

func (c *replyCollector) appendText(text string) {
	if text == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastText += text
	c.replaceTextLocked()
}

func (c *replyCollector) appendNotice(notice string) {
	if notice == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// A notice is shown as text so a client that renders only text still says
	// what is happening; the prefix marks it as a status rather than an answer.
	c.lastText += "\n\n_" + notice + "_"
	c.replaceTextLocked()
}

func (c *replyCollector) appendBlock(block chatdomain.Block) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// A block ends the text that came before it, so the text block is kept and
	// the next text starts a new one. That keeps the order the model produced.
	c.blocks = append(c.blocks, block)
	c.lastText = ""
}

// replaceTextLocked keeps the trailing text block in step with what arrived.
func (c *replyCollector) replaceTextLocked() {
	if c.lastText == "" {
		return
	}

	block, err := textBlock(c.lastText)
	if err != nil {
		return
	}

	if len(c.blocks) > 0 && c.blocks[len(c.blocks)-1].Type == chatdomain.BlockText {
		c.blocks[len(c.blocks)-1] = block
		return
	}
	c.blocks = append(c.blocks, block)
}

func (c *replyCollector) finish(status, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.reason = reason
	if status == chatdomain.MessageFailed || status == chatdomain.MessagePartial {
		c.err = fmt.Errorf("%s", reason)
	}
}

func (c *replyCollector) empty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, block := range c.blocks {
		if block.Type == chatdomain.BlockText {
			var text struct {
				Markdown string `json:"markdown"`
			}
			if err := json.Unmarshal(block.Body, &text); err == nil && strings.TrimSpace(text.Markdown) != "" {
				return false
			}
			continue
		}
		return false
	}
	return true
}

// historyLimit is how much of a conversation is given to the model. A chat needs
// recent context, not the whole archive, and every message costs tokens on every
// turn.
const historyLimit = 30

// textBlock builds a text block, which is the one block type the service itself
// produces.
func textBlock(markdown string) (chatdomain.Block, error) {
	document := map[string]any{"type": chatdomain.BlockText, "markdown": markdown}
	raw, err := json.Marshal(document)
	if err != nil {
		return chatdomain.Block{}, fmt.Errorf("chat: encode text block: %w", err)
	}

	block := chatdomain.Block{Type: chatdomain.BlockText, Body: raw}
	if err := chatdomain.ValidateBlock(block); err != nil {
		return chatdomain.Block{}, err
	}
	return block, nil
}

// decodeBlock reads a block a responder produced, so it can be validated before
// it is stored.
func decodeBlock(raw json.RawMessage) (chatdomain.Block, error) {
	if len(raw) == 0 {
		return chatdomain.Block{}, fmt.Errorf("%w: empty block", chatdomain.ErrInvalidBlock)
	}

	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return chatdomain.Block{}, fmt.Errorf("%w: %w", chatdomain.ErrInvalidBlock, err)
	}
	return chatdomain.Block{Type: header.Type, Body: raw}, nil
}

// withoutMessage drops one message from a history slice.
func withoutMessage(messages []chatdomain.Message, id uuid.UUID) []chatdomain.Message {
	kept := make([]chatdomain.Message, 0, len(messages))
	for _, message := range messages {
		if message.ID == id {
			continue
		}
		kept = append(kept, message)
	}
	return kept
}
