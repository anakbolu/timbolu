package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	chatdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/chat/domain"
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
	// A message opened for an upload is allowed to carry no text: the file is
	// the message, and the client sends it before the words.
	if text == "" && len(req.AttachmentIDs) == 0 && !req.ForAttachment {
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

	blocks := make([]chatdomain.Block, 0, 1)
	if text != "" {
		block, err := textBlock(text)
		if err != nil {
			return chatdomain.SendResult{}, err
		}
		blocks = append(blocks, block)
	}

	message := chatdomain.Message{
		ConversationID: conversation.ID,
		AuthorUserID:   scope.UserID,
		Blocks:         blocks,
		Status:         chatdomain.MessageComplete,
	}

	// A message that names staged files is stored *with* them, in one transaction, and
	// that is the whole point of the staged flow: the message and the files a client
	// will render become one committed fact before anything reads the message — the
	// reply below included. A file that cannot be bound takes the message down with it,
	// so a send is either refused outright or arrives complete.
	var userMessage chatdomain.Message
	if len(req.AttachmentIDs) > 0 {
		userMessage, err = s.deps.Attachments.SaveWithAttachments(
			ctx, scope, message, dedupe(req.AttachmentIDs))
	} else {
		userMessage, err = s.deps.Messages.Append(ctx, scope, message)
	}
	if err != nil {
		return chatdomain.SendResult{}, err
	}

	// One event, after the transaction committed: the message did not exist before this
	// request, and the files it carries are already part of it, so a client that renders
	// this message renders them without a second fetch.
	s.emitMessage(ctx, scope, userMessage, conversation.ID, chatdomain.EventMessageNew)

	result := chatdomain.SendResult{Message: userMessage}

	if s.deps.Memory != nil {
		// The conversation moved, so any extraction waiting on it starts over.
		// This runs before the reply decision on purpose: a message that does not
		// ask for an answer is still a message, and a thread that keeps being
		// written is not one to read yet.
		s.scheduleExtraction(ctx, scope, conversation, req.AgentID)
	}

	// A message opened for an attachment is a container, not a question: it
	// never starts a reply, whatever the caller asked for.
	if !req.Reply || req.ForAttachment {
		return result, nil
	}

	picks, err := s.pickResponders(ctx, scope, conversation, userMessage, req.AgentID, text)
	if err != nil {
		return result, err
	}

	if s.deps.Memory != nil {
		// A message that asks for something to be remembered is answered here
		// instead of by the model: the module that stores it is the module that
		// names what it stored, so the confirmation cannot describe a row that
		// does not exist. It also costs no tokens.
		if handled, err := s.remember(ctx, scope, conversation, picks[0].Agent, userMessage, text, &result); handled {
			// One Bolu wrote the confirmation, so one Bolu is what answered: the
			// list must not name the ones whose answer never started.
			result.Responders = []chatdomain.AgentRef{picks[0].Agent}
			return result, err
		}
	}

	// Every picked Bolu answers: a group message that names two of them is a
	// request to two of them, and the answer of one is not the answer of the
	// other.
	result.Responders = make([]chatdomain.AgentRef, 0, len(picks))
	for _, pick := range picks {
		result.Responders = append(result.Responders, pick.Agent)
	}
	// Routed describes how the message was addressed, and naming a Bolu is not
	// the router's doing; only the router's own pick reports itself.
	result.Routed = picks[0].Routed

	for _, pick := range picks {
		// The placeholder is written before the answer starts, so a client can show
		// the Bolu thinking and follow that one message rather than every message of
		// the thread. It also means a crash mid-answer leaves a visible partial
		// rather than nothing.
		placeholder, err := s.deps.Messages.Append(ctx, scope, chatdomain.Message{
			ConversationID: conversation.ID,
			AuthorAgentID:  pick.Agent.ID,
			Blocks:         []chatdomain.Block{},
			Status:         chatdomain.MessageStreaming,
		})
		if err != nil {
			return result, err
		}
		result.ReplyMessageIDs = append(result.ReplyMessageIDs, placeholder.ID)

		s.emitMessage(ctx, scope, placeholder, conversation.ID, chatdomain.EventMessageNew)
		if pick.Routed {
			s.emitRouterDecision(ctx, scope, conversation, userMessage, pick.Decision)
		}

		// The durable runtime owns the answer when it is configured. A task survives
		// the request, a restart, and an approval that takes hours; the in-process
		// reply below does not, which is why it is the fallback rather than the
		// default.
		if s.deps.Tasks != nil {
			dispatched, err := s.dispatchTask(ctx, scope, conversation, pick.Agent, userMessage, placeholder, text, result)
			result = dispatched
			if err != nil {
				return result, err
			}
			continue
		}

		// The reply runs on a context of its own: the request that started it ends
		// as soon as the message is stored, and the answer must not end with it.
		replyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replyTimeout)

		go func() {
			defer cancel()
			s.runReply(replyCtx, scope, conversation, pick.Agent, userMessage, placeholder)
		}()
	}

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

		// A refusal a person has to act on is passed through as it is: what fixes it is
		// money rather than another attempt, and the sentence the package wrote already
		// says which. Wrapping it in "could not open a task" would put the reason behind
		// a description of the symptom.
		if errors.Is(err, chatdomain.ErrNoBudget) {
			return result, err
		}
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

