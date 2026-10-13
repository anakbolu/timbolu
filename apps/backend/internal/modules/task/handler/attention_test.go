package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
)

// The attention list, and the link that diagnoses a stuck task.
//
// The link is a deployment fact, so it is built server-side: a frontend that assembled it from
// configuration it does not have would be a broken link in production and a working one on a laptop.

// attentionService answers the two reads the list makes, with scripted tasks.
type attentionService struct {
	domain.Service

	failed []domain.Task
	live   []domain.Task
	now    time.Time
	// filterSeen records what each read asked for, which is what proves the list is assembled from
	// the states it claims to cover.
	filterSeen []domain.Filter
}

func (s *attentionService) History(_ context.Context, _ domain.Scope, filter domain.Filter) ([]domain.Task, error) {
	s.filterSeen = append(s.filterSeen, filter)

	if filter.Live {
		return s.live, nil
	}

	return s.failed, nil
}

// attentionApp builds an app with the route under the same prefix the container uses.
func attentionApp(t *testing.T, svc domain.Service, uiBase, namespace string) *fiber.App {
	t.Helper()

	handler := New(svc, zap.NewNop()).WithTemporalUI(uiBase, namespace)

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(func(c *fiber.Ctx) error {
		c.Locals("workspace_id", "33333333-3333-3333-3333-333333333333")
		return c.Next()
	})
	group := app.Group("/api")
	Routes(group, handler)

	return app
}

// attentionFixture is one of each kind the list covers.
//
// The moments are relative to the wall clock rather than to a fixed date, because the handler derives
// each task's health from *now*: a fixture pinned to a date five months back would report its running
// tasks as stuck, which is the very distinction these tests are about.
func attentionFixture() (*attentionService, time.Time) {
	now := time.Now().UTC()
	stale := now.Add(-10 * time.Minute)
	fresh := now.Add(-10 * time.Second)

	base := domain.Task{
		WorkspaceID:   uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		AgentID:       uuid.MustParse("44444444-4444-4444-4444-444444444444"),
		Trigger:       domain.TriggerChat,
		Title:         "Rekap pesanan",
		WorkflowID:    "task-abc",
		TemporalRunID: "run-1",
	}

	failed := base
	failed.ID = uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	failed.Status = domain.StatusFailed
	failed.StoppedReason = "tugas berhenti karena mencapai batas langkah (1)"
	failed.FinishedAt = &now

	stuck := base
	stuck.ID = uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	stuck.Status = domain.StatusRunning
	stuck.HeartbeatAt = &stale

	waiting := base
	waiting.ID = uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	waiting.Status = domain.StatusWaitingApproval
	waiting.HeartbeatAt = &fresh

	// A task that is running normally is not on the list: it is doing its job, and listing it
	// would bury the ones that are not.
	running := base
	running.ID = uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	running.Status = domain.StatusRunning
	running.HeartbeatAt = &fresh

	return &attentionService{
		failed: []domain.Task{failed},
		live:   []domain.Task{stuck, waiting, running},
		now:    now,
	}, now
}

// TestTheListCoversTheThreeStatesAndNotTheFourth is the whole point of the screen: a task that is
// going to move on its own is not on it.
func TestTheListCoversTheThreeStatesAndNotTheFourth(t *testing.T) {
	svc, _ := attentionFixture()
	app := attentionApp(t, svc, "http://localhost:8233", "default")

	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/tasks/attention", nil), -1)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, 200, response.StatusCode)

	var payload struct {
		Data []attentionPayload `json:"data"`
		Meta struct {
			Count int `json:"count"`
		} `json:"meta"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&payload))

	require.Equal(t, 3, payload.Meta.Count)

	reasons := map[string]string{}
	for _, task := range payload.Data {
		reasons[task.TaskID] = task.Attention
	}

	require.Equal(t, attentionFailed, reasons["aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"])
	require.Equal(t, attentionStuck, reasons["bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"])
	require.Equal(t, attentionWaiting, reasons["cccccccc-cccc-cccc-cccc-cccccccccccc"])
	require.NotContains(t, reasons, "dddddddd-dddd-dddd-dddd-dddddddddddd",
		"a task that is running is doing its job")
}

// TestFailuresComeBeforeTheWaitingOnes: a person with an approval to make and a failure to look at
// should make the decision first — the parked task is holding work that is otherwise done.
func TestFailuresComeBeforeTheWaitingOnes(t *testing.T) {
	svc, _ := attentionFixture()
	app := attentionApp(t, svc, "http://localhost:8233", "default")

	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/tasks/attention", nil), -1)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()

	var payload struct {
		Data []attentionPayload `json:"data"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&payload))

	require.Len(t, payload.Data, 3)
	require.Equal(t, attentionFailed, payload.Data[0].Attention)
	require.Equal(t, attentionStuck, payload.Data[1].Attention)
	require.Equal(t, attentionWaiting, payload.Data[2].Attention)
}

