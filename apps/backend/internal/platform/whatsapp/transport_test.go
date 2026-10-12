package whatsapp

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow"

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
