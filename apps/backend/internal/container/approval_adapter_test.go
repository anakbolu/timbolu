package container

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	approvaldomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/domain"
	approvalhandler "github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/handler"
	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	taskdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/config"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
)

// The adapter tests cover the seam between the approval module and the runtime:
// what the task module learns from a proposal, and — most importantly — which of
// the approval module's errors cross the boundary as the task module's own. The
// runtime has to tell a permanent state from a hiccup without importing the module
// that decided it, so a mistranslated error would make the workflow retry
// something that will never succeed.

// stubApproval is an approval service that answers what a test asks.
type stubApproval struct {
	outcome   approvaldomain.Outcome
	carryErr  error
	requested approvaldomain.Request
}

func (s *stubApproval) Request(_ context.Context, _ approvaldomain.Scope, req approvaldomain.Request) (approvaldomain.Outcome, error) {
	s.requested = req
	return s.outcome, nil
}

func (s *stubApproval) Approve(context.Context, approvaldomain.Scope, uuid.UUID, uuid.UUID, string) (approvaldomain.Draft, error) {
	return approvaldomain.Draft{}, errors.New("not used")
}

func (s *stubApproval) Revise(context.Context, approvaldomain.Scope, uuid.UUID, uuid.UUID, string) (approvaldomain.Draft, error) {
	return approvaldomain.Draft{}, errors.New("not used")
}

func (s *stubApproval) Cancel(context.Context, approvaldomain.Scope, uuid.UUID, uuid.UUID, string) (approvaldomain.Draft, error) {
	return approvaldomain.Draft{}, errors.New("not used")
}

func (s *stubApproval) CarryOut(context.Context, approvaldomain.Scope, uuid.UUID) (approvaldomain.Outcome, error) {
	return s.outcome, s.carryErr
}

func (s *stubApproval) SendWindow(context.Context, approvaldomain.Scope) (time.Time, error) {
	return time.Time{}, errors.New("not used")
}

func (s *stubApproval) Drafts(context.Context, approvaldomain.Scope, approvaldomain.Filter) ([]approvaldomain.Draft, error) {
	return nil, errors.New("not used")
}

func (s *stubApproval) Draft(context.Context, approvaldomain.Scope, uuid.UUID) (approvaldomain.Draft, error) {
	return approvaldomain.Draft{}, errors.New("not used")
}

func (s *stubApproval) Revisions(context.Context, approvaldomain.Scope, uuid.UUID) ([]approvaldomain.Revision, error) {
	return nil, errors.New("not used")
}

func (s *stubApproval) Policies(context.Context, approvaldomain.Scope) ([]approvaldomain.Policy, error) {
	return nil, errors.New("not used")
}

func (s *stubApproval) SetPolicy(context.Context, approvaldomain.Scope, approvaldomain.PolicyRequest) (approvaldomain.Policy, error) {
	return approvaldomain.Policy{}, errors.New("not used")
}

func (s *stubApproval) Audit(context.Context, approvaldomain.Scope, int) ([]approvaldomain.AuditRecord, error) {
	return nil, errors.New("not used")
}

// TestTheGateReportsAParkedDraft is the ordinary case: a person has to decide, so
// the task records what it waits on and does not treat the action as done.
func TestTheGateReportsAParkedDraft(t *testing.T) {
	draftID := uuid.New()
	service := &stubApproval{outcome: approvaldomain.Outcome{
		DraftID: draftID,
		Status:  approvaldomain.StatusPending,
		Title:   "Kirim tagihan Maret",
	}}

	gate := approvalGate{service: service}
	draft, err := gate.RequestDraft(context.Background(), taskdomain.Scope{
		UserID: uuid.New(), WorkspaceID: uuid.New(),
	}, taskdomain.DraftRequest{
		TaskID:     uuid.New(),
		AgentID:    uuid.New(),
		ActionKind: "gmail.send",
		Title:      "Kirim tagihan Maret",
		Payload:    json.RawMessage(`{"to":"a@b.c"}`),
	})
	require.NoError(t, err)

	require.Equal(t, draftID, draft.ID)
	require.Equal(t, approvaldomain.StatusPending, draft.Status)
	require.False(t, draft.Auto, "a draft a person must decide is not auto-approved")
	require.Empty(t, draft.Content, "nothing ran, so there is nothing to report")

	// The title doubles as the summary, because the task module only carries one
	// label and a card with no description is worse than a short one.
	require.Equal(t, "Kirim tagihan Maret", service.requested.Title)
	require.Equal(t, "Kirim tagihan Maret", service.requested.Summary)
	require.Equal(t, "gmail.send", service.requested.ActionKind)
}

