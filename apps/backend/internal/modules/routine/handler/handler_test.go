package handler

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/routine/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/routine/domain/mocks"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/middleware"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/response"
	"github.com/anakbolu/timbolu/packages/platform/database"
)

// newTestApp mounts the routine routes behind the identity the container's middlewares
// would have put on the request.
func newTestApp(t *testing.T, service domain.Service, workspaceID uuid.UUID) *fiber.App {
	t.Helper()

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			var fiberErr *fiber.Error
			code := fiber.StatusInternalServerError
			if errors.As(err, &fiberErr) {
				code = fiberErr.Code
			}
			return response.Error(c, code, err.Error(), "http_error", nil)
		},
	})

	group := app.Group("/api", func(c *fiber.Ctx) error {
		userID := uuid.New()
		middleware.SetIdentity(c, userID, "owner@example.com")
		middleware.SetScope(c, database.Scope{UserID: userID, WorkspaceID: workspaceID})
		return c.Next()
	})

	Routes(group, New(service, zap.NewNop()))
	return app
}

func do(t *testing.T, app *fiber.App, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)

	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	recorder := httptest.NewRecorder()
	recorder.Code = resp.StatusCode
	if content, err := io.ReadAll(resp.Body); err == nil {
		recorder.Body.Write(content)
	}
	return recorder
}

func routineFixture() domain.Routine {
	return domain.Routine{
		ID:          uuid.New(),
		WorkspaceID: uuid.New(),
		AgentID:     uuid.New(),
		Name:        "Cek dokumen baru",
		Kind:        domain.KindDaily,
		HourStart:   6,
		Minute:      0,
		Instruction: "Buka dokumen baru dan rapikan.",
		Enabled:     true,
		ScheduleID:  "routine-abc",
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
}

// TestListCarriesTheNextRuns: "when does this run next" is the first question a routine
// list answers, so the times travel with the row rather than in a request per row.
func TestListCarriesTheNextRuns(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	routine := routineFixture()
	next := time.Now().Add(time.Hour)

	service.EXPECT().
		Routines(mock.Anything, mock.Anything, mock.Anything).
		Return([]domain.View{{Routine: routine, NextRuns: []time.Time{next}}}, nil).
		Once()

	resp := do(t, app, fiber.MethodGet, "/api/routines", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"schedule":"setiap hari 06:00"`)
	require.Contains(t, resp.Body.String(), `"next_runs":["`+next.Format(time.RFC3339)+`"]`)
	require.Contains(t, resp.Body.String(), `"scheduled":true`)
	require.Contains(t, resp.Body.String(), `"count":1`)
}

// TestATriggerRoutineReportsNoNextRun: it has no clock, and an empty list is how the
// screen knows not to show one.
func TestATriggerRoutineReportsNoNextRun(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	routine := routineFixture()
	routine.Kind = domain.KindTrigger
	routine.TriggerSource = "gmail.message_received"

	service.EXPECT().
		Routines(mock.Anything, mock.Anything, mock.Anything).
		Return([]domain.View{{Routine: routine}}, nil).
		Once()

	resp := do(t, app, fiber.MethodGet, "/api/routines", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"scheduled":false`)
	require.Contains(t, resp.Body.String(), `"next_runs":[]`)
	require.Contains(t, resp.Body.String(), `"schedule":"saat ada kejadian"`)
}

// TestCreatePassesTheCadenceThrough: the form's fields reach the service as the
// cadence the engine is registered with.
func TestCreatePassesTheCadenceThrough(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())
	agentID := uuid.New()

	service.EXPECT().
		Create(mock.Anything, mock.Anything, mock.MatchedBy(func(request domain.WriteRequest) bool {
			return request.AgentID == agentID &&
				request.Kind == domain.KindHourlyRange &&
				request.HourStart == 8 && request.HourEnd == 21 &&
				request.Minute == 30 &&
				request.Enabled
		})).
		Return(domain.View{Routine: routineFixture()}, nil).
		Once()

	resp := do(t, app, fiber.MethodPost, "/api/routines",
		`{"agent_id":"`+agentID.String()+`","name":"Sepanjang hari","kind":"hourly_range",`+
			`"hour_start":8,"hour_end":21,"minute":30,"instruction":"Cek sesuatu."}`)

	require.Equal(t, fiber.StatusCreated, resp.Code)
	require.Contains(t, resp.Body.String(), "routine created")
}

