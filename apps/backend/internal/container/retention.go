package container

import (
	"context"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	retentiondomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/retention/domain"
	retentionrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/retention/repository"
	retentionservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/retention/service"
	taskdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	workspacerepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/repository"
	"github.com/robzlabz/employeebot/packages/platform/config"
)

// openRetention builds the retention job: how long the deployment keeps what a customer said
// and what a model answered.
//
// It is assembled last, after the two modules whose tables it prunes, because it prunes none of
// them itself. The step bulk-update is the task repository's, and an attachment takes its row
// and its object with it, which is the chat service's — it is the module that owns the bucket
// path. The two adapters below are the only place the three meet, which is what the container
// is for.
func (c *Container) openRetention(_ context.Context, cfg *config.Config) error {
	if c.DB == nil {
		c.Logger.Info("retention is disabled: no database connection")
		return nil
	}

	c.Repositories.Retention = retentionrepo.New(c.DB)
	c.Retention = buildRetention(cfg, c.Repositories.Retention,
		taskStepPruner{steps: c.Repositories.Task},
		chatAttachmentPruner{chat: c.Services.Chat},
		retentionpolicy{
			records:    c.Repositories.Retention,
			workspaces: c.Repositories.Workspace,
			objects:    c.Services.Chat,
		},
		c.Logger)
	c.Services.Retention = c.Retention

	return nil
}

// buildRetention assembles the job from its parts.
//
// The configuration's days become the service's windows here and nowhere else, because a zero
// means two different things: the product default of thirty days for a step's raw payload, and
// "keep the file as long as the message that carries it" for an attachment. Deriving both in one
// place is what stops a caller from picking the other reading.
func buildRetention(cfg *config.Config, repository retentiondomain.Repository, steps retentiondomain.StepPruner, attachments retentiondomain.AttachmentPruner, erasure retentiondomain.ErasurePolicy, logger *zap.Logger) *retentionservice.Service {
	deps := retentionservice.Deps{
		Repository:  repository,
		Steps:       steps,
		Attachments: attachments,
		Logger:      logger,
	}
	if erasure != nil {
		deps.Erasures = erasure
		deps.Workspaces = erasure
		deps.Objects = erasure
	}

	return retentionservice.New(deps, retentionservice.Config{
		StepWindow:       time.Duration(cfg.Retention.StepDays) * 24 * time.Hour,
		AttachmentWindow: time.Duration(cfg.Retention.AttachmentDays) * 24 * time.Hour,
		// The staged TTL is the one window that is not expressed in days, because it is
		// not a policy about what a customer said: a file that was uploaded and never
		// sent has no message to be kept with, and an operator tuning it is tuning how
		// long a client may take to finish a send.
		StagedTTL: cfg.Storage.AttachmentStagedTTL,
		// The staged clock has a schedule of its own, because the deployment that turns the
		// pass off is the one whose staged uploads would otherwise never leave: nothing
		// carries them and no reader waits, so only this loop would ever take them.
		StagedSweepInterval: cfg.Storage.AttachmentStagedSweepInterval,
		Interval:            cfg.Retention.Interval,
		Limit:               cfg.Retention.Batch,
		SweepLimit:          cfg.Retention.SweepLimit,
	})
}

// retentionpolicy adapts the modules that own each half of an erasure to the job's ports.
//
// The split is the same one the pruners use and for the same reason: the workspace module owns the
// tenancy tables, and the chat module owns the bucket prefixes. Neither knows the other is involved,
// and the container is the only package allowed to know both.
type retentionpolicy struct {
	records    retentiondomain.ErasureRepository
	workspaces *workspacerepo.Repository
	objects    chatdomain.Service
}

// Start implements retentiondomain.ErasureRepository.
func (p retentionpolicy) Start(ctx context.Context, request retentiondomain.ErasureRequest, name, email string) (int64, error) {
	return p.records.Start(ctx, request, name, email)
}

// Finish implements retentiondomain.ErasureRepository.
func (p retentionpolicy) Finish(ctx context.Context, id int64, result retentiondomain.ErasureResult, failure error) error {
	return p.records.Finish(ctx, id, result, failure)
}