// TestTheGateReportsAnAutoApprovedAction is the policy case: the workspace allowed
// the kind unattended, so the task must not park on a signal nobody will send.
func TestTheGateReportsAnAutoApprovedAction(t *testing.T) {
	service := &stubApproval{outcome: approvaldomain.Outcome{
		DraftID:  uuid.New(),
		Status:   approvaldomain.StatusSent,
		Title:    "Kirim tagihan Maret",
		Executed: true,
		Content:  json.RawMessage(`{"text":"terkirim"}`),
	}}

	gate := approvalGate{service: service}
	draft, err := gate.RequestDraft(context.Background(), taskdomain.Scope{WorkspaceID: uuid.New()},
		taskdomain.DraftRequest{TaskID: uuid.New(), AgentID: uuid.New(), ActionKind: "gmail.send"})
	require.NoError(t, err)

	require.True(t, draft.Auto, "an action the policy allowed must not be waited on")
	require.Equal(t, approvaldomain.StatusSent, draft.Status)
	require.JSONEq(t, `{"text":"terkirim"}`, string(draft.Content))
}

// TestTheGateReportsAHold is the working-hours case reached through the gate: the
// action is approved and waiting, and the task is told both.
func TestTheGateReportsAHold(t *testing.T) {
	opensAt := time.Now().Add(6 * time.Hour)
	service := &stubApproval{outcome: approvaldomain.Outcome{
		DraftID:   uuid.New(),
		Status:    approvaldomain.StatusApproved,
		Reason:    "ditahan sampai jam kerja: 12 Okt 08:00",
		NotBefore: &opensAt,
	}}

	gate := approvalGate{service: service}
	execution, err := gate.CarryOut(context.Background(), taskdomain.Scope{WorkspaceID: uuid.New()}, uuid.New())
	require.NoError(t, err)

	require.True(t, execution.Held, "an approved action outside hours is held, not sent")
	require.False(t, execution.Status == approvaldomain.StatusSent)
	require.NotNil(t, execution.NotBefore)
	require.Contains(t, execution.Reason, "jam kerja")
}

// TestAnApprovedSendIsNotReportedAsHeld is the guard on the flag's derivation: an
// action that actually ran must not be marked held just because a previous hold
// left a timestamp on the draft.
func TestAnApprovedSendIsNotReportedAsHeld(t *testing.T) {
	opensAt := time.Now().Add(-time.Hour)
	service := &stubApproval{outcome: approvaldomain.Outcome{
		DraftID:   uuid.New(),
		Status:    approvaldomain.StatusSent,
		Executed:  true,
		Content:   json.RawMessage(`{"ok":true}`),
		NotBefore: &opensAt,
	}}

	gate := approvalGate{service: service}
	execution, err := gate.CarryOut(context.Background(), taskdomain.Scope{WorkspaceID: uuid.New()}, uuid.New())
	require.NoError(t, err)

	require.False(t, execution.Held, "an action that ran is not held")
	require.JSONEq(t, `{"ok":true}`, string(execution.Content))
}

// TestPermanentFailuresCrossTheBoundaryAsTheirOwn is the translation the runtime
// depends on: a missing executor and an unapproved draft are states, not hiccups,
// and the task module has to be able to tell without importing the approval
// module. Getting this wrong makes the workflow retry something that will never
// succeed.
func TestPermanentFailuresCrossTheBoundaryAsTheirOwn(t *testing.T) {
	tests := []struct {
		name string
		from error
		want error
	}{
		{"nothing can send", approvaldomain.ErrNoSender, taskdomain.ErrDraftNoSender},
		{"nobody approved", approvaldomain.ErrNotApproved, taskdomain.ErrDraftNotApproved},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gate := approvalGate{service: &stubApproval{carryErr: test.from}}

			_, err := gate.CarryOut(context.Background(), taskdomain.Scope{WorkspaceID: uuid.New()}, uuid.New())
			require.ErrorIs(t, err, test.want, "the runtime must see its own sentinel")
		})
	}
}

