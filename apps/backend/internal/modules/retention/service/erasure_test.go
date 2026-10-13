package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/retention/domain"
)

// The erasure path, and the two properties that make it different from the retention pass: the bytes
// go before the rows, and the evidence outlives the data.

// erasureFixture is the three ports an erasure needs, scripted.
type erasureFixture struct {
	records    *fakeErasures
	workspaces *fakeWorkspaces
	objects    *fakeObjects
}

type fakeErasures struct {
	started   []startCall
	finished  []finishCall
	startErr  error
	finishErr error
}

type startCall struct {
	request domain.ErasureRequest
	name    string
	email   string
}

type finishCall struct {
	id      int64
	result  domain.ErasureResult
	failure error
}

func (f *fakeErasures) Start(_ context.Context, request domain.ErasureRequest, name, email string) (int64, error) {
	if f.startErr != nil {
		return 0, f.startErr
	}
	f.started = append(f.started, startCall{request: request, name: name, email: email})

	return int64(len(f.started)), nil
}

func (f *fakeErasures) Finish(_ context.Context, id int64, result domain.ErasureResult, failure error) error {
	f.finished = append(f.finished, finishCall{id: id, result: result, failure: failure})

	return f.finishErr
}

func (f *fakeErasures) List(context.Context, int) ([]domain.Erasure, error) { return nil, nil }

func (f *fakeErasures) Find(context.Context, uuid.UUID) (domain.Erasure, error) {
	return domain.Erasure{}, domain.ErrErasureNotFound
}

// fakeWorkspaces records the order the two halves of an erasure happen in.
type fakeWorkspaces struct {
	name     string
	email    string
	rows     int64
	nameErr  error
	emailErr error
	eraseErr error
	// erased is the workspace ids deleted, in order.
	erased []uuid.UUID
	// sequence records the calls across both halves, which is what proves the objects went first.
	sequence *[]string
}

func (f *fakeWorkspaces) WorkspaceName(context.Context, uuid.UUID) (string, error) {
	return f.name, f.nameErr
}

func (f *fakeWorkspaces) RequesterEmail(context.Context, uuid.UUID) (string, error) {
	return f.email, f.emailErr
}

func (f *fakeWorkspaces) EraseWorkspace(_ context.Context, workspaceID uuid.UUID) (int64, error) {
	if f.eraseErr != nil {
		return 0, f.eraseErr
	}
	f.erased = append(f.erased, workspaceID)
	if f.sequence != nil {
		*f.sequence = append(*f.sequence, "rows")
	}

	return f.rows, nil
}

// fakeObjects is the module that owns the bucket prefixes.
type fakeObjects struct {
	objects  int64
	err      error
	sequence *[]string
	seen     []uuid.UUID
}

func (f *fakeObjects) EraseObjects(_ context.Context, workspaceID uuid.UUID) (int64, error) {
	f.seen = append(f.seen, workspaceID)
	if f.sequence != nil {
		*f.sequence = append(*f.sequence, "objects")
	}
	if f.err != nil {
		return 0, f.err
	}

	return f.objects, nil
}

// newErasureService builds the service over the fixture.
func newErasureService(t *testing.T, fixture *erasureFixture) *Service {
	t.Helper()

	return New(Deps{
		Erasures:   fixture.records,
		Workspaces: fixture.workspaces,
		Objects:    fixture.objects,
	}, Config{})
}

func newErasureFixture() *erasureFixture {
	sequence := []string{}

	return &erasureFixture{
		records: &fakeErasures{},
		workspaces: &fakeWorkspaces{
			name: "Toko Sinar", email: "pemilik@example.com", rows: 42, sequence: &sequence,
		},
		objects: &fakeObjects{objects: 7, sequence: &sequence},
	}
}

// TestTheObjectsGoBeforeTheRows is the property that makes an erasure complete: deleting the
// workspace first cascades its attachment rows away, and the storage keys go with them — after which
// the bytes are unreachable by any query, in a bucket nothing points at.
func TestTheObjectsGoBeforeTheRows(t *testing.T) {
	fixture := newErasureFixture()
	service := newErasureService(t, fixture)

	workspaceID := uuid.New()

	result, err := service.Erase(context.Background(), domain.ErasureRequest{
		WorkspaceID: workspaceID,
		RequestedBy: uuid.New(),
	})
	require.NoError(t, err)

	require.Equal(t, []string{"objects", "rows"}, *fixture.workspaces.sequence,
		"the bytes are removed while the keys that name them still exist")
	require.Equal(t, int64(7), result.Objects)
	require.Equal(t, int64(42), result.Rows)
	require.Equal(t, []uuid.UUID{workspaceID}, fixture.objects.seen)
	require.Equal(t, []uuid.UUID{workspaceID}, fixture.workspaces.erased)
}

// TestTheEvidenceKeepsTheNames: a record that says a uuid was deleted cannot be shown to the person
// who asked for their data back, so both names are read before the rows that hold them are gone.
func TestTheEvidenceKeepsTheNames(t *testing.T) {
	fixture := newErasureFixture()
	service := newErasureService(t, fixture)

	request := domain.ErasureRequest{WorkspaceID: uuid.New(), RequestedBy: uuid.New()}

	_, err := service.Erase(context.Background(), request)
	require.NoError(t, err)

	require.Len(t, fixture.records.started, 1)
	require.Equal(t, "Toko Sinar", fixture.records.started[0].name)
	require.Equal(t, "pemilik@example.com", fixture.records.started[0].email)
	require.Equal(t, request, fixture.records.started[0].request)

	require.Len(t, fixture.records.finished, 1)
	require.NoError(t, fixture.records.finished[0].failure)
	require.Equal(t, domain.ErasureResult{Objects: 7, Rows: 42}, fixture.records.finished[0].result)
}

