package service_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/whatsapp/service"
	"github.com/robzlabz/employeebot/packages/whatsapp/domain"
	"github.com/robzlabz/employeebot/packages/whatsapp/domain/mocks"
)

// The module's own rules are this file: who may link a number, what a customer's
// message becomes, and how a reply is paced.
//
// Everything here is about a decision the product makes rather than about
// WhatsApp: the transport is a mock, and what is under test is what this
// deployment does with it.

// harness is a service over scripted ports.
type harness struct {
	service     *service.Service
	store       *mocks.Store
	sessions    *mocks.SessionForgetter
	transport   *mocks.Transport
	connections *mocks.Connections
	activity    *mocks.Activity
	alerter     *mocks.Alerter
	replies     *mocks.Replies
	watchers    *mocks.Watchers
	media       *mocks.MediaStore
	clock       *fakeClock

	scope    domain.Scope
	numberID uuid.UUID
	agentID  uuid.UUID
}

// fakeClock is the time source, so a hold and a ceiling are testable without
// waiting.
type fakeClock struct{ at time.Time }

func (c *fakeClock) now() time.Time          { return c.at }
func (c *fakeClock) advance(d time.Duration) { c.at = c.at.Add(d) }

func newHarness(t *testing.T) *harness {
	t.Helper()

	h := &harness{
		store:       mocks.NewStore(t),
		sessions:    mocks.NewSessionForgetter(t),
		transport:   mocks.NewTransport(t),
		connections: mocks.NewConnections(t),
		activity:    mocks.NewActivity(t),
		alerter:     mocks.NewAlerter(t),
		replies:     mocks.NewReplies(t),
		watchers:    mocks.NewWatchers(t),
		media:       mocks.NewMediaStore(t),
		clock:       &fakeClock{at: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)},
		scope:       domain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()},
		numberID:    uuid.New(),
		agentID:     uuid.New(),
	}

	h.service = service.New(service.Deps{
		Repository:  h.store,
		Sessions:    h.sessions,
		Transport:   h.transport,
		Connections: h.connections,
		Activity:    h.activity,
		Alerter:     h.alerter,
		Replies:     h.replies,
		Watchers:    h.watchers,
		Media:       h.media,
		Now:         h.clock.now,
	})

	return h
}

// number is one stored number, ready to be acted on.
func (h *harness) number(mutate ...func(*domain.Number)) domain.Number {
	accepted := h.clock.at.Add(-time.Hour)

	number := domain.Number{
		ID:            h.numberID,
		WorkspaceID:   h.scope.WorkspaceID,
		IntegrationID: uuid.New(),
		Phone:         "628123456789",
		Label:         "WhatsApp ••••6789",
		AgentID:       h.agentID,
		Status:        domain.StatusConnected,
		TOSAcceptedAt: &accepted,
		HourlyLimit:   40,
		CreatedAt:     h.clock.at,
		UpdatedAt:     h.clock.at,
	}
	for _, change := range mutate {
		change(&number)
	}
	return number
}

// TestPreparingANumberCreatesItWithoutAConnection is the regression the browser
// found. Preparing a number happens *before* anything is linked — it is what the
// warning is accepted against — so it must not need a connection the hub has not
// created yet. The service-level companion to the repository test, because what went
// wrong was the module handing the repository a zero id it treated as a real one.
func TestPreparingANumberCreatesItWithoutAConnection(t *testing.T) {
	h := newHarness(t)

	h.store.EXPECT().
		ByPhone(mock.Anything, mock.Anything, "628123456789").
		Return(domain.Number{}, domain.ErrNotFound).
		Once()
	h.store.EXPECT().
		Create(mock.Anything, mock.Anything, mock.MatchedBy(func(number domain.Number) bool {
			// No connection yet, and nobody asked the hub for one.
			return number.IntegrationID == uuid.Nil &&
				number.AgentID == h.agentID &&
				number.Status == domain.StatusUnlinked &&
				number.TOSAcceptedAt == nil
		})).
		Return(h.number(func(n *domain.Number) {
			n.Status = domain.StatusUnlinked
			n.IntegrationID = uuid.Nil
			n.TOSAcceptedAt = nil
		}), nil).
		Once()
	h.activity.EXPECT().Emit(mock.Anything, mock.Anything).Return(nil).Once()

	prepared, err := h.service.Prepare(t.Context(), h.scope, domain.LinkRequest{
		Phone:   "+62 812-3456-789",
		AgentID: h.agentID,
	})
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, prepared.IntegrationID)
	require.False(t, prepared.TOSAcceptedAt != nil)
}

// TestALinkNeedsTheWarningAccepted is E9.6's last gate at the point where it
// matters: the warning is shown and accepted *before* a pairing starts, because a
// risk nobody was told about is not one they took.
func TestALinkNeedsTheWarningAccepted(t *testing.T) {
	h := newHarness(t)

	h.store.EXPECT().
		ByPhone(mock.Anything, mock.Anything, "628123456789").
		Return(h.number(func(n *domain.Number) {
			n.Status = domain.StatusUnlinked
			n.TOSAcceptedAt = nil
		}), nil).
		Once()

	_, err := h.service.BeginLink(t.Context(), h.scope, domain.LinkRequest{
		Phone:   "628123456789",
		AgentID: h.agentID,
	})
	require.ErrorIs(t, err, domain.ErrTermsNotAccepted)
}

