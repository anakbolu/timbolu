package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	whatsappdomain "github.com/robzlabz/employeebot/packages/whatsapp/domain"
)

// The engine's own flow is this file: what it does with the fleet it is told to
// serve, with the events WhatsApp sends, and with the outbox.
//
// Everything here runs against the ports rather than a socket, which is the point:
// these are the decisions that go wrong in production without anything failing
// loudly — a number dropped at the wrong moment, a message returned to the queue
// twice, a session written under a workspace that does not own it.

// TestRunNeedsItsPorts: a deployment wired without the three things every pass needs
// is refused at startup rather than serving nothing quietly.
func TestRunNeedsItsPorts(t *testing.T) {
	engine := NewEngine(DefaultEngineConfig(), EngineDeps{}, nil)

	require.ErrorIs(t, engine.Run(t.Context()), whatsappdomain.ErrNotConfigured)
}

// TestRunServesTheFleetUntilItIsStopped: the first pass runs before the loops start,
// so an instance that comes up serves what it may immediately rather than after one
// poll; and stopping it writes what it holds before it returns.
func TestRunServesTheFleetUntilItIsStopped(t *testing.T) {
	h := newEngineHarness(t)
	h.directory.EXPECT().Serving(mock.Anything).Return(nil, nil).Maybe()

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		// Long enough for the first pass, short enough that the test is not waiting
		// on the poll interval.
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	require.NoError(t, h.engine.Run(ctx))
	require.Empty(t, h.engine.snapshot(), "nothing was held, so nothing had to be written")
}

// TestANumberThatIsNoLongerServedIsLetGo: the fleet's word is what a number's
// presence follows, and holding a client for a number the workspace removed keeps it
// offline everywhere else.
func TestANumberThatIsNoLongerServedIsLetGo(t *testing.T) {
	h := newEngineHarness(t)
	runtime := servedRuntime(t, h, h.assignment())
	h.engine.numbers[runtime.numberID] = runtime

	h.directory.EXPECT().Serving(mock.Anything).Return(nil, nil).Once()
	h.status.EXPECT().Report(mock.Anything, mock.Anything, mock.Anything).Return(whatsappdomain.Number{}, nil).Once()
	h.lease.EXPECT().Release(mock.Anything, h.numberID, "test-instance").Return(nil).Once()

	h.engine.reconcile(t.Context())

	require.False(t, h.engine.running(h.numberID), "the number is given up")
	require.Error(t, runtime.ctx.Err(), "and its client's lifetime is over")
}

// TestAReconciliationThatCannotAnswerKeepsWhatItHolds: a directory that cannot be
// read is not a fleet that is empty, and dropping every number on a transient read
// failure would take a workspace's numbers offline for no reason.
func TestAReconciliationThatCannotAnswerKeepsWhatItHolds(t *testing.T) {
	h := newEngineHarness(t)
	runtime := servedRuntime(t, h, h.assignment())
	h.engine.numbers[runtime.numberID] = runtime

	h.directory.EXPECT().Serving(mock.Anything).Return(nil, errors.New("the directory is down")).Once()

	h.engine.reconcile(t.Context())

	require.True(t, h.engine.running(h.numberID), "what this instance already serves is kept")
}

// TestANumberThatCannotBeClaimedIsLeftAlone: a lease that cannot be taken is either
// somebody else's number or a Redis that is not answering, and both are answered by
// leaving it where it is rather than connecting a second client.
func TestANumberThatCannotBeClaimedIsLeftAlone(t *testing.T) {
	h := newEngineHarness(t)

	h.directory.EXPECT().Serving(mock.Anything).Return([]whatsappdomain.Assignment{h.assignment()}, nil).Once()
	h.lease.EXPECT().Acquire(mock.Anything, h.numberID, "test-instance", mock.Anything).
		Return(false, errors.New("redis is down")).Once()

	require.NotPanics(t, func() { h.engine.reconcile(t.Context()) })
	require.False(t, h.engine.running(h.numberID))

	// Somebody else holds it, which is the healthy answer: nothing is reported and
	// nothing is started.
	other := newEngineHarness(t)
	other.directory.EXPECT().Serving(mock.Anything).Return([]whatsappdomain.Assignment{other.assignment()}, nil).Once()
	other.lease.EXPECT().Acquire(mock.Anything, other.numberID, "test-instance", mock.Anything).Return(false, nil).Once()

	other.engine.reconcile(t.Context())
	require.False(t, other.engine.running(other.numberID))
}

