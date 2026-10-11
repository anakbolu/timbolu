package domain

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// This file is the inbound half of the adapter contract: what a provider pushes
// at us, and how we decide whether it is really from them.

// Errors the webhook path reports.
var (
	// ErrSignatureInvalid means the request did not carry a signature this
	// deployment accepts. The endpoint answers 401 and queues nothing.
	ErrSignatureInvalid = errors.New("integration: the webhook signature is not valid")
	// ErrNoWebhook means an application that pushes nothing was called anyway.
	ErrNoWebhook = errors.New("integration: this application does not send webhooks")
	// ErrDeliveryExists means this delivery is already queued, which is what a
	// provider's retry looks like.
	ErrDeliveryExists = errors.New("integration: this delivery is already queued")
)

// TriggerAdapter is an adapter whose application pushes events at us.
//
// Verification lives with the adapter because only it knows how its provider
// signs: an HMAC over the body, a shared secret in a header, a JWT with a
// published key. The endpoint's job is to refuse anything this refuses.
type TriggerAdapter interface {
	Adapter
	// VerifyWebhook checks a request and returns the events it carries. A
	// signature that does not check out returns ErrSignatureInvalid; anything
	// else means the request was genuine but could not be read.
	//
	// It must not do slow work: the caller is a provider waiting for a 200.
	VerifyWebhook(ctx context.Context, request WebhookRequest) ([]TriggerEvent, error)
}

// WebhookRequest is what the endpoint hands the adapter.
type WebhookRequest struct {
	App string
	// Headers are the request's headers, lowercased, because providers disagree
	// about capitalisation and an adapter should not have to.
	Headers map[string]string
	// Body is the raw bytes, which is what a signature is computed over. A parsed
	// body would not verify.
	Body []byte
	// Query is the request's query string, for the providers that sign a token
	// into the URL instead of the body.
	Query map[string]string
	// ReceivedAt is when the request arrived, which a signature with a timestamp
	// is checked against.
	ReceivedAt time.Time
}

// Header reads a header, treating a missing one and an empty one the same way.
func (r WebhookRequest) Header(name string) string {
	return r.Headers[lower(name)]
}

// WebhookRef is the connection a webhook URL belongs to.
//
// A provider calls an endpoint with no session, so the URL has to say which
// workspace and which connection it is for, and the deployment has to be able to
// tell a URL it issued from one somebody made up. That is why the reference is
// signed rather than merely present in the path: the workspace is a UUID that
// appears in API responses, so an unsigned one would let a genuine delivery for
// one workspace be replayed against another.
type WebhookRef struct {
	WorkspaceID   uuid.UUID
	IntegrationID uuid.UUID
	App           string
}

// RefCodec signs and verifies a webhook reference.
type RefCodec interface {
	SignRef(ref WebhookRef) (string, error)
	VerifyRef(value string) (WebhookRef, error)
}

// QueuedEvent is one delivery waiting to be acted on.
type QueuedEvent struct {
	ID            uuid.UUID
	WorkspaceID   uuid.UUID
	IntegrationID uuid.UUID
	App           string
	DeliveryID    string
	Event         string
	Payload       []byte
	ReceivedAt    time.Time
	Attempts      int
}

// EnqueueInput is a verified delivery on its way into the queue.
type EnqueueInput struct {
	WorkspaceID   uuid.UUID
	IntegrationID uuid.UUID
	App           string
	DeliveryID    string
	Event         string
	Payload       []byte
}

// Queue is the durable list of deliveries that arrived and have not been acted
// on yet.
type Queue interface {
	// Enqueue stores a verified delivery. A delivery that is already queued
	// returns ErrDeliveryExists rather than a second row: providers retry, and a
	// retry is not a second event.
	Enqueue(ctx context.Context, input EnqueueInput) (QueuedEvent, error)
	// Pending returns the oldest unprocessed deliveries of a workspace.
	Pending(ctx context.Context, scope Scope, limit int) ([]QueuedEvent, error)
	// Processed marks a delivery as acted on.
	Processed(ctx context.Context, scope Scope, id uuid.UUID) error
	// Failed records an attempt that did not work, so a delivery that keeps
	// failing is visible rather than silently retried forever.
	Failed(ctx context.Context, scope Scope, id uuid.UUID, reason string) error
}

// lower lowercases a header name, so an adapter can ask for it the way its
// provider documents it without caring how Go canonicalised it.
func lower(value string) string {
	out := make([]byte, len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}
