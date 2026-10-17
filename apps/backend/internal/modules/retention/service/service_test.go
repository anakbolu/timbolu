package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/retention/domain"
)

// retentionFixture is a fake repository that records what the pass did, plus the two pruners.
type retentionFixture struct {
	mu sync.Mutex

	// due is what the queue answers, in order.
	due []uuid.UUID
	// touched is the next pass each workspace was scheduled for.
	touched map[uuid.UUID]time.Time
	// touchedOrder keeps the order the calls happened in, which is what tells a
	// scheduled-after-prune from a scheduled-before.
	touchedOrder []uuid.UUID

	runs       []domain.RunResult
	runFailure []error
	dueErr     error
	startErr   error
	finishErr  error

	steps       *fakeSteps
	attachments *fakeAttachments
}

func newRetentionFixture(due ...uuid.UUID) *retentionFixture {
	return &retentionFixture{
		due:     due,
		touched: map[uuid.UUID]time.Time{},
		steps:   &fakeSteps{pruned: map[uuid.UUID]domain.PrunedSteps{}},
		attachments: &fakeAttachments{
			pruned:       map[uuid.UUID]domain.PrunedAttachments{},
			stagedPruned: map[uuid.UUID]domain.PrunedAttachments{},
		},
	}
}

func (f *retentionFixture) DueTargets(_ context.Context, _ time.Time, _ int) ([]uuid.UUID, error) {
	if f.dueErr != nil {
		return nil, f.dueErr
	}

	return f.due, nil
}

func (f *retentionFixture) TouchTarget(_ context.Context, scope domain.Scope, next time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.touched[scope.WorkspaceID] = next
	f.touchedOrder = append(f.touchedOrder, scope.WorkspaceID)

	return nil
}

func (f *retentionFixture) StartRun(_ context.Context) (int64, error) {
	if f.startErr != nil {
		return 0, f.startErr
	}

	return int64(len(f.runs) + 1), nil
}

func (f *retentionFixture) FinishRun(_ context.Context, id int64, result domain.RunResult, failure error) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.runs = append(f.runs, result)
	f.runFailure = append(f.runFailure, failure)

	return f.finishErr
}

func (f *retentionFixture) Runs(context.Context, int) ([]domain.Run, error) { return nil, nil }

// fakeSteps prunes with the same semantics as the SQL: what is past the window loses its
// payload, the batch is bounded, and what remains past the window is counted.
type fakeSteps struct {
	mu     sync.Mutex
	seen   []fakeCall
	pruned map[uuid.UUID]domain.PrunedSteps
	err    map[uuid.UUID]error
}

type fakeCall struct {
	workspace uuid.UUID
	olderThan time.Time
	limit     int
}

func (f *fakeSteps) PruneSteps(_ context.Context, scope domain.Scope, olderThan time.Time, limit int) (domain.PrunedSteps, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.seen = append(f.seen, fakeCall{workspace: scope.WorkspaceID, olderThan: olderThan, limit: limit})

	return f.pruned[scope.WorkspaceID], f.err[scope.WorkspaceID]
}

// fakeAttachments does the same for attachments, and keeps the staged sweep in a list of its
// own: the two windows mean different things, so a test that asserts which one a workspace was
// swept with has to be able to tell them apart.
type fakeAttachments struct {
	mu           sync.Mutex
	seen         []fakeCall
	pruned       map[uuid.UUID]domain.PrunedAttachments
	stagedSeen   []fakeCall
	stagedPruned map[uuid.UUID]domain.PrunedAttachments
	err          map[uuid.UUID]error
}

func (f *fakeAttachments) PruneAttachments(_ context.Context, scope domain.Scope, olderThan time.Time, limit int) (domain.PrunedAttachments, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.seen = append(f.seen, fakeCall{workspace: scope.WorkspaceID, olderThan: olderThan, limit: limit})

	return f.pruned[scope.WorkspaceID], f.err[scope.WorkspaceID]
}

// PruneStagedAttachments implements domain.AttachmentPruner.
func (f *fakeAttachments) PruneStagedAttachments(_ context.Context, scope domain.Scope, olderThan time.Time, limit int) (domain.PrunedAttachments, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.stagedSeen = append(f.stagedSeen, fakeCall{workspace: scope.WorkspaceID, olderThan: olderThan, limit: limit})

	return f.stagedPruned[scope.WorkspaceID], f.err[scope.WorkspaceID]
}

