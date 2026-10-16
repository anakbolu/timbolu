// Package domain defines the conversation contract: conversations with their
// participants, messages made of content blocks, attachments, and the activity
// event stream every live view is built from.
//
// A message body is a list of typed blocks rather than a string, because the
// frontend renders each type with its own component: Markdown text, a table, an
// approval card, a chart specification, Mermaid source, or a sandboxed HTML
// document. Validating a block here, before it is stored, is what keeps a
// malformed chart from ever reaching a renderer.
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Block types.
const (
	BlockText    = "text"
	BlockTable   = "table"
	BlockDraft   = "draft"
	BlockChart   = "chart"
	BlockMermaid = "mermaid"
	BlockHTML    = "html"
)

// BlockTypes lists every block type in the order the UI documents them.
var BlockTypes = []string{BlockText, BlockTable, BlockDraft, BlockChart, BlockMermaid, BlockHTML}

// Limits on block content. They exist to bound what one message can cost to
// render and to store, and the HTML limit is a security boundary as well: the
// document is served to a sandboxed iframe, so an unbounded one would be a way
// to make the content origin serve arbitrary payloads.
const (
	// HTMLMaxBytes caps a sandboxed HTML document.
	HTMLMaxBytes = 500 * 1024
	// MermaidMaxBytes caps Mermaid source. A diagram is meant to be read, not
	// generated at length.
	MermaidMaxBytes = 64 * 1024
	// TextMaxBytes caps one Markdown block.
	TextMaxBytes = 256 * 1024
	// TableMaxColumns and TableMaxRows bound a table block, which is stored
	// inline rather than in object storage.
	TableMaxColumns = 40
	TableMaxRows    = 2000
	// BlocksPerMessage bounds one message.
	BlocksPerMessage = 64
	// ChartMaxSeries and ChartMaxPoints bound a chart specification, so a
	// renderer is never handed a dataset that would freeze a phone.
	ChartMaxSeries = 12
	ChartMaxPoints = 2000
)

// Errors the service and the handlers map onto responses.
var (
	// ErrInvalidBlock means a block does not satisfy its schema.
	ErrInvalidBlock = errors.New("chat: invalid block")
	// ErrBlockTooLarge means a block exceeds its size limit.
	ErrBlockTooLarge = errors.New("chat: block is too large")
	// ErrConversationNotFound means the conversation is not in this workspace.
	ErrConversationNotFound = errors.New("chat: conversation not found")
	// ErrMessageNotFound means the message is not in this workspace.
	ErrMessageNotFound = errors.New("chat: message not found")
	// ErrAgentNotInConversation means the Bolu was not asked to answer here.
	ErrAgentNotInConversation = errors.New("chat: agent is not a participant")
	// ErrNoResponder means the group router could not pick a Bolu.
	ErrNoResponder = errors.New("chat: no Bolu can answer this message")
	// ErrInvalidInput means the request is malformed.
	ErrInvalidInput = errors.New("chat: invalid input")
	// ErrFileTypeNotAllowed means the upload's extension, or the content behind
	// it, is not a type this deployment accepts.
	ErrFileTypeNotAllowed = errors.New("chat: file type not allowed")
	// ErrAttachmentNotFound means the file is not in this workspace, or is not
	// one this caller may bind — the two are one answer on purpose, because a
	// file that belongs to somebody else's thread is not a file a client has any
	// business learning about.
	ErrAttachmentNotFound = errors.New("chat: attachment not found")
	// ErrAttachmentAlreadyBound means the file already belongs to a message. A
	// staged upload is bound once and never moved, so naming it a second time is
	// a conflict rather than a re-parenting.
	ErrAttachmentAlreadyBound = errors.New("chat: attachment is already bound")
	// ErrStorageUnavailable means no object storage is configured.
	ErrStorageUnavailable = errors.New("chat: object storage is not configured")
	// ErrRepairLimitReached means a block was regenerated as often as allowed.
	ErrRepairLimitReached = errors.New("chat: block repair limit reached")
)

// Block is one content block of a message.
//
// The concrete shape of a block is the JSON document in Body, and Type says
// which schema it must satisfy. Keeping the two together means a message is one
// JSONB value that the repository stores verbatim, while the type stays
// addressable for the renderer and for the repair loop.
type Block struct {
	Type string
	// Body is the whole block document, including its "type" field, so what is
	// stored is exactly what a client receives.
	Body json.RawMessage
}

