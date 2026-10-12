package whatsapp

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	whatsappdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/whatsapp/domain"
)

// The link flow's own bounds are this file, and the first one is a defect a browser
// found: an attempt whose connection stalled emitted nothing at all, so the screen sat
// on "menyiapkan kode…" forever. Waiting is not something a person can act on, which is
// why an attempt that produces no code within its deadline ends the way an expired code
// does.

// TestAStalledAttemptEndsOnItsOwnDeadline: the deadline is short enough that somebody
// watching the screen is not left guessing, and long enough to be past WhatsApp's own
// handshake.
func TestAStalledAttemptEndsOnItsOwnDeadline(t *testing.T) {
	require.LessOrEqual(t, attemptDeadline, 60*time.Second, "an attempt nobody can act on must not sit for minutes")
	require.Greater(t, attemptDeadline, 10*time.Second, "and it must be past the handshake's own time")
}

// TestAnAttemptThatFailedSaysWhy: the attempt's state carries the reason, and it is
// what a screen reads once pairing stops. A failure with no reason is the dead end the
// epic forbids.
func TestAnAttemptThatFailedSaysWhy(t *testing.T) {
	// The state a fresh attempt starts in, which is what the engine's own
	// constructor sets: a bare struct is not one anybody links with.
	attempt := &linkAttempt{id: "link-1", state: whatsappdomain.LinkWaiting}

	require.Equal(t, whatsappdomain.LinkWaiting, attempt.snapshot().State)

	attempt.fail("WhatsApp tidak mengirim kode; coba lagi")

	snapshot := attempt.snapshot()
	require.Equal(t, whatsappdomain.LinkFailed, snapshot.State)
	require.Contains(t, snapshot.Reason, "coba lagi", "a refusal has to say what to do next")
	require.Empty(t, snapshot.Code, "and it must not keep offering a code it cannot honour")
}

// TestAPairedAttemptIsNotTurnedIntoAFailure is why the state is checked before it is
// written: WhatsApp can deliver a late error event after a successful pairing, and a
// screen that had already shown "terhubung" must not flip back to a refusal.
func TestAPairedAttemptIsNotTurnedIntoAFailure(t *testing.T) {
	attempt := &linkAttempt{id: "link-1"}
	attempt.succeed("628123456789")

	attempt.fail("koneksi terputus setelahnya")

	snapshot := attempt.snapshot()
	require.Equal(t, whatsappdomain.LinkPaired, snapshot.State, "the first outcome stands")
	require.Equal(t, "628123456789", snapshot.Phone)
	require.Empty(t, snapshot.Reason)
}

// TestACodeCarriesItsOwnExpiry: WhatsApp rotates the code and says when, and the
// attempt keeps it so a screen does not have to guess.
func TestACodeCarriesItsOwnExpiry(t *testing.T) {
	attempt := &linkAttempt{id: "link-1"}

	attempt.setCode("qr-payload", 20*time.Second)
	snapshot := attempt.snapshot()
	require.Equal(t, whatsappdomain.LinkCode, snapshot.State)
	require.Equal(t, "qr-payload", snapshot.Code)
	require.WithinDuration(t, time.Now().Add(20*time.Second), snapshot.ExpiresAt, 2*time.Second)

	// A server that says nothing about the timeout still gets a sane one, so the
	// screen never shows a code with no expiry at all.
	attempt.setCode("qr-again", 0)
	require.WithinDuration(t, time.Now().Add(20*time.Second), attempt.snapshot().ExpiresAt, 2*time.Second)
}

// TestThePhoneCodeReplacesTheQr: a person who cannot scan gets a code to type, and
// it is what the screen shows instead of the picture.
func TestThePhoneCodeReplacesTheQr(t *testing.T) {
	attempt := &linkAttempt{id: "link-1"}
	attempt.setCode("qr-payload", 20*time.Second)
	attempt.setPairCode("ABCD1234")

	snapshot := attempt.snapshot()
	require.Equal(t, whatsappdomain.LinkCode, snapshot.State)
	require.Equal(t, "ABCD1234", snapshot.PairCode)
	require.NotZero(t, snapshot.ExpiresAt)
}