// TestCreateDefaultsToEnabled: a routine somebody just wrote is one they want to run.
func TestCreateDefaultsToEnabled(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	service.EXPECT().
		Create(mock.Anything, mock.Anything, mock.MatchedBy(func(request domain.WriteRequest) bool {
			return request.Enabled
		})).
		Return(domain.View{Routine: routineFixture()}, nil).
		Once()

	resp := do(t, app, fiber.MethodPost, "/api/routines",
		`{"agent_id":"`+uuid.NewString()+`","name":"Pagi","kind":"daily","hour_start":6,"instruction":"Kerjakan."}`)

	require.Equal(t, fiber.StatusCreated, resp.Code)
}

// TestCreateRefusesAMalformedBoluID: the id is the caller's problem, and saying so is
// cheaper than a database round trip.
func TestCreateRefusesAMalformedBoluID(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	resp := do(t, app, fiber.MethodPost, "/api/routines",
		`{"agent_id":"bukan-uuid","name":"X","kind":"daily","hour_start":6,"instruction":"Kerjakan."}`)

	require.Equal(t, fiber.StatusBadRequest, resp.Code)
}

// TestPauseAndResumeAreTheirOwnRoutes: the switch has two endpoints rather than one
// with a body, so a client cannot send "pause" as the wrong type.
func TestPauseAndResumeAreTheirOwnRoutes(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())
	id := uuid.New()

	paused := routineFixture()
	paused.Enabled = false

	service.EXPECT().SetEnabled(mock.Anything, mock.Anything, id, false).
		Return(domain.View{Routine: paused}, nil).Once()
	resp := do(t, app, fiber.MethodPost, "/api/routines/"+id.String()+"/pause", "")
	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"enabled":false`)
	require.Contains(t, resp.Body.String(), "dijeda")

	resumed := routineFixture()
	service.EXPECT().SetEnabled(mock.Anything, mock.Anything, id, true).
		Return(domain.View{Routine: resumed}, nil).Once()
	resp = do(t, app, fiber.MethodPost, "/api/routines/"+id.String()+"/resume", "")
	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), "dilanjutkan")
}

// TestRunsAreFilteredAndScoped: the history is read per routine, and reading the routine
// first is what makes another workspace's id answer "not found" rather than an empty
// list.
func TestRunsAreFilteredAndScoped(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())
	routine := routineFixture()

	service.EXPECT().Routine(mock.Anything, mock.Anything, routine.ID).
		Return(domain.View{Routine: routine}, nil).Once()

	finished := time.Now()
	taskID := uuid.New()
	service.EXPECT().
		Runs(mock.Anything, mock.Anything, mock.MatchedBy(func(filter domain.RunFilter) bool {
			return filter.RoutineID == routine.ID
		})).
		Return([]domain.Run{{
			ID:          uuid.New(),
			RoutineID:   routine.ID,
			RoutineName: routine.Name,
			TaskID:      taskID,
			Status:      domain.RunSucceeded,
			Summary:     "4 file dirapikan",
			StartedAt:   time.Now(),
			FinishedAt:  &finished,
		}}, nil).
		Once()

	resp := do(t, app, fiber.MethodGet, "/api/routines/"+routine.ID.String()+"/runs", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"summary":"4 file dirapikan"`)
	require.Contains(t, resp.Body.String(), taskID.String())
	require.Contains(t, resp.Body.String(), `"status":"succeeded"`)
}