// TestALeaseThatWasTakenOverStandsDown: two clients on one session is the one thing
// the fleet must never do, so a lease this instance no longer holds ends its client
// immediately.
func TestALeaseThatWasTakenOverStandsDown(t *testing.T) {
	h := newEngineHarness(t)
	runtime := servedRuntime(t, h, h.assignment())
	h.engine.numbers[runtime.numberID] = runtime

	h.lease.EXPECT().Renew(mock.Anything, h.numberID, "test-instance", mock.Anything).Return(false, nil).Once()
	h.status.EXPECT().Report(mock.Anything, mock.Anything, mock.Anything).Return(whatsappdomain.Number{}, nil).Once()

	h.engine.renew(t.Context())

	require.False(t, h.engine.running(h.numberID))
	require.Error(t, runtime.ctx.Err())
}

// TestARenewalThatCannotBeAnsweredKeepsTheNumber: the lease still exists with its
// original deadline, so a Redis that cannot answer is not a reason to take a number
// offline.
func TestARenewalThatCannotBeAnsweredKeepsTheNumber(t *testing.T) {
	h := newEngineHarness(t)
	runtime := servedRuntime(t, h, h.assignment())
	h.engine.numbers[runtime.numberID] = runtime

	h.lease.EXPECT().Renew(mock.Anything, h.numberID, "test-instance", mock.Anything).
		Return(false, errors.New("redis is down")).Once()

	h.engine.renew(t.Context())

	require.True(t, h.engine.running(h.numberID))
	require.NoError(t, runtime.ctx.Err())
}

// TestANumberWhoseSessionRowIsGoneIsNotAFailure: a row can be gone while the number
// still reads as linked — a session deleted to force a re-link, or a workspace restored
// from a backup that predates it. There is nothing to serve, and that is a state rather
// than an error: the caller gives the lease back and waits for a person.
func TestANumberWhoseSessionRowIsGoneIsNotAFailure(t *testing.T) {
	h := newEngineHarness(t)
	assignment := h.assignment()

	h.status.EXPECT().Number(mock.Anything, mock.Anything, h.numberID).
		Return(whatsappdomain.Number{ID: h.numberID, WorkspaceID: h.workspace, Phone: "628123456789", Status: whatsappdomain.StatusConnected}, nil).Once()
	h.sessions.EXPECT().Load(mock.Anything, mock.Anything, h.numberID).
		Return(whatsappdomain.Session{}, whatsappdomain.ErrNotFound).Once()

	served, err := h.engine.startNumber(t.Context(), assignment)

	require.NoError(t, err, "a number nobody has finished linking is not a failure")
	require.False(t, served)
}