// TestANumberWhatsappDoesNotKnowIsNamed is the third defect a browser found: pairing
// asks WhatsApp for the number's identity first, and a number nobody registered comes
// back as a bad request. Reported as an internal failure it tells a person to try again
// when what they need to do is check the number they typed.
func TestANumberWhatsappDoesNotKnowIsNamed(t *testing.T) {
	// The classification is what matters, so it is stated where it is made: a phone
	// number the service does not know is a 404 with the number in it, and the
	// screen reads that as "periksa nomornya".
	// A bad request, wrapped the way the library wraps it: this is the shape that
	// reached the gateway log, and matching only the sentinel missed it.
	wrapped := fmt.Errorf("whatsapp: pair by phone: %w", &whatsmeow.IQError{Code: 400, Text: "bad-request"})
	err := linkFailure(wrapped, "628123456789")

	require.ErrorIs(t, err, whatsappdomain.ErrNotFound)
	require.Contains(t, err.Error(), "tidak mengenal")
	require.Contains(t, err.Error(), "6789", "the refusal names the number, masked")

	// An unreachable service is a different answer: nothing about the request was
	// wrong, so a person should be told to try again rather than to check anything.
	unavailable := linkFailure(&whatsmeow.IQError{Code: 503, Text: "service-unavailable"}, "628123456789")
	require.ErrorIs(t, unavailable, whatsappdomain.ErrUnavailable)

	// A throttle is its own answer: nothing is broken and nothing about the number is
	// wrong, so a person should be told to wait.
	throttled := linkFailure(&whatsmeow.IQError{Code: 429, Text: "rate-overlimit"}, "628123456789")
	require.ErrorIs(t, throttled, whatsappdomain.ErrUnavailable)
	require.Contains(t, throttled.Error(), "membatasi")
	require.Contains(t, throttled.Error(), "tunggu")

	// Anything else keeps its own words, because it is a protocol problem rather
	// than something the product can classify.
	other := linkFailure(errors.New("something new"), "628123456789")
	require.NotErrorIs(t, other, whatsappdomain.ErrNotFound)
	require.NotErrorIs(t, other, whatsappdomain.ErrUnavailable)
}

// TestLinkRefusesWhatItCannotCarryOut: each guard is a different thing a person
// would do next — sign in, ask an operator, or open the number that is already
// served — and none of them can be reached by starting a pairing first.
func TestLinkRefusesWhatItCannotCarryOut(t *testing.T) {
	h := newEngineHarness(t)
	scope := whatsappdomain.Scope{WorkspaceID: h.workspace}

	_, err := h.engine.Link(t.Context(), whatsappdomain.Scope{}, h.numberID, "628123456789")
	require.ErrorIs(t, err, whatsappdomain.ErrInvalidInput, "a link without a workspace has nowhere to belong")

	// A deployment wired without a link sink cannot record a pairing, so it must
	// refuse rather than pair a number nothing would hear about.
	unwired := NewEngine(DefaultEngineConfig(), EngineDeps{}, nil)
	_, err = unwired.Link(t.Context(), scope, h.numberID, "628123456789")
	require.ErrorIs(t, err, whatsappdomain.ErrNotConfigured)

	// A number this instance is already serving must not be re-linked underneath
	// its running client: two devices would compete for one session.
	runtime := servedRuntime(t, h, whatsappdomain.Assignment{NumberID: h.numberID, WorkspaceID: h.workspace})
	h.engine.numbers[runtime.numberID] = runtime

	_, err = h.engine.Link(t.Context(), scope, h.numberID, "628123456789")
	require.ErrorIs(t, err, whatsappdomain.ErrAlreadyLinked)
}

// TestAnAttemptThatIsGoneReadsAsExpired: a screen polls this until it stops asking,
// and answering "expired" is what offers a new code instead of an error nobody can
// act on.
func TestAnAttemptThatIsGoneReadsAsExpired(t *testing.T) {
	h := newEngineHarness(t)

	link, err := h.engine.LinkState(t.Context(), "link-nobody-knows")
	require.NoError(t, err)
	require.Equal(t, whatsappdomain.LinkExpired, link.State)
	require.Contains(t, link.Reason, "kedaluwarsa")

	// An attempt that is still there answers with its own state, including the code
	// a screen is showing.
	attempt := pairingAttempt(t, h, "628123456789")
	attempt.setCode("3@payload", 30*time.Second)
	h.engine.links[attempt.id] = attempt

	link, err = h.engine.LinkState(t.Context(), attempt.id)
	require.NoError(t, err)
	require.Equal(t, whatsappdomain.LinkCode, link.State)
	require.Equal(t, "3@payload", link.Code)
	require.Equal(t, h.numberID, link.NumberID)
}

