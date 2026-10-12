// Package domain defines the WhatsApp gateway contract: the number that stays
// online, the messages it carries, the queue that paces them, and the one
// process that owns the connection.
//
// Three rules run through it:
//
//   - one number has exactly one owner. A session that two processes drive
//     sends every message twice and gets the number banned, so ownership is a
//     lease rather than a convention.
//   - the session material is sealed with the workspace's own key and never
//     read outside the scope of the workspace that owns it.
//   - nothing leaves without a person's approval. This module carries an
//     approved action out; it never decides that an action should happen.
package domain

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Errors the module reports.
var (
	// ErrNotFound means no such number, link attempt, or message in this
	// workspace.
	ErrNotFound = errors.New("whatsapp: not found")
	// ErrInvalidInput means the request cannot be acted on as written.
	ErrInvalidInput = errors.New("whatsapp: invalid input")
	// ErrNotConfigured means no transport is wired, which is a deployment
	// without a gateway rather than a mistake by the caller.
	ErrNotConfigured = errors.New("whatsapp: the gateway is not configured")
	// ErrUnavailable means the runtime is there and WhatsApp could not be
	// reached: no network, no route, or a service that is down. It is separate
	// from a failure of the request, because the answer for a person is "try
	// again shortly" rather than "that did not work".
	ErrUnavailable = errors.New("whatsapp: WhatsApp is not reachable")
	// ErrTermsNotAccepted means the terms-of-service warning was not accepted,
	// so a link cannot start.
	ErrTermsNotAccepted = errors.New("whatsapp: the terms of service warning was not accepted")
	// ErrLinkExpired means the QR ran out of time. It is separate from a failure
	// because the answer is a new code, not an error a person has to read.
	ErrLinkExpired = errors.New("whatsapp: the link code expired")
	// ErrAlreadyLinked means this number is linked to another workspace. One
	// WhatsApp session belongs to the number, so a second tenant cannot have it.
	ErrAlreadyLinked = errors.New("whatsapp: this number is already linked")
	// ErrNotConnected means the number is not online, so nothing can be sent as
	// it.
	ErrNotConnected = errors.New("whatsapp: the number is not connected")
	// ErrHourlyLimit means the number spent its allowance for this hour. It is a
	// hold rather than a refusal: the message stays queued.
	ErrHourlyLimit = errors.New("whatsapp: the hourly send limit is reached")
	// ErrNoAgent means the record exists but has no Bolu assigned, so an inbound
	// message has nobody to answer it.
	ErrNoAgent = errors.New("whatsapp: no bolu is assigned to this number")
)

// Scope is the tenant a call runs in.
type Scope struct {
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
}

// IsZero reports whether the scope carries no tenant at all.
func (s Scope) IsZero() bool { return s.WorkspaceID == uuid.Nil }

// Number statuses.
const (
	// StatusUnlinked is a number row that has no session yet. It exists between
	// the moment a person starts linking and the moment they finish.
	StatusUnlinked = "unlinked"
	// StatusLinking is a link attempt in progress.
	StatusLinking = "linking"
	// StatusConnected is a number with a live session.
	StatusConnected = "connected"
	// StatusNeedsRelink is a session WhatsApp refused, which a person fixes by
	// scanning again rather than by waiting.
	StatusNeedsRelink = "needs_relink"
	// StatusDisconnected is a session that stopped without being refused: a
	// reboot, a network, an unlinked phone. It reconnects on its own when it can.
	StatusDisconnected = "disconnected"
)

