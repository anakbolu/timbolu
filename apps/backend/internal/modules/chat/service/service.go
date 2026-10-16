// Package service implements the conversation use cases: threads, block
// messages, attachments, the reply stream, the group router, and the activity
// events every live view is built from.
//
// Two rules run through the package:
//   - a message is a list of validated blocks, so a renderer is never handed a
//     shape the backend did not check;
//   - an event is written before it is published, so a subscriber that
//     reconnects finds the row it missed rather than a gap.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
)

// Limits the service enforces on top of the block schemas.
const (
	// TextMaxRunes bounds one message the user types. It is a chat message, not
	// a document.
	TextMaxRunes = 8000
	// GroupMaxParticipants bounds a group, so a routing call has a bounded set
	// of candidates to choose between.
	GroupMaxParticipants = 12
	// TitleMaxRunes bounds a group title.
	TitleMaxRunes = 120
	// EventPageLimit is the page a reconnecting client gets by default.
	EventPageLimit = 200
	// EventPageMax bounds one replay request.
	EventPageMax = 1000
	// replyTimeout bounds one reply, so a stuck provider cannot hold a
	// placeholder message open forever.
	replyTimeout = 5 * time.Minute
)

// Config holds the service settings.
type Config struct {
	// RepairBudget is how many times a block may be regenerated after a
	// validation failure. Zero means DefaultRepairBudget.
	RepairBudget int
	// StreamFlush is how often a growing reply is written and published. Zero
	// means the default below.
	StreamFlush time.Duration
}

// DefaultStreamFlush is the flush interval of a streaming reply.
//
// A token is not worth a database write and a broadcast, so a growing answer is
// flushed on a timer: the user sees text appear continuously, and the store sees
// a handful of writes per answer instead of hundreds.
const DefaultStreamFlush = 150 * time.Millisecond

// Deps are the service dependencies.
type Deps struct {
	Conversations chatdomain.ConversationRepository
	Messages      chatdomain.MessageRepository
	Attachments   chatdomain.AttachmentRepository
	Events        chatdomain.EventStore
	Publisher     chatdomain.EventPublisher
	Agents        chatdomain.AgentDirectory
	Responder     chatdomain.Responder
	Router        chatdomain.Router
	// Tasks opens the durable task a message belongs to. Optional: without it a
	// reply is still produced and recorded, it just has no task behind it.
	Tasks chatdomain.TaskStarter
	// Memory stores what a message asks to be remembered, and schedules the
	// conversation for extraction once it goes quiet. Optional: without it a
	// memory request is answered by the model like any other message, and nothing
	// is remembered automatically.
	Memory chatdomain.Memory
	// Storage holds attachment bytes. Optional: without it the attachment
	// endpoints report that storage is not configured.
	Storage chatdomain.ObjectStore
	Logger  *zap.Logger
	Config  Config
}

// Service is the conversation implementation.
type Service struct {
	deps     Deps
	settings Config
	// clock is the time source, so the stream flush is testable without waiting.
	clock func() time.Time
}

// New builds the service.
func New(deps Deps) *Service {
	settings := deps.Config
	if settings.RepairBudget <= 0 {
		settings.RepairBudget = chatdomain.DefaultRepairBudget
	}
	if settings.StreamFlush <= 0 {
		settings.StreamFlush = DefaultStreamFlush
	}

	return &Service{deps: deps, settings: settings, clock: time.Now}
}

// SetClock replaces the time source. The tests use it to drive the stream flush
// without waiting for a timer.
func (s *Service) SetClock(clock func() time.Time) { s.clock = clock }

// Conversations lists the threads of the workspace, most recent first.
func (s *Service) Conversations(ctx context.Context, scope chatdomain.Scope) ([]chatdomain.Conversation, error) {
	if err := requireWorkspace(scope); err != nil {
		return nil, err
	}

	return s.deps.Conversations.List(ctx, scope)
}

// Conversation returns one thread with its participants.
func (s *Service) Conversation(ctx context.Context, scope chatdomain.Scope, id uuid.UUID) (chatdomain.Conversation, error) {
	if err := requireWorkspace(scope); err != nil {
		return chatdomain.Conversation{}, err
	}
	if id == uuid.Nil {
		return chatdomain.Conversation{}, fmt.Errorf("%w: conversation id is required", chatdomain.ErrInvalidInput)
	}

	return s.deps.Conversations.GetConversation(ctx, scope, id)
}

