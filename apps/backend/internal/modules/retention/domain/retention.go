// Package domain defines the retention contract: how long the deployment keeps what a
// customer said and what a model answered, and which modules do the removing.
//
// The rule this module exists to express is one sentence long: a task's *shape* is kept for
// good and its raw payload is not. What a step was — which tool, how many tokens, when it ran —
// is what the task history and the cost report are built from; the input and the output are a
// copy of a conversation, which is the part with a clock on it. A retention rule that removed
// whole rows would take the history with it, and the deployment would have to choose between
// keeping records and keeping less data.
//
// This module owns the schedule and the policy and nothing else. The tables being pruned
// belong to the task and chat modules, so they are the modules that prune them: the step
// bulk-update is the task repository's, and an attachment takes its row *and* its object
// storage key with it, which is the chat module's business because it is the module that owns
// the bucket path. The container adapts the two ports below onto those implementations.
package domain

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Errors this module returns.
var (
	// ErrInvalidInput is a request that cannot be honoured.
	ErrInvalidInput = errors.New("retention: invalid input")
	// ErrNoRepository is a deployment without a database.
	ErrNoRepository = errors.New("retention: no repository configured")
)

// Scope is the tenant a pass runs as.
//
// It carries no user on purpose. The job is the deployment acting on its own schedule, not a
// person asking for something, so the scope a pruned statement runs in is a workspace and
// nothing else — and Row Level Security, which keys on the workspace, is what keeps one
// tenant's sweep out of another's rows.
type Scope struct {
	WorkspaceID uuid.UUID
}

// IsZero reports whether the scope names no workspace.
func (s Scope) IsZero() bool { return s.WorkspaceID == uuid.Nil }

// Policy is how long each kind of content is kept.
type Policy struct {
	// StepWindow is how long a task step's raw input and output are kept.
	StepWindow time.Duration
	// AttachmentWindow is how long an attachment is kept. Zero means attachments are
	// kept as long as the message that carries them, which is a deployment that has no
	// attachment policy — and a pass that then removes none.
	AttachmentWindow time.Duration
	// StagedTTL is how long an upload that was never named in a message is kept.
	//
	// It is not a window on what a customer said: nothing was ever sent with this file,
	// so the deployment that keeps every message forever still has no reason to keep it.
	// That is why it has a default of its own rather than following AttachmentWindow,
	// whose zero means "keep".
	StagedTTL time.Duration
	// Limit bounds how many steps and how many attachments one workspace's pass
	// removes, so a deployment with years of history makes progress in slices rather
	// than in one unbounded statement.
	Limit int
}

// DefaultStepWindow is how long a task step's raw payload is kept when the deployment does not
// say otherwise.
//
// Thirty days, and the number is a judgement rather than a law: it is long enough that a task
// somebody is investigating is still fully readable, and short enough that the words of a
// customer's conversation are not kept indefinitely by a deployment nobody is watching.
const DefaultStepWindow = 30 * 24 * time.Hour

// DefaultStagedTTL is how long an upload that was never named in a message is kept when the
// deployment does not say otherwise.
//
// A day, and the number is a judgement rather than a law: it is long enough that a client which
// uploaded a file and then lost its connection can still send the message that names it, and
// short enough that a file nobody ever sent does not sit in the bucket indefinitely.
const DefaultStagedTTL = 24 * time.Hour

// DefaultLimit bounds one workspace's pass.
const DefaultLimit = 500

// PrunedSteps is what one workspace's step prune removed.
type PrunedSteps struct {
	// Count is how many steps had their contents removed.
	Count int
	// Remaining is how many steps of that workspace were already past the window and
	// still hold their contents, which is what tells an operator whether one pass kept
	// up with the backlog or whether the batch is too small.
	Remaining int
}

// PrunedAttachments is what one workspace's attachment sweep removed.
type PrunedAttachments struct {
	// Count is how many attachments were removed, row and object together.
	Count int
	// BytesFreed is what object storage gave back, which is the number an operator
	// watches when the bucket is what is growing.
	BytesFreed int64
}

