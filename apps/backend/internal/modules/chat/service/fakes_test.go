package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
)

// memoryStore is an in-memory implementation of the conversation, message, and
// event ports.
//
// It is a real implementation rather than a mock because the reply flow is
// stateful: a test asserts what was finally stored after a stream ended, and a
// mock would only assert which calls happened.
type memoryStore struct {
	mu            sync.Mutex
	conversations map[uuid.UUID]chatdomain.Conversation
	messages      map[uuid.UUID]chatdomain.Message
	events        []chatdomain.Event
	nextEventID   int64
	attachments   map[uuid.UUID]chatdomain.Attachment
	// failUpdate makes the message update fail, which is how a test drives the
	// "could not store the reply" path.
	failUpdate bool
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		conversations: map[uuid.UUID]chatdomain.Conversation{},
		messages:      map[uuid.UUID]chatdomain.Message{},
		attachments:   map[uuid.UUID]chatdomain.Attachment{},
	}
}

func (s *memoryStore) List(context.Context, chatdomain.Scope) ([]chatdomain.Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	conversations := make([]chatdomain.Conversation, 0, len(s.conversations))
	for _, conversation := range s.conversations {
		conversations = append(conversations, conversation)
	}
	sort.Slice(conversations, func(i, j int) bool {
		return conversations[i].CreatedAt.After(conversations[j].CreatedAt)
	})
	return conversations, nil
}

func (s *memoryStore) GetConversation(_ context.Context, scope chatdomain.Scope, id uuid.UUID) (chatdomain.Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	conversation, ok := s.conversations[id]
	if !ok || conversation.WorkspaceID != scope.WorkspaceID {
		// The tenant check is part of the port, so the fake applies it too: a
		// fake that ignored it would let a workspace leak through unnoticed.
		return chatdomain.Conversation{}, chatdomain.ErrConversationNotFound
	}
	return conversation, nil
}