// Number is one linked WhatsApp number.
type Number struct {
	ID            uuid.UUID
	WorkspaceID   uuid.UUID
	IntegrationID uuid.UUID
	// Phone is E.164, as WhatsApp reports it.
	Phone string
	Label string
	// AgentID is the Bolu that answers this number. Zero means nobody has been
	// chosen yet, which is a state a person has to fix rather than one the
	// module guesses its way out of.
	AgentID      uuid.UUID
	Status       string
	StatusReason string
	// TOSAcceptedAt is when this workspace accepted the warning about linking a
	// number over an unsupported route. Nil blocks a link.
	TOSAcceptedAt *time.Time
	// HourlyLimit is how many messages this number may send in an hour.
	HourlyLimit int
	ConnectedAt *time.Time
	LastSeenAt  *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Connected reports whether the number currently has a live session.
func (n Number) Connected() bool { return n.Status == StatusConnected }

// Linked reports whether a session exists, whether or not it is online now.
func (n Number) Linked() bool {
	return n.Status == StatusConnected || n.Status == StatusDisconnected || n.Status == StatusNeedsRelink
}

// Session is the sealed device state of one number.
//
// It is opaque here on purpose: the module stores and hands it over, and only
// the transport knows what is inside. That is what lets the whatsmeow engine be
// replaced by the official WhatsApp Business Platform without this module
// learning a second serialization format.
type Session struct {
	NumberID    uuid.UUID
	WorkspaceID uuid.UUID
	Sealed      []byte
	// JID is the account the device belongs to, in the clear: it is what the
	// gateway matches against the number it was told to serve.
	JID       string
	UpdatedAt time.Time
}

// Link states.
const (
	// LinkWaiting is an attempt that has not produced a code yet.
	LinkWaiting = "waiting"
	// LinkCode is an attempt showing a code to scan.
	LinkCode = "code"
	// LinkPaired is an attempt that succeeded.
	LinkPaired = "paired"
	// LinkExpired is an attempt whose code ran out. The screen asks for a new
	// one rather than showing an error, because the answer is a new code.
	LinkExpired = "expired"
	// LinkFailed is an attempt the phone or the service refused.
	LinkFailed = "failed"
)

// Link is one attempt to link a number.
type Link struct {
	// ID is the transport's handle for this attempt.
	ID          string
	NumberID    uuid.UUID
	WorkspaceID uuid.UUID
	State       string
	// Code is the QR payload, present while State is LinkCode.
	Code string
	// PairCode is the alternative for a phone that cannot scan, present when one
	// was asked for.
	PairCode string
	// Phone is the number being linked, once the transport knows it.
	Phone     string
	ExpiresAt time.Time
	// Reason explains a failed or expired attempt in the user's words.
	Reason string
}

// Message kinds.
const (
	KindText     = "text"
	KindImage    = "image"
	KindVideo    = "video"
	KindAudio    = "audio"
	KindDocument = "document"
	KindSticker  = "sticker"
	KindOther    = "other"
)

// Media statuses.
const (
	// MediaPending is a message whose bytes have not been fetched yet. The
	// receive path records the message and moves on, so a large file never holds
	// up the listener.
	MediaPending = "pending"
	// MediaReady is a message whose bytes are in object storage.
	MediaReady = "ready"
	// MediaFailed is a message whose bytes could not be fetched. It is said out
	// loud rather than left pending forever.
	MediaFailed = "failed"
)

// Media is what has to be fetched after a message is recorded.
type Media struct {
	// Ref is the transport's own handle on the bytes. Only the transport can
	// resolve it, so it is kept until the download happens.
	Ref    string
	Kind   string
	Size   int64
	Status string
	// Key is where the bytes were stored, once they were.
	Key   string
	Error string
}

// Inbound is one message that arrived.
type Inbound struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	NumberID    uuid.UUID
	// WaMessageID is WhatsApp's own id. It is the dedupe key, which is what
	// makes a redelivery harmless.
	WaMessageID string
	ChatJID     string
	SenderJID   string
	SenderName  string
	IsGroup     bool
	IsFromMe    bool
	Kind        string
	Text        string
	Media       Media
	ReceivedAt  time.Time
	// TaskID is the task the message opened, once one was opened.
	TaskID    uuid.UUID
	CreatedAt time.Time
}

// Outbox statuses.
const (
	// OutboxQueued is a message waiting for its turn.
	OutboxQueued = "queued"
	// OutboxSending is a message claimed by the gateway and in flight.
	OutboxSending = "sending"
	// OutboxSent is a message WhatsApp accepted.
	OutboxSent = "sent"
	// OutboxFailed is a message that will not be retried. It is visible rather
	// than dropped: a person can see that an answer did not go out.
	OutboxFailed = "failed"
	// OutboxHeld is a message waiting on a limit. It is a pace, not a refusal,
	// so the queue keeps it and tries again.
	OutboxHeld = "held"
)