// responderPick is one Bolu that will answer, and how it was chosen.
type responderPick struct {
	Agent chatdomain.AgentRef
	// Decision is the router's decision, and only a routed pick carries one: a
	// pinned Bolu and a named one were asked for by the caller, and the message
	// itself says so, so there is no judgement to record.
	Decision chatdomain.RouterDecision
	// Routed says the router chose this Bolu, rather than the caller pinning one or
	// the message naming one.
	Routed bool
}

// pickResponders decides which Bolu answer.
//
// A pinned Bolu wins, which is what a client that resolved a mention itself sends.
// A direct thread answers with the one Bolu it is with. A group message that names
// Bolu goes to the ones it names, and each of them answers — naming two is asking
// two. A group message that names nobody goes to the router, which picks exactly
// one.
func (s *Service) pickResponders(ctx context.Context, scope chatdomain.Scope, conversation chatdomain.Conversation, message chatdomain.Message, pinned uuid.UUID, text string) ([]responderPick, error) {
	agents, err := s.agentsOf(ctx, scope, conversation)
	if err != nil {
		return nil, err
	}
	if len(agents) == 0 {
		return nil, chatdomain.ErrNoResponder
	}

	if pinned != uuid.Nil {
		for _, agent := range agents {
			if agent.ID == pinned {
				return []responderPick{{
					Agent:    agent,
					Decision: chatdomain.RouterDecision{AgentID: pinned, Reason: "diminta langsung", Source: chatdomain.RouterSourceFallback},
				}}, nil
			}
		}
		return nil, chatdomain.ErrAgentNotInConversation
	}

	// A thread is answered like a direct conversation: it carries one Bolu's work,
	// and whatever a person writes in it is for that Bolu.
	if conversation.Kind == chatdomain.KindDirect || conversation.Kind == chatdomain.KindThread {
		agent := agents[0]
		if !s.deps.Agents.Active(agent) {
			// A resting Bolu is left alone rather than woken: the switch is the
			// user's, and a reply would contradict it.
			return nil, fmt.Errorf("%w: %s sedang istirahat", chatdomain.ErrNoResponder, agent.Name)
		}
		return []responderPick{{
			Agent:    agent,
			Decision: chatdomain.RouterDecision{AgentID: agent.ID, Reason: "obrolan langsung", Source: chatdomain.RouterSourceFallback, Candidates: 1},
		}}, nil
	}

	// The names in the message come first: a person who types "@Biru" has said who
	// they want, and a router that second-guessed them would be answering a
	// question nobody asked.
	if named := mentioned(agents, text); len(named) > 0 {
		picks := make([]responderPick, 0, len(named))
		for _, agent := range named {
			picks = append(picks, responderPick{Agent: agent})
		}
		return picks, nil
	}

	if s.deps.Router == nil {
		return nil, chatdomain.ErrNoResponder
	}

	decision, err := s.deps.Router.Pick(ctx, scope, conversation, message)
	if err != nil {
		return nil, err
	}
	for _, agent := range agents {
		if agent.ID == decision.AgentID {
			return []responderPick{{Agent: agent, Decision: decision, Routed: true}}, nil
		}
	}

	return nil, chatdomain.ErrNoResponder
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

// emitMessage publishes a message event, and drops whether it went.
//
// The event type is passed in rather than derived from the status: a reply that
// finishes is an update to a message the client already has, and calling it new
// would make it appear twice.
func (s *Service) emitMessage(ctx context.Context, scope chatdomain.Scope, message chatdomain.Message, conversationID uuid.UUID, eventType string) {
	_ = s.emitMessageErr(ctx, scope, message, conversationID, eventType)
}

// emitMessageErr publishes a message event and reports whether it went, which is what a caller
// that has to answer for a lost update needs. The event type is passed in for the reason
// emitMessage gives.
func (s *Service) emitMessageErr(ctx context.Context, scope chatdomain.Scope, message chatdomain.Message, conversationID uuid.UUID, eventType string) error {
	payload := chatdomain.WireMessage(message)
	payload.ConversationID = conversationID.String()

	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("chat: encode %s event: %w", eventType, err)
	}

	return s.Emit(ctx, chatdomain.Event{
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