// TestASessionThatWillNotOpenAsksForARelink: a blob that does not open under its own
// workspace's key is either a rotation that dropped the key or a row that was tampered
// with, and retrying it forever would leave a person with a number that is silently
// offline.
func TestASessionThatWillNotOpenAsksForARelink(t *testing.T) {
	h := newEngineHarness(t)
	assignment := h.assignment()

	h.status.EXPECT().Number(mock.Anything, mock.Anything, h.numberID).
		Return(whatsappdomain.Number{ID: h.numberID, WorkspaceID: h.workspace, Phone: "628123456789", Status: whatsappdomain.StatusConnected}, nil).Once()
	h.sessions.EXPECT().Load(mock.Anything, mock.Anything, h.numberID).
		Return(whatsappdomain.Session{NumberID: h.numberID, WorkspaceID: h.workspace, Sealed: []byte("sealed")}, nil).Once()
	h.sealer.EXPECT().Open(mock.Anything, h.workspace, []byte("sealed")).Return(nil, errors.New("wrong key")).Once()
	h.status.EXPECT().Report(mock.Anything, mock.Anything, mock.MatchedBy(func(request whatsappdomain.ReportRequest) bool {
		return request.Status == whatsappdomain.StatusNeedsRelink && request.Reason != ""
	})).Return(whatsappdomain.Number{}, nil).Once()

	served, err := h.engine.startNumber(t.Context(), assignment)

	require.Error(t, err)
	require.False(t, served)
	require.False(t, h.engine.running(h.numberID))
}

// TestAStoreThatCannotAnswerIsAnError: a number's status and its session are both
// reads this instance cannot invent, so failing them fails the number rather than
// serving something wrong.
func TestAStoreThatCannotAnswerIsAnError(t *testing.T) {
	h := newEngineHarness(t)
	assignment := h.assignment()

	h.status.EXPECT().Number(mock.Anything, mock.Anything, h.numberID).
		Return(whatsappdomain.Number{}, errors.New("the status store is down")).Once()

	served, err := h.engine.startNumber(t.Context(), assignment)
	require.Error(t, err)
	require.False(t, served)

	other := newEngineHarness(t)
	other.status.EXPECT().Number(mock.Anything, mock.Anything, other.numberID).
		Return(whatsappdomain.Number{ID: other.numberID, WorkspaceID: other.workspace, Status: whatsappdomain.StatusConnected}, nil).Once()
	other.sessions.EXPECT().Load(mock.Anything, mock.Anything, other.numberID).
		Return(whatsappdomain.Session{}, errors.New("the session store is down")).Once()

	served, err = other.engine.startNumber(t.Context(), other.assignment())
	require.Error(t, err)
	require.False(t, served)
}

// TestTheSessionIsSealedUnderItsOwnWorkspace is the isolation the epic's first gate
// asks for, stated where the write happens: the seal is the workspace's, and the row is
// stored under the scope that owns it.
func TestTheSessionIsSealedUnderItsOwnWorkspace(t *testing.T) {
	h := newEngineHarness(t)
	scope := whatsappdomain.Scope{WorkspaceID: h.workspace}
	put := h.engine.persister(scope, h.numberID)

	h.sealer.EXPECT().Seal(mock.Anything, h.workspace, mock.Anything).Return([]byte("sealed"), nil).Once()
	h.sessions.EXPECT().Save(mock.Anything, scope, mock.MatchedBy(func(session whatsappdomain.Session) bool {
		return session.NumberID == h.numberID &&
			session.WorkspaceID == h.workspace &&
			string(session.Sealed) == "sealed" &&
			session.JID == "628123456789:12@s.whatsapp.net"
	})).Return(nil).Once()

	require.NoError(t, put(t.Context(), sealed{JID: "628123456789:12@s.whatsapp.net"}))

	// A sealer that refuses is a session that was not written, and it is reported
	// rather than swallowed: the caller logs it and the number keeps running on what
	// was last stored.
	h.sealer.EXPECT().Seal(mock.Anything, h.workspace, mock.Anything).Return(nil, errors.New("no key")).Once()
	require.Error(t, put(t.Context(), sealed{}))

	h.sealer.EXPECT().Seal(mock.Anything, h.workspace, mock.Anything).Return([]byte("sealed"), nil).Once()
	h.sessions.EXPECT().Save(mock.Anything, scope, mock.Anything).Return(errors.New("the session store is down")).Once()
	require.Error(t, put(t.Context(), sealed{}))
}

