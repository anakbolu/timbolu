package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// This file is what the transport needs from the module.
//
// It is five small ports rather than one big `Service` on purpose: the engine
// that owns the connection does four unrelated things with the product — it
// records what arrived, pulls what is due, reports what left, and says how a
// number is doing — and naming them separately is what keeps a driver from
// reaching parts of the product it has no business in. The concrete service
// implements all five; a driver only has to be handed the ones it uses.

// Inbox records what arrived.
type Inbox interface {
	// Receive records one inbound message and answers whether it was new. A
	// redelivery answers false, which is what stops a duplicate task.
	Receive(ctx context.Context, scope Scope, numberID uuid.UUID, message Inbound) (Inbound, bool, error)
	// PendingMedia returns the messages whose bytes have not been fetched.
	PendingMedia(ctx context.Context, scope Scope, limit int) ([]Inbound, error)
	// StoreMedia records where a message's bytes were stored, or that they could
	// not be.
	StoreMedia(ctx context.Context, scope Scope, id uuid.UUID, media Media) error
}

// Drain is what the transport asks for when it is ready to send.
type Drain interface {
	// Claim returns the messages of one number that may go out now, and how many
	// were held by the hourly ceiling.
	//
	// The ceiling is enforced here rather than by the sender because it is a
	// product rule: a number that has spent its hour is paced, not broken, and
	// what a person is told about it is written in the workspace's own words.
	Claim(ctx context.Context, scope Scope, numberID uuid.UUID, limit int) (Claimed, error)
	// Sent records that a message left, with WhatsApp's own id for it.
	Sent(ctx context.Context, scope Scope, id uuid.UUID, waMessageID string, at time.Time) error
	// Retry puts a message back in the queue with a later time. It is what a
	// transient failure deserves: a provider that was briefly unreachable is not
	// a reason to lose a customer's answer.
	Retry(ctx context.Context, scope Scope, id uuid.UUID, reason string, at time.Time) error
	// Failed records that a message will not be retried, so it stays visible.
	Failed(ctx context.Context, scope Scope, id uuid.UUID, reason string, attempts int) error
	// Reclaim puts messages a dead gateway left mid-send back in the queue.
	Reclaim(ctx context.Context, scope Scope, olderThan time.Time) (int, error)
}

// Claimed is one number's due work.
type Claimed struct {
	// Ready may be sent now, oldest first.
	Ready []Outbox
	// Held is how many the hourly ceiling pushed to later. It is reported so a
	// person can be told their message is paced rather than lost.
	Held int
}

// Status is what the transport knows about a number.
type Status interface {
	// Report records a status and why. A status that needs a person to act
	// alerts them.
	Report(ctx context.Context, scope Scope, request ReportRequest) (Number, error)
	// Number returns one number, which the transport reads for the number's phone
	// and its ceiling.
	Number(ctx context.Context, scope Scope, id uuid.UUID) (Number, error)
}

// LinkSink is told when an attempt to link a number finishes.
//
// The engine has already stored the sealed session by then — it is the only
// thing that can serialize a device — so what crosses this seam is the product
// fact: this number is now linked, here is who it is, and here is the account it
// answers as.
type LinkSink interface {
	Paired(ctx context.Context, scope Scope, paired PairedLink) error
}

// PairedLink is a number that finished pairing.
type PairedLink struct {
	NumberID uuid.UUID
	// Phone is what WhatsApp reported, in E.164 without the plus.
	Phone string
	// JID is the account the device answers as.
	JID string
}

// Watchers starts the routines watching an event a message announced.
//
// It is a port for the same reason the hub's is: this module does not know what a
// routine is, and a message that several routines asked about is their business
// rather than this one's.
type Watchers interface {
	// Fire starts the routines watching one event and reports how many started.
	Fire(ctx context.Context, scope Scope, event string) (int, error)
}

// Connections records a linked number as a connection the hub shows and the
// approval gate picks.
//
// It is a port because the hub owns connections: this module says "this number
// exists and the session handle that speaks as it is this", and the hub decides
// how a connection is stored, sealed, and granted to a Bolu. Linking a number
// without a connection would give the number a way to receive and no way to
// answer.
type Connections interface {
	Link(ctx context.Context, scope Scope, link ConnectionLink) (uuid.UUID, error)
	Disconnect(ctx context.Context, scope Scope, integrationID uuid.UUID) error
}

// ConnectionLink is one number as a connection.
type ConnectionLink struct {
	App string
	// AccountLabel is what a screen shows: the number, masked.
	AccountLabel string
	// Handle is the credential a tool call carries. For WhatsApp it names which
	// session speaks, and the hub seals it at rest like any other credential.
	Handle string
}

// MediaStore keeps the bytes of a message's attachment.
//
// It is a port because the engine downloads and the handler serves, and neither
// should know whether the bytes live on a disk or in an object store.
type MediaStore interface {
	// Put stores one message's bytes and answers where they went.
	Put(ctx context.Context, scope Scope, messageID uuid.UUID, media Media, content []byte) (StoredObject, error)
	// Get reads them back.
	Get(ctx context.Context, key string) ([]byte, error)
}

// StoredObject is where one attachment ended up.
type StoredObject struct {
	Key         string
	ContentType string
	Filename    string
	Size        int64
}