// Message status. A reply that failed halfway is kept as a partial message
// rather than dropped, because the tokens it did produce were paid for and the
// user already saw part of the answer.
const (
	MessageComplete  = "complete"
	MessageStreaming = "streaming"
	MessagePartial   = "partial"
	MessageFailed    = "failed"
)

// Conversation kinds.
const (
	KindDirect = "direct"
	KindGroup  = "group"
)

// Conversation is one chat thread: one Bolu and one user, or a group.
type Conversation struct {
	ID           uuid.UUID
	WorkspaceID  uuid.UUID
	Kind         string
	Title        string
	Participants []Participant
	// MessageCount and LastActivityAt let the list render without a second
	// query. Last activity falls back to the creation time, so the sidebar order
	// needs no null handling.
	MessageCount   int
	LastActivityAt time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Participant is a member of a conversation: exactly one of AgentID and UserID
// is set, which the schema enforces too.
type Participant struct {
	ID             uuid.UUID
	ConversationID uuid.UUID
	AgentID        uuid.UUID
	UserID         uuid.UUID
	// DisplayName and Role are joined for the UI so a participant list renders
	// without a second round trip.
	DisplayName string
	Role        string
	CreatedAt   time.Time
}

// IsAgent reports whether the participant is a Bolu.
func (p Participant) IsAgent() bool { return p.AgentID != uuid.Nil }

// Message is one turn of a conversation.
type Message struct {
	ID             uuid.UUID
	WorkspaceID    uuid.UUID
	ConversationID uuid.UUID
	AuthorAgentID  uuid.UUID
	AuthorUserID   uuid.UUID
	Blocks         []Block
	Attachments    []Attachment
	TaskID         uuid.UUID
	Status         string
	// FinishReason records why a partial answer stopped, so the UI can say it
	// rather than presenting a truncated reply as a complete one.
	FinishReason string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// FromAgent reports whether a Bolu wrote the message.
func (m Message) FromAgent() bool { return m.AuthorAgentID != uuid.Nil }

// AuthorID is whichever identity wrote the message.
func (m Message) AuthorID() uuid.UUID {
	if m.AuthorAgentID != uuid.Nil {
		return m.AuthorAgentID
	}
	return m.AuthorUserID
}

// Attachment is one file attached to a message. The bytes live in object
// storage; only the metadata is a row, so a large file never passes through
// Postgres.
//
// ConversationID is set on every row this version writes, including the ones a
// message already carries, because a file uploaded before the message that names
// it has no message to be found through — and the bind is what checks that the
// file came from the thread it is being bound in.
type Attachment struct {
	ID             uuid.UUID
	WorkspaceID    uuid.UUID
	MessageID      uuid.UUID
	ConversationID uuid.UUID
	StorageKey     string
	Filename       string
	ContentType    string
	ByteSize       int64
	ChecksumSHA256 string
	CreatedAt      time.Time
}

// Attachment status. A file is either bound to a message or staged, waiting for
// one, and the schema keeps the two halves of that in step: a row is bound
// exactly when it has a message id.
const (
	// AttachmentStaged is an upload nobody has named in a message yet.
	AttachmentStaged = "staged"
	// AttachmentBound is a file that belongs to a message.
	AttachmentBound = "bound"
)

// MarshalBlocks renders blocks for the JSONB column.
func MarshalBlocks(blocks []Block) ([]byte, error) {
	encoded := make([]json.RawMessage, 0, len(blocks))
	for _, block := range blocks {
		if len(block.Body) == 0 {
			return nil, fmt.Errorf("%w: empty block body", ErrInvalidBlock)
		}
		encoded = append(encoded, block.Body)
	}

	raw, err := json.Marshal(encoded)
	if err != nil {
		return nil, fmt.Errorf("chat: encode blocks: %w", err)
	}
	return raw, nil
}

// UnmarshalBlocks reads blocks from the JSONB column.
//
// It reads the "type" of each block out of the document itself, so the stored
// value is the single source of truth and a row cannot claim a type its body
// does not have.
func UnmarshalBlocks(raw []byte) ([]Block, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	var documents []json.RawMessage
	if err := json.Unmarshal(raw, &documents); err != nil {
		return nil, fmt.Errorf("chat: decode blocks: %w", err)
	}

	blocks := make([]Block, 0, len(documents))
	for _, document := range documents {
		var header struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(document, &header); err != nil {
			return nil, fmt.Errorf("chat: decode block header: %w", err)
		}
		blocks = append(blocks, Block{Type: header.Type, Body: document})
	}
	return blocks, nil
}