func (s *memoryStore) Create(_ context.Context, scope chatdomain.Scope, kind, title string, agentIDs, userIDs []uuid.UUID) (chatdomain.Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	conversation := chatdomain.Conversation{
		ID:          uuid.New(),
		WorkspaceID: scope.WorkspaceID,
		Kind:        kind,
		Title:       title,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	for _, agentID := range agentIDs {
		conversation.Participants = append(conversation.Participants, chatdomain.Participant{
			ID: uuid.New(), ConversationID: conversation.ID, AgentID: agentID,
		})
	}
	for _, userID := range userIDs {
		conversation.Participants = append(conversation.Participants, chatdomain.Participant{
			ID: uuid.New(), ConversationID: conversation.ID, UserID: userID,
		})
	}

	s.conversations[conversation.ID] = conversation
	return conversation, nil
}

func (s *memoryStore) AddParticipants(context.Context, chatdomain.Scope, uuid.UUID, []uuid.UUID, []uuid.UUID) error {
	return nil
}

func (s *memoryStore) Rename(_ context.Context, _ chatdomain.Scope, id uuid.UUID, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	conversation, ok := s.conversations[id]
	if !ok {
		return chatdomain.ErrConversationNotFound
	}
	conversation.Title = title
	s.conversations[id] = conversation
	return nil
}

func (s *memoryStore) Direct(ctx context.Context, scope chatdomain.Scope, agentID uuid.UUID) (chatdomain.Conversation, error) {
	s.mu.Lock()
	for _, conversation := range s.conversations {
		if conversation.Kind != chatdomain.KindDirect {
			continue
		}
		for _, participant := range conversation.Participants {
			if participant.AgentID == agentID {
				s.mu.Unlock()
				return conversation, nil
			}
		}
	}
	s.mu.Unlock()

	return s.Create(ctx, scope, chatdomain.KindDirect, "", []uuid.UUID{agentID}, []uuid.UUID{scope.UserID})
}

func (s *memoryStore) Append(_ context.Context, _ chatdomain.Scope, message chatdomain.Message) (chatdomain.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	message.ID = uuid.New()
	message.CreatedAt = time.Now()
	message.UpdatedAt = message.CreatedAt
	if message.Status == "" {
		message.Status = chatdomain.MessageComplete
	}
	s.messages[message.ID] = message
	return message, nil
}

func (s *memoryStore) Update(_ context.Context, _ chatdomain.Scope, message chatdomain.Message) (chatdomain.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failUpdate {
		return chatdomain.Message{}, fmt.Errorf("chat: update message: the database is down")
	}

	stored, ok := s.messages[message.ID]
	if !ok {
		return chatdomain.Message{}, chatdomain.ErrMessageNotFound
	}
	stored.Blocks = message.Blocks
	stored.Status = message.Status
	stored.FinishReason = message.FinishReason
	// The task link is set once and never cleared, which is the COALESCE in the
	// statement the fake stands in for: a later write of the answer must not
	// erase which task produced it.
	if message.TaskID != uuid.Nil {
		stored.TaskID = message.TaskID
	}
	stored.UpdatedAt = time.Now()
	s.messages[message.ID] = stored
	return stored, nil
}

func (s *memoryStore) GetMessage(_ context.Context, _ chatdomain.Scope, id uuid.UUID) (chatdomain.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	message, ok := s.messages[id]
	if !ok {
		return chatdomain.Message{}, chatdomain.ErrMessageNotFound
	}
	return message, nil
}

func (s *memoryStore) History(_ context.Context, _ chatdomain.Scope, conversationID uuid.UUID, cursor chatdomain.Cursor) (chatdomain.Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	all := s.messagesOf(conversationID)

	limit := cursor.Limit
	if limit <= 0 {
		limit = chatdomain.DefaultPageSize
	}

	// The cursor names the last message of the previous page, so the page after
	// it starts at the row before that one. Walking from the newest and stopping
	// at the cursor would return the same page again.
	start := len(all) - 1
	if cursor.BeforeID != uuid.Nil {
		for i := len(all) - 1; i >= 0; i-- {
			if all[i].ID == cursor.BeforeID {
				start = i - 1
				break
			}
		}
	}

	// One extra row is fetched and dropped, which is how the real repository
	// learns whether another page exists without a second count query.
	page := chatdomain.Page{}
	for i := start; i >= 0 && len(page.Messages) < limit+1; i-- {
		page.Messages = append(page.Messages, all[i])
	}
	if len(page.Messages) > limit {
		page.HasMore = true
		page.Messages = page.Messages[:limit]
	}
	if len(page.Messages) > 0 {
		tail := page.Messages[len(page.Messages)-1]
		page.NextBeforeCreatedAtNanos = tail.CreatedAt.UnixNano()
		page.NextBeforeID = tail.ID
	}
	return page, nil
}

func (s *memoryStore) LatestMessages(_ context.Context, _ chatdomain.Scope, conversationID uuid.UUID, limit int) ([]chatdomain.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	all := s.messagesOf(conversationID)
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, nil
}

func (s *memoryStore) Save(_ context.Context, _ chatdomain.Scope, attachment chatdomain.Attachment, content []byte) (chatdomain.Attachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	attachment.ID = uuid.New()
	attachment.ByteSize = int64(len(content))
	attachment.CreatedAt = time.Now()
	s.attachments[attachment.ID] = attachment
	return attachment, nil
}

func (s *memoryStore) Open(_ context.Context, scope chatdomain.Scope, id uuid.UUID) (chatdomain.Attachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	attachment, ok := s.attachments[id]
	if !ok {
		return chatdomain.Attachment{}, chatdomain.ErrAttachmentNotFound
	}
	_ = scope
	return attachment, nil
}

func (s *memoryStore) ListForMessage(context.Context, chatdomain.Scope, uuid.UUID) ([]chatdomain.Attachment, error) {
	return nil, nil
}

func (s *memoryStore) AppendEvent(_ context.Context, event chatdomain.Event) (chatdomain.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextEventID++
	event.ID = s.nextEventID
	event.CreatedAt = time.Now()
	s.events = append(s.events, event)
	return event, nil
}

func (s *memoryStore) SinceEvents(_ context.Context, workspaceID uuid.UUID, afterID int64, limit int) ([]chatdomain.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	events := make([]chatdomain.Event, 0, limit)
	for _, event := range s.events {
		if event.WorkspaceID != workspaceID || event.ID <= afterID {
			continue
		}
		events = append(events, event)
		if limit > 0 && len(events) == limit {
			break
		}
	}
	return events, nil
}

func (s *memoryStore) LatestEvents(_ context.Context, workspaceID uuid.UUID, limit int) ([]chatdomain.Event, error) {
	return s.SinceEvents(context.Background(), workspaceID, 0, limit)
}

// messagesOf returns a conversation's messages, oldest first.
func (s *memoryStore) messagesOf(conversationID uuid.UUID) []chatdomain.Message {
	messages := make([]chatdomain.Message, 0, len(s.messages))
	for _, message := range s.messages {
		if message.ConversationID == conversationID {
			messages = append(messages, message)
		}
	}
	sort.Slice(messages, func(i, j int) bool { return messages[i].CreatedAt.Before(messages[j].CreatedAt) })
	return messages
}

// eventsOfType returns the recorded events of one type.
func (s *memoryStore) eventsOfType(eventType string) []chatdomain.Event {
	s.mu.Lock()
	defer s.mu.Unlock()

	events := make([]chatdomain.Event, 0, len(s.events))
	for _, event := range s.events {
		if event.Type == eventType {
			events = append(events, event)
		}
	}
	return events
}

// memoryPublisher is an in-process stand-in for the Redis channel.
type memoryPublisher struct {
	mu          sync.Mutex
	subscribers map[uuid.UUID][]chan chatdomain.Event
	published   int
}

func newMemoryPublisher() *memoryPublisher {
	return &memoryPublisher{subscribers: map[uuid.UUID][]chan chatdomain.Event{}}
}

func (p *memoryPublisher) Publish(_ context.Context, workspaceID uuid.UUID, event chatdomain.Event) error {
	p.mu.Lock()
	p.published++
	subscribers := append([]chan chatdomain.Event(nil), p.subscribers[workspaceID]...)
	p.mu.Unlock()

	for _, subscriber := range subscribers {
		select {
		case subscriber <- event:
		default:
		}
	}
	return nil
}

func (p *memoryPublisher) Subscribe(ctx context.Context, workspaceID uuid.UUID) (<-chan chatdomain.Event, error) {
	events := make(chan chatdomain.Event, 64)

	p.mu.Lock()
	p.subscribers[workspaceID] = append(p.subscribers[workspaceID], events)
	p.mu.Unlock()

	go func() {
		<-ctx.Done()
		p.mu.Lock()
		defer p.mu.Unlock()

		remaining := make([]chan chatdomain.Event, 0, len(p.subscribers[workspaceID]))
		for _, subscriber := range p.subscribers[workspaceID] {
			if subscriber != events {
				remaining = append(remaining, subscriber)
			}
		}
		p.subscribers[workspaceID] = remaining
		close(events)
	}()

	return events, nil
}

// scriptedResponder answers with a fixed sequence of chunks.
type scriptedResponder struct {
	chunks []chatdomain.StreamChunk
	// err fails the reply before it starts, which is the provider being down.
	err error
	// capture records the request, so a test can assert the context the model got.
	capture *chatdomain.ReplyRequest
}

func (r *scriptedResponder) Reply(ctx context.Context, _ chatdomain.Scope, req chatdomain.ReplyRequest) (<-chan chatdomain.StreamChunk, error) {
	if r.capture != nil {
		*r.capture = req
	}
	if r.err != nil {
		return nil, r.err
	}

	chunks := make(chan chatdomain.StreamChunk, len(r.chunks))
	for _, chunk := range r.chunks {
		chunks <- chunk
	}
	close(chunks)
	return chunks, nil
}

// scriptedGateway answers model calls with a script, one entry per round.
//
// The responder only streams, so a script is a list of rounds and each round is
// the events of one answer.
type scriptedGateway struct {
	mu     sync.Mutex
	rounds [][]llmdomain.StreamEvent
	calls  []llmdomain.ChatRequest
	// chatAnswer is what a non-streaming call answers, which the group router
	// makes. Empty means the call is refused.
	chatAnswer string
}

func (g *scriptedGateway) Stream(_ context.Context, _ llmdomain.Scope, req llmdomain.ChatRequest) (<-chan llmdomain.StreamEvent, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.calls = append(g.calls, req)

	round := 0
	if len(g.calls) > 0 {
		round = len(g.calls) - 1
	}
	if round >= len(g.rounds) {
		return nil, fmt.Errorf("%w: no scripted round %d", llmdomain.ErrInvalidRequest, round)
	}

	events := make(chan llmdomain.StreamEvent, len(g.rounds[round])+1)
	for _, event := range g.rounds[round] {
		events <- event
	}
	close(events)
	return events, nil
}

func (g *scriptedGateway) Chat(_ context.Context, _ llmdomain.Scope, req llmdomain.ChatRequest) (llmdomain.ChatResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.calls = append(g.calls, req)
	if g.chatAnswer == "" {
		return llmdomain.ChatResponse{}, fmt.Errorf("%w: the scripted gateway only streams", llmdomain.ErrInvalidRequest)
	}
	return llmdomain.ChatResponse{Text: g.chatAnswer}, nil
}

func (g *scriptedGateway) Providers(context.Context, llmdomain.Scope) ([]llmdomain.Redacted, error) {
	return nil, nil
}

func (g *scriptedGateway) Upsert(context.Context, llmdomain.Scope, llmdomain.UpsertRequest) (llmdomain.Redacted, error) {
	return llmdomain.Redacted{}, nil
}

func (g *scriptedGateway) Delete(context.Context, llmdomain.Scope, uuid.UUID) error { return nil }

func (g *scriptedGateway) Test(context.Context, llmdomain.Scope, uuid.UUID) (llmdomain.Capabilities, error) {
	return llmdomain.Capabilities{}, nil
}

func (g *scriptedGateway) SetAgentModel(context.Context, llmdomain.Scope, llmdomain.AgentOverride) error {
	return nil
}

func (g *scriptedGateway) AgentModel(context.Context, llmdomain.Scope) (llmdomain.AgentOverride, error) {
	return llmdomain.AgentOverride{}, nil
}

func (g *scriptedGateway) Usage(context.Context, llmdomain.Scope, int) ([]llmdomain.UsageDaily, error) {
	return nil, nil
}

// recordedCalls returns the requests the gateway was asked to serve.
func (g *scriptedGateway) recordedCalls() []llmdomain.ChatRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]llmdomain.ChatRequest(nil), g.calls...)
}

