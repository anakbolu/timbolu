package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// EventMessageReceived is the event name an inbound message is announced under.
//
// It is the same shape the hub uses (`app.thing`), because a routine watching
// WhatsApp should be written the same way as one watching Gmail, and because the
// customer-message path is the hub's inbound queue seen from the other side.
const EventMessageReceived = "whatsapp.message_received"

// Service is what the HTTP handlers, the wa-gateway, and the integration adapter
// call.
//
// It has two halves by design. The first is what a person does: link a number,
// choose who answers it, unlink it. The second is what the gateway does: report
// it is online, pull what is due, report what left. Both are here rather than in
// two types because they are two views of one state machine, and splitting them
// would let a transition happen in one while the other still believed the old
// status.
type Service interface {
	// --- what a person does -------------------------------------------------

	// Numbers returns the workspace's numbers.
	Numbers(ctx context.Context, scope Scope) ([]Number, error)
	// Number returns one number.
	Number(ctx context.Context, scope Scope, id uuid.UUID) (Number, error)

	// Prepare readies one number to be linked: it records which number is about
	// to be linked and which Bolu answers it, so the warning can be accepted
	// against a row and the link has somewhere to land. It links nothing.
	Prepare(ctx context.Context, scope Scope, request LinkRequest) (Number, error)
	// MessageByID returns one message, which is what serving its attachment needs.
	MessageByID(ctx context.Context, scope Scope, id uuid.UUID) (Inbound, error)
	// MediaBytes reads one stored attachment.
	MediaBytes(ctx context.Context, key string) ([]byte, error)

	// BeginLink starts linking one number and answers the attempt to watch.
	//
	// It refuses until the terms-of-service warning has been accepted and a Bolu
	// has been chosen, because both are decisions a person has to make and
	// neither can be defaulted: the warning is a risk somebody accepts, and the
	// Bolu is who answers the customers.
	BeginLink(ctx context.Context, scope Scope, request LinkRequest) (Link, error)
	// LinkState reports where an attempt stands, and finishes the link when the
	// phone has paired. It is what the screen polls or streams while a person
	// scans.
	LinkState(ctx context.Context, scope Scope, linkID string) (Link, error)
	// PairCode asks for the phone-code alternative to scanning.
	PairCode(ctx context.Context, scope Scope, linkID string, phone string) (Link, error)
	// CancelLink abandons an attempt.
	CancelLink(ctx context.Context, scope Scope, linkID string) error
	// AcceptTerms records that the workspace accepted the warning, which is what
	// unblocks BeginLink.
	AcceptTerms(ctx context.Context, scope Scope, id uuid.UUID) (Number, error)
	// AssignAgent chooses which Bolu answers this number.
	AssignAgent(ctx context.Context, scope Scope, id uuid.UUID, agentID uuid.UUID) (Number, error)
	// SetLimit changes the number's hourly ceiling.
	SetLimit(ctx context.Context, scope Scope, id uuid.UUID, limit int) (Number, error)
	// SetLabel renames the number as the workspace refers to it.
	SetLabel(ctx context.Context, scope Scope, id uuid.UUID, label string) (Number, error)
	// Unlink ends the session and clears what was stored. The message history
	// stays: what the customers wrote is not the session's.
	Unlink(ctx context.Context, scope Scope, id uuid.UUID) (Number, error)

	// Messages returns what customers wrote, newest first.
	Messages(ctx context.Context, scope Scope, filter MessageFilter) ([]Inbound, error)
	// History returns one conversation oldest first, for the connection a call
	// names. It is what the reading tool hands a model.
	History(ctx context.Context, scope Scope, integrationID uuid.UUID, chatJID string, limit int) ([]Inbound, error)
	// NumberByIntegration answers which number a connection speaks as, which is
	// what a tool call that names only the connection needs.
	NumberByIntegration(ctx context.Context, scope Scope, integrationID uuid.UUID) (uuid.UUID, error)
	// Outbox returns what this number has been asked to send, newest first.
	Outbox(ctx context.Context, scope Scope, filter OutboxFilter) ([]Outbox, error)

	// --- what the gateway does ----------------------------------------------

	// Assignments returns the numbers the fleet is expected to serve. It is the
	// one call that is not tenant-scoped, and the reason is the directory table's:
	// a gateway has to find out which tenants have a number before it can open
	// any workspace's scope.
	Assignments(ctx context.Context) ([]Assignment, error)

	// Paired is what the transport calls when a phone finished pairing. It is the
	// one write that turns an attempt into a number that works: the connection
	// the hub shows is created, the number is marked connected, and the fleet is
	// told to serve it.
	Paired(ctx context.Context, scope Scope, paired PairedLink) error

	// Report records what the transport knows about a number: online, offline, or
	// refused. A status that needs a person to act alerts them.
	Report(ctx context.Context, scope Scope, request ReportRequest) (Number, error)

	// Claim returns the messages of one number that may go out now, claiming them
	// in the same step and holding them to the hourly ceiling. What it holds is
	// reported rather than dropped, because a limit is a pace.
	Claim(ctx context.Context, scope Scope, id uuid.UUID, limit int) (Claimed, error)

	// Sent records that a message left, with WhatsApp's own id for it.
	Sent(ctx context.Context, scope Scope, id uuid.UUID, waMessageID string, at time.Time) error
	// Retry puts a message back in the queue with a later time, which is what a
	// transient failure deserves.
	Retry(ctx context.Context, scope Scope, id uuid.UUID, reason string, at time.Time) error
	// Failed records that a message will not be retried, so it is visible rather
	// than dropped.
	Failed(ctx context.Context, scope Scope, id uuid.UUID, reason string, attempts int) error

	// Reclaim puts messages a dead gateway left mid-send back in the queue.
	Reclaim(ctx context.Context, scope Scope, olderThan time.Time) (int, error)

	// Enqueue records one message to send. It is what the integration adapter
	// calls when an approved action reaches WhatsApp, so nothing in this module
	// decides that a message should go out.
	Enqueue(ctx context.Context, scope Scope, request EnqueueRequest) (Outbox, error)

	// Receive records one inbound message and answers whether it was new.
	//
	// It is the whole receive path in one call: the message is deduplicated,
	// stored, and handed to whoever answers — the routine watching the event, or
	// the Bolu assigned to the number. A redelivery answers false and opens
	// nothing, which is what makes the path idempotent rather than merely
	// careful.
	Receive(ctx context.Context, scope Scope, numberID uuid.UUID, message Inbound) (Inbound, bool, error)

	// PendingMedia returns the messages whose bytes have not been fetched.
	PendingMedia(ctx context.Context, scope Scope, limit int) ([]Inbound, error)
	// StoreMedia records where a message's bytes were stored, or that they could
	// not be.
	StoreMedia(ctx context.Context, scope Scope, id uuid.UUID, media Media) error
}

// LinkRequest is one number to link.
type LinkRequest struct {
	// Phone is what the person says the number is, as E.164. It is used for the
	// phone-code alternative; the authoritative value comes back from WhatsApp.
	Phone string
	Label string
	// AgentID is the Bolu that answers this number. Required: without it an
	// inbound message has no reader.
	AgentID uuid.UUID
}

// ReportRequest is what the transport knows about a number.
type ReportRequest struct {
	NumberID uuid.UUID
	Status   string
	// Reason explains a status a person has to act on, in their words.
	Reason string
	// At is when the observation was made, so a stale report cannot move a
	// number's last-seen backwards.
	At time.Time
}

// EnqueueRequest is one message to send.
type EnqueueRequest struct {
	NumberID uuid.UUID
	ToJID    string
	Kind     string
	Text     string
	// MediaKey is where the attachment was stored, for a message that carries
	// one.
	MediaKey string
	// DraftID is the draft the message came from, when it came from one.
	DraftID uuid.UUID
}