// TestAFailureHalfWayKeepsTheRecordAndDoesNotRetry: an erasure that half-succeeded is a state a
// person has to look at. A background retry would be the deployment deciding on its own to try again
// with a customer's data.
func TestAFailureHalfWayKeepsTheRecordAndDoesNotRetry(t *testing.T) {
	t.Run("the objects failed, so the rows are untouched", func(t *testing.T) {
		fixture := newErasureFixture()
		fixture.objects.err = errors.New("the bucket refused the deletion")
		service := newErasureService(t, fixture)

		_, err := service.Erase(context.Background(), domain.ErasureRequest{
			WorkspaceID: uuid.New(), RequestedBy: uuid.New(),
		})
		require.Error(t, err)

		require.Equal(t, []string{"objects"}, *fixture.workspaces.sequence,
			"a workspace whose bytes survived is not deleted: the keys are the only way back to them")
		require.Empty(t, fixture.workspaces.erased)
		require.Len(t, fixture.records.finished, 1)
		require.Error(t, fixture.records.finished[0].failure, "the reason is on the record")
	})

	t.Run("the rows failed, so the reason is recorded", func(t *testing.T) {
		fixture := newErasureFixture()
		fixture.workspaces.eraseErr = errors.New("the database refused the deletion")
		service := newErasureService(t, fixture)

		_, err := service.Erase(context.Background(), domain.ErasureRequest{
			WorkspaceID: uuid.New(), RequestedBy: uuid.New(),
		})
		require.Error(t, err)

		require.Len(t, fixture.records.finished, 1)
		require.Error(t, fixture.records.finished[0].failure)
	})
}

// TestAnIncompleteDeploymentRefusesRatherThanHalfErases: without both ports an erasure would delete
// part of what the workspace held and report success, which is worse than refusing — the person
// asked for their data to be gone, and being told it is gone when it is not is the one answer this
// path must never give.
func TestAnIncompleteDeploymentRefusesRatherThanHalfErases(t *testing.T) {
	request := domain.ErasureRequest{WorkspaceID: uuid.New(), RequestedBy: uuid.New()}

	for name, deps := range map[string]Deps{
		"no record":      {Workspaces: &fakeWorkspaces{}, Objects: &fakeObjects{}},
		"no workspace":   {Erasures: &fakeErasures{}, Objects: &fakeObjects{}},
		"no objects":     {Erasures: &fakeErasures{}, Workspaces: &fakeWorkspaces{}},
		"nothing at all": {},
	} {
		service := New(deps, Config{})

		_, err := service.Erase(context.Background(), request)
		require.Error(t, err, name)
		require.NotErrorIs(t, err, domain.ErrErasureNotFound, name)
	}
}

// TestAnUnreadableRequesterDoesNotStopTheErasure: the person's right is to have their data removed,
// and an address that cannot be read is a detail on the record rather than a reason to keep it.
func TestAnUnreadableRequesterDoesNotStopTheErasure(t *testing.T) {
	fixture := newErasureFixture()
	fixture.workspaces.emailErr = errors.New("the user row is gone")
	service := newErasureService(t, fixture)

	_, err := service.Erase(context.Background(), domain.ErasureRequest{
		WorkspaceID: uuid.New(), RequestedBy: uuid.New(),
	})
	require.NoError(t, err)

	require.Len(t, fixture.records.started, 1)
	require.Empty(t, fixture.records.started[0].email)
	require.Equal(t, "Toko Sinar", fixture.records.started[0].name, "the name is still readable")
}

// TestAWorkspaceThatNoLongerExistsIsRefused: the name read fails, and an erasure of nothing is not
// reported as an erasure.
func TestAWorkspaceThatNoLongerExistsIsRefused(t *testing.T) {
	fixture := newErasureFixture()
	fixture.workspaces.nameErr = errors.New("no such workspace")
	service := newErasureService(t, fixture)

	_, err := service.Erase(context.Background(), domain.ErasureRequest{
		WorkspaceID: uuid.New(), RequestedBy: uuid.New(),
	})
	require.Error(t, err)

	require.Empty(t, fixture.records.started, "no record is opened for a workspace that is not there")
	require.Empty(t, fixture.objects.seen, "and nothing is deleted")
}

// TestAnEmptyWorkspaceIDIsRefused: the id is the tenant, and an erasure without one is an erasure of
// nothing that would otherwise succeed.
func TestAnEmptyWorkspaceIDIsRefused(t *testing.T) {
	fixture := newErasureFixture()
	service := newErasureService(t, fixture)

	_, err := service.Erase(context.Background(), domain.ErasureRequest{RequestedBy: uuid.New()})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
	require.Empty(t, fixture.workspaces.erased)
}

// TestARecordThatCannotBeClosedIsStillReportedAsDone: the erasure happened, and a bookkeeping failure
// must not be reported to the person as a failure to erase.
func TestARecordThatCannotBeClosedIsStillReportedAsDone(t *testing.T) {
	fixture := newErasureFixture()
	fixture.records.finishErr = errors.New("the database refused the update")
	service := newErasureService(t, fixture)

	result, err := service.Erase(context.Background(), domain.ErasureRequest{
		WorkspaceID: uuid.New(), RequestedBy: uuid.New(),
	})
	require.NoError(t, err)
	require.Equal(t, int64(42), result.Rows)
	require.Equal(t, []string{"objects", "rows"}, *fixture.workspaces.sequence)
}