// memoryDirectory is the Bolu registry the chat module talks to.
type memoryDirectory struct {
	agents map[uuid.UUID]chatdomain.AgentRef
}

func (d *memoryDirectory) Agent(_ context.Context, _ chatdomain.Scope, id uuid.UUID) (chatdomain.AgentRef, error) {
	agent, ok := d.agents[id]
	if !ok {
		return chatdomain.AgentRef{}, chatdomain.ErrAgentNotInConversation
	}
	return agent, nil
}

func (d *memoryDirectory) Agents(_ context.Context, _ chatdomain.Scope, ids []uuid.UUID) ([]chatdomain.AgentRef, error) {
	refs := make([]chatdomain.AgentRef, 0, len(ids))
	for _, id := range ids {
		agent, ok := d.agents[id]
		if !ok {
			return nil, chatdomain.ErrAgentNotInConversation
		}
		refs = append(refs, agent)
	}
	return refs, nil
}

func (d *memoryDirectory) Active(ref chatdomain.AgentRef) bool {
	return ref.Stored != "resting"
}

// memoryStorage is an in-process object store.
type memoryStorage struct {
	objects map[string][]byte
}

func newMemoryStorage() *memoryStorage {
	return &memoryStorage{objects: map[string][]byte{}}
}

func (s *memoryStorage) Put(_ context.Context, key, contentType string, content []byte) (chatdomain.StoredObject, error) {
	s.objects[key] = append([]byte(nil), content...)
	return chatdomain.StoredObject{
		Key:            key,
		ContentType:    contentType,
		ByteSize:       int64(len(content)),
		ChecksumSHA256: fmt.Sprintf("%x", len(content)),
	}, nil
}

func (s *memoryStorage) Get(_ context.Context, key string) ([]byte, error) {
	content, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("storage: object not found: %s", key)
	}
	return content, nil
}

func (s *memoryStorage) Delete(_ context.Context, key string) error {
	delete(s.objects, key)
	return nil
}

func (s *memoryStorage) Driver() string { return "memory" }

// compile-time checks that the test doubles satisfy the ports they stand in for.
var (
	_ chatdomain.ConversationRepository = (*memoryStore)(nil)
	_ chatdomain.MessageRepository      = (*memoryStore)(nil)
	_ chatdomain.EventStore             = (*memoryStore)(nil)
	_ chatdomain.EventPublisher         = (*memoryPublisher)(nil)
	_ chatdomain.Responder              = (*scriptedResponder)(nil)
	_ chatdomain.AgentDirectory         = (*memoryDirectory)(nil)
	_ chatdomain.ObjectStore            = (*memoryStorage)(nil)
	_ llmdomain.Gateway                 = (*scriptedGateway)(nil)
)

// jsonTextBlock builds a text block document, which is what a test compares
// against.
func jsonTextBlock(markdown string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"type": "text", "markdown": markdown})
	return raw
}