// Outbox is one message waiting to leave.
type Outbox struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	NumberID    uuid.UUID
	ToJID       string
	Kind        string
	Text        string
	MediaKey    string
	// DraftID is the draft this message came from, when it came from one.
	DraftID   uuid.UUID
	Status    string
	Attempts  int
	LastError string
	NotBefore time.Time
	ClaimedAt *time.Time
	// WaMessageID is WhatsApp's id for the sent message, once it left.
	WaMessageID string
	SentAt      *time.Time
	CreatedAt   time.Time
}

// ToSend is one message the transport is asked to deliver.
type ToSend struct {
	ID       uuid.UUID
	ToJID    string
	Kind     string
	Text     string
	MediaKey string
}

// Snapshot is everything the transport needs to speak as one number.
//
// It is a value rather than an id so the transport never reads the database: the
// gateway is the only thing that talks to WhatsApp, and giving it a reason to
// open a workspace's scope would be giving it a reason to read a tenant it is
// not serving at that moment.
type Snapshot struct {
	NumberID    uuid.UUID
	WorkspaceID uuid.UUID
	Phone       string
	// Session is nil for a number that was never linked.
	Session *Session
}

// Assignment is one directory row: a number the fleet is expected to serve.
type Assignment struct {
	NumberID    uuid.UUID
	WorkspaceID uuid.UUID
}

// Transport is the WhatsApp side: what can be done with a number.
//
// It is a port because two things implement it and a third is planned. The
// whatsmeow engine runs inside the gateway process and is the real connection;
// the HTTP client the API holds reaches that engine; and the official WhatsApp
// Business Platform is meant to slot in behind the same interface (E9.6), which
// is the whole reason the seam exists.
type Transport interface {
	// Link starts an attempt for one number and returns its handle.
	Link(ctx context.Context, scope Scope, numberID uuid.UUID, phone string) (Link, error)
	// LinkState reports where an attempt stands.
	LinkState(ctx context.Context, id string) (Link, error)
	// PairCode asks for the phone-code alternative to a QR.
	PairCode(ctx context.Context, id string, phone string) (Link, error)
	// Cancel abandons an attempt and discards its session.
	Cancel(ctx context.Context, id string) error
	// Send delivers one message and answers WhatsApp's id for it.
	Send(ctx context.Context, snapshot Snapshot, message ToSend) (string, error)
	// Typing shows the typing indicator in one chat. It is what makes a reply
	// read as somebody writing rather than as a machine posting.
	Typing(ctx context.Context, snapshot Snapshot, toJID string) error
	// Unlink ends the session at WhatsApp and forgets it locally.
	Unlink(ctx context.Context, snapshot Snapshot) error
	// Online reports whether the transport currently holds this number.
	Online(ctx context.Context, numberID uuid.UUID) (bool, error)
}

// SessionStore persists the sealed device state of one number.
//
// Every method takes a scope, and that is not ceremony: the blob is sealed with
// one workspace's key, so the read that returns it must already be running inside
// that workspace or Row Level Security refuses it. The gateway always has the
// scope, because the directory row it followed supplies it.
type SessionStore interface {
	// Load returns the stored session, or ErrNotFound when the number was never
	// linked.
	Load(ctx context.Context, scope Scope, numberID uuid.UUID) (Session, error)
	Save(ctx context.Context, scope Scope, session Session) error
	// ForgetSession deletes a device. It is named apart from the number's own
	// delete because the two are different acts: one ends a session, the other
	// removes the number.
	ForgetSession(ctx context.Context, scope Scope, numberID uuid.UUID) error
}

// Sealer seals and opens a device blob with one workspace's key.
//
// It is a port rather than a call to the platform's crypto because the key has
// to be the workspace's own: a blob sealed under one tenant's key must be
// unopenable under another's, and that is a property of the key ring the
// integration module already keeps.
type Sealer interface {
	Seal(ctx context.Context, workspaceID uuid.UUID, plaintext []byte) ([]byte, error)
	Open(ctx context.Context, workspaceID uuid.UUID, ciphertext []byte) ([]byte, error)
}