// Direct returns the 1:1 thread with one Bolu, creating it on first use.
func (s *Service) Direct(ctx context.Context, scope chatdomain.Scope, agentID uuid.UUID) (chatdomain.Conversation, error) {
	if err := requireWorkspace(scope); err != nil {
		return chatdomain.Conversation{}, err
	}
	if agentID == uuid.Nil {
		return chatdomain.Conversation{}, fmt.Errorf("%w: agent id is required", chatdomain.ErrInvalidInput)
	}

	// The Bolu must exist in this workspace, which the directory checks; a
	// conversation with a Bolu from another tenant would otherwise be creatable.
	if _, err := s.deps.Agents.Agent(ctx, scope, agentID); err != nil {
		return chatdomain.Conversation{}, err
	}

	return s.deps.Conversations.Direct(ctx, scope, agentID)
}

// CreateGroup makes a group with the named Bolu and members.
func (s *Service) CreateGroup(ctx context.Context, scope chatdomain.Scope, req chatdomain.CreateGroupRequest) (chatdomain.Conversation, error) {
	if err := requireWorkspace(scope); err != nil {
		return chatdomain.Conversation{}, err
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		return chatdomain.Conversation{}, fmt.Errorf("%w: a group needs a name", chatdomain.ErrInvalidInput)
	}
	if len([]rune(title)) > TitleMaxRunes {
		return chatdomain.Conversation{}, fmt.Errorf("%w: name is longer than %d characters",
			chatdomain.ErrInvalidInput, TitleMaxRunes)
	}

	agentIDs, err := s.knownAgents(ctx, scope, req.AgentIDs)
	if err != nil {
		return chatdomain.Conversation{}, err
	}
	userIDs := dedupe(req.UserIDs)
	if len(agentIDs)+len(userIDs) < 2 {
		return chatdomain.Conversation{}, fmt.Errorf("%w: a group needs at least two participants",
			chatdomain.ErrInvalidInput)
	}
	if len(agentIDs)+len(userIDs) > GroupMaxParticipants {
		return chatdomain.Conversation{}, fmt.Errorf("%w: at most %d participants are allowed",
			chatdomain.ErrInvalidInput, GroupMaxParticipants)
	}

	return s.deps.Conversations.Create(ctx, scope, chatdomain.KindGroup, title, agentIDs, userIDs)
}

// AddParticipants adds Bolu and members to a group.
func (s *Service) AddParticipants(ctx context.Context, scope chatdomain.Scope, id uuid.UUID, req chatdomain.AddParticipantsRequest) (chatdomain.Conversation, error) {
	if err := requireWorkspace(scope); err != nil {
		return chatdomain.Conversation{}, err
	}

	conversation, err := s.deps.Conversations.GetConversation(ctx, scope, id)
	if err != nil {
		return chatdomain.Conversation{}, err
	}
	if conversation.Kind != chatdomain.KindGroup {
		return chatdomain.Conversation{}, fmt.Errorf("%w: only a group can take new participants",
			chatdomain.ErrInvalidInput)
	}

	agentIDs, err := s.knownAgents(ctx, scope, req.AgentIDs)
	if err != nil {
		return chatdomain.Conversation{}, err
	}
	userIDs := dedupe(req.UserIDs)
	if len(agentIDs)+len(userIDs) == 0 {
		return chatdomain.Conversation{}, fmt.Errorf("%w: no participants were named", chatdomain.ErrInvalidInput)
	}
	if len(conversation.Participants)+len(agentIDs)+len(userIDs) > GroupMaxParticipants {
		return chatdomain.Conversation{}, fmt.Errorf("%w: at most %d participants are allowed",
			chatdomain.ErrInvalidInput, GroupMaxParticipants)
	}

	if err := s.deps.Conversations.AddParticipants(ctx, scope, id, agentIDs, userIDs); err != nil {
		return chatdomain.Conversation{}, err
	}

	return s.deps.Conversations.GetConversation(ctx, scope, id)
}

// History returns one page of a conversation's messages, newest first.
func (s *Service) History(ctx context.Context, scope chatdomain.Scope, conversationID uuid.UUID, cursor chatdomain.Cursor) (chatdomain.Page, error) {
	if err := requireWorkspace(scope); err != nil {
		return chatdomain.Page{}, err
	}

	// Reading a page confirms the caller may see the thread: a conversation id
	// from another workspace must answer "not found" rather than "empty".
	if _, err := s.deps.Conversations.GetConversation(ctx, scope, conversationID); err != nil {
		return chatdomain.Page{}, err
	}

	return s.deps.Messages.History(ctx, scope, conversationID, cursor)
}

