// Package domain defines the device module's contract: the Expo push address a phone registered, and
// the two writes a client makes about it.
//
// A device token is an address rather than a session. It is what the push sender will look up when a
// task finishes or a draft waits for a person, and the rules are deliberately small so the service
// can be tested without a database:
//
//   - **One row per account and token.** A phone re-registers on every launch; the second
//     registration updates the row it already has, because a table that grew per launch would be a
//     table of duplicates nobody can deduplicate later.
//   - **The token is the account's.** A workspace is recorded with it, but removing that workspace
//     must not remove the address, and another account's token is invisible rather than forbidden.
//     That invisibility is what lets the delete answer 404 without a second query.
package domain

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Platforms a device may register as, matching the check constraint on `device_tokens.platform`.
const (
	// PlatformIOS is an iPhone.
	PlatformIOS = "ios"
	// PlatformAndroid is an Android phone.
	PlatformAndroid = "android"
)

// Platforms lists every platform a registration may name, in the order the check constraint reads.
var Platforms = []string{PlatformIOS, PlatformAndroid}

// TokenMaxLength bounds one push token. Expo's tokens are far shorter, so the ceiling exists to
// refuse a body that is not a token at all rather than to describe a real one.
const TokenMaxLength = 512

// Errors the service and the handler map onto responses.
var (
	// ErrInvalidInput is a registration that cannot be honoured: no token, a token longer than a
	// push address can be, or a platform the app does not run on.
	ErrInvalidInput = errors.New("device: invalid input")
	// ErrNotFound is a token that is not this account's, or not registered at all. The two are
	// deliberately one answer.
	ErrNotFound = errors.New("device: not found")
)

// Scope is the tenant a call runs as. The user owns the token; the workspace is where it was last
// registered from.
type Scope struct {
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
}

// IsZero reports whether the scope names no workspace.
func (s Scope) IsZero() bool { return s.WorkspaceID == uuid.Nil }

// Device is one registered push address.
type Device struct {
	ID uuid.UUID
	// UserID is the account the address belongs to, which is also the only account that can read or
	// delete it.
	UserID uuid.UUID
	// WorkspaceID is where the registration came from. Zero when the workspace was removed; the
	// address survives it.
	WorkspaceID uuid.UUID
	Platform    string
	Token       string
	// AppVersion is what the phone reported. Empty when it did not say.
	AppVersion string
	// LastSeenAt moves on every registration, which is what tells a sender the address is still
	// live.
	LastSeenAt time.Time
	CreatedAt  time.Time
}

// RegisterRequest is one registration from a phone.
type RegisterRequest struct {
	Token      string
	Platform   string
	AppVersion string
}

// Repository persists device tokens.
type Repository interface {
	// Register stores a token, or updates the row the same account already has for it. The row it
	// answers with is the stored one, so `last_seen_at` is the moment of this registration.
	Register(ctx context.Context, scope Scope, request RegisterRequest) (Device, error)
	// Delete removes one of the caller's tokens. A token that is not the caller's is reported as
	// ErrNotFound rather than as a refusal.
	Delete(ctx context.Context, scope Scope, token string) error
}

// Service is what the HTTP handler calls.
type Service interface {
	// Register validates a registration and stores it, updating the row a repeated token already
	// has.
	Register(ctx context.Context, scope Scope, request RegisterRequest) (Device, error)
	// Delete removes one of the caller's tokens.
	Delete(ctx context.Context, scope Scope, token string) error
}
