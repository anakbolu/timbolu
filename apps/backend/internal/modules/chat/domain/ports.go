package domain

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Scope is the tenant identity a query runs as.
type Scope struct {
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
}

// IsZero reports whether the scope carries no identity.
func (s Scope) IsZero() bool {
	return s.UserID == uuid.Nil && s.WorkspaceID == uuid.Nil
}

// Cursor is one page request over a conversation's history.
//
// Pagination is by cursor rather than offset because a chat is append-heavy: an
// offset shifts under the reader as new messages arrive, so page two would
// repeat or skip a message. The cursor names the last message of the previous
// page and the index reads newest-first, so a page boundary is stable no matter
// how much arrives meanwhile.
type Cursor struct {
	// BeforeCreatedAtNanos and BeforeID identify the last message of the
	// previous page. A zero timestamp means "start from the newest".
	//
	// The timestamp is in nanoseconds because Postgres keeps microseconds: a
	// millisecond cursor would be older than every row it should follow, and the
	// next page would come back empty.
	BeforeCreatedAtNanos int64
	BeforeID             uuid.UUID
	// Limit is the page size. Zero means the repository default.
	Limit int
}

// Page is one page of messages plus the cursor for the next one.
type Page struct {
	Messages []Message
	// NextBeforeCreatedAtNanos and NextBeforeID are the cursor of the next page,
	// or zero when this was the last one.
	NextBeforeCreatedAtNanos int64
	NextBeforeID             uuid.UUID
	// HasMore says whether another page exists, so the UI can stop asking.
	HasMore bool
}

// NextCursor is the cursor that reads the page after this one.
func (p Page) NextCursor() Cursor {
	return Cursor{
		BeforeCreatedAtNanos: p.NextBeforeCreatedAtNanos,
		BeforeID:             p.NextBeforeID,
		Limit:                len(p.Messages),
	}
}

// DefaultPageSize is the page a client gets when it does not ask for one.
const DefaultPageSize = 40

// MaxPageSize bounds a page, so one request cannot pull a whole history.
const MaxPageSize = 200

// ConversationRepository is the persistence contract for conversations.
type ConversationRepository interface {
	// List returns the conversations of the workspace the caller may see,
	// most recently active first.
	List(ctx context.Context, scope Scope) ([]Conversation, error)
	// GetConversation returns one conversation with its participants.
	GetConversation(ctx context.Context, scope Scope, id uuid.UUID) (Conversation, error)
	// Create makes a conversation with its participants in one transaction.
	Create(ctx context.Context, scope Scope, kind, title string, agentIDs, userIDs []uuid.UUID) (Conversation, error)
	// AddParticipants adds Bolu and members to an existing conversation.
	AddParticipants(ctx context.Context, scope Scope, id uuid.UUID, agentIDs, userIDs []uuid.UUID) error
	// Rename changes a group's title.
	Rename(ctx context.Context, scope Scope, id uuid.UUID, title string) error
	// Direct returns the 1:1 conversation with one Bolu, creating it when the
	// workspace does not have one yet. A chat with a Bolu is one thread, not a
	// new one per visit.
	Direct(ctx context.Context, scope Scope, agentID uuid.UUID) (Conversation, error)
}

// MessageRepository is the persistence contract for messages.
type MessageRepository interface {
	// Append stores one message and returns it as stored.
	Append(ctx context.Context, scope Scope, message Message) (Message, error)
	// Update replaces a message's body and status. It is how a streaming reply
	// is finished, and how a partial one is marked.
	Update(ctx context.Context, scope Scope, message Message) (Message, error)
	// GetMessage returns one message.
	GetMessage(ctx context.Context, scope Scope, id uuid.UUID) (Message, error)
	// History returns one page of a conversation, newest first.
	History(ctx context.Context, scope Scope, conversationID uuid.UUID, cursor Cursor) (Page, error)
	// LatestMessages returns the newest messages of a conversation, oldest
	// first, which is what a chat opens with.
	LatestMessages(ctx context.Context, scope Scope, conversationID uuid.UUID, limit int) ([]Message, error)
}

