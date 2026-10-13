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

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
)

// The handler tests cover the endpoints' shape: what a client reads, which
// decision reaches the service, and which error becomes which status code.

// stubService records what the handler asked of the runtime.
type stubService struct {
	drafts    []domain.Draft
	revisions []domain.Revision
	policies  []domain.Policy
	audit     []domain.AuditRecord
	draft     domain.Draft
	policy    domain.Policy
	err       error
	// decided records the decision the handler made.
	decided   string
	gotNote   string
	gotFilter domain.Filter
	gotPolicy domain.PolicyRequest
}

func (s *stubService) Request(context.Context, domain.Scope, domain.Request) (domain.Outcome, error) {
	return domain.Outcome{}, errors.New("not used")
}

func (s *stubService) Approve(_ context.Context, _ domain.Scope, draftID, userID uuid.UUID, note string) (domain.Draft, error) {
	s.decided, s.gotNote = "approve", note
	if s.err != nil {
		return domain.Draft{}, s.err
	}
	draft := s.draft
	draft.ID = draftID
	draft.Status = domain.StatusApproved
	draft.DecidedBy = userID
	draft.DecisionNote = note
	return draft, nil
}

func (s *stubService) Revise(_ context.Context, _ domain.Scope, draftID, userID uuid.UUID, note string) (domain.Draft, error) {
	s.decided, s.gotNote = "revise", note
	if s.err != nil {
		return domain.Draft{}, s.err
	}
	draft := s.draft
	draft.ID = draftID
	draft.Status = domain.StatusRevise
	draft.DecidedBy = userID
	draft.DecisionNote = note
	return draft, nil
}

func (s *stubService) Cancel(_ context.Context, _ domain.Scope, draftID, userID uuid.UUID, note string) (domain.Draft, error) {
	s.decided, s.gotNote = "cancel", note
	if s.err != nil {
		return domain.Draft{}, s.err
	}
	draft := s.draft
	draft.ID = draftID
	draft.Status = domain.StatusCanceled
	draft.DecidedBy = userID
	return draft, nil
}

func (s *stubService) CarryOut(context.Context, domain.Scope, uuid.UUID) (domain.Outcome, error) {
	return domain.Outcome{}, errors.New("not used")
}

func (s *stubService) SendWindow(context.Context, domain.Scope) (time.Time, error) {
	return time.Time{}, errors.New("not used")
}

func (s *stubService) Drafts(_ context.Context, _ domain.Scope, filter domain.Filter) ([]domain.Draft, error) {
	s.gotFilter = filter
	return s.drafts, s.err
}

func (s *stubService) Draft(_ context.Context, _ domain.Scope, _ uuid.UUID) (domain.Draft, error) {
	return s.draft, s.err
}

func (s *stubService) Revisions(context.Context, domain.Scope, uuid.UUID) ([]domain.Revision, error) {
	return s.revisions, s.err
}

func (s *stubService) Policies(context.Context, domain.Scope) ([]domain.Policy, error) {
	return s.policies, s.err
}

func (s *stubService) SetPolicy(_ context.Context, _ domain.Scope, req domain.PolicyRequest) (domain.Policy, error) {
	s.gotPolicy = req
	return s.policy, s.err
}

func (s *stubService) Audit(context.Context, domain.Scope, domain.AuditFilter) ([]domain.AuditRecord, error) {
	return s.audit, s.err
}

// recordingSignaler records the decisions handed to the runtime.
type recordingSignaler struct {
	calls    int
	approved bool
	note     string
	draftID  uuid.UUID
	err      error
}

func (s *recordingSignaler) SignalApproval(_ context.Context, _ domain.Scope, _, draftID uuid.UUID, approved bool, note string) error {
	s.calls++
	s.approved, s.note, s.draftID = approved, note, draftID
	return s.err
}

func newApp(t *testing.T, service domain.Service, signaler Signaler) *fiber.App {
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

	handler := New(service, zap.NewNop())
	if signaler != nil {
		handler = handler.WithSignaler(signaler)
	}

	group := app.Group("/api", func(c *fiber.Ctx) error {
		userID := uuid.New()
		middleware.SetIdentity(c, userID, "owner@example.com")
		middleware.SetScope(c, database.Scope{UserID: userID, WorkspaceID: workspaceID})
		middleware.SetRole(c, "owner")
		return c.Next()
	})

	Routes(group, handler)
	ManagerRoutes(group, handler)

	return app
}

var workspaceID = uuid.New()

func do(t *testing.T, app *fiber.App, method, path, body string) (int, map[string]any) {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)

	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var envelope map[string]any
	require.NoError(t, json.Unmarshal(raw, &envelope))

	return resp.StatusCode, envelope
}