// TestAnUnrelatedFailureCrossesUnchanged is the other half: a real hiccup must stay
// retryable, so it is not mistaken for a permanent state.
func TestAnUnrelatedFailureCrossesUnchanged(t *testing.T) {
	unexpected := errors.New("the database is down")
	gate := approvalGate{service: &stubApproval{carryErr: unexpected}}

	_, err := gate.CarryOut(context.Background(), taskdomain.Scope{WorkspaceID: uuid.New()}, uuid.New())
	require.ErrorIs(t, err, unexpected)
	require.NotErrorIs(t, err, taskdomain.ErrDraftNoSender)
	require.NotErrorIs(t, err, taskdomain.ErrDraftNotApproved)
}

// TestTheCardBlockCarriesTheHold is the card's shape: what the person sees,
// including why an approved action has not gone out.
func TestTheCardBlockCarriesTheHold(t *testing.T) {
	opensAt := time.Date(2026, 3, 3, 8, 0, 0, 0, time.UTC)
	draft := approvaldomain.Draft{
		ID:            uuid.New(),
		ActionKind:    "gmail.send",
		Title:         "Kirim tagihan Maret",
		Summary:       "Tagihan untuk pelanggan A",
		Status:        approvaldomain.StatusApproved,
		Revision:      2,
		Payload:       json.RawMessage(`{"to":"a@b.c","subject":"Tagihan Maret","body":"Mohon dibayar"}`),
		NotBefore:     &opensAt,
		BlockedReason: "ditahan sampai jam kerja",
	}

	block := draftCardBlock(draft)
	require.Equal(t, chatdomain.BlockDraft, block.Type)

	var body struct {
		Type       string     `json:"type"`
		DraftID    string     `json:"draft_id"`
		ActionKind string     `json:"action_kind"`
		Title      string     `json:"title"`
		Status     string     `json:"status"`
		Revision   int        `json:"revision"`
		Blocked    string     `json:"blocked_reason"`
		NotBefore  string     `json:"not_before"`
		Fields     [][]string `json:"fields"`
	}
	require.NoError(t, json.Unmarshal(block.Body, &body))

	require.Equal(t, "draft", body.Type)
	require.Equal(t, draft.ID.String(), body.DraftID)
	require.Equal(t, "gmail.send", body.ActionKind)
	require.Equal(t, approvaldomain.StatusApproved, body.Status)
	require.Equal(t, 2, body.Revision)
	require.Equal(t, "ditahan sampai jam kerja", body.Blocked)
	require.Equal(t, opensAt.Format(time.RFC3339Nano), body.NotBefore)

	// The fields are the action's own, labelled, which is what the card shows.
	labelled := map[string]string{}
	for _, pair := range body.Fields {
		require.Len(t, pair, 2)
		labelled[pair[0]] = pair[1]
	}
	require.Equal(t, "a@b.c", labelled["to"])
	require.Equal(t, "Tagihan Maret", labelled["subject"])
	require.Contains(t, labelled, "body")
}

// TestTheCardBlockSurvivesAPayloadThatIsNotAnObject is the honest degradation: a
// payload the card cannot read becomes an empty field list rather than a card
// that fails to render at all.
func TestTheCardBlockSurvivesAPayloadThatIsNotAnObject(t *testing.T) {
	block := draftCardBlock(approvaldomain.Draft{
		ID:         uuid.New(),
		ActionKind: "gmail.send",
		Title:      "Kirim tagihan",
		Payload:    json.RawMessage(`"bukan objek"`),
	})

	var body struct {
		Fields [][]string `json:"fields"`
	}
	require.NoError(t, json.Unmarshal(block.Body, &body))
	require.Empty(t, body.Fields)
	require.Equal(t, chatdomain.BlockDraft, block.Type, "the card still renders")
}

// TestTheCardBoundsALongField is what keeps one field from filling the thread.
func TestTheCardBoundsALongField(t *testing.T) {
	long := make([]rune, 0, 500)
	for range 500 {
		long = append(long, 'a')
	}

	block := draftCardBlock(approvaldomain.Draft{
		ID:         uuid.New(),
		ActionKind: "gmail.send",
		Payload:    json.RawMessage(`{"body":"` + string(long) + `"}`),
	})

	var body struct {
		Fields [][]string `json:"fields"`
	}
	require.NoError(t, json.Unmarshal(block.Body, &body))
	require.Len(t, body.Fields, 1)
	require.Len(t, []rune(body.Fields[0][1]), 200)
	require.True(t, len([]rune(body.Fields[0][1])) < 500)
}