// Events returns the workspace stream after an ID, oldest first.
//
// This is the read a reconnecting client makes: it names the last event it saw
// and gets exactly what it missed, in order, which is what keeps the feed whole
// across a dropped connection.
func (s *Service) Events(ctx context.Context, scope chatdomain.Scope, afterID int64, limit int) ([]chatdomain.Event, error) {
	if err := requireWorkspace(scope); err != nil {
		return nil, err
	}

	if limit <= 0 {
		limit = EventPageLimit
	}
	if limit > EventPageMax {
		limit = EventPageMax
	}

	return s.deps.Events.SinceEvents(ctx, scope.WorkspaceID, afterID, limit)
}

// Attach stores a file and returns its metadata.
func (s *Service) Attach(ctx context.Context, scope chatdomain.Scope, req chatdomain.AttachRequest, content []byte) (chatdomain.Attachment, error) {
	if err := requireWorkspace(scope); err != nil {
		return chatdomain.Attachment{}, err
	}
	if s.deps.Storage == nil {
		return chatdomain.Attachment{}, chatdomain.ErrStorageUnavailable
	}
	if req.MessageID == uuid.Nil {
		return chatdomain.Attachment{}, fmt.Errorf("%w: message id is required", chatdomain.ErrInvalidInput)
	}
	if len(content) == 0 {
		return chatdomain.Attachment{}, fmt.Errorf("%w: the file is empty", chatdomain.ErrInvalidInput)
	}
	if len(content) > chatdomain.AttachmentMaxBytes {
		return chatdomain.Attachment{}, fmt.Errorf("%w: the file is larger than %d bytes",
			chatdomain.ErrInvalidInput, chatdomain.AttachmentMaxBytes)
	}

	// The message must exist in this workspace before a file may point at it.
	if _, err := s.deps.Messages.GetMessage(ctx, scope, req.MessageID); err != nil {
		return chatdomain.Attachment{}, err
	}

	filename := strings.TrimSpace(req.Filename)
	if filename == "" {
		filename = "lampiran"
	}
	contentType := strings.TrimSpace(req.ContentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	attachmentID := uuid.New()
	key := chatdomain.ObjectKey(
		"attachments", scope.WorkspaceID.String(), attachmentID.String(), filename)

	// The bytes go to storage first: a row that points at nothing would be worse
	// than an object nobody points at, because the row is what a user sees.
	object, err := s.deps.Storage.Put(ctx, key, contentType, content)
	if err != nil {
		return chatdomain.Attachment{}, fmt.Errorf("chat: store attachment: %w", err)
	}

	stored, err := s.deps.Attachments.Save(ctx, scope, chatdomain.Attachment{
		ID:             attachmentID,
		MessageID:      req.MessageID,
		StorageKey:     object.Key,
		Filename:       filename,
		ContentType:    contentType,
		ChecksumSHA256: object.ChecksumSHA256,
	}, content)
	if err != nil {
		// The row is the record, so a failure to write it must not leave the
		// object behind.
		if deleteErr := s.deps.Storage.Delete(ctx, key); deleteErr != nil && s.deps.Logger != nil {
			s.deps.Logger.Warn("attachment object left behind after a failed write",
				zap.String("key", key), zap.Error(deleteErr))
		}
		return chatdomain.Attachment{}, err
	}

	// The message is reloaded and published, because what a client renders is the
	// message's attachments, not the row just written. A failure to read it back
	// must not fail the upload: the file is stored and the next history read shows
	// it, while a 500 here would tell the user a file they can see was rejected.
	if message, err := s.deps.Messages.GetMessage(ctx, scope, req.MessageID); err != nil {
		if s.deps.Logger != nil {
			s.deps.Logger.Warn("could not reload the message an attachment was added to",
				zap.String("message_id", req.MessageID.String()), zap.Error(err))
		}
	} else {
		s.emitMessage(ctx, scope, message, message.ConversationID, chatdomain.EventMessageUpdated)
	}

	return stored, nil
}

// Attachment returns the metadata of one attachment.
func (s *Service) Attachment(ctx context.Context, scope chatdomain.Scope, id uuid.UUID) (chatdomain.Attachment, error) {
	if err := requireWorkspace(scope); err != nil {
		return chatdomain.Attachment{}, err
	}

	return s.deps.Attachments.Open(ctx, scope, id)
}

// AttachmentContent returns the bytes of one attachment from object storage.
func (s *Service) AttachmentContent(ctx context.Context, scope chatdomain.Scope, id uuid.UUID) (chatdomain.Attachment, []byte, error) {
	if err := requireWorkspace(scope); err != nil {
		return chatdomain.Attachment{}, nil, err
	}
	if s.deps.Storage == nil {
		return chatdomain.Attachment{}, nil, chatdomain.ErrStorageUnavailable
	}

	attachment, err := s.deps.Attachments.Open(ctx, scope, id)
	if err != nil {
		return chatdomain.Attachment{}, nil, err
	}

	content, err := s.deps.Storage.Get(ctx, attachment.StorageKey)
	if err != nil {
		return chatdomain.Attachment{}, nil, fmt.Errorf("chat: read attachment: %w", err)
	}
	return attachment, content, nil
}

// PruneAttachments removes the attachments of one workspace that are past a moment, row and
// object together, and reports what went.
//
// The order is the object first and the row second, which is the reverse of Attach and is for
// the same reason it is: a row is what a user can see, so it is the half that must not point at
// nothing. Removing the object first can leave a row whose bytes are gone — which the *next*
// pass fixes, because removing an object that is absent is not an error and the row is deleted
// then. Doing it the other way round would leak an object nothing points at, and no pass can
// ever find it again: it is invisible to the database, and only a bucket audit would see it.
//
// The bytes counted are the ones whose row was actually removed, so the number a monitor reads
// is storage that is really gone rather than storage that was attempted.
func (s *Service) PruneAttachments(ctx context.Context, scope chatdomain.Scope, olderThan time.Time, limit int) (chatdomain.AttachmentPrune, error) {
	if err := requireWorkspace(scope); err != nil {
		return chatdomain.AttachmentPrune{}, err
	}
	if s.deps.Storage == nil {
		return chatdomain.AttachmentPrune{}, chatdomain.ErrStorageUnavailable
	}

	expired, err := s.deps.Attachments.ListExpired(ctx, scope, olderThan, limit)
	if err != nil {
		return chatdomain.AttachmentPrune{}, err
	}

	var result chatdomain.AttachmentPrune
	var failures []error

	for _, attachment := range expired {
		if ctx.Err() != nil {
			failures = append(failures, ctx.Err())
			break
		}

		if err := s.deps.Storage.Delete(ctx, attachment.StorageKey); err != nil {
			// The row stays, so the attachment is still whole and still readable —
			// and the next pass tries again rather than losing the file silently.
			failures = append(failures, fmt.Errorf("chat: remove attachment object: %w", err))
			continue
		}

		if err := s.deps.Attachments.Delete(ctx, scope, attachment.ID); err != nil {
			failures = append(failures, fmt.Errorf("chat: remove attachment row: %w", err))
			continue
		}

		result.Count++
		result.BytesFreed += attachment.ByteSize
	}

	return result, errors.Join(failures...)
}

// EraseObjects removes every object this workspace owns and reports how many went.
//
// It walks the module's own prefixes rather than the attachment rows, and that distinction is the
// whole point: a sandboxed HTML document a Bolu wrote is an object with no row at all, so a deletion
// driven from the database would leave it in the bucket with nothing pointing at it — a copy of a
// customer's work that survives the request to remove it and that no query will ever find again. The
// prefixes are walked after the rows that name them are still there, so a failure is retryable.
//
// The bytes are removed one at a time and the failures are joined rather than swallowed: a single
// object that cannot be deleted must not be reported as a successful erasure.
func (s *Service) EraseObjects(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	if workspaceID == uuid.Nil {
		return 0, fmt.Errorf("%w: workspace id is required", chatdomain.ErrInvalidInput)
	}
	if s.deps.Storage == nil {
		return 0, chatdomain.ErrStorageUnavailable
	}

	var removed int64
	var failures []error

	for _, prefix := range chatdomain.ObjectPrefixes {
		keys, err := s.deps.Storage.List(ctx, chatdomain.WorkspaceObjectPrefix(prefix, workspaceID.String()))
		if err != nil {
			failures = append(failures, fmt.Errorf("chat: list %s objects: %w", prefix, err))

			continue
		}

		for _, key := range keys {
			if ctx.Err() != nil {
				failures = append(failures, ctx.Err())

				break
			}

			if err := s.deps.Storage.Delete(ctx, key); err != nil {
				failures = append(failures, fmt.Errorf("chat: delete object: %w", err))

				continue
			}

			removed++
		}
	}

	return removed, errors.Join(failures...)
}

// Content returns one sandboxed HTML document by its reference.
//
// The reference names an object under the content prefix and nothing else, which
// is what keeps this read from becoming a way to fetch any object in the bucket:
// a reference that does not decode to a content key is refused before storage is
// touched.
func (s *Service) Content(ctx context.Context, reference string) ([]byte, error) {
	if s.deps.Storage == nil {
		return nil, chatdomain.ErrStorageUnavailable
	}

	key, err := chatdomain.DecodeContentRef(reference)
	if err != nil {
		return nil, err
	}

	document, err := s.deps.Storage.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("chat: read content: %w", err)
	}
	return document, nil
}