// newService builds the service over the fixture with a pinned clock.
func newFixtureService(t *testing.T, f *retentionFixture, config Config, now time.Time) *Service {
	t.Helper()

	return New(Deps{
		Repository:  f,
		Steps:       f.steps,
		Attachments: f.attachments,
		Clock:       func() time.Time { return now },
	}, config)
}

// TestAnAgedStepLosesItsContentAndTheWindowIsWhatDecides is the E13.2 rule at the service
// level: what is past the window is pruned, the window itself is derived from the settings,
// and nothing younger than it is touched.
func TestAnAgedStepLosesItsContentAndTheWindowIsWhatDecides(t *testing.T) {
	workspace := uuid.New()
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

	fixture := newRetentionFixture(workspace)
	fixture.steps.pruned[workspace] = domain.PrunedSteps{Count: 3}

	// A zero step window takes the product default rather than meaning "nothing" — which is
	// the difference that matters: a zero that pruned everything on the next pass would be
	// the worst possible reading of a missing setting.
	service := newFixtureService(t, fixture, Config{}, now)

	result, err := service.Run(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, result.Workspaces)
	require.Equal(t, 3, result.StepsPruned)

	require.Len(t, fixture.steps.seen, 1)
	require.Equal(t, workspace, fixture.steps.seen[0].workspace)
	require.Equal(t, now.Add(-domain.DefaultStepWindow), fixture.steps.seen[0].olderThan,
		"the cutoff is the window before now, which is what leaves the last thirty days alone")
	require.Equal(t, domain.DefaultLimit, fixture.steps.seen[0].limit)

	// The window is the deployment's when it says so, in the same terms.
	configured := newFixtureService(t, fixture, Config{StepWindow: 48 * time.Hour}, now)
	require.Equal(t, 48*time.Hour, configured.Policy().StepWindow)

	_, err = configured.Run(context.Background())
	require.NoError(t, err)

	require.Len(t, fixture.steps.seen, 2)
	require.Equal(t, now.Add(-48*time.Hour), fixture.steps.seen[1].olderThan)
}

// TestAnAttachmentWindowOfZeroRemovesNothing: keeping an attachment as long as its message is
// a real policy, and it must not be read as a window of zero seconds — which would delete every
// file in the deployment on the next pass.
func TestAnAttachmentWindowOfZeroRemovesNothing(t *testing.T) {
	workspace := uuid.New()
	fixture := newRetentionFixture(workspace)

	service := newFixtureService(t, fixture, Config{AttachmentWindow: 0}, time.Now())

	_, err := service.Run(context.Background())
	require.NoError(t, err)

	require.Empty(t, fixture.attachments.seen, "no window means no sweep, not a sweep of everything")

	// And a deployment that names one sweeps by it.
	service = newFixtureService(t, fixture, Config{AttachmentWindow: 90 * 24 * time.Hour}, time.Now())
	_, err = service.Run(context.Background())
	require.NoError(t, err)

	require.Len(t, fixture.attachments.seen, 1)
}

// TestThePassKeepsGoingWhenOneWorkspaceFails: the rest of the fleet is waiting, and a workspace
// that failed is due again on the interval rather than in a tight loop.
func TestThePassKeepsGoingWhenOneWorkspaceFails(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

	fixture := newRetentionFixture(first, second)
	fixture.steps.err = map[uuid.UUID]error{first: errors.New("the database refused the update")}
	fixture.steps.pruned[second] = domain.PrunedSteps{Count: 2}
	fixture.attachments.pruned[second] = domain.PrunedAttachments{Count: 1, BytesFreed: 4096}

	service := newFixtureService(t, fixture, Config{Interval: time.Hour, AttachmentWindow: 90 * 24 * time.Hour}, now)

	result, err := service.Run(context.Background())
	require.Error(t, err, "the failure is reported to the caller")
	require.Equal(t, 2, result.Workspaces, "both workspaces were looked at")
	require.Equal(t, 2, result.StepsPruned)
	require.Equal(t, 1, result.Attachments)
	require.Equal(t, int64(4096), result.BytesFreed)

	// Both are scheduled, including the one that failed, so the next interval retries it.
	require.Len(t, fixture.touched, 2)
	require.Equal(t, now.Add(time.Hour), fixture.touched[first])
	require.Equal(t, now.Add(time.Hour), fixture.touched[second])

	// And the run is closed with the reason on it, which is what makes a job that stopped
	// working three weeks ago visible.
	require.Len(t, fixture.runs, 1)
	require.Equal(t, 2, fixture.runs[0].StepsPruned)
	require.Error(t, fixture.runFailure[0])
}

