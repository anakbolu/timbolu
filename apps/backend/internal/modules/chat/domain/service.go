package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Service is the conversation use case contract.
type Service interface {
	// Conversations lists the threads of the workspace, most recent first.
	Conversations(ctx context.Context, scope Scope) ([]Conversation, error)
	// Conversation returns one thread with its participants.
	Conversation(ctx context.Context, scope Scope, id uuid.UUID) (Conversation, error)
	// Direct returns the 1:1 thread with one Bolu, creating it on first use.
	Direct(ctx context.Context, scope Scope, agentID uuid.UUID) (Conversation, error)
	// CreateGroup makes a group with the named Bolu and members.
	CreateGroup(ctx context.Context, scope Scope, req CreateGroupRequest) (Conversation, error)
	// AddParticipants adds Bolu and members to a group.
	AddParticipants(ctx context.Context, scope Scope, id uuid.UUID, req AddParticipantsRequest) (Conversation, error)
	// History returns one page of a conversation's messages, newest first.
	History(ctx context.Context, scope Scope, conversationID uuid.UUID, cursor Cursor) (Page, error)
	// Send stores the user's message and starts the reply. It returns as soon as
	// the message is stored: the answer arrives on the event stream, so closing
	// the tab does not lose it.
	Send(ctx context.Context, scope Scope, req SendRequest) (SendResult, error)
	// Events returns the workspace stream after an ID, which is how a
	// reconnecting client fills the gap it missed.
	Events(ctx context.Context, scope Scope, afterID int64, limit int) ([]Event, error)
	// Stream returns one client's view of the workspace stream: the backlog it
	// missed, then everything live. The channel closes when the context ends.
	Stream(ctx context.Context, scope Scope, afterID int64) (<-chan Event, error)
	// Attach stores a file and returns its metadata.
	Attach(ctx context.Context, scope Scope, req AttachRequest, content []byte) (Attachment, error)
	// Attachment returns the metadata of one file.
	Attachment(ctx context.Context, scope Scope, id uuid.UUID) (Attachment, error)
	// AttachmentContent returns the metadata and the bytes of one file.
	AttachmentContent(ctx context.Context, scope Scope, id uuid.UUID) (Attachment, []byte, error)
	// EraseObjects removes every object this workspace owns — attachments and sandboxed
	// content — and reports how many went.
	//
	// It is on the service rather than on the repository because it walks the bucket, and the
	// prefixes are this module's own knowledge.
	EraseObjects(ctx context.Context, workspaceID uuid.UUID) (int64, error)
	// PruneAttachments removes the files past a moment, row and object together, and
	// reports what went.
	//
	// It is on the service rather than on the repository because an attachment is two
	// halves, and this is the layer that holds both.
	PruneAttachments(ctx context.Context, scope Scope, olderThan time.Time, limit int) (AttachmentPrune, error)
	// Content returns one sandboxed HTML document by its reference. It carries
	// no tenant scope on purpose: the reference is the capability, and the
	// document is served to an iframe that cannot send a cookie.
	Content(ctx context.Context, reference string) ([]byte, error)
}

// CreateGroupRequest is a new group.
type CreateGroupRequest struct {
	Title    string
	AgentIDs []uuid.UUID
	UserIDs  []uuid.UUID
}

// AddParticipantsRequest adds people to an existing group.
type AddParticipantsRequest struct {
	AgentIDs []uuid.UUID
	UserIDs  []uuid.UUID
}

// SendRequest is one message from the user.
type SendRequest struct {
	ConversationID uuid.UUID
	// Text is the message body. It is stored as one text block.
	Text string
	// Attachments are files already uploaded, referenced by ID.
	AttachmentIDs []uuid.UUID
	// Reply asks for an answer. A client that only wants to post (a note, or a
	// forwarded message) leaves it false.
	Reply bool
	// AgentID pins the responder. In a group it overrides the router, which is
	// what an explicit "@Bolu" does.
	AgentID uuid.UUID
}

// SendResult is what storing a message produced.
type SendResult struct {
	Message Message
	// ReplyMessageID is the placeholder the answer will fill. It is set as soon
	// as a reply was started, so a client can follow that one message rather
	// than every message of the conversation.
	ReplyMessageID uuid.UUID
	// Responder is the Bolu that will answer, which the group router decided.
	Responder *AgentRef
	// Routed says the router chose, rather than the caller pinning a Bolu.
	Routed bool
}

// AttachRequest is one file to store.
type AttachRequest struct {
	MessageID   uuid.UUID
	Filename    string
	ContentType string
}

// AttachmentMaxBytes caps one upload. The limit is the chat surface's, not the
// storage's: an attachment is rendered inline next to a message, so a huge one
// would be a way to make the UI unusable.
const AttachmentMaxBytes = 25 * 1024 * 1024

// AttachmentPrune is what one attachment sweep removed.
type AttachmentPrune struct {
	// Count is how many attachments were removed, row and object together.
	Count int
	// BytesFreed is what object storage gave back, which is the number that matters when
	// the bucket is what is growing.
	BytesFreed int64
}

// DefaultPruneLimit bounds one attachment sweep when the caller does not say how many.
//
// It is a default rather than a policy: the same batch size every pass is what keeps a
// deployment with years of attachments from removing a month of storage in one statement.
const DefaultPruneLimit = 500