// TestTruncateRunesLeavesAShortStringAlone is the boundary: a value within the
// limit keeps its exact text rather than being rewritten.
func TestTruncateRunesLeavesAShortStringAlone(t *testing.T) {
	require.Equal(t, "pendek", truncateRunes("pendek", 200))
	require.Equal(t, "", truncateRunes("", 200))
	require.Equal(t, "empat", truncateRunes("empat", 5))
}

// TestWireDraftTimeRendersAbsenceAsEmpty is the wire rule every shape follows.
func TestWireDraftTimeRendersAbsenceAsEmpty(t *testing.T) {
	require.Empty(t, wireDraftTime(nil))

	moment := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	require.Equal(t, moment.Format(time.RFC3339Nano), wireDraftTime(&moment))
}

// TestChatScopeForCarriesBothIDs is the translation between two scopes that mean
// the same thing. Dropping the user id here would make a card write run without
// one, which the store refuses.
func TestChatScopeForCarriesBothIDs(t *testing.T) {
	userID, workspaceID := uuid.New(), uuid.New()

	scope := chatScopeFor(approvaldomain.Scope{UserID: userID, WorkspaceID: workspaceID})
	require.Equal(t, userID, scope.UserID)
	require.Equal(t, workspaceID, scope.WorkspaceID)
}

// TestTheApprovalEventSinkKeepsTheWholeEvent is the stream's contract: a draft
// event reaches the same durable row every other event uses, with the draft id
// that makes it findable.
func TestTheApprovalEventSinkKeepsTheWholeEvent(t *testing.T) {
	store := &memoryEvents{}
	sink := approvalEventSink{sink: chatEventSink{store: store}}

	workspaceID, draftID, taskID, agentID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	require.NoError(t, sink.Emit(context.Background(), approvaldomain.Event{
		WorkspaceID:  workspaceID,
		Type:         approvaldomain.EventDraftCreated,
		ActorAgentID: agentID,
		TaskID:       taskID,
		DraftID:      draftID,
		Payload:      []byte(`{"status":"pending"}`),
	}))

	require.Len(t, store.appended, 1)
	stored := store.appended[0]
	require.Equal(t, workspaceID, stored.WorkspaceID)
	require.Equal(t, approvaldomain.EventDraftCreated, stored.Type)
	require.Equal(t, draftID, stored.DraftID, "the draft id is what makes the event findable")
	require.Equal(t, taskID, stored.TaskID)
	require.Equal(t, agentID, stored.ActorAgentID)
}

// TestTheWorkingHoursReaderSaysWhenItIsNotConfigured is the honest answer for a
// container that was never assembled, rather than a zero window that would read as
// "always closed" and hold every action forever.
func TestTheWorkingHoursReaderSaysWhenItIsNotConfigured(t *testing.T) {
	_, err := workspaceHours{}.WorkingHours(context.Background(), uuid.New())
	require.ErrorContains(t, err, "not configured")
}

// TestTheSignalerRefusesWithoutTemporal is the same rule for the decision path: a
// deployment without an engine reports it rather than panicking.
func TestTheSignalerRefusesWithoutTemporal(t *testing.T) {
	err := approvalSignaler{}.SignalApproval(context.Background(), approvaldomain.Scope{},
		uuid.New(), uuid.New(), true, "kirim")
	require.ErrorContains(t, err, "temporal is not configured")
}

// TestTheGateIsAbsentWithoutTheApprovalService keeps the container honest: a
// deployment without the module gets a nil port, and the runtime then refuses an
// outbound action rather than taking it.
func TestTheGateIsAbsentWithoutTheApprovalService(t *testing.T) {
	c := &Container{Logger: zap.NewNop(), Services: &Services{}}
	require.Nil(t, c.draftGate())

	withoutDatabase := &Container{
		Config:       testConfig(),
		Logger:       zap.NewNop(),
		Repositories: &Repositories{},
		Services:     &Services{},
	}
	require.NoError(t, withoutDatabase.openApproval(context.Background(), &config.Config{}))
	require.Nil(t, withoutDatabase.Services.Approval)
}