// TestTheEventHandlerRoutesWhatWhatsappSends goes through the handler the engine
// registers, because that is what decides which port hears about which event — and a
// connection, a disconnect, and a refusal all have to reach the workspace.
func TestTheEventHandlerRoutesWhatWhatsappSends(t *testing.T) {
	h := newEngineHarness(t)
	runtime := servedRuntime(t, h, h.assignment())
	ctx := t.Context()
	handler := h.engine.handler(ctx, runtime)

	// Connected: the session is written because that is the moment the ADV identity
	// is known to be the one WhatsApp accepts, and the workspace is told it is up.
	h.status.EXPECT().Report(mock.Anything, mock.Anything, mock.MatchedBy(func(request whatsappdomain.ReportRequest) bool {
		return request.Status == whatsappdomain.StatusConnected
	})).Return(whatsappdomain.Number{}, nil).Once()
	handler(&events.Connected{})
	require.True(t, runtime.online.Load())

	// Disconnected: not the number's fault, so it is reported without claiming the
	// session is gone.
	h.status.EXPECT().Report(mock.Anything, mock.Anything, mock.MatchedBy(func(request whatsappdomain.ReportRequest) bool {
		return request.Status == whatsappdomain.StatusDisconnected && request.Reason != ""
	})).Return(whatsappdomain.Number{}, nil).Once()
	handler(&events.Disconnected{})
	require.False(t, runtime.online.Load())

	// A message with no body is not a message.
	handler(&events.Message{})

	// Logged out: the session is gone at WhatsApp's end, so it is forgotten locally
	// and the number is reported as needing a person — and that report is what the
	// screen reads to offer a re-link.
	h.sessions.EXPECT().ForgetSession(mock.Anything, mock.Anything, h.numberID).Return(nil).Once()
	h.status.EXPECT().Report(mock.Anything, mock.Anything, mock.MatchedBy(func(request whatsappdomain.ReportRequest) bool {
		return request.Status == whatsappdomain.StatusNeedsRelink
	})).Return(whatsappdomain.Number{}, nil).Once()
	h.lease.EXPECT().Release(mock.Anything, h.numberID, "test-instance").Return(nil).Once()
	handler(&events.LoggedOut{OnConnect: true})
	require.False(t, h.engine.running(h.numberID), "a refused session is not served")

	// An event this product does not care about changes nothing.
	handler(&events.Presence{})
}

// TestAnInboundMessageCarriesWhatTheModuleNeeds: the row is what the workspace reads,
// so every field it needs is on it before the listener returns — including the media
// descriptor, which is kept for the download pass rather than fetched here.
func TestAnInboundMessageCarriesWhatTheModuleNeeds(t *testing.T) {
	h := newEngineHarness(t)
	runtime := servedRuntime(t, h, h.assignment())

	message := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Caption:    proto.String("nota bulan ini"),
		DirectPath: proto.String("/v/t62/abc"),
		FileLength: proto.Uint64(2048),
	}}
	event := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:     types.NewJID("628999", types.DefaultUserServer),
				Sender:   types.NewJID("628999", types.DefaultUserServer),
				IsGroup:  true,
				IsFromMe: false,
			},
			ID:        "wa-1",
			PushName:  "Bu Sari",
			Timestamp: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC),
		},
		Message: message,
	}

	h.inbox.EXPECT().Receive(mock.Anything, mock.Anything, h.numberID, mock.MatchedBy(func(inbound whatsappdomain.Inbound) bool {
		return inbound.WaMessageID == "wa-1" &&
			inbound.ChatJID == "628999@s.whatsapp.net" &&
			inbound.SenderJID == "628999@s.whatsapp.net" &&
			inbound.SenderName == "Bu Sari" &&
			inbound.Kind == whatsappdomain.KindImage &&
			inbound.Text == "nota bulan ini" &&
			inbound.IsGroup &&
			inbound.Media.Status == whatsappdomain.MediaPending &&
			inbound.Media.Kind == whatsappdomain.KindImage &&
			inbound.Media.Ref == "/v/t62/abc" &&
			inbound.Media.Size == 2048 &&
			inbound.ReceivedAt.Equal(time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC))
	})).Return(whatsappdomain.Inbound{}, false, nil).Once()

	h.engine.handler(t.Context(), runtime)(event)

	require.Same(t, message, runtime.takeRawMedia("wa-1"), "the descriptor is kept for the download pass")
	require.Nil(t, runtime.takeRawMedia("wa-1"), "and is taken once")
}