// TestTheTemporalLinkNamesTheNamespaceTheWorkflowAndTheRun: a workflow id is not unique over time, so
// a URL without the run opens whichever run the UI picks.
func TestTheTemporalLinkNamesTheNamespaceTheWorkflowAndTheRun(t *testing.T) {
	svc, _ := attentionFixture()
	app := attentionApp(t, svc, "http://localhost:8233/", "produksi")

	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/tasks/attention", nil), -1)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()

	var payload struct {
		Data []attentionPayload `json:"data"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&payload))

	require.Equal(t, "http://localhost:8233/namespaces/produksi/workflows/task-abc/run-1",
		payload.Data[0].TemporalURL, "the base's trailing slash is not doubled")
}

// TestNoConfiguredUITakesTheLinkOffThePayload: a deployment whose UI is not reachable from where the
// person reading must not offer a link that does not answer.
func TestNoConfiguredUITakesTheLinkOffThePayload(t *testing.T) {
	svc, _ := attentionFixture()
	app := attentionApp(t, svc, "  ", "")

	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/tasks/attention", nil), -1)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()

	var payload struct {
		Data []attentionPayload `json:"data"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&payload))

	for _, task := range payload.Data {
		require.Empty(t, task.TemporalURL)
	}
}

// TestATaskThatNeverReachedTheEngineHasNoLink: a task recorded and queued has no workflow, and a link
// to a workflow that does not exist is worse than no link.
func TestATaskThatNeverReachedTheEngineHasNoLink(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	svc := &attentionService{
		failed: []domain.Task{{
			ID:            uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
			WorkspaceID:   uuid.MustParse("33333333-3333-3333-3333-333333333333"),
			Status:        domain.StatusFailed,
			StoppedReason: "tidak bisa dijalankan",
			Title:         "Rekap pesanan",
			FinishedAt:    &now,
		}},
		now: now,
	}

	app := attentionApp(t, svc, "http://localhost:8233", "default")

	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/tasks/attention", nil), -1)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()

	var payload struct {
		Data []attentionPayload `json:"data"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&payload))

	require.Len(t, payload.Data, 1)
	require.Empty(t, payload.Data[0].TemporalURL)
	require.Equal(t, "tidak bisa dijalankan", payload.Data[0].StoppedReason,
		"and the reason it is on the list is on the payload")
}

// TestTheListIsRegisteredBeforeTheByIdRoute is the routing order from the other side: `/tasks/attention`
// must not be read as a task id.
func TestTheListIsRegisteredBeforeTheByIdRoute(t *testing.T) {
	svc, _ := attentionFixture()
	app := attentionApp(t, svc, "", "")

	paths := []string{}
	for _, route := range app.GetRoutes() {
		if route.Method == http.MethodGet {
			paths = append(paths, route.Path)
		}
	}

	attention := indexOf(paths, "/api/tasks/attention")
	byID := indexOf(paths, "/api/tasks/:taskID")
	require.GreaterOrEqual(t, attention, 0, "the route exists")
	require.GreaterOrEqual(t, byID, 0)
	require.Less(t, attention, byID,
		"Fiber matches in registration order, so the literal route must come first")
}

// indexOf is the position of a value, or -1.
func indexOf(values []string, want string) int {
	for index, value := range values {
		if value == want {
			return index
		}
	}

	return -1
}

// TestTheListAsksForTheStatesItCovers: the two reads are what the list is assembled from, so a filter
// that stopped asking for one of them would silently drop a whole section.
func TestTheListAsksForTheStatesItCovers(t *testing.T) {
	svc, _ := attentionFixture()
	app := attentionApp(t, svc, "", "")

	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/tasks/attention?limit=5", nil), -1)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, 200, response.StatusCode)

	require.Len(t, svc.filterSeen, 2)
	require.Equal(t, domain.StatusFailed, svc.filterSeen[0].Status)
	require.False(t, svc.filterSeen[0].Live)
	require.True(t, svc.filterSeen[1].Live)

	for _, filter := range svc.filterSeen {
		require.Equal(t, 5, filter.Limit, "the bound applies to each read, so each kind is shown")
	}
}