// TestCancellingAnAttemptNobodyIsWatchingIsFine: a screen that closed twice, or a
// person who gave up after the attempt ended on its own, both arrive here, and the
// answer is the same as for the attempt it was meant to stop.
func TestCancellingAnAttemptNobodyIsWatchingIsFine(t *testing.T) {
	h := newEngineHarness(t)
	require.NoError(t, h.engine.Cancel(t.Context(), "link-nobody-knows"))

	attempt := pairingAttempt(t, h, "628123456789")
	h.engine.links[attempt.id] = attempt

	require.NoError(t, h.engine.Cancel(t.Context(), attempt.id))
	require.Nil(t, h.engine.linkFor(attempt.id), "the attempt is dropped")
	require.Error(t, attempt.ctx.Err(), "and its lifetime is over")
}

// TestAPairCodeNeedsAnAttemptAndANumber is the guard in front of the one call that
// asks WhatsApp to send a person a code: a number that cannot be one is refused
// before the service is asked at all.
func TestAPairCodeNeedsAnAttemptAndANumber(t *testing.T) {
	h := newEngineHarness(t)

	_, err := h.engine.PairCode(t.Context(), "link-nobody-knows", "628123456789")
	require.ErrorIs(t, err, whatsappdomain.ErrLinkExpired)

	attempt := pairingAttempt(t, h, "628123456789")
	h.engine.links[attempt.id] = attempt

	// Not a number: too short to be one, so nothing is asked of WhatsApp.
	_, err = h.engine.PairCode(t.Context(), attempt.id, "12345")
	require.ErrorIs(t, err, whatsappdomain.ErrInvalidInput)
}

// TestSendingFromANumberThisInstanceDoesNotServeIsRefused: the fleet routes a
// message to whichever instance holds the number, so this is the answer when a
// message arrives at the wrong one.
func TestSendingFromANumberThisInstanceDoesNotServeIsRefused(t *testing.T) {
	h := newEngineHarness(t)
	snapshot := whatsappdomain.Snapshot{WorkspaceID: h.workspace, NumberID: h.numberID}

	_, err := h.engine.Send(t.Context(), snapshot, whatsappdomain.ToSend{ToJID: "628999@s.whatsapp.net", Kind: whatsappdomain.KindText, Text: "halo"})
	require.ErrorIs(t, err, whatsappdomain.ErrNotConnected)

	// A number that is served but offline is refused as well: the message must wait
	// in the outbox rather than fail, and the module decides that from this answer.
	runtime := servedRuntime(t, h, whatsappdomain.Assignment{NumberID: h.numberID, WorkspaceID: h.workspace})
	h.engine.numbers[runtime.numberID] = runtime

	_, err = h.engine.Send(t.Context(), snapshot, whatsappdomain.ToSend{ToJID: "628999@s.whatsapp.net", Kind: whatsappdomain.KindText, Text: "halo"})
	require.ErrorIs(t, err, whatsappdomain.ErrNotConnected)
	require.Contains(t, err.Error(), "offline")

	err = h.engine.Typing(t.Context(), snapshot, "628999@s.whatsapp.net")
	require.ErrorIs(t, err, whatsappdomain.ErrNotConnected)

	online, err := h.engine.Online(t.Context(), h.numberID)
	require.NoError(t, err)
	require.False(t, online, "a client that is not connected is not online, whatever was reported before")

	// A number this instance does not hold is not online either, and that is not an
	// error: it is the fleet's routing question.
	online, err = h.engine.Online(t.Context(), uuid.New())
	require.NoError(t, err)
	require.False(t, online)
}

