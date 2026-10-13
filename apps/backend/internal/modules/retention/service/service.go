// Package service implements the retention pass.
//
// One pass over the fleet, and the shape of it follows from what it must not do:
//
//   - It removes *content* and never rows. A task step keeps its kind, its tool, its token
//     counts and when it ran, so the history a person reads is the same afterwards; only the
//     input and the output — a copy of a conversation — go.
//   - It runs per workspace rather than in one statement. Every table it prunes is protected by
//     Row Level Security, so there is no such thing as "prune every tenant" without opening a
//     workspace scope, and opening one per tenant is what makes the isolation real rather than
//     a filter somebody has to remember.
//   - It is a loop rather than a durable workflow, for the reason the billing sweep is one:
//     nothing in it waits. `retention_targets.next_prune_at` is what makes a missed pass a pass
//     that is still due, so a process that was down for a day catches up on its next run
//     instead of skipping the day.
//   - One workspace that cannot be pruned does not stop the pass. The rest of the fleet is
//     waiting, and this one is due again on the interval.
package service

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/retention/domain"
)

// Deps are the service dependencies.
type Deps struct {
	Repository domain.Repository
	// Steps prunes a task's raw step contents. Optional: without it the pass removes
	// attachments only, and says so rather than reporting a silent zero.
	Steps domain.StepPruner
	// Attachments removes attachments past their window, row and object together.
	// Optional for the same reason.
	Attachments domain.AttachmentPruner
	Logger      *zap.Logger
	Config      Config
	// Clock is the source of "now", so the windows are testable.
	Clock func() time.Time
	// Erasures is the record of what was removed on request: the evidence a person who asked has
	// a right to be answered with. Optional: without it an erasure request is refused rather than
	// done silently.
	Erasures domain.ErasureRepository
	// Workspaces removes a workspace and everything that cascades from it, and reads the two names
	// the erasure record keeps. Optional: without it an erasure request is refused rather than
	// half-done.
	Workspaces domain.WorkspaceEraser
	// Objects removes every object a workspace owns: the attachments and the sandboxed content,
	// which live in a bucket rather than in Postgres. Optional, for the same reason.
	Objects domain.ObjectEraser
}

// Config is the policy the service applies.
type Config struct {
	// StepWindow is how long a task step's raw payload is kept. Zero takes the module
	// default of thirty days.
	StepWindow time.Duration
	// AttachmentWindow is how long an attachment is kept. Zero keeps attachments as long
	// as the message that carries them, and the pass then removes none.
	AttachmentWindow time.Duration
	// Interval is how far forward a workspace's next pass is scheduled after one runs.
	// Zero takes the default of a day.
	Interval time.Duration
	// Limit bounds how many steps and how many attachments one workspace's pass removes.
	Limit int
	// SweepLimit bounds how many workspaces one pass looks at.
	SweepLimit int
}

// DefaultInterval is how long a workspace waits between passes.
const DefaultInterval = 24 * time.Hour

// DefaultSweepLimit bounds how many workspaces one pass looks at.
const DefaultSweepLimit = 200

// Service is the retention implementation.
type Service struct {
	deps     Deps
	settings Config
}

// New builds the service.
func New(deps Deps, config Config) *Service {
	if deps.Clock == nil {
		deps.Clock = time.Now
	}

	return &Service{deps: deps, settings: config}
}

// Policy is what the deployment's settings mean in the terms the pass works in.
//
// It is exported because it is the answer to "what does this deployment actually keep", which
// is a question an operator asks and a screen may one day show, and because deriving it in one
// place is what stops the config's zeroes from meaning three different things in three call
// sites.
func (s *Service) Policy() domain.Policy {
	window := s.settings.StepWindow
	if window <= 0 {
		window = domain.DefaultStepWindow
	}

	limit := s.settings.Limit
	if limit <= 0 {
		limit = domain.DefaultLimit
	}

	// A zero attachment window means "keep them with their message" — it is not a window of
	// zero seconds, which would delete every attachment on the next pass. The distinction is
	// the whole reason the value is expressed as a duration rather than as a day count.
	return domain.Policy{
		StepWindow:       window,
		AttachmentWindow: s.settings.AttachmentWindow,
		Limit:            limit,
	}
}