// List implements retentiondomain.ErasureRepository.
func (p retentionpolicy) List(ctx context.Context, limit int) ([]retentiondomain.Erasure, error) {
	return p.records.List(ctx, limit)
}

// Find implements retentiondomain.ErasureRepository.
func (p retentionpolicy) Find(ctx context.Context, workspaceID uuid.UUID) (retentiondomain.Erasure, error) {
	return p.records.Find(ctx, workspaceID)
}

// WorkspaceName implements retentiondomain.WorkspaceEraser.
func (p retentionpolicy) WorkspaceName(ctx context.Context, workspaceID uuid.UUID) (string, error) {
	return p.workspaces.WorkspaceName(ctx, workspaceID)
}

// RequesterEmail implements retentiondomain.WorkspaceEraser.
func (p retentionpolicy) RequesterEmail(ctx context.Context, userID uuid.UUID) (string, error) {
	return p.workspaces.RequesterEmail(ctx, userID)
}

// EraseWorkspace implements retentiondomain.WorkspaceEraser.
func (p retentionpolicy) EraseWorkspace(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	return p.workspaces.EraseWorkspace(ctx, workspaceID)
}

// EraseObjects implements retentiondomain.ObjectEraser.
func (p retentionpolicy) EraseObjects(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	if p.objects == nil {
		return 0, chatdomain.ErrStorageUnavailable
	}

	return p.objects.EraseObjects(ctx, workspaceID)
}

// taskStepPruner adapts the task module's pruner to the retention job's port.
type taskStepPruner struct {
	steps taskdomain.StepPruner
}

// PruneSteps implements retentiondomain.StepPruner.
func (p taskStepPruner) PruneSteps(ctx context.Context, scope retentiondomain.Scope, olderThan time.Time, limit int) (retentiondomain.PrunedSteps, error) {
	if p.steps == nil {
		return retentiondomain.PrunedSteps{}, nil
	}

	result, err := p.steps.PruneContents(ctx, taskdomain.Scope{WorkspaceID: scope.WorkspaceID}, olderThan, limit)

	return retentiondomain.PrunedSteps{Count: result.Pruned, Remaining: result.Remaining}, err
}

// chatAttachmentPruner adapts the chat service's sweep to the retention job's port.
//
// It is the service rather than the repository because an attachment is two halves — a row and
// an object — and the service is the layer that holds both.
type chatAttachmentPruner struct {
	chat chatdomain.Service
}

// PruneAttachments implements retentiondomain.AttachmentPruner.
func (p chatAttachmentPruner) PruneAttachments(ctx context.Context, scope retentiondomain.Scope, olderThan time.Time, limit int) (retentiondomain.PrunedAttachments, error) {
	if p.chat == nil {
		return retentiondomain.PrunedAttachments{}, nil
	}

	result, err := p.chat.PruneAttachments(
		ctx, chatdomain.Scope{WorkspaceID: scope.WorkspaceID}, olderThan, limit)

	return retentiondomain.PrunedAttachments{Count: result.Count, BytesFreed: result.BytesFreed}, err
}

// PruneStagedAttachments implements retentiondomain.AttachmentPruner.
func (p chatAttachmentPruner) PruneStagedAttachments(ctx context.Context, scope retentiondomain.Scope, olderThan time.Time, limit int) (retentiondomain.PrunedAttachments, error) {
	if p.chat == nil {
		return retentiondomain.PrunedAttachments{}, nil
	}

	result, err := p.chat.PruneStagedAttachments(
		ctx, chatdomain.Scope{WorkspaceID: scope.WorkspaceID}, olderThan, limit)

	return retentiondomain.PrunedAttachments{Count: result.Count, BytesFreed: result.BytesFreed}, err
}

// ---------------------------------------------------------------- the job

// RunRetention makes a retention pass on an interval until the context ends.
//
// It is a loop in the process rather than a durable workflow, for the reason the billing sweep
// is one: nothing in it waits, and every state it looks at is derived from the times and the
// contents. `retention_targets.next_prune_at` is what makes that true — a pass that never ran
// is still a due pass, and it is picked up as soon as the process is back.
//
// It runs in the API process, next to the billing sweep. That is a choice and not an accident:
// the job has no workflow behind it and nothing to wait for, so a separate binary would be a
// second deployment to keep running for a loop that spends most of its time asleep.
func (c *Container) RunRetention(ctx context.Context) {
	if c.Retention == nil || c.Config == nil {
		return
	}
	interval := c.Config.Retention.Interval
	if interval <= 0 {
		c.Logger.Info("retention is disabled")
		return
	}

	c.pruneContent(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.pruneContent(ctx)
		}
	}
}

