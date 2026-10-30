package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	whatsappdomain "github.com/anakbolu/timbolu/packages/whatsapp/domain"
)

// The pairing flow is this file: what the engine does with each item WhatsApp
// sends while a person is linking a number, and in which order it writes what
// survives.
//
// It is worth testing away from a live socket because the order is the part that
// cannot be recovered later. A session written after the workspace is told means a
// crash in between leaves a number that claims to be linked with nothing behind it;
// a session written first means the fleet finds it and serves it. The failure cases
// matter for the same reason: each one is a screen that has to say what to do next.

// pairingAttempt builds an attempt over a real device and a client that is never
// connected, which is what the pairing flow actually has: WhatsApp's socket is only
// dialled by Link, and everything here decides what to do with what arrives on it.
func pairingAttempt(t *testing.T, h *engineHarness, phone string) *linkAttempt {
	t.Helper()

	device, inner := newDevice(h.numberID, h.workspace, waLog.Noop, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	return &linkAttempt{
		id:          "link-test",
		numberID:    h.numberID,
		workspaceID: h.workspace,
		phone:       phone,
		device:      device,
		store:       inner,
		client:      whatsmeow.NewClient(device, waLog.Noop),
		ctx:         ctx,
		cancel:      cancel,
		state:       whatsappdomain.LinkWaiting,
	}
}

// identify gives the device the identity a completed pairing carries.
func identify(t *testing.T, attempt *linkAttempt, jid string) {
	t.Helper()

	parsed, err := types.ParseJID(jid)
	require.NoError(t, err)
	attempt.device.ID = &parsed
}

// TestASuccessfulPairingIsStoredBeforeItIsAnnounced is the ordering the epic asks
// for and the one thing here that cannot be fixed up afterwards: a crash between the
// two writes must leave a session the fleet can serve, not a number that claims to be
// linked with nothing behind it.
func TestASuccessfulPairingIsStoredBeforeItIsAnnounced(t *testing.T) {
	h := newEngineHarness(t)
	attempt := pairingAttempt(t, h, "628123456789")
	identify(t, attempt, "628123456789:12@s.whatsapp.net")

	var order []string
	attempt.store.put = func(_ context.Context, _ sealed) error {
		order = append(order, "session")
		return nil
	}
	h.links.EXPECT().Paired(mock.Anything, mock.Anything, mock.MatchedBy(func(link whatsappdomain.PairedLink) bool {
		order = append(order, "workspace")
		return link.NumberID == h.numberID && link.Phone == "628123456789" && link.JID != ""
	})).Return(nil).Once()

	done := h.engine.handleQRItem(whatsappdomain.Scope{WorkspaceID: h.workspace}, attempt, whatsmeow.QRChannelSuccess)

	require.True(t, done, "a success ends the attempt")
	require.Equal(t, []string{"session", "workspace"}, order,
		"the session is stored before the workspace is told")

	snapshot := attempt.snapshot()
	require.Equal(t, whatsappdomain.LinkPaired, snapshot.State)
	require.Equal(t, "628123456789", snapshot.Phone)
	require.Empty(t, snapshot.Code, "a paired attempt stops offering a code")
}

// TestAPairingWithNoIdentityIsRefused: the phone number is the identity, and an
// attempt that ends without one is a number nothing can be addressed to. Recorded as
// paired it would be a number the fleet serves and no message can reach.
func TestAPairingWithNoIdentityIsRefused(t *testing.T) {
	h := newEngineHarness(t)
	attempt := pairingAttempt(t, h, "628123456789")

	done := h.engine.handleQRItem(whatsappdomain.Scope{WorkspaceID: h.workspace}, attempt, whatsmeow.QRChannelSuccess)

	require.True(t, done)
	snapshot := attempt.snapshot()
	require.Equal(t, whatsappdomain.LinkFailed, snapshot.State)
	require.Contains(t, snapshot.Reason, "tanpa memberi identitas")
}

// TestAPairingThatCannotBeStoredIsRefused is why the store is written first: a
// session that cannot be written is a number that would go offline on the next
// restart, and saying so now is a person re-linking while they are still looking at
// the screen.
func TestAPairingThatCannotBeStoredIsRefused(t *testing.T) {
	h := newEngineHarness(t)
	attempt := pairingAttempt(t, h, "628123456789")
	identify(t, attempt, "628123456789:12@s.whatsapp.net")

	attempt.store.put = func(context.Context, sealed) error {
		return errors.New("the session store is down")
	}
	// The workspace must not be told about a number that was not stored, so no
	// expectation is set: a call would fail the mock.

	done := h.engine.handleQRItem(whatsappdomain.Scope{WorkspaceID: h.workspace}, attempt, whatsmeow.QRChannelSuccess)

	require.True(t, done)
	snapshot := attempt.snapshot()
	require.Equal(t, whatsappdomain.LinkFailed, snapshot.State)
	require.Contains(t, snapshot.Reason, "sesi tidak bisa disimpan")
}

// TestAPairingThatCannotBeRecordedIsRefused: the session exists at WhatsApp's end, so
// the attempt cannot pretend nothing happened — but the workspace does not know the
// number either, and a screen that showed "terhubung" would be lying about a number
// nothing can send to.
func TestAPairingThatCannotBeRecordedIsRefused(t *testing.T) {
	h := newEngineHarness(t)
	attempt := pairingAttempt(t, h, "628123456789")
	identify(t, attempt, "628123456789:12@s.whatsapp.net")
	attempt.store.put = func(context.Context, sealed) error { return nil }

	h.links.EXPECT().Paired(mock.Anything, mock.Anything, mock.Anything).
		Return(errors.New("the module is down")).Once()

	done := h.engine.handleQRItem(whatsappdomain.Scope{WorkspaceID: h.workspace}, attempt, whatsmeow.QRChannelSuccess)

	require.True(t, done)
	snapshot := attempt.snapshot()
	require.Equal(t, whatsappdomain.LinkFailed, snapshot.State)
	require.Contains(t, snapshot.Reason, "tidak bisa dicatat")
}

// TestThePhoneNumberFallsBackToWhatWasTyped: a LID-addressed account carries no phone
// number in its JID, and the number the person typed is what the workspace should be
// told about rather than an empty string.
func TestThePhoneNumberFallsBackToWhatWasTyped(t *testing.T) {
	h := newEngineHarness(t)
	attempt := pairingAttempt(t, h, "628123456789")
	// A LID is a number too, which is why the server part is what marks it as not
	// a phone number.
	identify(t, attempt, "999888777:12@lid")
	attempt.store.put = func(context.Context, sealed) error { return nil }

	h.links.EXPECT().Paired(mock.Anything, mock.Anything, mock.MatchedBy(func(link whatsappdomain.PairedLink) bool {
		return link.Phone == "628123456789"
	})).Return(nil).Once()

	require.True(t, h.engine.handleQRItem(whatsappdomain.Scope{WorkspaceID: h.workspace}, attempt, whatsmeow.QRChannelSuccess))
	require.Equal(t, whatsappdomain.LinkPaired, attempt.snapshot().State)
}

// TestEveryEndOfPairingTellsThePersonWhatToDo: each end is a different thing to do
// next — ask for a new code, update the gateway, enable linked devices — and a screen
// with no reason is the dead end the epic forbids.
func TestEveryEndOfPairingTellsThePersonWhatToDo(t *testing.T) {
	cases := map[string]whatsmeow.QRChannelItem{
		"an expired code":              whatsmeow.QRChannelTimeout,
		"a client that is too old":     whatsmeow.QRChannelClientOutdated,
		"a phone with no linked devic": whatsmeow.QRChannelScannedWithoutMultidevice,
		"a refused pairing":            {Event: whatsmeow.QRChannelEventError, Error: errors.New("connection refused")},
		"a refusal with no words":      {Event: whatsmeow.QRChannelEventError},
		"a passkey prompt":             {Event: whatsmeow.QRChannelEventPasskeyRequest},
		"a passkey answer":             {Event: whatsmeow.QRChannelEventPasskeyResponse},
	}

	for name, item := range cases {
		t.Run(name, func(t *testing.T) {
			h := newEngineHarness(t)
			attempt := pairingAttempt(t, h, "628123456789")

			done := h.engine.handleQRItem(whatsappdomain.Scope{WorkspaceID: h.workspace}, attempt, item)

			require.True(t, done, "the attempt is over")
			snapshot := attempt.snapshot()
			require.Equal(t, whatsappdomain.LinkFailed, snapshot.State)
			require.NotEmpty(t, snapshot.Reason, "a person is told why it stopped")
			require.Empty(t, snapshot.Code, "and is not left with a code that cannot work")
		})
	}

	// A code is the one item that does not end the attempt: WhatsApp rotates them by
	// itself, and the rotation is what makes an expired QR a new QR.
	h := newEngineHarness(t)
	attempt := pairingAttempt(t, h, "628123456789")
	done := h.engine.handleQRItem(whatsappdomain.Scope{WorkspaceID: h.workspace}, attempt, whatsmeow.QRChannelItem{
		Event: whatsmeow.QRChannelEventCode, Code: "2@qr-payload", Timeout: 30 * time.Second,
	})
	require.False(t, done)
	require.Equal(t, whatsappdomain.LinkCode, attempt.snapshot().State)
	require.Equal(t, "2@qr-payload", attempt.snapshot().Code)
}

// TestAPairingThatEndsWithoutAVerdictSaysSo: the channel closes without a verdict
// when the emitter runs out of codes or the socket goes away, and an attempt left
// open would be a screen waiting on a socket that is gone.
func TestAPairingThatEndsWithoutAVerdictSaysSo(t *testing.T) {
	h := newEngineHarness(t)
	attempt := pairingAttempt(t, h, "628123456789")
	h.engine.links[attempt.id] = attempt

	closed := make(chan whatsmeow.QRChannelItem)
	close(closed)

	h.engine.watchPairing(whatsappdomain.Scope{WorkspaceID: h.workspace}, attempt, closed)

	snapshot := attempt.snapshot()
	require.Equal(t, whatsappdomain.LinkFailed, snapshot.State)
	require.Contains(t, snapshot.Reason, "coba lagi")
	require.Nil(t, h.engine.linkFor(attempt.id), "a finished attempt is dropped from the register")
}

// TestAPairingThatKeepsShowingCodesStaysOpen: once a code has arrived the deadline is
// WhatsApp's own, delivered with each item, so a closed channel after a code must not
// turn a person's live attempt into a refusal.
func TestAPairingThatKeepsShowingCodesStaysOpen(t *testing.T) {
	h := newEngineHarness(t)
	attempt := pairingAttempt(t, h, "628123456789")
	h.engine.links[attempt.id] = attempt

	items := make(chan whatsmeow.QRChannelItem, 2)
	items <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: "1@first", Timeout: 30 * time.Second}
	items <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: "2@second", Timeout: 30 * time.Second}
	close(items)

	h.engine.watchPairing(whatsappdomain.Scope{WorkspaceID: h.workspace}, attempt, items)

	snapshot := attempt.snapshot()
	require.Equal(t, whatsappdomain.LinkCode, snapshot.State, "the last code stands after the channel closes")
	require.Equal(t, "2@second", snapshot.Code)
	require.Empty(t, snapshot.Reason)
	require.Nil(t, h.engine.linkFor(attempt.id))
}

// TestAnAbandonedAttemptStopsWaiting: a cancelled attempt is one a person stopped
// watching, and the loop has to end with it rather than sit on a socket nobody is
// polling.
func TestAnAbandonedAttemptStopsWaiting(t *testing.T) {
	h := newEngineHarness(t)
	attempt := pairingAttempt(t, h, "628123456789")
	h.engine.links[attempt.id] = attempt

	attempt.cancel()

	// The channel never yields, so the loop can only end through the attempt's own
	// lifetime.
	silent := make(chan whatsmeow.QRChannelItem)
	done := make(chan struct{})
	go func() {
		h.engine.watchPairing(whatsappdomain.Scope{WorkspaceID: h.workspace}, attempt, silent)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("an abandoned attempt kept waiting")
	}

	require.Equal(t, whatsappdomain.LinkWaiting, attempt.snapshot().State, "nothing was decided about it")
}