// TestTheApprovalRoutesAnswer503WhenTheModuleIsMissing is the documented shape for
// a deployment without a database.
func TestTheApprovalRoutesAnswer503WhenTheModuleIsMissing(t *testing.T) {
	app := approvalApp(t, nil)

	for _, path := range []string{
		"/api/drafts",
		"/api/approval-policies",
		"/api/audit",
	} {
		code, body := approvalGet(t, app, path)
		require.Equal(t, 503, code, path)
		require.Contains(t, body, "approval_not_configured", path)
	}
}

// approvalApp mounts the approval routes the way the container does, so what is
// under test is that the routes exist and answer as documented.
func approvalApp(t *testing.T, service approvaldomain.Service) *fiber.App {
	t.Helper()

	app := fiber.New(fiber.Config{DisableStartupMessage: true})

	handlers := &Handlers{}
	if service != nil {
		handlers.Approval = approvalhandler.New(service, zap.NewNop())
	}

	tenant := app.Group("/api", func(ctx *fiber.Ctx) error {
		userID := uuid.New()
		middleware.SetIdentity(ctx, userID, "owner@example.com")
		middleware.SetScope(ctx, database.Scope{UserID: userID, WorkspaceID: uuid.New()})
		middleware.SetRole(ctx, "owner")
		return ctx.Next()
	})

	if handlers.Approval == nil {
		registerApprovalFallback(tenant)
	} else {
		approvalhandler.Routes(tenant, handlers.Approval)
	}

	return app
}

// approvalGet performs one request and returns its status and body.
func approvalGet(t *testing.T, app *fiber.App, path string) (int, string) {
	t.Helper()

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil), -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

// TestTheCardIsWrittenAndRewritten is the card's lifecycle in the thread.
//
// The card is its own message rather than an edit of the task's answer, which is
// what lets a decision rewrite exactly one message: a reload then shows what
// happened instead of a card still offering to approve something already sent.
func TestTheCardIsWrittenAndRewritten(t *testing.T) {
	messages := newMemoryMessages()
	events := &memoryEvents{}
	cards := approvalCards{messages: messages, sink: approvalEventSink{sink: chatEventSink{store: events, publisher: events}}}

	scope := approvaldomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
	conversationID := uuid.New()
	agentID := uuid.New()

	draft := approvaldomain.Draft{
		ID:             uuid.New(),
		TaskID:         uuid.New(),
		AgentID:        agentID,
		ConversationID: conversationID,
		ActionKind:     "gmail.send",
		Title:          "Kirim tagihan Maret",
		Status:         approvaldomain.StatusPending,
		Payload:        json.RawMessage(`{"to":"a@b.c"}`),
	}

	messageID, err := cards.Write(context.Background(), scope, draft)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, messageID)

	stored, err := messages.GetMessage(context.Background(), chatdomain.Scope{
		UserID: scope.UserID, WorkspaceID: scope.WorkspaceID,
	}, messageID)
	require.NoError(t, err)
	require.Equal(t, agentID, stored.AuthorAgentID, "the card is the Bolu's own message")
	require.Equal(t, draft.TaskID, stored.TaskID)
	require.Len(t, stored.Blocks, 1)
	require.Equal(t, chatdomain.BlockDraft, stored.Blocks[0].Type)

	// The write published the new message, which is how a live client sees it
	// appear without a reload.
	require.Len(t, events.appended, 1)
	require.Equal(t, chatdomain.EventMessageUpdated, events.appended[0].Type)

	// A decision rewrites the same message, so the thread does not grow a second
	// card for one proposal.
	decided := draft
	decided.Status = approvaldomain.StatusSent
	require.NoError(t, cards.Rewrite(context.Background(), scope, messageID, decided))

	updated, err := messages.GetMessage(context.Background(), chatdomain.Scope{
		UserID: scope.UserID, WorkspaceID: scope.WorkspaceID,
	}, messageID)
	require.NoError(t, err)

	var body struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(updated.Blocks[0].Body, &body))
	require.Equal(t, approvaldomain.StatusSent, body.Status, "the card follows the decision")
	require.Equal(t, messageID, updated.ID, "one proposal has one card")
}