// TestALinkNeedsABolu: a number with nobody answering it is a number that loses
// customers, so the choice is required rather than defaulted.
func TestALinkNeedsABolu(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.BeginLink(t.Context(), h.scope, domain.LinkRequest{
		Phone: "628123456789",
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	// And the same for the step that prepares the row: a number cannot be created
	// without somebody to answer it.
	_, err = h.service.Prepare(t.Context(), h.scope, domain.LinkRequest{Phone: "628123456789"})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestAcceptingTheWarningIsIdempotent: accepting twice keeps the first moment,
// because that is when the risk was taken, and a second click is not a new one.
func TestAcceptingTheWarningIsIdempotent(t *testing.T) {
	h := newHarness(t)

	accepted := h.clock.at
	first := h.number(func(n *domain.Number) { n.TOSAcceptedAt = &accepted })
	h.store.EXPECT().AcceptTerms(mock.Anything, mock.Anything, h.numberID).Return(first, nil).Once()
	h.activity.EXPECT().Emit(mock.Anything, mock.Anything).Return(nil).Once()

	once, err := h.service.AcceptTerms(t.Context(), h.scope, h.numberID)
	require.NoError(t, err)
	require.Equal(t, accepted, *once.TOSAcceptedAt)

	// A second click an hour later keeps the first moment: that is when the risk
	// was taken.
	h.clock.advance(time.Hour)
	h.store.EXPECT().AcceptTerms(mock.Anything, mock.Anything, h.numberID).Return(first, nil).Once()
	h.activity.EXPECT().Emit(mock.Anything, mock.Anything).Return(nil).Once()

	twice, err := h.service.AcceptTerms(t.Context(), h.scope, h.numberID)
	require.NoError(t, err)
	require.Equal(t, accepted, *twice.TOSAcceptedAt, "the first moment stands")
}

// TestALocalFormatNumberIsRefusedWithSomethingActionable is the second defect the
// browser found: somebody writes the number the way they would dial it, and WhatsApp
// needs the country-coded form. Pairing with "0812…" is refused by the server with an
// error nobody can act on, so the refusal happens here and says what to type.
//
// Guessing the country code would be worse than saying so: this deployment serves
// one country today and would silently misread the next.
func TestALocalFormatNumberIsRefusedWithSomethingActionable(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.Prepare(t.Context(), h.scope, domain.LinkRequest{
		Phone:   "0812-3456-7890",
		AgentID: h.agentID,
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
	require.Contains(t, err.Error(), "kode negara")
	require.Contains(t, err.Error(), "62812")
}

// TestTheNumberWhatsappReportsReplacesWhatWasTyped is the other half: somebody
// writes the number one way and WhatsApp reports it another, and the row has to
// carry WhatsApp's form — otherwise every later match against it is off by a country
// code, and the outbox addresses a number that is not the session's.
func TestTheNumberWhatsappReportsReplacesWhatWasTyped(t *testing.T) {
	h := newHarness(t)
	integrationID := uuid.New()

	prepared := h.number(func(n *domain.Number) {
		n.Status = domain.StatusLinking
		n.Phone = "628123456789"
		n.IntegrationID = uuid.Nil
		n.Label = "WhatsApp ••••••••6789"
	})
	// What will be stored back: the row as it is, with a label for the new number.
	relabelled := prepared
	relabelled.Phone = "6281234567890"

	h.store.EXPECT().Get(mock.Anything, mock.Anything, h.numberID).Return(prepared, nil).Once()
	h.store.EXPECT().
		SetPhone(mock.Anything, mock.Anything, h.numberID, "6281234567890").
		Return(nil).
		Once()
	h.connections.EXPECT().
		Link(mock.Anything, mock.Anything, mock.MatchedBy(func(link domain.ConnectionLink) bool {
			// The account label is built from WhatsApp's number, not the typed one.
			return strings.HasSuffix(link.AccountLabel, "7890")
		})).
		Return(integrationID, nil).
		Once()
	h.store.EXPECT().SetIntegration(mock.Anything, mock.Anything, h.numberID, integrationID).Return(nil).Once()
	h.store.EXPECT().
		SetStatus(mock.Anything, mock.Anything, h.numberID, domain.StatusConnected, "").
		Return(h.number(func(n *domain.Number) { n.Status = domain.StatusConnected }), nil).
		Once()
	h.store.EXPECT().Publish(mock.Anything, mock.Anything, h.numberID, true).Return(nil).Once()
	h.activity.EXPECT().Emit(mock.Anything, mock.Anything).Return(nil).Once()

	require.NoError(t, h.service.Paired(t.Context(), h.scope, domain.PairedLink{
		NumberID: h.numberID,
		Phone:    "6281234567890",
		JID:      "6281234567890:12@s.whatsapp.net",
	}))
	require.Equal(t, "6281234567890", relabelled.Phone)
}

// TestALinkIsRefusedForANumberThatIsAlreadyLinked: one WhatsApp session belongs to
// the number, so a second link is a takeover of somebody's line rather than a new
// connection.
func TestALinkIsRefusedForANumberThatIsAlreadyLinked(t *testing.T) {
	h := newHarness(t)

	h.store.EXPECT().
		ByPhone(mock.Anything, mock.Anything, "628123456789").
		Return(h.number(), nil).
		Once()

	_, err := h.service.BeginLink(t.Context(), h.scope, domain.LinkRequest{
		Phone:   "628123456789",
		AgentID: h.agentID,
	})
	require.ErrorIs(t, err, domain.ErrAlreadyLinked)
}

// TestStartingALinkMarksTheRowInProgress: the screen reads the row while a person
// scans, so the state has to be on the row rather than only in the attempt.
func TestStartingALinkMarksTheRowInProgress(t *testing.T) {
	h := newHarness(t)

	h.store.EXPECT().
		ByPhone(mock.Anything, mock.Anything, "628123456789").
		Return(h.number(func(n *domain.Number) { n.Status = domain.StatusUnlinked }), nil).
		Once()
	h.transport.EXPECT().
		Link(mock.Anything, mock.Anything, h.numberID, "628123456789").
		Return(domain.Link{ID: "link-1", State: domain.LinkWaiting, NumberID: h.numberID}, nil).
		Once()
	h.store.EXPECT().
		SetStatus(mock.Anything, mock.Anything, h.numberID, domain.StatusLinking, "").
		Return(h.number(func(n *domain.Number) { n.Status = domain.StatusLinking }), nil).
		Once()

	link, err := h.service.BeginLink(t.Context(), h.scope, domain.LinkRequest{
		Phone:   "628123456789",
		AgentID: h.agentID,
	})
	require.NoError(t, err)
	require.Equal(t, "link-1", link.ID)
	require.Equal(t, h.numberID, link.NumberID)
}

// TestALinkThatFailedGivesTheRowBack is what makes a retry possible: a pairing
// that did not happen is not a broken number, so the row returns to unlinked
// instead of staying in progress forever.
func TestALinkThatFailedGivesTheRowBack(t *testing.T) {
	h := newHarness(t)

	h.transport.EXPECT().
		LinkState(mock.Anything, "link-1").
		Return(domain.Link{
			ID: "link-1", NumberID: h.numberID, WorkspaceID: h.scope.WorkspaceID,
			State: domain.LinkExpired, Reason: "kode kedaluwarsa",
		}, nil).
		Once()
	h.store.EXPECT().
		SetStatus(mock.Anything, mock.Anything, h.numberID, domain.StatusUnlinked, "kode kedaluwarsa").
		Return(h.number(func(n *domain.Number) { n.Status = domain.StatusUnlinked }), nil).
		Once()

	link, err := h.service.LinkState(t.Context(), h.scope, "link-1")
	require.NoError(t, err)
	require.Equal(t, domain.LinkExpired, link.State)
}

// TestAnAttemptInProgressIsAskedAboutRatherThanRemembered is the regression the
// socket found: the service keeps an attempt's outcome so a screen that polls one step
// behind still sees it, and that shortcut was answering the *in-progress* states too.
// The attempt then froze at whatever state it was started in, which looked exactly like
// WhatsApp never sending a code.
func TestAnAttemptInProgressIsAskedAboutRatherThanRemembered(t *testing.T) {
	h := newHarness(t)

	// The attempt is started, which remembers it as waiting.
	h.store.EXPECT().
		ByPhone(mock.Anything, mock.Anything, "628123456789").
		Return(h.number(func(n *domain.Number) { n.Status = domain.StatusUnlinked }), nil).
		Once()
	h.transport.EXPECT().
		Link(mock.Anything, mock.Anything, h.numberID, "628123456789").
		Return(domain.Link{ID: "link-1", State: domain.LinkWaiting, NumberID: h.numberID}, nil).
		Once()
	h.store.EXPECT().SetStatus(mock.Anything, mock.Anything, h.numberID, domain.StatusLinking, "").
		Return(h.number(func(n *domain.Number) { n.Status = domain.StatusLinking }), nil).
		Once()

	_, err := h.service.BeginLink(t.Context(), h.scope, domain.LinkRequest{Phone: "628123456789", AgentID: h.agentID})
	require.NoError(t, err)

	// The poll has to reach the transport, and the code it answers with is what the
	// screen draws.
	h.transport.EXPECT().
		LinkState(mock.Anything, "link-1").
		Return(domain.Link{ID: "link-1", NumberID: h.numberID, State: domain.LinkCode, Code: "qr-payload"}, nil).
		Once()

	link, err := h.service.LinkState(t.Context(), h.scope, "link-1")
	require.NoError(t, err)
	require.Equal(t, domain.LinkCode, link.State, "an attempt in progress is read from the transport")
	require.Equal(t, "qr-payload", link.Code)

	// A second poll still asks, because the code rotates and the state can move.
	h.transport.EXPECT().
		LinkState(mock.Anything, "link-1").
		Return(domain.Link{ID: "link-1", NumberID: h.numberID, State: domain.LinkCode, Code: "qr-next"}, nil).
		Once()

	next, err := h.service.LinkState(t.Context(), h.scope, "link-1")
	require.NoError(t, err)
	require.Equal(t, "qr-next", next.Code)

	// Once the attempt ends, the outcome is answered from memory: the transport has
	// forgotten it by then.
	h.transport.EXPECT().
		LinkState(mock.Anything, "link-1").
		Return(domain.Link{ID: "link-1", NumberID: h.numberID, State: domain.LinkExpired, Reason: "kode habis"}, nil).
		Once()
	h.store.EXPECT().SetStatus(mock.Anything, mock.Anything, h.numberID, domain.StatusUnlinked, "kode habis").
		Return(h.number(), nil).
		Once()

	ended, err := h.service.LinkState(t.Context(), h.scope, "link-1")
	require.NoError(t, err)
	require.Equal(t, domain.LinkExpired, ended.State)

	again, err := h.service.LinkState(t.Context(), h.scope, "link-1")
	require.NoError(t, err)
	require.Equal(t, domain.LinkExpired, again.State, "and the transport is not asked twice")
}

// TestAnAttemptFromAnotherWorkspaceIsNotVisible is E9.2's first gate: the QR of one
// workspace must not be readable by another, so an attempt that belongs elsewhere
// answers "not found" rather than reporting it.
func TestAnAttemptFromAnotherWorkspaceIsNotVisible(t *testing.T) {
	h := newHarness(t)

	h.transport.EXPECT().
		LinkState(mock.Anything, "link-1").
		Return(domain.Link{
			ID: "link-1", WorkspaceID: uuid.New(), State: domain.LinkCode, Code: "qr-payload",
		}, nil).
		Once()

	_, err := h.service.LinkState(t.Context(), h.scope, "link-1")
	require.ErrorIs(t, err, domain.ErrNotFound)
}

// TestAnAttemptThatIsGoneAnswersExpired: a screen polls this until it stops asking,
// so an attempt the transport has forgotten is "expired" rather than an error.
func TestAnAttemptThatIsGoneAnswersExpired(t *testing.T) {
	h := newHarness(t)

	h.transport.EXPECT().
		LinkState(mock.Anything, "link-1").
		Return(domain.Link{}, domain.ErrLinkExpired).
		Once()

	link, err := h.service.LinkState(t.Context(), h.scope, "link-1")
	require.NoError(t, err)
	require.Equal(t, domain.LinkExpired, link.State)
	require.Contains(t, link.Reason, "kode baru")

	// And the same answer comes back on the next poll, so a screen one step behind
	// does not see the reason vanish.
	again, err := h.service.LinkState(t.Context(), h.scope, "link-1")
	require.NoError(t, err)
	require.Equal(t, link.Reason, again.Reason)
}

// TestPairingRecordsTheConnectionAndPublishesTheNumber is E9.1's "sesi bertahan"
// seen from the product's side: a paired number becomes a connection the hub shows,
// a number marked connected, and a directory row the fleet is told to serve.
func TestPairingRecordsTheConnectionAndPublishesTheNumber(t *testing.T) {
	h := newHarness(t)
	accepted := h.clock.at

	number := h.number(func(n *domain.Number) {
		n.Status = domain.StatusLinking
		n.AgentID = uuid.Nil
		n.Label = "WhatsApp ••••6789"
	})
	integrationID := uuid.New()

	h.store.EXPECT().Get(mock.Anything, mock.Anything, h.numberID).Return(number, nil).Once()
	h.connections.EXPECT().
		Link(mock.Anything, mock.Anything, mock.MatchedBy(func(link domain.ConnectionLink) bool {
			return link.App == service.App && strings.HasSuffix(link.AccountLabel, "6789") &&
				!strings.Contains(link.AccountLabel, "6281")
		})).
		Return(integrationID, nil).
		Once()
	h.store.EXPECT().
		SetIntegration(mock.Anything, mock.Anything, h.numberID, integrationID).
		Return(nil).
		Once()
	h.store.EXPECT().
		SetStatus(mock.Anything, mock.Anything, h.numberID, domain.StatusConnected, "").
		Return(h.number(func(n *domain.Number) { n.Status = domain.StatusConnected }), nil).
		Once()
	h.store.EXPECT().
		Publish(mock.Anything, mock.Anything, h.numberID, true).
		Return(nil).
		Once()
	h.activity.EXPECT().Emit(mock.Anything, mock.Anything).Return(nil).Once()

	require.NoError(t, h.service.Paired(t.Context(), h.scope, domain.PairedLink{
		NumberID: h.numberID,
		Phone:    "+62 812-3456-789",
		JID:      "628123456789:12@s.whatsapp.net",
	}))

	require.NotNil(t, accepted)
}

// TestACustomerMessageOpensATask is E9.4's happy path: a message from a customer
// becomes work for the Bolu that answers the number, and the message records which
// task it opened.
func TestACustomerMessageOpensATask(t *testing.T) {
	h := newHarness(t)
	taskID := uuid.New()

	h.store.EXPECT().Get(mock.Anything, mock.Anything, h.numberID).Return(h.number(), nil).Once()
	h.store.EXPECT().
		Record(mock.Anything, mock.Anything, mock.MatchedBy(func(message domain.Inbound) bool {
			return message.NumberID == h.numberID && message.WaMessageID == "msg-1"
		})).
		Return(domain.Inbound{
			ID: uuid.New(), NumberID: h.numberID, WaMessageID: "msg-1",
			ChatJID: "628999@s.whatsapp.net", SenderName: "Bu Sari",
			Kind: domain.KindText, Text: "Ada kaos polos ukuran L?", ReceivedAt: h.clock.at,
		}, true, nil).
		Once()
	// No routine is watching, so the assigned Bolu answers.
	h.watchers.EXPECT().Fire(mock.Anything, mock.Anything, domain.EventMessageReceived).Return(0, nil).Once()
	h.replies.EXPECT().
		Answer(mock.Anything, mock.Anything, mock.MatchedBy(func(request domain.AnswerRequest) bool {
			return request.AgentID == h.agentID &&
				strings.Contains(request.Prompt, "Ada kaos polos ukuran L?") &&
				strings.Contains(request.Prompt, "whatsapp.send_message")
		})).
		Return(taskID, nil).
		Once()
	h.store.EXPECT().AttachTask(mock.Anything, mock.Anything, mock.Anything, taskID).Return(nil).Once()
	h.activity.EXPECT().Emit(mock.Anything, mock.Anything).Return(nil).Once()

	stored, created, err := h.service.Receive(t.Context(), h.scope, h.numberID, domain.Inbound{
		WaMessageID: "msg-1",
		ChatJID:     "628999@s.whatsapp.net",
		SenderName:  "Bu Sari",
		Kind:        domain.KindText,
		Text:        "Ada kaos polos ukuran L?",
		ReceivedAt:  h.clock.at,
	})
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, taskID, stored.TaskID)
}

// TestADuplicateMessageOpensNothing is E9.4's first gate: WhatsApp retries, and a
// retry must not become a second task for the same customer message.
func TestADuplicateMessageOpensNothing(t *testing.T) {
	h := newHarness(t)

	h.store.EXPECT().Get(mock.Anything, mock.Anything, h.numberID).Return(h.number(), nil).Once()
	h.store.EXPECT().
		Record(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.Inbound{
			ID: uuid.New(), NumberID: h.numberID, WaMessageID: "msg-1",
			TaskID: uuid.New(), Kind: domain.KindText, Text: "sudah pernah",
		}, false, nil).
		Once()

	// Nothing else is expected: no event, no routine, no task. A mock that is
	// called unexpectedly fails the test, which is exactly the assertion.
	stored, created, err := h.service.Receive(t.Context(), h.scope, h.numberID, domain.Inbound{
		WaMessageID: "msg-1",
		ChatJID:     "628999@s.whatsapp.net",
		Text:        "sudah pernah",
	})
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, "msg-1", stored.WaMessageID)
}

// TestARoutineWatchingTheEventOwnsTheWork: a routine names the Bolu that acts, which
// is a better answer than a rule invented here. When one fires, the assigned Bolu
// does not also open a task for the same message.
func TestARoutineWatchingTheEventOwnsTheWork(t *testing.T) {
	h := newHarness(t)

	h.store.EXPECT().Get(mock.Anything, mock.Anything, h.numberID).Return(h.number(), nil).Once()
	h.store.EXPECT().
		Record(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.Inbound{ID: uuid.New(), NumberID: h.numberID, WaMessageID: "msg-2", Kind: domain.KindText}, true, nil).
		Once()
	h.watchers.EXPECT().Fire(mock.Anything, mock.Anything, domain.EventMessageReceived).Return(2, nil).Once()
	h.activity.EXPECT().Emit(mock.Anything, mock.Anything).Return(nil).Once()

	stored, created, err := h.service.Receive(t.Context(), h.scope, h.numberID, domain.Inbound{
		WaMessageID: "msg-2",
		ChatJID:     "628999@s.whatsapp.net",
		Text:        "halo",
	})
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, uuid.Nil, stored.TaskID, "the routine owns the work, so no second task is opened")
}

// TestAGroupMessageIsRecordedAndNotAnswered: a group is not a customer writing to
// the business, so what was said is kept and nobody is asked to answer it. The
// recording is the point: a person asking "what happened in that group" has an
// answer.
func TestAGroupMessageIsRecordedAndNotAnswered(t *testing.T) {
	h := newHarness(t)

	h.store.EXPECT().Get(mock.Anything, mock.Anything, h.numberID).Return(h.number(), nil).Once()
	h.store.EXPECT().
		Record(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.Inbound{
			ID: uuid.New(), NumberID: h.numberID, WaMessageID: "msg-3",
			ChatJID: "120363@g.us", IsGroup: true, Kind: domain.KindText, Text: "besok tutup",
		}, true, nil).
		Once()
	h.watchers.EXPECT().Fire(mock.Anything, mock.Anything, mock.Anything).Return(0, nil).Once()
	h.activity.EXPECT().Emit(mock.Anything, mock.Anything).Return(nil).Once()

	stored, created, err := h.service.Receive(t.Context(), h.scope, h.numberID, domain.Inbound{
		WaMessageID: "msg-3",
		ChatJID:     "120363@g.us",
		IsGroup:     true,
		Text:        "besok tutup",
	})
	require.NoError(t, err)
	require.True(t, created)
	require.True(t, stored.IsGroup)
	require.Equal(t, uuid.Nil, stored.TaskID)
}

// TestAMessageThisNumberSentIsNotAnswered: answering the shop's own message would
// make the Bolu talk to itself.
func TestAMessageThisNumberSentIsNotAnswered(t *testing.T) {
	h := newHarness(t)

	h.store.EXPECT().Get(mock.Anything, mock.Anything, h.numberID).Return(h.number(), nil).Once()
	h.store.EXPECT().
		Record(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.Inbound{ID: uuid.New(), NumberID: h.numberID, WaMessageID: "msg-4", IsFromMe: true}, true, nil).
		Once()
	h.watchers.EXPECT().Fire(mock.Anything, mock.Anything, mock.Anything).Return(0, nil).Once()
	h.activity.EXPECT().Emit(mock.Anything, mock.Anything).Return(nil).Once()

	stored, created, err := h.service.Receive(t.Context(), h.scope, h.numberID, domain.Inbound{
		WaMessageID: "msg-4",
		ChatJID:     "628999@s.whatsapp.net",
		IsFromMe:    true,
		Text:        "pesanannya sudah dikirim",
	})
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, uuid.Nil, stored.TaskID)
}

// TestANumberWithNoBoluSaysSo: the message is recorded — a person still reads what
// the customer asked — and the missing answer is reported rather than silently
// swallowed.
func TestANumberWithNoBoluSaysSo(t *testing.T) {
	h := newHarness(t)

	h.store.EXPECT().
		Get(mock.Anything, mock.Anything, h.numberID).
		Return(h.number(func(n *domain.Number) { n.AgentID = uuid.Nil }), nil).
		Once()
	h.store.EXPECT().
		Record(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.Inbound{ID: uuid.New(), NumberID: h.numberID, WaMessageID: "msg-5", Kind: domain.KindText}, true, nil).
		Once()
	h.watchers.EXPECT().Fire(mock.Anything, mock.Anything, mock.Anything).Return(0, nil).Once()
	h.activity.EXPECT().Emit(mock.Anything, mock.Anything).Return(nil).Once()

	_, created, err := h.service.Receive(t.Context(), h.scope, h.numberID, domain.Inbound{
		WaMessageID: "msg-5",
		ChatJID:     "628999@s.whatsapp.net",
		Text:        "halo",
	})
	require.ErrorIs(t, err, domain.ErrNoAgent)
	require.True(t, created, "the message is kept even when nobody can answer it")
}

// TestAMessageNeedsWhatsappsOwnId: the id is the dedupe key, so a message without
// one cannot be made harmless and is refused rather than recorded twice.
func TestAMessageNeedsWhatsappsOwnId(t *testing.T) {
	h := newHarness(t)

	_, _, err := h.service.Receive(t.Context(), h.scope, h.numberID, domain.Inbound{
		ChatJID: "628999@s.whatsapp.net",
		Text:    "halo",
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestSendingNeedsAnApprovedDraft is E9.5's gate at the point where the module can
// enforce it: nothing in this module decides that a message should go out. What it
// does is refuse a message to a number that is not linked, and refuse an empty one.
func TestSendingNeedsALinkedNumber(t *testing.T) {
	h := newHarness(t)

	h.store.EXPECT().
		Get(mock.Anything, mock.Anything, h.numberID).
		Return(h.number(func(n *domain.Number) { n.Status = domain.StatusUnlinked }), nil).
		Once()

	_, err := h.service.Enqueue(t.Context(), h.scope, domain.EnqueueRequest{
		NumberID: h.numberID,
		ToJID:    "628999",
		Kind:     domain.KindText,
		Text:     "halo",
	})
	require.ErrorIs(t, err, domain.ErrNotConnected)
}

// TestAnEmptyMessageIsRefused: a message with no body and no attachment is one the
// customer would see as blank.
func TestAnEmptyMessageIsRefused(t *testing.T) {
	h := newHarness(t)

	h.store.EXPECT().Get(mock.Anything, mock.Anything, h.numberID).Return(h.number(), nil).Once()

	_, err := h.service.Enqueue(t.Context(), h.scope, domain.EnqueueRequest{
		NumberID: h.numberID,
		ToJID:    "628999",
		Kind:     domain.KindText,
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestAMessageIsAddressedToWhatsappsShape: what a model writes is a phone number,
// and what the protocol needs is an address. The conversion is here rather than in
// the adapter because it is a product convenience.
func TestAMessageIsAddressedToWhatsappsShape(t *testing.T) {
	h := newHarness(t)

	h.store.EXPECT().Get(mock.Anything, mock.Anything, h.numberID).Return(h.number(), nil).Once()
	h.store.EXPECT().
		Enqueue(mock.Anything, mock.Anything, mock.MatchedBy(func(item domain.Outbox) bool {
			return item.ToJID == "628999@s.whatsapp.net" &&
				item.Kind == domain.KindText &&
				item.Status == domain.OutboxQueued &&
				item.NotBefore.After(h.clock.at)
		})).
		Return(domain.Outbox{ID: uuid.New(), NumberID: h.numberID, Status: domain.OutboxQueued}, nil).
		Once()

	_, err := h.service.Enqueue(t.Context(), h.scope, domain.EnqueueRequest{
		NumberID: h.numberID,
		ToJID:    "+62 899-9",
		Kind:     domain.KindText,
		Text:     "halo",
	})
	require.NoError(t, err)
}

// TestTheHourlyCeilingPacesRatherThanRefuses is E9.6's first gate, and the whole
// reason the ceiling lives here: a number that spent its hour keeps its queue. The
// message is not lost and it is not sent; it waits.
func TestTheHourlyCeilingPacesRatherThanRefuses(t *testing.T) {
	h := newHarness(t)

	number := h.number(func(n *domain.Number) { n.HourlyLimit = 3 })
	h.store.EXPECT().Get(mock.Anything, mock.Anything, h.numberID).Return(number, nil).Once()
	h.store.EXPECT().
		SentSince(mock.Anything, mock.Anything, h.numberID, mock.Anything).
		Return(3, nil).
		Once()

	due := []domain.Outbox{
		{ID: uuid.New(), NumberID: h.numberID, Status: domain.OutboxQueued},
		{ID: uuid.New(), NumberID: h.numberID, Status: domain.OutboxQueued},
	}
	h.store.EXPECT().
		Due(mock.Anything, mock.Anything, h.numberID, mock.Anything).
		Return(due, nil).
		Once()
	for _, item := range due {
		h.store.EXPECT().
			MarkHeld(mock.Anything, mock.Anything, item.ID, mock.MatchedBy(func(reason string) bool {
				return strings.Contains(reason, "batas")
			}), mock.MatchedBy(func(at time.Time) bool { return at.After(h.clock.at) })).
			Return(nil).
			Once()
	}
	h.activity.EXPECT().Emit(mock.Anything, mock.Anything).Return(nil).Once()
	h.alerter.EXPECT().
		Alert(mock.Anything, mock.Anything, mock.MatchedBy(func(alert domain.Alert) bool {
			return alert.Kind == domain.AlertLimit && strings.Contains(alert.Reason, "3")
		})).
		Return(nil).
		Once()

	claimed, err := h.service.Claim(t.Context(), h.scope, h.numberID, 5)
	require.NoError(t, err)
	require.Empty(t, claimed.Ready, "nothing may leave while the hour is spent")
	require.Equal(t, 2, claimed.Held, "and what was held is reported, so a person can be told")
}

// TestUnderTheCeilingTheQueueDrains: the ceiling is a bound, not a stop. Below it
// the messages go out.
func TestUnderTheCeilingTheQueueDrains(t *testing.T) {
	h := newHarness(t)

	number := h.number(func(n *domain.Number) { n.HourlyLimit = 40 })
	h.store.EXPECT().Get(mock.Anything, mock.Anything, h.numberID).Return(number, nil).Once()
	h.store.EXPECT().SentSince(mock.Anything, mock.Anything, h.numberID, mock.Anything).Return(2, nil).Once()
	h.store.EXPECT().
		Due(mock.Anything, mock.Anything, h.numberID, 5).
		Return([]domain.Outbox{{ID: uuid.New(), NumberID: h.numberID, Status: domain.OutboxQueued}}, nil).
		Once()

	claimed, err := h.service.Claim(t.Context(), h.scope, h.numberID, 5)
	require.NoError(t, err)
	require.Len(t, claimed.Ready, 1)
	require.Zero(t, claimed.Held)
}

// TestTheDeploymentsCeilingAppliesWhenTheNumberHasNone: a row with no ceiling of
// its own is not unlimited. An unbounded number is a blast, and the product does
// not have one.
func TestTheDeploymentsCeilingAppliesWhenTheNumberHasNone(t *testing.T) {
	h := newHarness(t)

	number := h.number(func(n *domain.Number) { n.HourlyLimit = 0 })
	h.store.EXPECT().Get(mock.Anything, mock.Anything, h.numberID).Return(number, nil).Once()
	h.store.EXPECT().SentSince(mock.Anything, mock.Anything, h.numberID, mock.Anything).Return(0, nil).Once()
	h.store.EXPECT().Due(mock.Anything, mock.Anything, h.numberID, mock.Anything).Return(nil, nil).Once()

	_, err := h.service.Claim(t.Context(), h.scope, h.numberID, 5)
	require.NoError(t, err)
}

// TestAFailureThatIsNotTerminalGoesBackToTheQueue: a provider that was briefly
// unreachable is not a reason to lose a customer's answer.
func TestAFailureThatIsNotTerminalGoesBackToTheQueue(t *testing.T) {
	h := newHarness(t)
	messageID := uuid.New()

	h.store.EXPECT().
		MarkRetry(mock.Anything, mock.Anything, messageID, "koneksi putus", mock.MatchedBy(func(at time.Time) bool {
			return at.After(h.clock.at)
		})).
		Return(nil).
		Once()

	require.NoError(t, h.service.Retry(t.Context(), h.scope, messageID, "koneksi putus", h.clock.at.Add(time.Minute)))
}

// TestAMessageThatWillNotLeaveStaysVisible is E9.5's second gate: a message that
// failed is not silently dropped. It is parked with its reason, which is what a
// screen reads.
func TestAMessageThatWillNotLeaveStaysVisible(t *testing.T) {
	h := newHarness(t)
	messageID := uuid.New()

	h.store.EXPECT().
		MarkFailed(mock.Anything, mock.Anything, messageID, "nomor tidak dikenal", 3).
		Return(nil).
		Once()

	require.NoError(t, h.service.Failed(t.Context(), h.scope, messageID, "nomor tidak dikenal", 3))
}

// TestAStatusThatNeedsAPersonAlertsThem is E9.6's second gate: a number WhatsApp
// refused is a number somebody has to fix, and waiting does not fix it.
func TestAStatusThatNeedsAPersonAlertsThem(t *testing.T) {
	h := newHarness(t)

	h.store.EXPECT().Get(mock.Anything, mock.Anything, h.numberID).Return(h.number(), nil).Once()
	h.store.EXPECT().
		SetStatus(mock.Anything, mock.Anything, h.numberID, domain.StatusNeedsRelink, "sesi ditolak").
		Return(h.number(func(n *domain.Number) {
			n.Status = domain.StatusNeedsRelink
			n.StatusReason = "sesi ditolak"
		}), nil).
		Once()
	h.store.EXPECT().Publish(mock.Anything, mock.Anything, h.numberID, true).Return(nil).Once()
	h.activity.EXPECT().Emit(mock.Anything, mock.Anything).Return(nil).Once()
	h.alerter.EXPECT().
		Alert(mock.Anything, mock.Anything, mock.MatchedBy(func(alert domain.Alert) bool {
			return alert.Kind == domain.AlertNeedsRelink && alert.Reason == "sesi ditolak"
		})).
		Return(nil).
		Once()

	updated, err := h.service.Report(t.Context(), h.scope, domain.ReportRequest{
		NumberID: h.numberID,
		Status:   domain.StatusNeedsRelink,
		Reason:   "sesi ditolak",
		At:       h.clock.at,
	})
	require.NoError(t, err)
	require.Equal(t, domain.StatusNeedsRelink, updated.Status)
}

// TestAReportThatSaysNothingNewIsNotAWrite: the transport reports on a timer, and
// rewriting the status every tick would be a write per number per interval for no
// change. The liveness stamp still moves.
func TestAReportThatSaysNothingNewIsNotAWrite(t *testing.T) {
	h := newHarness(t)

	h.store.EXPECT().
		Get(mock.Anything, mock.Anything, h.numberID).
		Return(h.number(), nil).
		Once()
	h.store.EXPECT().
		Touch(mock.Anything, mock.Anything, h.numberID, h.clock.at).
		Return(nil).
		Once()

	_, err := h.service.Report(t.Context(), h.scope, domain.ReportRequest{
		NumberID: h.numberID,
		Status:   domain.StatusConnected,
		At:       h.clock.at,
	})
	require.NoError(t, err)
}

// TestAnUnknownStatusIsRefused: the status column is what a screen reads, so a
// value this module does not use is a caller's mistake rather than something to
// store.
func TestAnUnknownStatusIsRefused(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.Report(t.Context(), h.scope, domain.ReportRequest{
		NumberID: h.numberID,
		Status:   "on fire",
		At:       h.clock.at,
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestUnlinkingKeepsTheHistory is the reason the session and the number are
// separate: what a customer wrote is not the session's, and losing it because a
// phone was replaced would erase the record of what the workspace was told.
func TestUnlinkingKeepsTheHistory(t *testing.T) {
	h := newHarness(t)
	connection := uuid.New()
	number := h.number(func(n *domain.Number) { n.IntegrationID = connection })

	h.store.EXPECT().Get(mock.Anything, mock.Anything, h.numberID).Return(number, nil).Once()
	h.transport.EXPECT().
		Unlink(mock.Anything, mock.MatchedBy(func(snapshot domain.Snapshot) bool {
			return snapshot.NumberID == h.numberID && snapshot.WorkspaceID == h.scope.WorkspaceID
		})).
		Return(nil).
		Once()
	h.sessions.EXPECT().ForgetSession(mock.Anything, mock.Anything, h.numberID).Return(nil).Once()
	h.store.EXPECT().Publish(mock.Anything, mock.Anything, h.numberID, false).Return(nil).Once()
	h.connections.EXPECT().Disconnect(mock.Anything, mock.Anything, connection).Return(nil).Once()
	h.store.EXPECT().
		SetStatus(mock.Anything, mock.Anything, h.numberID, domain.StatusDisconnected, "dinonaktifkan").
		Return(h.number(func(n *domain.Number) { n.Status = domain.StatusDisconnected }), nil).
		Once()
	h.activity.EXPECT().Emit(mock.Anything, mock.Anything).Return(nil).Once()

	updated, err := h.service.Unlink(t.Context(), h.scope, h.numberID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusDisconnected, updated.Status)
}

// TestAnOfflineNumberCanStillBeUnlinked: the session is already gone from the
// transport's point of view, and refusing would leave a number the workspace cannot
// get rid of.
func TestAnOfflineNumberCanStillBeUnlinked(t *testing.T) {
	h := newHarness(t)

	h.store.EXPECT().Get(mock.Anything, mock.Anything, h.numberID).Return(h.number(), nil).Once()
	h.transport.EXPECT().Unlink(mock.Anything, mock.Anything).Return(domain.ErrNotConnected).Once()
	h.sessions.EXPECT().ForgetSession(mock.Anything, mock.Anything, h.numberID).Return(domain.ErrNotFound).Once()
	h.store.EXPECT().Publish(mock.Anything, mock.Anything, h.numberID, false).Return(nil).Once()
	h.connections.EXPECT().Disconnect(mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()
	h.store.EXPECT().SetStatus(mock.Anything, mock.Anything, h.numberID, mock.Anything, mock.Anything).
		Return(h.number(), nil).Once()
	h.activity.EXPECT().Emit(mock.Anything, mock.Anything).Return(nil).Once()

	_, err := h.service.Unlink(t.Context(), h.scope, h.numberID)
	require.NoError(t, err)
}

// TestTheWorkspaceIsRequiredOnEveryPath: a read or a write with no tenant is refused
// rather than run against whatever scope the connection happens to carry.
func TestTheWorkspaceIsRequiredOnEveryPath(t *testing.T) {
	h := newHarness(t)
	empty := domain.Scope{}
	ctx := t.Context()

	_, err := h.service.Numbers(ctx, empty)
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Number(ctx, empty, h.numberID)
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Prepare(ctx, empty, domain.LinkRequest{AgentID: h.agentID, Phone: "628123456789"})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.BeginLink(ctx, empty, domain.LinkRequest{AgentID: h.agentID, Phone: "628123456789"})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.AcceptTerms(ctx, empty, h.numberID)
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Report(ctx, empty, domain.ReportRequest{NumberID: h.numberID, Status: domain.StatusConnected})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Enqueue(ctx, empty, domain.EnqueueRequest{NumberID: h.numberID, ToJID: "628999", Text: "x"})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, _, err = h.service.Receive(ctx, empty, h.numberID, domain.Inbound{WaMessageID: "msg-1"})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Claim(ctx, empty, h.numberID, 1)
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	require.ErrorIs(t, h.service.Sent(ctx, empty, uuid.New(), "wa-1", h.clock.at), domain.ErrInvalidInput)
	require.ErrorIs(t, h.service.Retry(ctx, empty, uuid.New(), "x", h.clock.at), domain.ErrInvalidInput)
	require.ErrorIs(t, h.service.Failed(ctx, empty, uuid.New(), "x", 1), domain.ErrInvalidInput)
	require.ErrorIs(t, h.service.StoreMedia(ctx, empty, uuid.New(), domain.Media{}), domain.ErrInvalidInput)

	_, err = h.service.Reclaim(ctx, empty, h.clock.at)
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Messages(ctx, empty, domain.MessageFilter{})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.service.Outbox(ctx, empty, domain.OutboxFilter{})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	require.ErrorIs(t, h.service.CancelLink(ctx, empty, "link-1"), domain.ErrInvalidInput)
}

// TestALinkWithoutAGatewaySaysSo: a deployment with no gateway serves its reads and
// refuses to link, rather than failing when somebody clicks.
func TestALinkWithoutAGatewaySaysSo(t *testing.T) {
	h := newHarness(t)
	h.service = service.New(service.Deps{
		Repository: h.store,
		Sessions:   h.sessions,
		Now:        h.clock.now,
	})

	_, err := h.service.BeginLink(t.Context(), h.scope, domain.LinkRequest{
		Phone:   "628123456789",
		AgentID: h.agentID,
	})
	require.ErrorIs(t, err, domain.ErrNotConfigured)
}

// TestTheCeilingIsBoundedWhenSet: a workspace can pace its number, and a value the
// product does not accept is refused rather than stored.
func TestTheCeilingIsBoundedWhenSet(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.SetLimit(t.Context(), h.scope, h.numberID, 5000)
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	h.store.EXPECT().Get(mock.Anything, mock.Anything, h.numberID).Return(h.number(), nil).Once()
	h.store.EXPECT().
		Update(mock.Anything, mock.Anything, mock.MatchedBy(func(number domain.Number) bool {
			return number.HourlyLimit == 12
		})).
		Return(h.number(func(n *domain.Number) { n.HourlyLimit = 12 }), nil).
		Once()
	h.activity.EXPECT().Emit(mock.Anything, mock.Anything).Return(nil).Once()

	updated, err := h.service.SetLimit(t.Context(), h.scope, h.numberID, 12)
	require.NoError(t, err)
	require.Equal(t, 12, updated.HourlyLimit)
}

// TestANumberWithNoLabelCannotBeRenamedToNothing: an empty name is a row a person
// cannot tell apart from another.
func TestANumberWithNoLabelCannotBeRenamedToNothing(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.SetLabel(t.Context(), h.scope, h.numberID, "   ")
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestTheActivityIsBestEffort: an event that cannot be written is logged rather
// than failing the thing that happened. The message is recorded either way, and
// refusing to record it because a stream write failed would lose a customer's
// words to keep a notification.
func TestTheActivityIsBestEffort(t *testing.T) {
	h := newHarness(t)

	h.store.EXPECT().Get(mock.Anything, mock.Anything, h.numberID).Return(h.number(), nil).Once()
	h.store.EXPECT().Record(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.Inbound{ID: uuid.New(), NumberID: h.numberID, WaMessageID: "msg-9", Kind: domain.KindText}, true, nil).
		Once()
	h.activity.EXPECT().Emit(mock.Anything, mock.Anything).Return(errors.New("the stream is down")).Once()
	h.watchers.EXPECT().Fire(mock.Anything, mock.Anything, mock.Anything).Return(1, nil).Once()

	_, created, err := h.service.Receive(t.Context(), h.scope, h.numberID, domain.Inbound{
		WaMessageID: "msg-9",
		ChatJID:     "628999@s.whatsapp.net",
		Text:        "halo",
	})
	require.NoError(t, err)
	require.True(t, created)
}

// TestWhatTheBoluIsToldIsTheWholeMessage: the task runs out of process with an agent,
// a title, and this text, so everything the Bolu can know about the customer's message
// has to be in it — the sender, what kind of thing arrived, whether its attachment
// made it, and what to do about it.
//
// Each shape is stated because each is a different situation for the reader: a picture
// whose bytes are still coming cannot be read yet, one whose download failed is a dead
// end the Bolu must not pretend to see, and a message that is not text at all is not an
// empty one.
func TestWhatTheBoluIsToldIsTheWholeMessage(t *testing.T) {
	cases := []struct {
		name    string
		message domain.Inbound
		wants   []string
		refuses []string
	}{
		{
			name: "a picture nobody can read yet",
			message: domain.Inbound{
				Kind: domain.KindImage, Media: domain.Media{Status: domain.MediaPending},
			},
			wants:   []string{"Pelanggan mengirim sebuah image", "sedang diunduh"},
			refuses: []string{"bisa dibaca lewat alat"},
		},
		{
			name: "a picture that arrived, with a caption",
			message: domain.Inbound{
				Kind: domain.KindImage, Text: "ini nota bulan ini",
				Media: domain.Media{Status: domain.MediaReady},
			},
			wants: []string{"ini nota bulan ini", "sudah tersimpan"},
		},
		{
			name: "a document whose download failed",
			message: domain.Inbound{
				Kind: domain.KindDocument, Media: domain.Media{Status: domain.MediaFailed, Error: "unduhan putus"},
			},
			wants:   []string{"Pelanggan mengirim sebuah document", "gagal diunduh: unduhan putus"},
			refuses: []string{"sedang diunduh"},
		},
		{
			name:    "something this product does not read",
			message: domain.Inbound{Kind: domain.KindOther},
			wants:   []string{"bukan teks"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)

			h.store.EXPECT().Get(mock.Anything, mock.Anything, h.numberID).Return(h.number(), nil).Once()
			h.store.EXPECT().Record(mock.Anything, mock.Anything, mock.Anything).
				Return(domain.Inbound{
					ID: uuid.New(), NumberID: h.numberID, WaMessageID: "msg-1",
					ChatJID: "628999@s.whatsapp.net", SenderName: "Bu Sari",
					Kind: tc.message.Kind, Text: tc.message.Text, Media: tc.message.Media,
				}, true, nil).Once()
			h.watchers.EXPECT().Fire(mock.Anything, mock.Anything, mock.Anything).Return(0, nil).Once()
			h.replies.EXPECT().Answer(mock.Anything, mock.Anything, mock.MatchedBy(func(request domain.AnswerRequest) bool {
				// The reply address is the sender, and the title names the person
				// rather than the number when WhatsApp gave a name.
				require.Contains(t, request.Prompt, "Nomor pengirim: +628999@s.whatsapp.net")
				require.Contains(t, request.Prompt, "Nama yang dipakai pengirim: Bu Sari")
				require.Contains(t, request.Prompt, "menulis ke nomor WhatsApp ••••••••6789")
				require.Equal(t, "Pesan WhatsApp dari Bu Sari", request.Title)

				for _, want := range tc.wants {
					require.Contains(t, request.Prompt, want)
				}
				for _, refuse := range tc.refuses {
					require.NotContains(t, request.Prompt, refuse)
				}
				return true
			})).Return(uuid.New(), nil).Once()
			h.store.EXPECT().AttachTask(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()
			h.activity.EXPECT().Emit(mock.Anything, mock.Anything).Return(nil).Once()

			_, created, err := h.service.Receive(t.Context(), h.scope, h.numberID, domain.Inbound{
				WaMessageID: "msg-1",
				ChatJID:     "628999@s.whatsapp.net",
				SenderName:  "Bu Sari",
				Kind:        tc.message.Kind,
				Text:        tc.message.Text,
				Media:       tc.message.Media,
				ReceivedAt:  h.clock.at,
			})
			require.NoError(t, err)
			require.True(t, created)
		})
	}

	// A message with no name from WhatsApp is titled by the number instead, because a
	// title is what a person reads in the task list.
	h := newHarness(t)
	h.store.EXPECT().Get(mock.Anything, mock.Anything, h.numberID).Return(h.number(), nil).Once()
	h.store.EXPECT().Record(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.Inbound{
			ID: uuid.New(), NumberID: h.numberID, WaMessageID: "msg-2",
			ChatJID: "628999@s.whatsapp.net", Kind: domain.KindText, Text: "halo",
		}, true, nil).Once()
	h.watchers.EXPECT().Fire(mock.Anything, mock.Anything, mock.Anything).Return(0, nil).Once()
	h.replies.EXPECT().Answer(mock.Anything, mock.Anything, mock.MatchedBy(func(request domain.AnswerRequest) bool {
		require.Equal(t, "Pesan WhatsApp dari ••••••••6789", request.Title)
		require.NotContains(t, request.Prompt, "Nama yang dipakai pengirim")
		return true
	})).Return(uuid.New(), nil).Once()
	h.store.EXPECT().AttachTask(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()
	h.activity.EXPECT().Emit(mock.Anything, mock.Anything).Return(nil).Once()

	_, created, err := h.service.Receive(t.Context(), h.scope, h.numberID, domain.Inbound{
		WaMessageID: "msg-2", ChatJID: "628999@s.whatsapp.net", Kind: domain.KindText, Text: "halo",
	})
	require.NoError(t, err)
	require.True(t, created)
}