func sampleDraft() domain.Draft {
	now := time.Now()
	opensAt := now.Add(3 * time.Hour)

	return domain.Draft{
		ID:             uuid.New(),
		WorkspaceID:    workspaceID,
		TaskID:         uuid.New(),
		AgentID:        uuid.New(),
		ConversationID: uuid.New(),
		ActionKind:     "gmail.send",
		Title:          "Kirim tagihan Maret",
		Summary:        "Tagihan untuk pelanggan A",
		Payload:        json.RawMessage(`{"to":"a@b.c","subject":"Tagihan Maret","amount":150000}`),
		Status:         domain.StatusApproved,
		Revision:       1,
		DecidedBy:      uuid.New(),
		DecidedAt:      &now,
		DecisionNote:   "kirim",
		NotBefore:      &opensAt,
		BlockedReason:  "ditahan sampai jam kerja",
		CardMessageID:  uuid.New(),
		IdempotencyKey: "key-1",
		CreatedAt:      now,
	}
}

// TestListReturnsTheDraftsWithTheirFilters is the dashboard's read: the payload
// carries the card's fields and the filters reach the service.
func TestListReturnsTheDraftsWithTheirFilters(t *testing.T) {
	service := &stubService{drafts: []domain.Draft{sampleDraft()}}
	app := newApp(t, service, nil)

	agentID := uuid.New()
	code, envelope := do(t, app, http.MethodGet,
		"/api/drafts?status=pending&action_kind=gmail.send&limit=5&agent_id="+agentID.String(), "")

	require.Equal(t, http.StatusOK, code)
	require.Equal(t, domain.StatusPending, service.gotFilter.Status)
	require.Equal(t, "gmail.send", service.gotFilter.ActionKind)
	require.Equal(t, 5, service.gotFilter.Limit)
	require.Equal(t, agentID, service.gotFilter.AgentID)

	data, ok := envelope["data"].([]any)
	require.True(t, ok)
	require.Len(t, data, 1)

	first, ok := data[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "gmail.send", first["action_kind"])
	require.Equal(t, "Kirim tagihan Maret", first["title"])
	require.Equal(t, "approved", first["status"])
	require.Equal(t, float64(1), first["revision"])
	require.Equal(t, "ditahan sampai jam kerja", first["blocked_reason"])
	require.NotEmpty(t, first["not_before"], "the card says until when it is held")

	// The action's own payload reaches the client, which is what the card shows.
	payload, ok := first["payload"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "a@b.c", payload["to"])
	require.Equal(t, float64(150000), payload["amount"])
}

// TestListRefusesAMalformedFilter is the query guard: a filter that is not a uuid
// is refused rather than silently ignored.
func TestListRefusesAMalformedFilter(t *testing.T) {
	app := newApp(t, &stubService{}, nil)

	code, envelope := do(t, app, http.MethodGet, "/api/drafts?agent_id=bukan-uuid", "")
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, false, envelope["success"])

	code, _ = do(t, app, http.MethodGet, "/api/drafts?task_id=bukan-uuid", "")
	require.Equal(t, http.StatusBadRequest, code)
}

// TestGetReturnsOneDraft covers the read the card makes.
func TestGetReturnsOneDraft(t *testing.T) {
	draft := sampleDraft()
	app := newApp(t, &stubService{draft: draft}, nil)

	code, envelope := do(t, app, http.MethodGet, "/api/drafts/"+draft.ID.String(), "")
	require.Equal(t, http.StatusOK, code)

	data, ok := envelope["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, draft.ID.String(), data["draft_id"])
	require.Equal(t, draft.TaskID.String(), data["task_id"])
	require.Equal(t, draft.CardMessageID.String(), data["card_message_id"])
	require.Equal(t, "key-1", data["idempotency_key"])
}

// TestGetMapsAMissingDraftTo404 is the error mapping.
func TestGetMapsAMissingDraftTo404(t *testing.T) {
	app := newApp(t, &stubService{err: domain.ErrDraftNotFound}, nil)

	code, envelope := do(t, app, http.MethodGet, "/api/drafts/"+uuid.NewString(), "")
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, "draft_not_found", envelope["error"].(map[string]any)["code"])
}