// TestTheCardIsSkippedWithoutAConversation covers a task that answers no thread: a
// routine or a webhook task has nowhere to put a card, and writing nothing is the
// right answer rather than an error.
func TestTheCardIsSkippedWithoutAConversation(t *testing.T) {
	cards := approvalCards{messages: newMemoryMessages()}

	messageID, err := cards.Write(context.Background(), approvaldomain.Scope{WorkspaceID: uuid.New()},
		approvaldomain.Draft{ID: uuid.New(), ConversationID: uuid.Nil})
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, messageID)
}

// TestRewritingAMissingCardIsAnError is the honest failure: a decision that cannot
// reach its card is reported rather than silently dropping the update.
func TestRewritingAMissingCardIsAnError(t *testing.T) {
	cards := approvalCards{messages: newMemoryMessages()}

	err := cards.Rewrite(context.Background(), approvaldomain.Scope{WorkspaceID: uuid.New()},
		uuid.New(), approvaldomain.Draft{ID: uuid.New()})
	require.Error(t, err)
}

// TestTheCardStillWorksWithoutAStream keeps the optional dependency optional: a
// deployment without an event store still writes the card, and only the live
// update is behind.
func TestTheCardStillWorksWithoutAStream(t *testing.T) {
	messages := newMemoryMessages()
	cards := approvalCards{messages: messages}

	messageID, err := cards.Write(context.Background(), approvaldomain.Scope{WorkspaceID: uuid.New()},
		approvaldomain.Draft{
			ID: uuid.New(), AgentID: uuid.New(), ConversationID: uuid.New(),
			ActionKind: "gmail.send", Title: "Kirim tagihan",
		})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, messageID)
}

// TestTheApprovalSenderIsAbsentUntilEpic8 owns the gap rather than hiding it: no
// integration executor exists, so an approved action stops at that boundary and the
// task says why instead of recording a send that never happened.
func TestTheApprovalSenderIsAbsentUntilEpic8(t *testing.T) {
	c := &Container{Logger: zap.NewNop()}
	require.Nil(t, c.approvalSender(), "EPIC 8 (#66) supplies the executor")
}

// TestAPublishThatFailsStillStoresTheCard is why the publish is not allowed to
// fail the write: the card is what a person reads, and the live update is a
// convenience on top of it.
func TestAPublishThatFailsStillStoresTheCard(t *testing.T) {
	messages := newMemoryMessages()
	events := &memoryEvents{failWrite: true}
	cards := approvalCards{
		messages: messages,
		sink:     approvalEventSink{sink: chatEventSink{store: events, publisher: events}},
	}

	scope := approvaldomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
	draft := approvaldomain.Draft{
		ID:             uuid.New(),
		TaskID:         uuid.New(),
		AgentID:        uuid.New(),
		ConversationID: uuid.New(),
		ActionKind:     "gmail.send",
		Title:          "Kirim penawaran",
		Status:         approvaldomain.StatusPending,
	}

	messageID, err := cards.Write(context.Background(), scope, draft)
	require.NoError(t, err, "a card that is stored is written even when its publish fails")
	require.NotEqual(t, uuid.Nil, messageID)

	stored, err := messages.GetMessage(context.Background(), chatScopeFor(scope), messageID)
	require.NoError(t, err, "the card reached the thread")
	require.Equal(t, chatdomain.BlockDraft, stored.Blocks[0].Type)

	// The decision still rewrites the card, so a decision is never lost to a
	// stream that is down.
	decided := draft
	decided.Status = approvaldomain.StatusSent
	require.NoError(t, cards.Rewrite(context.Background(), scope, messageID, decided))

	updated, err := messages.GetMessage(context.Background(), chatScopeFor(scope), messageID)
	require.NoError(t, err)

	var card map[string]any
	require.NoError(t, json.Unmarshal(updated.Blocks[0].Body, &card))
	require.Equal(t, string(approvaldomain.StatusSent), card["status"],
		"the card says what happened rather than what was proposed")
}

// TestRewritingAMissingCardIsReported keeps the update path honest about the
// message it claims to change.
func TestRewritingAMissingCardIsReported(t *testing.T) {
	cards := approvalCards{messages: newMemoryMessages()}

	err := cards.Rewrite(context.Background(), approvaldomain.Scope{UserID: uuid.New()}, uuid.New(), approvaldomain.Draft{})
	require.ErrorIs(t, err, chatdomain.ErrMessageNotFound)
}