// AttachmentRepository stores attachment metadata and the bytes behind it.
type AttachmentRepository interface {
	// Save stores the bytes and their metadata.
	Save(ctx context.Context, scope Scope, attachment Attachment, content []byte) (Attachment, error)
	// SaveWithNewMessage stores the bytes, opens the message that carries them,
	// and writes the row and the message's read model, all in one transaction.
	//
	// It exists so an upload without a message does not need two: a message
	// opened first would be a carrier that outlives a store that failed, and a
	// client would see an empty bubble where its file should be. The message is
	// appended with the attachment already in it, and a failure anywhere rolls
	// all of it back.
	SaveWithNewMessage(ctx context.Context, scope Scope, message Message, attachment Attachment, content []byte) (Message, Attachment, error)
	// Open returns the metadata of one attachment. The bytes are fetched from
	// object storage by the key it carries, so a large file never passes through
	// Postgres.
	Open(ctx context.Context, scope Scope, id uuid.UUID) (Attachment, error)
	// ListForMessage returns the attachments of one message.
	ListForMessage(ctx context.Context, scope Scope, messageID uuid.UUID) ([]Attachment, error)
	// ListExpired returns the attachments past a moment, oldest first, with the
	// storage key their bytes live under.
	ListExpired(ctx context.Context, scope Scope, olderThan time.Time, limit int) ([]Attachment, error)
	// Delete removes one attachment's metadata row and the element it left in its
	// message's read model, in one transaction, and returns that message as it
	// now reads.
	//
	// The bytes are the storage half and are removed by the service, which is the only
	// layer that holds both the repository and the object store — and removing half of
	// an attachment is what it exists to prevent.
	//
	// A row that is already gone is not an error: the zero Message comes back with a nil
	// error, because a retry after a partial failure has to converge rather than report
	// a failure that is really "this was already done".
	Delete(ctx context.Context, scope Scope, id uuid.UUID) (Message, error)
}

// StoredObject is what a write to object storage produced.
type StoredObject struct {
	Key         string
	ContentType string
	ByteSize    int64
	// ChecksumSHA256 is the hex digest of the content, which is what lets a
	// caller verify that what it stored is what it meant to store.
	ChecksumSHA256 string
}

// ObjectStore holds the bytes that do not belong in Postgres: attachments and
// the sandboxed HTML documents a message renders.
//
// It is a port rather than the storage package so the module depends on the
// shape it needs and not on a driver: the local-directory store, the
// S3-compatible one, and a test's in-memory one all satisfy it.
type ObjectStore interface {
	Put(ctx context.Context, key, contentType string, content []byte) (StoredObject, error)
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
	// List returns every key under a prefix, which is what an erasure needs: the bytes of one
	// workspace are in a bucket, and the keys that name them are rows that are about to be
	// deleted.
	List(ctx context.Context, prefix string) ([]string, error)
	Driver() string
}

// ObjectKey builds the key one object is stored under.
//
// The key is namespaced by workspace so a listing of one tenant's objects is a
// prefix scan, and the filename is sanitized so a name from a client cannot
// escape its own directory.
func ObjectKey(prefix, workspaceID, objectID, filename string) string {
	return strings.Join([]string{
		prefix,
		"workspaces", workspaceID,
		"objects", objectID,
		SanitizeFilename(filename),
	}, "/")
}

// ObjectPrefixes are the namespaces this module writes objects under.
//
// They are listed here rather than derived at the call site because they are the module's own
// knowledge, and because an erasure has to walk all of them: a prefix missing from this list is a
// copy of a customer's file that survives the request to remove it, and nothing in the database
// would ever point at it again.
var ObjectPrefixes = []string{AttachmentsPrefix, ContentRefPrefix}

// AttachmentsPrefix is where uploaded files live.
const AttachmentsPrefix = "attachments"

// WorkspaceObjectPrefix is the key prefix under which one workspace's objects live, for each
// namespace above. It is what an erasure lists.
func WorkspaceObjectPrefix(prefix, workspaceID string) string {
	return strings.Join([]string{prefix, "workspaces", workspaceID, "objects"}, "/")
}

// SanitizeFilename keeps a filename usable as one path segment.
//
// A name from a client is untrusted input, so everything that could change the
// path is replaced rather than escaped: a slash would let a crafted name write
// outside the object's own directory.
func SanitizeFilename(name string) string {
	cleaned := strings.TrimSpace(name)
	cleaned = strings.ReplaceAll(cleaned, "\\", "-")
	cleaned = strings.ReplaceAll(cleaned, "/", "-")
	cleaned = strings.ReplaceAll(cleaned, "\x00", "")
	cleaned = strings.ReplaceAll(cleaned, "..", ".")
	cleaned = strings.TrimLeft(cleaned, ".")
	if cleaned == "" {
		return "file"
	}
	if len(cleaned) > 120 {
		// Keep the extension, which is what a browser needs to render it.
		extension := filepath.Ext(cleaned)
		cleaned = cleaned[:120-len(extension)] + extension
	}
	return cleaned
}

// AgentRef is the minimum a conversation needs to know about a Bolu: who it is,
// what it is for, and which model it uses. It is a port rather than an import so
// the chat module does not depend on the registry module.
type AgentRef struct {
	ID           uuid.UUID
	Name         string
	Role         string
	Persona      string
	Tone         string
	Stored       string
	Display      string
	Tools        []string
	DefaultModel map[string]any
}

// AgentDirectory reads the Bolu of a workspace.
type AgentDirectory interface {
	// Agent returns one Bolu, or an error when it is not in the workspace.
	Agent(ctx context.Context, scope Scope, id uuid.UUID) (AgentRef, error)
	// Agents returns the named Bolu, in the order they were asked for.
	Agents(ctx context.Context, scope Scope, ids []uuid.UUID) ([]AgentRef, error)
	// Active reports whether a Bolu accepts new work. A resting Bolu never does.
	Active(ref AgentRef) bool
}

