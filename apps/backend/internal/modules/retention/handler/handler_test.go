package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/retention/domain"
)

// The erasure endpoints, and the two properties the handler is responsible for: who is recorded as
// having asked, and what a failure says.

// stubErasureService answers with a scripted result.
type stubErasureService struct {
	result  domain.ErasureResult
	erasure domain.Erasure
	err     error
	// seen records the request, which is what proves the person comes from the session.
	seen []domain.ErasureRequest
}

func (s *stubErasureService) Erase(_ context.Context, request domain.ErasureRequest) (domain.ErasureResult, error) {
	s.seen = append(s.seen, request)
	if s.err != nil {
		return domain.ErasureResult{}, s.err
	}

	return s.result, nil
}

func (s *stubErasureService) FindErasure(context.Context, uuid.UUID) (domain.Erasure, error) {
	if s.err != nil {
		return domain.Erasure{}, s.err
	}

	return s.erasure, nil
}

func (s *stubErasureService) Erasures(context.Context, int) ([]domain.Erasure, error) {
	if s.err != nil {
		return nil, s.err
	}

	return []domain.Erasure{s.erasure}, nil
}

// erasureApp builds an app with the routes under a session identity, which is what the middleware
// hands the handler in the container.
func erasureApp(t *testing.T, service Service, workspaceID, userID uuid.UUID) *fiber.App {
	t.Helper()

	handler := New(service, zap.NewNop())

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(func(c *fiber.Ctx) error {
		// The two values the container's own middleware sets: the active workspace as a string and
		// the authenticated user as a uuid. Local keys are unexported, so the shapes here are the
		// ones the accessors read.
		c.Locals("workspace_id", workspaceID.String())
		c.Locals("user_id", userID)
		return c.Next()
	})
	group := app.Group("/api")
	OwnerRoutes(group, handler)

	return app
}

// TestEraseRecordsThePersonFromTheSession: who asked is part of the record, and it must come from the
// authenticated request rather than from a body a caller could type somebody else's id into.
func TestEraseRecordsThePersonFromTheSession(t *testing.T) {
	service := &stubErasureService{result: domain.ErasureResult{Objects: 3, Rows: 41}}
	workspaceID, userID := uuid.New(), uuid.New()

	app := erasureApp(t, service, workspaceID, userID)

	// The body carries nothing, and a body that tried to name somebody else would still not change
	// who is recorded — the handler never reads one.
	request := httptest.NewRequest(http.MethodPost, "/api/workspaces/current/erasure",
		strings.NewReader(`{"requested_by":"00000000-0000-0000-0000-000000000001"}`))
	request.Header.Set("Content-Type", "application/json")

	response, err := app.Test(request, -1)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, http.StatusOK, response.StatusCode)

	require.Len(t, service.seen, 1)
	require.Equal(t, workspaceID, service.seen[0].WorkspaceID, "the workspace is the active one")
	require.Equal(t, userID, service.seen[0].RequestedBy, "the person is the session's, not the body's")

	var payload struct {
		Data struct {
			WorkspaceID string `json:"workspace_id"`
			Objects     int64  `json:"objects"`
			Rows        int64  `json:"rows"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&payload))
	require.Equal(t, workspaceID.String(), payload.Data.WorkspaceID)
	require.Equal(t, int64(3), payload.Data.Objects)
	require.Equal(t, int64(41), payload.Data.Rows)
}

// TestAFailedErasureSaysWhatHappened: the answer a person gets when it did not work has to say
// whether anything was removed, because "we could not erase your data" and "we erased part of it"
// are different things to be told.
func TestAFailedErasureSaysWhatHappened(t *testing.T) {
	service := &stubErasureService{err: errors.New("the bucket refused the deletion")}
	workspaceID, userID := uuid.New(), uuid.New()

	app := erasureApp(t, service, workspaceID, userID)

	response, err := app.Test(httptest.NewRequest(http.MethodPost, "/api/workspaces/current/erasure", nil), -1)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()

	require.Equal(t, http.StatusInternalServerError, response.StatusCode)

	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "erasure_failed")
	require.Contains(t, string(body), "tidak ada yang setengah dihapus",
		"the answer says nothing was half-removed, which is the promise the order of the two steps makes")
}

// TestAnIncompleteDeploymentAnswers503RatherThanNothing: without the record, the workspace deletion,
// and the object walk, the endpoints exist and say the path is not configured — rather than reporting
// a success that removed nothing.
func TestAnIncompleteDeploymentAnswers503RatherThanNothing(t *testing.T) {
	service := &stubErasureService{err: domain.ErrInvalidInput}
	workspaceID, userID := uuid.New(), uuid.New()

	app := erasureApp(t, service, workspaceID, userID)

	response, err := app.Test(httptest.NewRequest(http.MethodPost, "/api/workspaces/current/erasure", nil), -1)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()

	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode)

	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "erasure_unavailable")
}

// TestTheRecordIsReadableInTheUsersWords: the lookup answers with the names and the counts, which is
// what a person who asked for their data to be removed is owed.
func TestTheRecordIsReadableInTheUsersWords(t *testing.T) {
	completed := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	service := &stubErasureService{erasure: domain.Erasure{
		WorkspaceID:     uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		WorkspaceName:   "Toko Sinar",
		RequestedBy:     uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		RequestedByMail: "pemilik@example.com",
		RequestedAt:     completed.Add(-time.Minute),
		CompletedAt:     completed,
		Objects:         7,
		Rows:            42,
	}}
	app := erasureApp(t, service, uuid.MustParse("11111111-1111-1111-1111-111111111111"), uuid.New())

	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/workspaces/current/erasure", nil), -1)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, http.StatusOK, response.StatusCode)

	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	require.Contains(t, string(body), "Toko Sinar")
	require.Contains(t, string(body), "pemilik@example.com")
	require.Contains(t, string(body), `"complete":true`)
	require.Contains(t, string(body), `"rows_deleted":42`)
}

// TestAWorkspaceWithNoRecordAnswers404: a workspace that was never erased is told so, rather than
// being shown an empty record that reads as "nothing was ever removed".
func TestAWorkspaceWithNoRecordAnswers404(t *testing.T) {
	service := &stubErasureService{err: domain.ErrErasureNotFound}
	app := erasureApp(t, service, uuid.New(), uuid.New())

	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/workspaces/current/erasure", nil), -1)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()

	require.Equal(t, http.StatusNotFound, response.StatusCode)
}