// TestApproveDecidesAndSignalsTheTask is the whole decision path: the answer is
// stored, and the task waiting on it is told.
func TestApproveDecidesAndSignalsTheTask(t *testing.T) {
	draft := sampleDraft()
	signaler := &recordingSignaler{}
	app := newApp(t, &stubService{draft: draft}, signaler)

	code, envelope := do(t, app, http.MethodPost, "/api/drafts/"+draft.ID.String()+"/approve",
		`{"note":"kirim ya"}`)

	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "approved", envelope["data"].(map[string]any)["status"])

	// The decision reached the running task, with the draft it is about.
	require.Equal(t, 1, signaler.calls)
	require.True(t, signaler.approved)
	require.Equal(t, "kirim ya", signaler.note)
	require.Equal(t, draft.ID, signaler.draftID)
}

// TestReviseSendsTheNoteToTheTask is the other decision: the note travels with it,
// because that is what the agent reads on its next round.
func TestReviseSendsTheNoteToTheTask(t *testing.T) {
	draft := sampleDraft()
	signaler := &recordingSignaler{}
	app := newApp(t, &stubService{draft: draft}, signaler)

	code, envelope := do(t, app, http.MethodPost, "/api/drafts/"+draft.ID.String()+"/revise",
		`{"note":"salah penerima"}`)

	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "revise", envelope["data"].(map[string]any)["status"])
	require.Equal(t, 1, signaler.calls)
	require.False(t, signaler.approved)
	require.Equal(t, "salah penerima", signaler.note)
}

// TestASecondDecisionIsRefused is the idempotence the epic asks for: a signal for a
// draft that already has an answer changes nothing.
func TestASecondDecisionIsRefused(t *testing.T) {
	app := newApp(t, &stubService{err: domain.ErrAlreadyDecided}, nil)

	code, envelope := do(t, app, http.MethodPost, "/api/drafts/"+uuid.NewString()+"/approve", `{}`)
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, "draft_already_decided", envelope["error"].(map[string]any)["code"])
}

// TestADecisionIsStoredEvenWhenTheTaskCannotBeTold is the honest degradation: the
// decision is durable, and a signal that could not be delivered costs latency
// rather than the decision, because the workflow reads its own status.
func TestADecisionIsStoredEvenWhenTheTaskCannotBeTold(t *testing.T) {
	draft := sampleDraft()
	signaler := &recordingSignaler{err: errors.New("temporal is unreachable")}
	app := newApp(t, &stubService{draft: draft}, signaler)

	code, envelope := do(t, app, http.MethodPost, "/api/drafts/"+draft.ID.String()+"/approve", `{}`)

	require.Equal(t, http.StatusOK, code, "the decision is what the caller asked for")
	require.Equal(t, "approved", envelope["data"].(map[string]any)["status"])
	require.Equal(t, 1, signaler.calls, "the signal was attempted")
}

// TestCarryOutRefusalMapsToConflict is the guardrail's answer at the API.
func TestCarryOutRefusalMapsToConflict(t *testing.T) {
	app := newApp(t, &stubService{err: domain.ErrNotApproved}, nil)

	code, envelope := do(t, app, http.MethodPost, "/api/drafts/"+uuid.NewString()+"/cancel", `{}`)
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, "draft_not_approved", envelope["error"].(map[string]any)["code"])
}

// TestRevisionsReturnTheTrail covers the read the card uses to show why a draft
// was sent back.
func TestRevisionsReturnTheTrail(t *testing.T) {
	service := &stubService{revisions: []domain.Revision{
		{ID: uuid.New(), Revision: 1, Note: "salah penerima", Payload: json.RawMessage(`{"to":"a@b.c"}`), CreatedAt: time.Now()},
		{ID: uuid.New(), Revision: 2, Note: "nominalnya salah", Payload: json.RawMessage(`{"to":"z@b.c"}`), CreatedAt: time.Now()},
	}}
	app := newApp(t, service, nil)

	code, envelope := do(t, app, http.MethodGet, "/api/drafts/"+uuid.NewString()+"/revisions", "")
	require.Equal(t, http.StatusOK, code)

	data, ok := envelope["data"].([]any)
	require.True(t, ok)
	require.Len(t, data, 2)
	require.Equal(t, "salah penerima", data[0].(map[string]any)["note"])
	require.Equal(t, float64(2), data[1].(map[string]any)["revision"])
}