// TestAnInboundMessageWithNoTimestampStillHasOne: not every event carries a time —
// a message replayed from an old session may not — and a row with no time is one the
// workspace cannot order.
func TestAnInboundMessageWithNoTimestampStillHasOne(t *testing.T) {
	h := newEngineHarness(t)
	runtime := servedRuntime(t, h, h.assignment())

	event := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: types.NewJID("628999", types.DefaultUserServer)},
			ID:            "wa-2",
		},
		Message: &waE2E.Message{Conversation: proto.String("halo")},
	}

	h.inbox.EXPECT().Receive(mock.Anything, mock.Anything, h.numberID, mock.MatchedBy(func(inbound whatsappdomain.Inbound) bool {
		return !inbound.ReceivedAt.IsZero() && inbound.Kind == whatsappdomain.KindText && inbound.Text == "halo"
	})).Return(whatsappdomain.Inbound{}, false, nil).Once()

	h.engine.handler(t.Context(), runtime)(event)
}

// TestAnInboundMessageThatCannotBeRecordedIsNotLostSilently: the listener has no
// caller to return an error to, so the failure is logged and the next message is read.
// The gateway must not stop reading because one row could not be written.
func TestAnInboundMessageThatCannotBeRecordedIsNotLostSilently(t *testing.T) {
	h := newEngineHarness(t)
	runtime := servedRuntime(t, h, h.assignment())

	h.inbox.EXPECT().Receive(mock.Anything, mock.Anything, h.numberID, mock.Anything).
		Return(whatsappdomain.Inbound{}, false, errors.New("the inbox is down")).Once()

	event := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: types.NewJID("628999", types.DefaultUserServer)},
			ID:            "wa-3",
		},
		Message: &waE2E.Message{Conversation: proto.String("halo")},
	}

	require.NotPanics(t, func() { h.engine.handler(t.Context(), runtime)(event) })
}

// TestTheMediaBufferIsBounded: WhatsApp sends the descriptor with the message and
// does not keep it, so it is held until the download pass — and holding all of them
// would be a gateway that grows with every attachment a workspace receives.
func TestTheMediaBufferIsBounded(t *testing.T) {
	runtime := &numberRuntime{media: map[string]*waE2E.Message{}}

	for index := range maxPendingMedia + 50 {
		runtime.rememberMedia(string(rune('a'+index%26))+string(rune('0'+index%10))+string(rune(index)), &waE2E.Message{})
	}

	require.Len(t, runtime.media, maxPendingMedia, "the buffer does not grow past its bound")
	require.Len(t, runtime.mediaSeq, maxPendingMedia)

	// Remembering the same message twice does not queue it twice, which is what
	// keeps a redelivery from evicting somebody else's descriptor.
	runtime.rememberMedia("same", &waE2E.Message{})
	runtime.rememberMedia("same", &waE2E.Message{})
	require.Len(t, runtime.mediaSeq, maxPendingMedia)
}

// TestTheOutboxIsOnlyDrainedForANumberThatIsOnline: a message sent on a client that
// is not connected is a message that fails and is retried, and retrying it here would
// spend the number's attempts on a connection nobody has.
func TestTheOutboxIsOnlyDrainedForANumberThatIsOnline(t *testing.T) {
	h := newEngineHarness(t)
	runtime := servedRuntime(t, h, h.assignment())
	h.engine.numbers[runtime.numberID] = runtime

	// No Drain expectation is set: an offline number must not claim anything.
	h.engine.drainAll(t.Context())

	// And the drain pass claims only what the ceiling allows, at the configured
	// limit.
	h.drain.EXPECT().Claim(mock.Anything, mock.Anything, h.numberID, h.engine.cfg.SendLimit).
		Return(whatsappdomain.Claimed{}, nil).Once()
	h.engine.drainNumber(t.Context(), runtime)
}