// Directory lists the numbers a gateway instance is expected to serve.
//
// It is the one read that is not tenant-scoped, for the reason
// `whatsapp_directory` states: a fleet worker has to discover which tenants have
// a number before it can open any workspace's scope at all.
type Directory interface {
	Serving(ctx context.Context) ([]Assignment, error)
}

// Lease decides which gateway instance owns one number.
//
// It exists because a WhatsApp session driven by two processes sends everything
// twice and gets the number banned. Redis holds it rather than Postgres because
// the value is a liveness fact with a deadline, not a record.
type Lease interface {
	// Acquire takes the lease when it is free or already ours, and answers
	// whether we hold it.
	Acquire(ctx context.Context, numberID uuid.UUID, owner string, ttl time.Duration) (bool, error)
	// Renew extends a lease we hold. False means somebody else has it now, which
	// is what a released or expired lease looks like from here.
	Renew(ctx context.Context, numberID uuid.UUID, owner string, ttl time.Duration) (bool, error)
	// Release gives it up, so a takeover is immediate rather than a wait.
	Release(ctx context.Context, numberID uuid.UUID, owner string) error
}

// Event is one entry in the workspace's live stream.
type Event struct {
	WorkspaceID uuid.UUID
	Type        string
	Payload     any
}

// Activity records what happened to a number in the workspace's stream, which is
// what puts it in front of a person without them opening the screen.
type Activity interface {
	Emit(ctx context.Context, event Event) error
}

// Alert is something a workspace's owners have to act on.
type Alert struct {
	// Kind is one of the Alert* constants.
	Kind        string
	NumberID    uuid.UUID
	Phone       string
	Status      string
	Reason      string
	WorkspaceID uuid.UUID
}

// Alert kinds.
const (
	// AlertNeedsRelink is a number WhatsApp refused. Waiting does not fix it.
	AlertNeedsRelink = "whatsapp.needs_relink"
	// AlertDisconnected is a number that went offline.
	AlertDisconnected = "whatsapp.disconnected"
	// AlertLimit is a number that reached its hourly ceiling. It is reported
	// because a person waiting for an answer should know it is paced, not lost.
	AlertLimit = "whatsapp.limit_reached"
)

// Alerter tells the workspace's owners that a number needs them.
//
// It is one port for what is two effects — a line in the live stream and an
// email to the owners — because they are one decision: "somebody has to know
// this". Splitting them would let a caller send one without the other.
type Alerter interface {
	Alert(ctx context.Context, scope Scope, alert Alert) error
}

// Replies opens the task an inbound message asks for.
//
// It is a port for the reason the routine module's is: this module does not know
// what a task is. A customer's message becomes work because that is what gives
// it a durable workflow and an approval gate.
type Replies interface {
	// Answer opens a task for the Bolu that answers one number.
	Answer(ctx context.Context, scope Scope, request AnswerRequest) (uuid.UUID, error)
}

// AnswerRequest is one inbound message that needs an answer.
type AnswerRequest struct {
	AgentID  uuid.UUID
	NumberID uuid.UUID
	// Phone is the customer's number, as E.164.
	Phone string
	// Name is what WhatsApp calls the sender, when it says.
	Name  string
	Title string
	// Prompt is what the Bolu is told, with the customer's own words in it.
	Prompt string
}