// TestNothingIsPrunedWhenThereIsNoRepositoryOrNoWorkspaceDue.
func TestNothingIsPrunedWhenThereIsNoRepositoryOrNoWorkspaceDue(t *testing.T) {
	service := New(Deps{Clock: time.Now}, Config{})
	_, err := service.Run(context.Background())
	require.ErrorIs(t, err, domain.ErrNoRepository)

	fixture := newRetentionFixture()
	service = newFixtureService(t, fixture, Config{}, time.Now())

	result, err := service.Run(context.Background())
	require.NoError(t, err)
	require.Equal(t, domain.RunResult{}, result)
	require.Empty(t, fixture.steps.seen)
	require.Empty(t, fixture.touched)
}

// TestARunThatCannotReadItsQueueIsStillRecorded: the run log is the only durable trace the job
// leaves, so a pass that failed before it could do anything still has to say so.
func TestARunThatCannotReadItsQueueIsStillRecorded(t *testing.T) {
	fixture := newRetentionFixture()
	fixture.dueErr = errors.New("the database is unreachable")

	service := newFixtureService(t, fixture, Config{}, time.Now())

	_, err := service.Run(context.Background())
	require.Error(t, err)
	require.Len(t, fixture.runs, 1, "the run was opened and closed")
	require.Error(t, fixture.runFailure[0])
}

// TestAnUnavailablePrunerIsNotAFailure: a deployment without object storage prunes steps and
// reports attachments as zero rather than reporting the whole pass as broken.
func TestAnUnavailablePrunerIsNotAFailure(t *testing.T) {
	workspace := uuid.New()
	fixture := newRetentionFixture(workspace)
	fixture.steps.pruned[workspace] = domain.PrunedSteps{Count: 1}

	service := New(Deps{
		Repository: fixture,
		Steps:      fixture.steps,
		Clock:      time.Now,
	}, Config{})

	result, err := service.Run(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, result.StepsPruned)
	require.Equal(t, 0, result.Attachments)
}

// TestABacklogIsReportedRatherThanSilentlyLeft: a workspace with more past the window than one
// batch removes is not a failure, and it is the number that says the batch is too small.
func TestABacklogIsReportedRatherThanSilentlyLeft(t *testing.T) {
	workspace := uuid.New()
	fixture := newRetentionFixture(workspace)
	fixture.steps.pruned[workspace] = domain.PrunedSteps{Count: 500, Remaining: 1200}

	service := newFixtureService(t, fixture, Config{Limit: 500}, time.Now())

	result, err := service.Run(context.Background())
	require.NoError(t, err)
	require.Equal(t, 500, result.StepsPruned)

	require.Len(t, fixture.steps.seen, 1)
	require.Equal(t, 500, fixture.steps.seen[0].limit)
}

// TestTheStagedSweepRunsWhenThePassIsOff is the reason the sweep is a pass of its own: a
// deployment that keeps every attachment with its message — the local stack, and any deployment
// that set RETENTION_INTERVAL=0 — still must not keep the file nobody ever sent, because nothing
// carries it and no reader waits for it. The pass's clock is not the staged clock.
func TestTheStagedSweepRunsWhenThePassIsOff(t *testing.T) {
	workspace := uuid.New()
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

	fixture := newRetentionFixture(workspace)
	fixture.attachments.stagedPruned[workspace] = domain.PrunedAttachments{Count: 2, BytesFreed: 8192}

	// The pass is off: no interval, and an attachment window of zero, which is the policy that
	// keeps everything a customer sent.
	service := newFixtureService(t, fixture, Config{Interval: 0, AttachmentWindow: 0}, now)

	result, err := service.SweepStaged(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, result.Workspaces)
	require.Equal(t, 2, result.Attachments)
	require.Equal(t, int64(8192), result.BytesFreed)
	require.Zero(t, result.StepsPruned)

	require.Len(t, fixture.attachments.stagedSeen, 1)
	require.Equal(t, now.Add(-domain.DefaultStagedTTL), fixture.attachments.stagedSeen[0].olderThan,
		"an unset staged TTL is the module's day, not a window of zero")
	require.Equal(t, domain.DefaultLimit, fixture.attachments.stagedSeen[0].limit)

	// Nothing else was touched: the sweep is the staged clock and not a second pass.
	require.Empty(t, fixture.steps.seen)
	require.Empty(t, fixture.attachments.seen)

	// And the workspace is due again on the sweep's own interval, not the pass's, which is
	// what keeps a deployment with the pass off from walking the queue in a tight loop.
	require.Equal(t, now.Add(DefaultStagedSweepInterval), fixture.touched[workspace])
}