// pruneContent runs one pass and reports what it did.
func (c *Container) pruneContent(ctx context.Context) {
	result, err := c.Retention.Run(ctx)
	if err != nil && ctx.Err() == nil {
		// A pass that failed part way still reports what it managed, so the log line
		// says both.
		c.Logger.Error("the retention pass failed",
			zap.Int("workspaces", result.Workspaces),
			zap.Int("steps_pruned", result.StepsPruned),
			zap.Int("attachments_pruned", result.Attachments),
			zap.Int64("bytes_freed", result.BytesFreed),
			zap.Error(err))
		return
	}

	if result.Workspaces == 0 {
		return
	}

	// The pass reports what it removed, so a chart answers "is retention keeping up" without a
	// search through the run table. It is recorded here rather than in the module because the
	// job is assembled by the container and owns no telemetry — the same reason every other
	// measurement in this deployment passes through an adapter.
	c.Metrics.RetentionPruned(ctx, "step", int64(result.StepsPruned))
	c.Metrics.RetentionPruned(ctx, "attachment", int64(result.Attachments))

	c.Logger.Info("the retention pass ran",
		zap.Int("workspaces", result.Workspaces),
		zap.Int("steps_pruned", result.StepsPruned),
		zap.Int("attachments_pruned", result.Attachments),
		zap.Int64("bytes_freed", result.BytesFreed),
		zap.Duration("step_window", c.Retention.Policy().StepWindow),
	)
}

// RunStagedSweep removes the uploads nobody ever sent, on an interval until the context ends.
//
// It is a loop of its own rather than the tail of the retention pass, because the two answer to
// different switches. RETENTION_INTERVAL=0 is a deployment that chose not to delete what a
// customer said — a stack somebody is stepping through — and it is that deployment whose staged
// uploads would otherwise accumulate forever: nothing carries them, so the attachment window never
// reaches them, and no reader waits for them either.
//
// It stands down while the pass is running, and that is not a saving but a correctness rule: both
// walks use `retention_targets` as their queue and both push `next_prune_at` forward, so two of
// them at once would each schedule a workspace for the other and the pair would drift out of both
// intervals. The pass carries the staged half itself, so nothing is left unswept by standing down.
func (c *Container) RunStagedSweep(ctx context.Context) {
	if c.Retention == nil || c.Config == nil {
		return
	}
	if c.Config.Retention.Interval > 0 {
		// The pass is on: its own walk carries the staged uploads, and starting this would
		// take its queue out from under it.
		c.Logger.Info("the staged sweep is carried by the retention pass")
		return
	}

	interval := c.Config.Storage.AttachmentStagedSweepInterval
	if interval <= 0 {
		interval = retentionservice.DefaultStagedSweepInterval
	}

	c.sweepStaged(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sweepStaged(ctx)
		}
	}
}

// sweepStaged runs one staged sweep and reports what it did.
func (c *Container) sweepStaged(ctx context.Context) {
	result, err := c.Retention.SweepStaged(ctx)
	if err != nil && ctx.Err() == nil {
		c.Logger.Error("the staged sweep failed",
			zap.Int("workspaces", result.Workspaces),
			zap.Int("attachments_pruned", result.Attachments),
			zap.Int64("bytes_freed", result.BytesFreed),
			zap.Error(err))
		return
	}

	if result.Attachments == 0 && result.BytesFreed == 0 {
		return
	}

	// The same series the pass writes, because it is the same question: a staged upload is an
	// attachment, and the kind says what was removed rather than which loop removed it.
	c.Metrics.RetentionPruned(ctx, "attachment", int64(result.Attachments))

	c.Logger.Info("the staged sweep ran",
		zap.Int("workspaces", result.Workspaces),
		zap.Int("attachments_pruned", result.Attachments),
		zap.Int64("bytes_freed", result.BytesFreed),
		zap.Duration("staged_ttl", c.Retention.Policy().StagedTTL),
	)
}