// TestAHistoryLineForADeletedRoutineOmitsItsLink: the run survives its routine, so its
// id is absent rather than the zero uuid — which a client would otherwise read as a
// routine that exists.
func TestAHistoryLineForADeletedRoutineOmitsItsLink(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	service.EXPECT().
		Runs(mock.Anything, mock.Anything, mock.Anything).
		Return([]domain.Run{{
			ID:          uuid.New(),
			RoutineName: "Cek dokumen yang sudah dihapus",
			Status:      domain.RunSucceeded,
			Summary:     "3 file dirapikan",
			StartedAt:   time.Now(),
		}}, nil).
		Once()

	resp := do(t, app, fiber.MethodGet, "/api/routine-runs", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"routine_name":"Cek dokumen yang sudah dihapus"`)
	require.NotContains(t, resp.Body.String(), "00000000-0000-0000-0000-000000000000")
}

// TestAllRunsAcceptsABoluFilter is what the screen's per-Bolu filter reads.
func TestAllRunsAcceptsABoluFilter(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())
	agentID := uuid.New()

	service.EXPECT().
		Runs(mock.Anything, mock.Anything, mock.MatchedBy(func(filter domain.RunFilter) bool {
			return filter.AgentID == agentID && filter.Status == domain.RunSkipped
		})).
		Return(nil, nil).
		Once()

	resp := do(t, app, fiber.MethodGet, "/api/routine-runs?agent_id="+agentID.String()+"&status=skipped", "")
	require.Equal(t, fiber.StatusOK, resp.Code)
}

// TestPreviewAnswersTimes: the form asks before anything is stored.
func TestPreviewAnswersTimes(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())
	at := time.Now().Add(2 * time.Hour)

	service.EXPECT().
		Preview(mock.Anything, mock.Anything, mock.MatchedBy(func(request domain.PreviewRequest) bool {
			return request.Kind == domain.KindWeekly && request.DayOfWeek == 3 && request.HourStart == 7
		})).
		Return([]time.Time{at}, nil).
		Once()

	resp := do(t, app, fiber.MethodPost, "/api/routines/preview",
		`{"kind":"weekly","hour_start":7,"minute":0,"day_of_week":3}`)

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), at.Format(time.RFC3339))
}

// TestAnUnreachableEngineIsA503: the request was fine and the platform cannot schedule
// it, which is not the caller's fault.
func TestAnUnreachableEngineIsA503(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	service.EXPECT().
		Create(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.View{}, domain.ErrScheduleUnavailable).
		Once()

	resp := do(t, app, fiber.MethodPost, "/api/routines",
		`{"agent_id":"`+uuid.NewString()+`","name":"Pagi","kind":"daily","hour_start":6,"instruction":"Kerjakan."}`)

	require.Equal(t, fiber.StatusServiceUnavailable, resp.Code)
	require.Contains(t, resp.Body.String(), "routine_unavailable")
}

// TestAnInvalidCadenceReadsAsASentence: the refusal names what is wrong, because the
// handler returns the service's own message.
func TestAnInvalidCadenceReadsAsASentence(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	service.EXPECT().
		Create(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.View{}, domain.ErrInvalidInput).
		Once()

	resp := do(t, app, fiber.MethodPost, "/api/routines",
		`{"agent_id":"`+uuid.NewString()+`","name":"Terbalik","kind":"hourly_range","hour_start":21,"hour_end":8,"instruction":"Kerjakan."}`)

	require.Equal(t, fiber.StatusBadRequest, resp.Code)
	require.Contains(t, resp.Body.String(), "invalid_input")
	require.NotContains(t, resp.Body.String(), "routine: invalid input", "the prefix is this module's own name")
}

// TestAMissingRoutineIsA404, and a malformed id is the caller's problem.
func TestMissingAndMalformedIdentifiers(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	service.EXPECT().Routine(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.View{}, domain.ErrNotFound).Once()
	require.Equal(t, fiber.StatusNotFound, do(t, app, fiber.MethodGet, "/api/routines/"+uuid.NewString(), "").Code)

	require.Equal(t, fiber.StatusBadRequest, do(t, app, fiber.MethodGet, "/api/routines/bukan-uuid", "").Code)
	require.Equal(t, fiber.StatusBadRequest, do(t, app, fiber.MethodPost, "/api/routines/bukan-uuid/pause", "").Code)
	require.Equal(t, fiber.StatusBadRequest, do(t, app, fiber.MethodGet, "/api/routines?agent_id=bukan-uuid", "").Code)
	require.Equal(t, fiber.StatusBadRequest, do(t, app, fiber.MethodPost, "/api/routines", "{").Code)
	require.Equal(t, fiber.StatusBadRequest, do(t, app, fiber.MethodPost, "/api/routines/preview", "{").Code)
}

// TestAnUnexpectedFailureIsAnInternalError: an error the handler does not recognise is
// a 500 with a code, and the detail stays in the log.
func TestAnUnexpectedFailureIsAnInternalError(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	service.EXPECT().Routines(mock.Anything, mock.Anything, mock.Anything).
		Return(nil, errors.New("the database is on fire")).Once()

	resp := do(t, app, fiber.MethodGet, "/api/routines", "")
	require.Equal(t, fiber.StatusInternalServerError, resp.Code)
	require.NotContains(t, resp.Body.String(), "on fire")
}
