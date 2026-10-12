package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
)

// Scope is the identity a query runs as. Row Level Security reads both parts:
// WorkspaceID is the active tenant and UserID is the authenticated user.
//
// The type lives in the domain so the service can pass it through without
// importing the database package; the repository translates it.
type Scope struct {
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
}

// IsZero reports whether the scope carries no identity at all.
func (s Scope) IsZero() bool {
	return s.UserID == uuid.Nil && s.WorkspaceID == uuid.Nil
}

// Mailer delivers a transactional email. It is declared here so the service
// does not depend on the mail platform package; the container adapts it.
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// HashToken hashes an invitation token for storage. Only the hash is persisted,
// so a database leak does not hand over usable invitations.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Tokens generates one-time tokens and the hash to store.
type Tokens interface {
	Generate() (raw string, hash string, err error)
}

// PlanLimits reports how many members a workspace's package allows.
//
// It is a port for the reason the other plan ports are: this module does not know
// what a subscription is, and the container hands it the module that does. Unwired
// means the deployment sells no packages, and nothing is capped.
type PlanLimits interface {
	MaxMembers(ctx context.Context, workspaceID uuid.UUID) (int, error)
}

// Clock reports the current time, so expiry rules can be tested.
type Clock func() time.Time
