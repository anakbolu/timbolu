package oauthstate

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
)

// signingKey is long enough to be accepted, which is the point of the minimum.
const signingKey = "an-integration-state-signing-key-32-chars"

func newCodec(t *testing.T) *Codec {
	t.Helper()

	codec, err := New(signingKey)
	require.NoError(t, err)
	return codec
}

func sampleState() integrationdomain.OAuthState {
	return integrationdomain.OAuthState{
		WorkspaceID: uuid.New(),
		UserID:      uuid.New(),
		App:         "gmail",
		Nonce:       "a-random-nonce",
		ReturnTo:    "/dashboard/integrations",
		ExpiresAt:   time.Now().Add(10 * time.Minute).Truncate(time.Second),
	}
}

// TestASignedStateReadsBack is the round trip the callback depends on.
func TestASignedStateReadsBack(t *testing.T) {
	codec := newCodec(t)
	state := sampleState()

	value, err := codec.Sign(state)
	require.NoError(t, err)
	require.NotEmpty(t, value)

	// The payload is readable base64, which is fine: it is not a secret. What
	// matters is that it cannot be changed.
	require.Contains(t, value, ".")

	read, err := codec.Verify(value)
	require.NoError(t, err)
	require.Equal(t, state.WorkspaceID, read.WorkspaceID)
	require.Equal(t, state.UserID, read.UserID)
	require.Equal(t, state.App, read.App)
	require.Equal(t, state.Nonce, read.Nonce)
	require.Equal(t, state.ReturnTo, read.ReturnTo)
	require.True(t, state.ExpiresAt.Equal(read.ExpiresAt))
}

// TestAForgedStateIsRefused is the anti-CSRF property: the value travels through
// a browser, so anybody can hand one back.
func TestAForgedStateIsRefused(t *testing.T) {
	codec := newCodec(t)
	state := sampleState()

	value, err := codec.Sign(state)
	require.NoError(t, err)

	// A different key must not verify, which is what stops a state minted by
	// another deployment from being accepted here.
	other, err := New("a-completely-different-signing-key-32ch")
	require.NoError(t, err)
	_, err = other.Verify(value)
	require.ErrorIs(t, err, integrationdomain.ErrInvalidState)

	// Swapping the payload for another one must not verify either.
	swapped, err := codec.Sign(integrationdomain.OAuthState{
		WorkspaceID: uuid.New(),
		UserID:      state.UserID,
		App:         "gmail",
		Nonce:       "somebody-elses-nonce",
		ExpiresAt:   state.ExpiresAt,
	})
	require.NoError(t, err)
	forged := swapped[:len(swapped)-len(swapped)/3] + value[len(value)-len(value)/3:]
	_, err = codec.Verify(forged)
	require.ErrorIs(t, err, integrationdomain.ErrInvalidState)

	// A value with no signature, an empty value, and a signature that is not
	// base64 all fail the same way rather than panicking.
	for _, broken := range []string{"", "no-dot", ".", "payload.", ".signature", "payload.not base64!"} {
		_, err := codec.Verify(broken)
		require.ErrorIs(t, err, integrationdomain.ErrInvalidState, broken)
	}
}

// TestAnUnsignedPayloadIsRefusedWithoutParsingIt keeps a forged value from
// reaching the decoder at all.
func TestAnUnsignedPayloadIsRefusedWithoutParsingIt(t *testing.T) {
	codec := newCodec(t)

	// A payload that is valid base64 and valid JSON, but was never signed.
	// Signing it with a different key is the same situation as a forgery.
	other, err := New("yet-another-signing-key-of-32-characters")
	require.NoError(t, err)
	value, err := other.Sign(sampleState())
	require.NoError(t, err)

	_, err = codec.Verify(value)
	require.ErrorIs(t, err, integrationdomain.ErrInvalidState)
}

// TestAShortKeyIsRefused is why the minimum exists: a short key is a key
// somebody can guess, and a guessable signature is a forged callback.
func TestAShortKeyIsRefused(t *testing.T) {
	_, err := New("too-short")
	require.ErrorContains(t, err, "at least 32 characters")

	_, err = New("")
	require.Error(t, err)

	// And a codec that was never built refuses rather than panics.
	var absent *Codec
	_, err = absent.Sign(sampleState())
	require.Error(t, err)
	_, err = absent.Verify("anything")
	require.Error(t, err)
}

// TestExpiryTravelsWithTheState keeps the check in the callback honest: the codec
// reports the moment rather than deciding, because the decision belongs where the
// clock is.
func TestExpiryTravelsWithTheState(t *testing.T) {
	codec := newCodec(t)

	state := sampleState()
	state.ExpiresAt = time.Now().Add(-time.Minute).Truncate(time.Second)

	value, err := codec.Sign(state)
	require.NoError(t, err)

	read, err := codec.Verify(value)
	require.NoError(t, err, "the codec reports what was signed, it does not judge it")
	require.True(t, read.Expired(time.Now()))
}