// TestAQueuedMessageIsReturnedToTheQueueWhenItCannotBeSent: a transient failure
// deserves the next attempt, and a message that has run out of attempts is parked
// rather than retried forever.
func TestAQueuedMessageIsReturnedToTheQueueWhenItCannotBeSent(t *testing.T) {
	h := newEngineHarness(t)
	runtime := servedRuntime(t, h, h.assignment())
	item := whatsappdomain.Outbox{
		ID:       uuid.New(),
		ToJID:    "628999@s.whatsapp.net",
		Kind:     whatsappdomain.KindText,
		Text:     "halo",
		Attempts: 1,
	}

	h.drain.EXPECT().Claim(mock.Anything, mock.Anything, h.numberID, mock.Anything).
		Return(whatsappdomain.Claimed{Ready: []whatsappdomain.Outbox{item}, Held: 3}, nil).Once()
	h.drain.EXPECT().Retry(mock.Anything, mock.Anything, item.ID, mock.Anything, mock.Anything).Return(nil).Once()

	h.engine.drainNumber(t.Context(), runtime)

	// The same message on its last attempt is parked with its reason instead.
	other := newEngineHarness(t)
	otherRuntime := servedRuntime(t, other, other.assignment())
	last := item
	last.Attempts = other.engine.cfg.MaxAttempts

	other.drain.EXPECT().Claim(mock.Anything, mock.Anything, other.numberID, mock.Anything).
		Return(whatsappdomain.Claimed{Ready: []whatsappdomain.Outbox{last}}, nil).Once()
	other.drain.EXPECT().Failed(mock.Anything, mock.Anything, last.ID, mock.Anything, last.Attempts).Return(nil).Once()

	other.engine.drainNumber(t.Context(), otherRuntime)
}

// TestAnOutboxThatCannotBeReadIsNotFatal: the drain pass is one of four loops, and
// failing it must not take the other three — or the connection — with it.
func TestAnOutboxThatCannotBeReadIsNotFatal(t *testing.T) {
	h := newEngineHarness(t)
	runtime := servedRuntime(t, h, h.assignment())

	h.drain.EXPECT().Claim(mock.Anything, mock.Anything, h.numberID, mock.Anything).
		Return(whatsappdomain.Claimed{}, errors.New("the outbox is down")).Once()

	require.NotPanics(t, func() { h.engine.drainNumber(t.Context(), runtime) })
}

// TestSendBackoffGrowsAndIsBounded: a number that keeps failing should be tried less
// often, and a customer's answer must be delayed rather than lost, which is why the
// wait is capped instead of doubling without end.
func TestSendBackoffGrowsAndIsBounded(t *testing.T) {
	require.Equal(t, 5*time.Second, sendBackoff(0), "a first failure is retried soon")
	require.Equal(t, 10*time.Second, sendBackoff(2))
	require.Equal(t, time.Minute, sendBackoff(30), "and the wait is capped rather than doubling")
}

// TestTheStrayMessagesOfADeadGatewayAreReclaimed: a gateway replaced mid-send leaves
// rows in `sending`, and the successor is the only thing that can return them to the
// queue.
func TestTheStrayMessagesOfADeadGatewayAreReclaimed(t *testing.T) {
	h := newEngineHarness(t)
	scope := whatsappdomain.Scope{WorkspaceID: h.workspace}

	h.drain.EXPECT().Reclaim(mock.Anything, scope, mock.Anything).Return(2, nil).Once()
	require.NotPanics(t, func() { h.engine.reclaimNumber(t.Context(), scope, h.numberID) })

	// A deployment without a drain has nothing to reclaim, which is not an error.
	unwired := NewEngine(DefaultEngineConfig(), EngineDeps{}, nil)
	require.NotPanics(t, func() { unwired.reclaimNumber(t.Context(), scope, h.numberID) })
}