// StepPruner removes the raw payload of a task's steps.
//
// The row stays: the step's kind, its tool, its sequence, its token counts and when it ran are
// what a task history is made of, and they are not content. Only `input` and `output` go, and
// the step is marked so a reader can tell an empty payload from a pruned one.
type StepPruner interface {
	PruneSteps(ctx context.Context, scope Scope, olderThan time.Time, limit int) (PrunedSteps, error)
}

// AttachmentPruner removes attachments past their window, row and object together.
//
// It is one port rather than two calls because the two halves must not drift: a row removed
// without its object leaks storage forever, and an object removed without its row leaves a
// message pointing at bytes that are gone.
//
// The staged sweep is on the same port for the same reason. A staged upload is a different
// kind of thing — nothing was ever sent with it — but it is removed the same way, and an
// operator reading this interface should see every way an attachment leaves.
type AttachmentPruner interface {
	PruneAttachments(ctx context.Context, scope Scope, olderThan time.Time, limit int) (PrunedAttachments, error)
	// PruneStagedAttachments removes the uploads that were never named in a message and are
	// past the staged TTL.
	PruneStagedAttachments(ctx context.Context, scope Scope, olderThan time.Time, limit int) (PrunedAttachments, error)
}

// ErasureService is the half of the module the erasure endpoints call: run a request, and read back
// what one recorded.
//
// It is declared separately from Service so the container can install the erasure routes only when
// the deployment wired the three ports they need — a handler built from a service that would refuse
// every call is a screen that exists and cannot work.
type ErasureService interface {
	// Erase removes a workspace's data on request.
	Erase(ctx context.Context, request ErasureRequest) (ErasureResult, error)
	// FindErasure answers whether a workspace's data was erased.
	FindErasure(ctx context.Context, workspaceID uuid.UUID) (Erasure, error)
	// Erasures returns the newest erasures, which is what an auditor reads.
	Erasures(ctx context.Context, limit int) ([]Erasure, error)
}

// Service is what the container calls: one pass, and the policy it applies — plus the erasure half,
// which is the same policy answering a request instead of a clock.
//
// `Policy` is on the interface rather than kept private because "what does this deployment
// actually keep" is a question an operator asks, and deriving it in one place is what stops a
// configured zero from meaning three different things in three call sites.
//
// The erasure methods are on this interface rather than on a second one because a deployment that
// has the retention module has both: the ports an erasure needs are wired by the same assembly, and
// a caller holding the module should not have to type-assert to ask it to erase something.
type Service interface {
	ErasureService
	// Run makes one pass over every workspace that is due, and reports what it did.
	Run(ctx context.Context) (RunResult, error)
	// Policy is the window and the batch this deployment applies.
	Policy() Policy
}

// Repository is the storage this module owns: the queue of workspaces due a pass, and the
// record of what each pass did.
type Repository interface {
	// DueTargets returns the workspaces whose next pass is due, oldest first.
	//
	// It is the one read with no tenant scope, and it carries a workspace id and a moment
	// and nothing else: the sweep cannot open a scope until a row tells it which one.
	DueTargets(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error)
	// TouchTarget moves a workspace's next pass forward.
	TouchTarget(ctx context.Context, scope Scope, next time.Time) error

	// StartRun opens a run record and returns its id.
	StartRun(ctx context.Context) (int64, error)
	// FinishRun closes a run record with what it did.
	FinishRun(ctx context.Context, id int64, result RunResult, failure error) error
	// Runs returns the newest runs, which is what a monitor reads.
	Runs(ctx context.Context, limit int) ([]Run, error)
}

// RunResult is what one pass over the fleet did.
type RunResult struct {
	Workspaces  int   `json:"workspaces"`
	StepsPruned int   `json:"steps_pruned"`
	Attachments int   `json:"attachments_pruned"`
	BytesFreed  int64 `json:"bytes_freed"`
}

// Run is one recorded pass.
type Run struct {
	ID          int64
	StartedAt   time.Time
	FinishedAt  time.Time
	StepsPruned int
	Attachments int
	BytesFreed  int64
	Error       string
}

// Done reports whether the run closed, which is what tells a reader whether the process died
// mid-pass or the pass finished.
func (r Run) Done() bool { return !r.FinishedAt.IsZero() }