// TestUnlinkForgivesASessionThatIsAlreadyGone: a number unlinked twice, or one whose
// session was already dropped, is a goal already met rather than a failure a person
// has to act on.
func TestUnlinkForgivesASessionThatIsAlreadyGone(t *testing.T) {
	h := newEngineHarness(t)
	snapshot := whatsappdomain.Snapshot{WorkspaceID: h.workspace, NumberID: h.numberID}

	h.sessions.EXPECT().ForgetSession(mock.Anything, mock.Anything, h.numberID).
		Return(whatsappdomain.ErrNotFound).Once()

	require.NoError(t, h.engine.Unlink(t.Context(), snapshot))

	// A served number is given up in the same pass, and the session is forgotten
	// afterwards. The lease is released so another instance may serve the number the
	// moment it is linked again.
	runtime := servedRuntime(t, h, whatsappdomain.Assignment{NumberID: h.numberID, WorkspaceID: h.workspace})
	h.engine.numbers[runtime.numberID] = runtime
	h.lease.EXPECT().Release(mock.Anything, h.numberID, "test-instance").Return(nil).Once()
	h.status.EXPECT().Report(mock.Anything, mock.Anything, mock.MatchedBy(func(request whatsappdomain.ReportRequest) bool {
		return request.NumberID == h.numberID && request.Status == whatsappdomain.StatusDisconnected
	})).Return(whatsappdomain.Number{}, nil).Once()
	h.sessions.EXPECT().ForgetSession(mock.Anything, mock.Anything, h.numberID).Return(nil).Once()

	require.NoError(t, h.engine.Unlink(t.Context(), snapshot))
	require.False(t, h.engine.running(h.numberID), "the number is no longer served here")
}

// TestTheTypedNumberIsReducedToDigits: people type `+62 812-3456-789`, and WhatsApp
// wants the digits. A number that keeps its grouping is a number the service refuses.
func TestTheTypedNumberIsReducedToDigits(t *testing.T) {
	require.Equal(t, "628123456789", normalizePhone("+62 812-3456-789"))
	require.Equal(t, "628123456789", normalizePhone("628123456789"))
	require.Equal(t, "62", normalizePhone("+62 ()"), "only the digits survive the grouping a person types")
}

// TestThePhoneNumberIsReadOutOfAJid is what the workspace is told the number is.
//
// The device suffix is dropped, and a LID-addressed account has no number at all:
// reporting the LID as a phone number would show — and try to send to — a number that
// is not anybody's.
func TestThePhoneNumberIsReadOutOfAJid(t *testing.T) {
	require.Equal(t, "628123456789", phoneFromJID(types.NewJID("628123456789", types.DefaultUserServer)))
	require.Equal(t, "628123456789", phoneFromJID(types.JID{
		User: "628123456789:12", Server: types.DefaultUserServer, Device: 12,
	}))
	require.Empty(t, phoneFromJID(types.NewJID("999888777", types.HiddenUserServer)),
		"a LID is a number too, which is why the server is what tells them apart")
	require.Empty(t, phoneFromJID(types.NewJID("62812345a", types.DefaultUserServer)))
}

// TestAnAttachmentKeepsItsName: the filename is the last segment of the stored key,
// and WhatsApp shows it to the person who receives the file. A key with nothing after
// the slash still has to produce a name rather than an empty attachment.
func TestAnAttachmentKeepsItsName(t *testing.T) {
	require.Equal(t, "nota.pdf", filenameFromKey("workspace/2026/nota.pdf"))
	require.Equal(t, "nota.pdf", filenameFromKey("nota.pdf"))
	require.Equal(t, "lampiran", filenameFromKey("workspace/2026/"))
}

// servedRuntime builds the runtime this instance would hold for a number, without
// dialling anything: the client exists and is not connected, which is the state a
// number sits in while a takeover or a reconnect is in flight.
func servedRuntime(t *testing.T, h *engineHarness, assignment whatsappdomain.Assignment) *numberRuntime {
	t.Helper()

	device, inner := newDevice(assignment.NumberID, assignment.WorkspaceID, waLog.Noop, nil)

	runtime := h.engine.newRuntime(assignment, "628123456789", device, inner)
	t.Cleanup(runtime.cancel)

	return runtime
}