// ErrNoBudget is what a message becomes when the workspace's package does not allow the
// reply to be opened.
//
// It is this module's own sentinel rather than the task runtime's, because the two are
// separate modules: whoever wires them translates between the two, and this is the half a
// screen reads. The answer is not "try again" — it is "buy a top-up or wait for the next
// period", and a person acts on it differently from every other refusal.
var ErrNoBudget = errors.New("chat: the workspace's package does not allow a reply")

// TaskStarter hands a message to the task runtime, which is EPIC 6.
//
// The chat module only records what the user asked for and shows what comes
// back; running the agent loop is not its job. Until the runtime exists the port
// is unimplemented, and a chat message is answered by the fallback below.
type TaskStarter interface {
	// Start opens a task for a message and returns its ID.
	Start(ctx context.Context, scope Scope, req StartRequest) (uuid.UUID, error)
}

// StartRequest is one task to open.
type StartRequest struct {
	ConversationID uuid.UUID
	// MessageID is the placeholder message the task's answer fills. It is the
	// message the user is watching, not the one they wrote.
	MessageID uuid.UUID
	AgentID   uuid.UUID
	// Trigger is what caused the task: chat, routine, webhook, or handoff.
	Trigger string
	Title   string
}

// Task triggers, matching the schema's check constraint.
const (
	TriggerChat    = "chat"
	TriggerRoutine = "routine"
	TriggerWebhook = "webhook"
	TriggerHandoff = "handoff"
)

// Memory is what the chat module asks of the memory module.
//
// Two questions, and both belong to the message rather than to the answer: a
// message may ask for something to be remembered, and every message is a sign
// that a conversation is still moving. Neither is the chat module's business, so
// both are ports.
type Memory interface {
	// Remember stores what the message asked to be remembered and hands back the
	// sentence the Bolu answers with. A false Handled means the message was not a
	// memory request, and the ordinary reply runs.
	Remember(ctx context.Context, scope Scope, req RememberRequest) (RememberResult, error)
	// ConversationActive tells the extractor the thread moved, which pushes its
	// quiet timer out. A conversation that keeps moving is never read.
	ConversationActive(ctx context.Context, scope Scope, conversationID, agentID uuid.UUID) error
}

// RememberRequest is one message that may ask for something to be remembered.
type RememberRequest struct {
	Text string
	// AgentID is the Bolu being addressed, whose private memory a role-specific
	// fact belongs to.
	AgentID uuid.UUID
	// MessageID is the message it came from, so the memory can point back at it.
	MessageID uuid.UUID
}

// RememberResult is what the memory module answered.
type RememberResult struct {
	// Handled says the message was answered by the memory module.
	Handled bool
	// Confirmation is what the Bolu answers. It names exactly what was stored.
	Confirmation string
	// Refused says nothing was stored: the content was sensitive.
	Refused bool
}

// Responder answers a message with a stream of chunks.
//
// It is the seam the agent runtime plugs into: today the implementation answers
// from the configured model directly, and EPIC 6 replaces it with the durable
// agent loop. The chat module neither knows nor cares which one it is.
type Responder interface {
	// Reply streams the answer of one Bolu to one message. The channel closes
	// when the answer ends; the last chunk is Done or Error.
	Reply(ctx context.Context, scope Scope, req ReplyRequest) (<-chan StreamChunk, error)
}

// RouterDecision is who the group router picked and why.
//
// The reason is recorded in the activity event, so "why did Biru answer and not
// Oren" has an answer after the fact rather than being inferred from the text.
type RouterDecision struct {
	AgentID uuid.UUID
	// Reason is a short explanation, in the language the UI shows.
	Reason string
	// Source says how the decision was made: "model" or "fallback". The cost of
	// a routing call is recorded separately, so this makes it attributable.
	Source string
	// Candidates is how many Bolu were considered.
	Candidates int
}

// Router picks the Bolu that should answer a group message.
type Router interface {
	// Pick returns the responder, or ErrNoResponder when none of the
	// participants can answer.
	Pick(ctx context.Context, scope Scope, conversation Conversation, message Message) (RouterDecision, error)
}

// Router sources.
const (
	RouterSourceModel    = "model"
	RouterSourceFallback = "fallback"
)

// ReplyRequest is one answer to produce.
type ReplyRequest struct {
	Conversation Conversation
	Agent        AgentRef
	Message      Message
	// History is the conversation so far, oldest first, which is the context
	// the model is given.
	History []Message
	// RepairBudget is how many times a block may be regenerated after a
	// validation failure. It bounds the token cost of a repair loop.
	RepairBudget int
}

// DefaultRepairBudget is how many regeneration attempts a block gets. The
// architecture document asks for a bounded loop, and two attempts covers a
// mistyped field without paying for a long argument.
const DefaultRepairBudget = 2