// Emit records an event and publishes it, in that order.
//
// The order is the whole point: the row is the record, so a publish that fails
// costs latency for the clients connected right now and never an event. A
// subscriber that reconnects reads the row from the store.
func (s *Service) Emit(ctx context.Context, event chatdomain.Event) error {
	if event.WorkspaceID == uuid.Nil {
		return fmt.Errorf("%w: an event needs a workspace", chatdomain.ErrInvalidInput)
	}

	stored, err := s.deps.Events.AppendEvent(ctx, event)
	if err != nil {
		return err
	}

	if s.deps.Publisher == nil {
		return nil
	}
	if err := s.deps.Publisher.Publish(ctx, stored.WorkspaceID, stored); err != nil && s.deps.Logger != nil {
		// Not an error: the row is written, so a reconnecting client still sees
		// it. It does mean the live view is behind until then.
		s.deps.Logger.Warn("event published only to the store",
			zap.String("type", stored.Type), zap.Error(err))
	}

	return nil
}

// emitAgentState publishes a Bolu's derived state to the live stream.
func (s *Service) emitAgentState(ctx context.Context, scope chatdomain.Scope, agentID uuid.UUID, state string, payload chatdomain.AgentStatePayload) {
	encoded, err := json.Marshal(chatdomain.WireState(
		agentID, state, payload.Reason, parseWireID(payload.TaskID), parseWireID(payload.DraftID)))
	if err != nil {
		return
	}

	_ = s.Emit(ctx, chatdomain.Event{
		WorkspaceID:  scope.WorkspaceID,
		Type:         chatdomain.EventAgentState,
		ActorAgentID: agentID,
		TaskID:       parseWireID(payload.TaskID),
		DraftID:      parseWireID(payload.DraftID),
		Payload:      encoded,
	})
}