// Run makes one pass over every workspace that is due.
func (s *Service) Run(ctx context.Context) (domain.RunResult, error) {
	var result domain.RunResult

	if s.deps.Repository == nil {
		return result, domain.ErrNoRepository
	}

	now := s.deps.Clock()

	// The run is opened before any pruning: a pass that dies half way leaves a row saying
	// it started and never finished, which is exactly the state an operator needs to see.
	runID, err := s.deps.Repository.StartRun(ctx)
	if err != nil {
		return result, err
	}

	due, err := s.deps.Repository.DueTargets(ctx, now, s.sweepLimit())
	if err != nil {
		// A pass that cannot even read its queue failed as a whole, so it has nothing
		// to report per workspace and the run is closed with the reason.
		s.close(ctx, runID, result, err)

		return result, err
	}

	var failure error

	for _, workspaceID := range due {
		if ctx.Err() != nil {
			failure = ctx.Err()
			break
		}

		scope := domain.Scope{WorkspaceID: workspaceID}
		result.Workspaces++

		pruned, err := s.pruneWorkspace(ctx, scope, now)
		result.StepsPruned += pruned.Steps
		result.Attachments += pruned.Attachments
		result.BytesFreed += pruned.Bytes
		if err != nil {
			// One workspace must not stop the pass, and it must not stop the run from
			// being closed either — the other workspaces are waiting.
			failure = err
			s.warn("could not prune a workspace's content",
				zap.String("workspace", workspaceID.String()), zap.Error(err))
		}

		// The next pass is scheduled whether or not this one worked, so a workspace that
		// failed is retried on the interval rather than in a tight loop — and a pass that
		// never ran is caught by the next one rather than lost.
		if err := s.deps.Repository.TouchTarget(ctx, scope, now.Add(s.interval())); err != nil {
			s.warn("could not schedule a workspace's next prune",
				zap.String("workspace", workspaceID.String()), zap.Error(err))
		}
	}

	s.close(ctx, runID, result, failure)

	return result, failure
}

// pruned is what one workspace's pass removed.
type pruned struct {
	Steps       int
	Attachments int
	Bytes       int64
}

// pruneWorkspace prunes one workspace's steps and attachments.
//
// The two are independent: a deployment may run with a storage driver but no attachment policy,
// or the reverse, and one of them failing must not skip the other. So each is attempted and its
// error collected, rather than the first failure returning.
func (s *Service) pruneWorkspace(ctx context.Context, scope domain.Scope, now time.Time) (pruned, error) {
	policy := s.Policy()
	var out pruned
	var failures []error

	if s.deps.Steps != nil && policy.StepWindow > 0 {
		steps, err := s.deps.Steps.PruneSteps(ctx, scope, now.Add(-policy.StepWindow), policy.Limit)
		out.Steps = steps.Count
		if err != nil {
			failures = append(failures, err)
		} else if steps.Remaining > 0 {
			// Not a failure, and worth saying: a workspace with more past the window
			// than one batch removes is a deployment whose batch is too small for its
			// history, and the backlog only grows.
			s.warn("a workspace still has prunable steps after its pass",
				zap.String("workspace", scope.WorkspaceID.String()),
				zap.Int("remaining", steps.Remaining), zap.Int("limit", policy.Limit))
		}
	}

	if s.deps.Attachments != nil && policy.AttachmentWindow > 0 {
		attachments, err := s.deps.Attachments.PruneAttachments(
			ctx, scope, now.Add(-policy.AttachmentWindow), policy.Limit)
		out.Attachments = attachments.Count
		out.Bytes = attachments.BytesFreed
		if err != nil {
			failures = append(failures, err)
		}
	}

	return out, errors.Join(failures...)
}

// close writes what the run did, and reports a failure it could not record.
//
// A run log that cannot be written is not swallowed: it is the only durable trace the job
// leaves, so losing it is a failure of the job itself.
func (s *Service) close(ctx context.Context, runID int64, result domain.RunResult, failure error) {
	if err := s.deps.Repository.FinishRun(ctx, runID, result, failure); err != nil {
		s.warn("could not record the retention run", zap.Error(err))
	}
}

// interval takes the deployment's schedule or the default.
func (s *Service) interval() time.Duration {
	if s.settings.Interval > 0 {
		return s.settings.Interval
	}

	return DefaultInterval
}

// sweepLimit takes the deployment's bound or the default.
func (s *Service) sweepLimit() int {
	if s.settings.SweepLimit > 0 {
		return s.settings.SweepLimit
	}

	return DefaultSweepLimit
}

// warn logs one workspace's failure, and stays quiet when there is no logger to write to —
// which is what a unit test passes when it does not care about the log.
func (s *Service) warn(message string, fields ...zap.Field) {
	if s.deps.Logger != nil {
		s.deps.Logger.Warn(message, fields...)
	}
}
