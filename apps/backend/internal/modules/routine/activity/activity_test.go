package activity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/routine/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/routine/workflow"
)

// stubService answers one tick.
type stubService struct {
	run   domain.Run
	err   error
	asked []uuid.UUID
}

func (s *stubService) RunOnce(_ context.Context, _ domain.Scope, routineID uuid.UUID) (domain.Run, error) {
	s.asked = append(s.asked, routineID)
	if s.err != nil {
		return domain.Run{}, s.err
	}
	return s.run, nil
}

// The remaining methods are not reached by this activity. A stub that implemented
// them would be a second copy of the service's contract, so the interface is
// satisfied by embedding the behaviour they share: they panic if called, which is
// what a test wants if it drifts.
func (s *stubService) Routines(context.Context, domain.Scope, domain.Filter) ([]domain.View, error) {
	panic("not used")
}

func (s *stubService) Routine(context.Context, domain.Scope, uuid.UUID) (domain.View, error) {
	panic("not used")
}

func (s *stubService) RoutinesFor(context.Context, domain.Scope, string) ([]domain.Routine, error) {
	panic("not used")
}

func (s *stubService) Create(context.Context, domain.Scope, domain.WriteRequest) (domain.View, error) {
	panic("not used")
}

func (s *stubService) Update(context.Context, domain.Scope, uuid.UUID, domain.WriteRequest) (domain.View, error) {
	panic("not used")
}

func (s *stubService) SetEnabled(context.Context, domain.Scope, uuid.UUID, bool) (domain.View, error) {
	panic("not used")
}

func (s *stubService) Delete(context.Context, domain.Scope, uuid.UUID) error { panic("not used") }

func (s *stubService) Runs(context.Context, domain.Scope, domain.RunFilter) ([]domain.Run, error) {
	panic("not used")
}

func (s *stubService) Preview(context.Context, domain.Scope, domain.PreviewRequest) ([]time.Time, error) {
	panic("not used")
}

func (s *stubService) Reschedule(context.Context, domain.Scope) error { panic("not used") }

func (s *stubService) Fire(context.Context, domain.Scope, string) (int, error) { panic("not used") }

func (s *stubService) RecordTaskOutcome(context.Context, domain.Scope, uuid.UUID, domain.Outcome) error {
	panic("not used")
}

// TestRunRoutineReportsWhatTheTickDid: the workflow logs the run's id, status, and
// summary, so a person watching Temporal can see what a tick produced.
func TestRunRoutineReportsWhatTheTickDid(t *testing.T) {
	runID := uuid.New()
	service := &stubService{run: domain.Run{
		ID:      runID,
		Status:  domain.RunSkipped,
		Summary: "tidak ada kejadian baru",
	}}

	activities := New(Deps{Routines: service})

	workspace := uuid.New()
	routine := uuid.New()

	result, err := activities.RunRoutine(context.Background(), domain.RunInput{
		RoutineID:   routine.String(),
		WorkspaceID: workspace.String(),
	})
	require.NoError(t, err)

	require.Equal(t, runID.String(), result.RunID)
	require.Equal(t, domain.RunSkipped, result.Status)
	require.Equal(t, "tidak ada kejadian baru", result.Summary)

	require.Equal(t, []uuid.UUID{routine}, service.asked, "the tick runs as the routine it names")
}

// TestARoutineThatWentAwayIsNotAFailure is the race a schedule leaves behind: the
// routine is deleted, its schedule is removed, and a tick already in flight still
// arrives. The activity reports no work rather than an error, so Temporal does not
// retry a tick whose outcome can no longer change.
func TestARoutineThatWentAwayIsNotAFailure(t *testing.T) {
	for _, cause := range []error{domain.ErrNotFound, domain.ErrInvalidInput} {
		service := &stubService{err: cause}
		activities := New(Deps{Routines: service})

		result, err := activities.RunRoutine(context.Background(), domain.RunInput{
			RoutineID:   uuid.NewString(),
			WorkspaceID: uuid.NewString(),
		})
		require.NoError(t, err, cause)
		require.Empty(t, result.RunID)
	}
}

// TestAnUnexpectedFailureIsReturned: something the service did not classify is worth
// surfacing, because it means nothing was recorded at all.
func TestAnUnexpectedFailureIsReturned(t *testing.T) {
	service := &stubService{err: errors.New("the database is on fire")}
	activities := New(Deps{Routines: service})

	_, err := activities.RunRoutine(context.Background(), domain.RunInput{
		RoutineID:   uuid.NewString(),
		WorkspaceID: uuid.NewString(),
	})
	require.Error(t, err)
	require.NotErrorIs(t, err, domain.ErrNotFound)
}

// TestTheIdsTravelAsStrings: the workflow carries ids as strings because that is what
// survives a history replay, and an unparsable one is refused before anything runs.
func TestTheIdsTravelAsStrings(t *testing.T) {
	activities := New(Deps{Routines: &stubService{}})

	_, err := activities.RunRoutine(context.Background(), domain.RunInput{
		RoutineID:   "bukan-uuid",
		WorkspaceID: uuid.NewString(),
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = activities.RunRoutine(context.Background(), domain.RunInput{
		RoutineID:   uuid.NewString(),
		WorkspaceID: "bukan-uuid",
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = activities.RunRoutine(context.Background(), domain.RunInput{WorkspaceID: uuid.NewString()})
	require.ErrorIs(t, err, domain.ErrInvalidInput, "a routine id is required")

	_, err = activities.RunRoutine(context.Background(), domain.RunInput{RoutineID: uuid.NewString()})
	require.ErrorIs(t, err, domain.ErrInvalidInput, "and a workspace id is required")
}

// TestWithoutAServiceReportsIt: a deployment whose routine runtime was never assembled
// says so rather than dereferencing nil.
func TestWithoutAServiceReportsIt(t *testing.T) {
	activities := New(Deps{})

	_, err := activities.RunRoutine(context.Background(), domain.RunInput{
		RoutineID:   uuid.NewString(),
		WorkspaceID: uuid.NewString(),
	})
	require.Error(t, err)
}

// TestTheActivityNameIsTheMethodName is the guard the whole run depends on: the
// workflow schedules an activity by name, and nothing in the compiler connects that
// name to this method.
func TestTheActivityNameIsTheMethodName(t *testing.T) {
	require.Equal(t, "RunRoutine", workflow.ActivityRun)

	activities := New(Deps{Routines: &stubService{}})
	require.NotNil(t, activities)
}