// TestShutdownWritesAndReleasesWhatItHolds: a clean stop is a successor taking over in
// milliseconds instead of waiting for the lease to expire, and a session that was
// already written is written once more so nothing in memory is lost.
func TestShutdownWritesAndReleasesWhatItHolds(t *testing.T) {
	h := newEngineHarness(t)
	runtime := servedRuntime(t, h, h.assignment())
	h.engine.numbers[runtime.numberID] = runtime

	var written bool
	runtime.store.put = func(context.Context, sealed) error {
		written = true
		return nil
	}

	attempt := pairingAttempt(t, h, "628123456789")
	h.engine.links[attempt.id] = attempt

	h.lease.EXPECT().Release(mock.Anything, h.numberID, "test-instance").Return(nil).Once()

	h.engine.shutdown()

	require.True(t, written, "the session is written on the way out")
	require.Empty(t, h.engine.numbers)
	require.Empty(t, h.engine.links)
	require.Error(t, runtime.ctx.Err())
	require.Error(t, attempt.ctx.Err())
}

// TestTheLogLinesHideAllButTheLastFourDigits: a number is what a person recognises a
// connection by, and four digits is enough for that and not enough to publish.
func TestTheLogLinesHideAllButTheLastFourDigits(t *testing.T) {
	require.Equal(t, "••••••••6789", maskPhone("628123456789"))
	require.Equal(t, "••••", maskPhone("1234"), "a number shorter than the mask is hidden whole")
	require.Equal(t, "•", maskPhone(" 7 "))
	require.Equal(t, "62812345", shortID(uuid.MustParse("62812345-0000-0000-0000-000000000000")))
}

