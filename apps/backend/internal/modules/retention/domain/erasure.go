// The erasure half of the retention module: removing a workspace's data on request.
//
// Retention is a clock and erasure is a request, but they are the same policy seen from two sides —
// how long this deployment keeps a customer's data, and what it does when the customer asks for it
// back. Putting them in one module is what keeps a single answer to "what do we hold about a
// workspace": the same code that knows a step's payload has a window knows the attachments live in
// two object-storage prefixes, and a second module would be a second list that drifts.
//
// Three things about the design are worth stating rather than discovering later:
//
//   - **Everything that points at a workspace cascades from it.** Every table with a `workspace_id`
//     is `REFERENCES workspaces (id) ON DELETE CASCADE`, which is what makes "delete the workspace"
//     a deletion of the tenant's rows rather than a walk over twenty tables that a new migration can
//     forget to extend.
//   - **Object storage is not a row and does not cascade.** Attachments and sandboxed content are
//     bytes in a bucket, so they are removed first and deliberately: a bucket object nothing points
//     at is a copy of a customer's file that no query will ever find again.
//   - **The evidence outlives the data.** The record names the workspace and the person who asked,
//     as values rather than as references, because both have to be answerable after the rows are
//     gone. That is the half of UU PDP that is easy to forget: the right to be answered.
package domain

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrErasureNotFound is what a lookup for a workspace's erasure returns when there is none.
var ErrErasureNotFound = errors.New("retention: no erasure recorded")

// ErasureRequest is a person asking for a workspace's data to be removed.
type ErasureRequest struct {
	WorkspaceID uuid.UUID
	// RequestedBy is who asked. It is kept as a value on the record, because the person may no
	// longer be a member — or may no longer exist — by the time the record is read.
	RequestedBy uuid.UUID
}

// Erasure is the outcome of one request.
type Erasure struct {
	ID              int64
	WorkspaceID     uuid.UUID
	WorkspaceName   string
	RequestedBy     uuid.UUID
	RequestedByMail string
	RequestedAt     time.Time
	CompletedAt     time.Time
	// Objects is what was removed from object storage: the attachments and the sandboxed content,
	// which live in a bucket rather than in Postgres.
	Objects int64
	// Rows is what the database cascaded from the workspace. It is reported rather than checked
	// against an expected total, because the number depends on how much work the tenant had done —
	// what matters is that the row exists and the workspace does not.
	Rows  int64
	Error string
}

// Done reports whether the erasure completed.
func (e Erasure) Done() bool { return !e.CompletedAt.IsZero() }

// ErasureResult is what one erasure removed.
type ErasureResult struct {
	Objects int64
	Rows    int64
}

// WorkspaceEraser removes a workspace and everything that cascades from it.
//
// It is one method rather than a walk over each module's delete, and that is the point: the cascade
// is a fact about the schema — every `workspace_id` references `workspaces` — so the deletion is one
// statement rather than twenty calls whose order somebody has to get right. A module that adds a
// table is covered by the foreign key it declares; a module that added one *without* the reference
// is a table the schema check is there to catch.
type WorkspaceEraser interface {
	// EraseWorkspace deletes the workspace and reports how many rows went with it.
	EraseWorkspace(ctx context.Context, workspaceID uuid.UUID) (int64, error)
	// WorkspaceName reads the name before it is deleted, so the record says "Toko Sinar" rather
	// than a uuid.
	WorkspaceName(ctx context.Context, workspaceID uuid.UUID) (string, error)
	// RequesterEmail reads the requester's address, for the same reason.
	RequesterEmail(ctx context.Context, userID uuid.UUID) (string, error)
}

// ObjectEraser removes every object one workspace owns.
//
// It is the module that owns the bucket path which implements this — the chat module, which writes
// attachments and sandboxed content — because the prefixes are its own knowledge. The job knows only
// that a workspace's bytes have to go.
type ObjectEraser interface {
	// EraseObjects removes everything under a workspace's prefixes and reports how many objects
	// went.
	EraseObjects(ctx context.Context, workspaceID uuid.UUID) (int64, error)
}

// ErasurePolicy is the three ports one erasure needs, declared together because a deployment wires
// them as a set: an erasure that had the record but not the object walk would report a success it
// did not achieve, and one that had the walk but not the deletion would leave the rows.
type ErasurePolicy interface {
	ErasureRepository
	WorkspaceEraser
	ObjectEraser
}

// ErasureRepository is the storage for the evidence.
type ErasureRepository interface {
	// Start opens a record and returns its id, before anything is removed.
	Start(ctx context.Context, request ErasureRequest, workspaceName, requesterEmail string) (int64, error)
	// Finish closes a record with what was removed, or with why it stopped.
	Finish(ctx context.Context, id int64, result ErasureResult, failure error) error
	// List returns the newest erasures, which is what an auditor reads.
	List(ctx context.Context, limit int) ([]Erasure, error)
	// Find returns one workspace's completed erasure, or ErrErasureNotFound.
	Find(ctx context.Context, workspaceID uuid.UUID) (Erasure, error)
}