// TestTheStagedSweepSchedulesItselfOnItsOwnClock: the schedule and the window are both the
// deployment's when it names them, and the two are separate settings because they answer
// different questions — how long a file is kept, and how often somebody looks.
func TestTheStagedSweepSchedulesItselfOnItsOwnClock(t *testing.T) {
	workspace := uuid.New()
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

	fixture := newRetentionFixture(workspace)
	service := newFixtureService(t, fixture, Config{
		StagedTTL:           6 * time.Hour,
		StagedSweepInterval: 15 * time.Minute,
	}, now)

	_, err := service.SweepStaged(context.Background())
	require.NoError(t, err)

	require.Len(t, fixture.attachments.stagedSeen, 1)
	require.Equal(t, now.Add(-6*time.Hour), fixture.attachments.stagedSeen[0].olderThan)
	require.Equal(t, now.Add(15*time.Minute), fixture.touched[workspace])
}

// TestThePassCarriesTheStagedUploads: a deployment that runs the pass is not also running the
// sweep — the container starts one or the other — so the pass has to take the staged half itself.
// The two walk one queue and both push a workspace's next look forward, so a pass that left them
// to the sweep would leave them to a walker nobody started.
func TestThePassCarriesTheStagedUploads(t *testing.T) {
	workspace := uuid.New()
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

	fixture := newRetentionFixture(workspace)
	fixture.attachments.pruned[workspace] = domain.PrunedAttachments{Count: 1, BytesFreed: 512}
	fixture.attachments.stagedPruned[workspace] = domain.PrunedAttachments{Count: 2, BytesFreed: 2048}

	service := newFixtureService(t, fixture, Config{
		Interval:         time.Hour,
		AttachmentWindow: 90 * 24 * time.Hour,
	}, now)

	result, err := service.Run(context.Background())
	require.NoError(t, err)

	// Both windows were swept, with their own cutoffs, and both counts reach the caller.
	require.Len(t, fixture.attachments.seen, 1)
	require.Equal(t, now.Add(-90*24*time.Hour), fixture.attachments.seen[0].olderThan)
	require.Len(t, fixture.attachments.stagedSeen, 1)
	require.Equal(t, now.Add(-domain.DefaultStagedTTL), fixture.attachments.stagedSeen[0].olderThan,
		"the staged half goes on its own window, not the attachment policy")
	require.Equal(t, 3, result.Attachments)
	require.Equal(t, int64(2560), result.BytesFreed)

	// And the workspace is scheduled on the pass's interval, because the pass is what ran.
	require.Equal(t, now.Add(time.Hour), fixture.touched[workspace])
}

// TestTheStagedSweepKeepsGoingWhenOneWorkspaceFails: the sweep walks the same queue the pass
// does, so it inherits the same rule — the rest of the fleet is waiting, and the workspace that
// failed is due again on the interval rather than in a tight loop.
func TestTheStagedSweepKeepsGoingWhenOneWorkspaceFails(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

	fixture := newRetentionFixture(first, second)
	fixture.attachments.err = map[uuid.UUID]error{first: errors.New("the bucket refused the delete")}
	fixture.attachments.stagedPruned[second] = domain.PrunedAttachments{Count: 1, BytesFreed: 2048}

	service := newFixtureService(t, fixture, Config{StagedSweepInterval: time.Hour}, now)

	result, err := service.SweepStaged(context.Background())
	require.Error(t, err)
	require.Equal(t, 2, result.Workspaces)
	require.Equal(t, 1, result.Attachments)
	require.Equal(t, int64(2048), result.BytesFreed)

	require.Len(t, fixture.touched, 2)
	require.Equal(t, now.Add(time.Hour), fixture.touched[first])
	require.Len(t, fixture.runs, 1)
	require.Error(t, fixture.runFailure[0])
}

// TestTheStagedSweepNeedsAnAttachmentPruner: a deployment without object storage has no staged
// uploads to remove, and that is not a failure of the sweep.
func TestTheStagedSweepNeedsAnAttachmentPruner(t *testing.T) {
	fixture := newRetentionFixture(uuid.New())
	service := New(Deps{Repository: fixture, Steps: fixture.steps, Clock: time.Now}, Config{})

	result, err := service.SweepStaged(context.Background())
	require.NoError(t, err)
	require.Zero(t, result.Attachments)
	require.Empty(t, fixture.attachments.stagedSeen)
}