// TestTheMessageHelpersNameWhatWhatsappSent covers the translation from protobuf to
// this module's own words, which is what the workspace sees: a caption that arrived as
// an image is read as the message's text, and a kind this product does not handle is
// still a message rather than a panic.
func TestTheMessageHelpersNameWhatWhatsappSent(t *testing.T) {
	require.Equal(t, whatsappdomain.KindText, messageKind(&waE2E.Message{Conversation: proto.String("halo")}))
	require.Equal(t, whatsappdomain.KindText, messageKind(&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("halo")}}))
	require.Equal(t, whatsappdomain.KindImage, messageKind(&waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}))
	require.Equal(t, whatsappdomain.KindVideo, messageKind(&waE2E.Message{VideoMessage: &waE2E.VideoMessage{}}))
	require.Equal(t, whatsappdomain.KindAudio, messageKind(&waE2E.Message{AudioMessage: &waE2E.AudioMessage{}}))
	require.Equal(t, whatsappdomain.KindDocument, messageKind(&waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{}}))
	require.Equal(t, whatsappdomain.KindSticker, messageKind(&waE2E.Message{StickerMessage: &waE2E.StickerMessage{}}))
	require.Equal(t, whatsappdomain.KindOther, messageKind(&waE2E.Message{}))

	require.Equal(t, "halo", messageText(&waE2E.Message{Conversation: proto.String("halo")}))
	require.Equal(t, "halo", messageText(&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("halo")}}))
	require.Equal(t, "nota", messageText(&waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("nota")}}))
	require.Equal(t, "rekap", messageText(&waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: proto.String("rekap")}}))
	require.Equal(t, "faktur", messageText(&waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Caption: proto.String("faktur")}}))
	require.Empty(t, messageText(&waE2E.Message{AudioMessage: &waE2E.AudioMessage{}}), "an audio message has no caption to read")

	media, ok := messageMedia(&waE2E.Message{VideoMessage: &waE2E.VideoMessage{DirectPath: proto.String("/v/1"), FileLength: proto.Uint64(9)}})
	require.True(t, ok)
	require.Equal(t, whatsappdomain.KindVideo, media.Kind)
	require.Equal(t, int64(9), media.Size)

	audio, ok := messageMedia(&waE2E.Message{AudioMessage: &waE2E.AudioMessage{DirectPath: proto.String("/v/2")}})
	require.True(t, ok)
	require.Equal(t, whatsappdomain.KindAudio, audio.Kind)

	document, ok := messageMedia(&waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{DirectPath: proto.String("/v/3")}})
	require.True(t, ok)
	require.Equal(t, whatsappdomain.KindDocument, document.Kind)

	sticker, ok := messageMedia(&waE2E.Message{StickerMessage: &waE2E.StickerMessage{DirectPath: proto.String("/v/4")}})
	require.True(t, ok)
	require.Equal(t, whatsappdomain.KindSticker, sticker.Kind)

	_, ok = messageMedia(&waE2E.Message{Conversation: proto.String("halo")})
	require.False(t, ok, "a message with no attachment has no media")

	require.Equal(t, "image/jpeg", mediaContentType(whatsappdomain.KindImage))
	require.Equal(t, "video/mp4", mediaContentType(whatsappdomain.KindVideo))
	require.Equal(t, "audio/ogg", mediaContentType(whatsappdomain.KindAudio))
	require.Equal(t, "image/webp", mediaContentType(whatsappdomain.KindSticker))
	require.Equal(t, "application/octet-stream", mediaContentType(whatsappdomain.KindDocument))

	require.NotNil(t, downloadInfo(&waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}))
	require.NotNil(t, downloadInfo(&waE2E.Message{VideoMessage: &waE2E.VideoMessage{}}))
	require.NotNil(t, downloadInfo(&waE2E.Message{AudioMessage: &waE2E.AudioMessage{}}))
	require.NotNil(t, downloadInfo(&waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{}}))
	require.NotNil(t, downloadInfo(&waE2E.Message{StickerMessage: &waE2E.StickerMessage{}}))
	require.Nil(t, downloadInfo(&waE2E.Message{Conversation: proto.String("halo")}))
}

// TestTheSentProtobufsCarryWhatWhatsappNeeds: a send with a missing field is a
// message WhatsApp accepts and shows as empty, so the fields are stated here.
func TestTheSentProtobufsCarryWhatWhatsappNeeds(t *testing.T) {
	require.Equal(t, "halo", textMessage("halo").GetConversation())

	upload := whatsmeow.UploadResponse{
		URL:        "https://mmg.whatsapp.net/x",
		DirectPath: "/v/t62/y",
		MediaKey:   []byte("key"),
		FileLength: 12,
	}
	image := mediaMessage(whatsappdomain.KindImage, upload, "nota.jpg", "ini notanya")
	require.Equal(t, "ini notanya", image.GetImageMessage().GetCaption())
	require.Equal(t, "https://mmg.whatsapp.net/x", image.GetImageMessage().GetURL())
	require.Equal(t, "/v/t62/y", image.GetImageMessage().GetDirectPath())
	require.Equal(t, "image/jpeg", image.GetImageMessage().GetMimetype())
	require.Equal(t, uint64(12), image.GetImageMessage().GetFileLength())

	// Anything that is not an image is sent as a document, because that is the only
	// other shape WhatsApp accepts for a file.
	document := mediaMessage(whatsappdomain.KindDocument, upload, "nota.pdf", "")
	require.Equal(t, "nota.pdf", document.GetDocumentMessage().GetFileName())
	require.Equal(t, "application/octet-stream", document.GetDocumentMessage().GetMimetype())

	require.Equal(t, types.ChatPresenceComposing, presenceFor(true))
	require.Equal(t, types.ChatPresencePaused, presenceFor(false))
}