// TestPoliciesAreReadableAndWritable covers the policy screen.
func TestPoliciesAreReadableAndWritable(t *testing.T) {
	ceiling := int64(250_000)
	service := &stubService{
		policies: []domain.Policy{
			{ActionKind: "gmail.send", Mode: domain.ModeAuto, MaxAmount: &ceiling, UpdatedAt: time.Now()},
			{ActionKind: "whatsapp.send_message", Mode: domain.ModeAlwaysDraft},
		},
		policy: domain.Policy{ActionKind: "gmail.send", Mode: domain.ModeAuto, MaxAmount: &ceiling, UpdatedAt: time.Now()},
	}
	app := newApp(t, service, nil)

	code, envelope := do(t, app, http.MethodGet, "/api/approval-policies", "")
	require.Equal(t, http.StatusOK, code)

	data, ok := envelope["data"].([]any)
	require.True(t, ok)
	require.Len(t, data, 2)
	require.Equal(t, "auto", data[0].(map[string]any)["mode"])
	require.Equal(t, float64(250_000), data[0].(map[string]any)["max_amount"])
	require.Equal(t, "always_draft", data[1].(map[string]any)["mode"])

	code, envelope = do(t, app, http.MethodPut, "/api/approval-policies",
		`{"action_kind":"gmail.send","mode":"auto","max_amount":250000}`)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "gmail.send", service.gotPolicy.ActionKind)
	require.Equal(t, domain.ModeAuto, service.gotPolicy.Mode)
	require.NotNil(t, service.gotPolicy.MaxAmount)
	require.Equal(t, ceiling, *service.gotPolicy.MaxAmount)
	require.Equal(t, "auto", envelope["data"].(map[string]any)["mode"])
}

// TestSetPolicyRefusesAnUnknownMode maps the guard.
func TestSetPolicyRefusesAnUnknownMode(t *testing.T) {
	app := newApp(t, &stubService{err: domain.ErrUnknownMode}, nil)

	code, envelope := do(t, app, http.MethodPut, "/api/approval-policies",
		`{"action_kind":"gmail.send","mode":"kadang"}`)
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, "unknown_mode", envelope["error"].(map[string]any)["code"])
}

// TestSetPolicyRefusesAMalformedBody covers the request parse.
func TestSetPolicyRefusesAMalformedBody(t *testing.T) {
	app := newApp(t, &stubService{}, nil)

	code, envelope := do(t, app, http.MethodPut, "/api/approval-policies", `{bukan json`)
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, "invalid_request", envelope["error"].(map[string]any)["code"])
}

// TestAuditReturnsTheTrail covers the read the audit page is built from.
func TestAuditReturnsTheTrail(t *testing.T) {
	service := &stubService{audit: []domain.AuditRecord{
		{
			ID: 2, Action: "draft.sent", ObjectType: "draft", ObjectID: uuid.NewString(),
			ActorUserID: uuid.New(), ActorAgentID: uuid.New(),
			Metadata:  map[string]any{"action_kind": "gmail.send"},
			CreatedAt: time.Now(),
		},
	}}
	app := newApp(t, service, nil)

	code, envelope := do(t, app, http.MethodGet, "/api/audit?limit=10", "")
	require.Equal(t, http.StatusOK, code)

	data, ok := envelope["data"].([]any)
	require.True(t, ok)
	require.Len(t, data, 1)

	first := data[0].(map[string]any)
	require.Equal(t, "draft.sent", first["action"])
	require.Equal(t, "draft", first["object_type"])
	require.NotEmpty(t, first["actor_user_id"])
	require.NotEmpty(t, first["actor_agent_id"])
}

// TestMalformedIDsAreRefused covers the path parameters: a path that is not a uuid
// never reaches the service.
func TestMalformedIDsAreRefused(t *testing.T) {
	app := newApp(t, &stubService{}, nil)

	for _, path := range []string{
		"/api/drafts/bukan-uuid",
		"/api/drafts/bukan-uuid/revisions",
	} {
		code, _ := do(t, app, http.MethodGet, path, "")
		require.Equal(t, http.StatusBadRequest, code, path)
	}

	for _, path := range []string{
		"/api/drafts/bukan-uuid/approve",
		"/api/drafts/bukan-uuid/revise",
		"/api/drafts/bukan-uuid/cancel",
	} {
		code, _ := do(t, app, http.MethodPost, path, `{}`)
		require.Equal(t, http.StatusBadRequest, code, path)
	}
}

// TestInternalErrorsDoNotLeakDetail is the safe failure: an unexpected error is
// logged and answered generically, never echoed to the client.
func TestInternalErrorsDoNotLeakDetail(t *testing.T) {
	app := newApp(t, &stubService{err: errors.New("pq: password authentication failed for user \"bolu_app\"")}, nil)

	code, envelope := do(t, app, http.MethodGet, "/api/drafts/"+uuid.NewString(), "")
	require.Equal(t, http.StatusInternalServerError, code)
	require.Equal(t, "internal server error", envelope["message"])
	require.NotContains(t, envelope["message"], "password")
}