// knownAgents validates the Bolu of a request and returns them deduplicated, in
// the order they were asked for.
func (s *Service) knownAgents(ctx context.Context, scope chatdomain.Scope, ids []uuid.UUID) ([]uuid.UUID, error) {
	unique := dedupe(ids)
	if len(unique) == 0 {
		return nil, nil
	}

	// The directory checks membership, so a Bolu from another workspace cannot
	// be added to a conversation here.
	if _, err := s.deps.Agents.Agents(ctx, scope, unique); err != nil {
		return nil, err
	}
	return unique, nil
}

// parseWireID reads an id off the wire shape, which carries absence as an empty
// string. An unreadable value is treated as absent rather than as an error: the
// event's identity is the actor, and a bad task id must not drop the event.
func parseWireID(value string) uuid.UUID {
	id, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil
	}
	return id
}

func requireWorkspace(scope chatdomain.Scope) error {
	if scope.WorkspaceID == uuid.Nil {
		return fmt.Errorf("%w: workspace is required", chatdomain.ErrInvalidInput)
	}
	return nil
}

func dedupe(ids []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]bool, len(ids))
	unique := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if id == uuid.Nil || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	return unique
}

// llmScope is the tenant identity a model call runs as.
func llmScope(scope chatdomain.Scope, agentID uuid.UUID) llmdomain.Scope {
	return llmdomain.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
		AgentID:     agentID,
	}
}