// Store persists numbers, sessions, messages, the queue, and the directory.
type Store interface {
	// Numbers
	Create(ctx context.Context, scope Scope, number Number) (Number, error)
	Get(ctx context.Context, scope Scope, id uuid.UUID) (Number, error)
	ByPhone(ctx context.Context, scope Scope, phone string) (Number, error)
	// ByIntegration returns the number a connection speaks as, which is how a tool
	// call that names only the connection finds its number.
	ByIntegration(ctx context.Context, scope Scope, integrationID uuid.UUID) (Number, error)
	List(ctx context.Context, scope Scope) ([]Number, error)
	Update(ctx context.Context, scope Scope, number Number) (Number, error)
	// SetStatus records a status and why, which is what the screen and the alert
	// both read.
	SetStatus(ctx context.Context, scope Scope, id uuid.UUID, status, reason string) (Number, error)
	// SetIntegration records the connection the hub created for this number. It is
	// written once, when a link finishes.
	SetIntegration(ctx context.Context, scope Scope, id, integrationID uuid.UUID) error
	// SetPhone records the number WhatsApp reported when the phone paired. Until
	// then the row carries what the person typed, which is a hint rather than the
	// number itself.
	SetPhone(ctx context.Context, scope Scope, id uuid.UUID, phone string) error
	// AcceptTerms records that the workspace accepted the warning about linking a
	// number over an unsupported route. It is idempotent and keeps the first
	// moment, because that is when the risk was taken.
	AcceptTerms(ctx context.Context, scope Scope, id uuid.UUID) (Number, error)
	// Touch records that the gateway saw the number, so "is it alive" is
	// answerable without asking the gateway.
	Touch(ctx context.Context, scope Scope, id uuid.UUID, at time.Time) error
	Delete(ctx context.Context, scope Scope, id uuid.UUID) error

	// Directory
	Publish(ctx context.Context, scope Scope, numberID uuid.UUID, enabled bool) error
	Serving(ctx context.Context) ([]Assignment, error)

	// Messages
	// Record stores an inbound message and answers whether it was new. A
	// redelivery answers false, which is what stops a duplicate task.
	Record(ctx context.Context, scope Scope, message Inbound) (Inbound, bool, error)
	Messages(ctx context.Context, scope Scope, filter MessageFilter) ([]Inbound, error)
	ByWaID(ctx context.Context, scope Scope, waMessageID string) (Inbound, error)
	// MessageByID returns one message, which is what serving its attachment needs.
	MessageByID(ctx context.Context, scope Scope, id uuid.UUID) (Inbound, error)
	PendingMedia(ctx context.Context, scope Scope, limit int) ([]Inbound, error)
	SetMedia(ctx context.Context, scope Scope, id uuid.UUID, media Media) error
	AttachTask(ctx context.Context, scope Scope, id, taskID uuid.UUID) error

	// Outbox
	Enqueue(ctx context.Context, scope Scope, item Outbox) (Outbox, error)
	// Due returns the messages of one number that are ready, claiming them in the
	// same step so a second reader cannot pick up the same one.
	Due(ctx context.Context, scope Scope, numberID uuid.UUID, limit int) ([]Outbox, error)
	MarkSent(ctx context.Context, scope Scope, id uuid.UUID, waMessageID string, at time.Time) error
	MarkHeld(ctx context.Context, scope Scope, id uuid.UUID, reason string, at time.Time) error
	// MarkRetry puts a message back in the queue after a failed attempt, keeping
	// the attempt count so a message that keeps failing is eventually parked
	// rather than retried forever.
	MarkRetry(ctx context.Context, scope Scope, id uuid.UUID, reason string, at time.Time) error
	MarkFailed(ctx context.Context, scope Scope, id uuid.UUID, reason string, attempts int) error
	// SentSince counts what left since a moment, which is how the hourly ceiling
	// is measured: it is a count of real sends rather than of a counter that can
	// drift.
	SentSince(ctx context.Context, scope Scope, numberID uuid.UUID, since time.Time) (int, error)
	Outbox(ctx context.Context, scope Scope, filter OutboxFilter) ([]Outbox, error)
	// Reclaim returns messages stuck in `sending` to the queue. It is what a
	// gateway killed mid-send leaves behind.
	Reclaim(ctx context.Context, scope Scope, olderThan time.Time) (int, error)
}

// MessageFilter narrows an inbound read.
type MessageFilter struct {
	NumberID uuid.UUID
	ChatJID  string
	Limit    int
}

// OutboxFilter narrows an outbox read.
type OutboxFilter struct {
	NumberID uuid.UUID
	Status   string
	Limit    int
}
