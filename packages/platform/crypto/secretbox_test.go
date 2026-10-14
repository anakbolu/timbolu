package crypto

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// testKey is 32 bytes so it is a valid AES-256 key.
const testKey = "0123456789abcdef0123456789abcdef"

func newBox(t *testing.T) *Box {
	t.Helper()

	box, err := NewFromString(testKey)
	require.NoError(t, err)
	return box
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	box := newBox(t)

	for _, plaintext := range []string{
		"sk-proj-abc123",
		"",
		"kunci dengan spasi dan émoji 🔑",
		strings.Repeat("x", 4096),
	} {
		sealed, err := box.Encrypt(plaintext)
		require.NoError(t, err)

		opened, err := box.Decrypt(sealed)
		require.NoError(t, err)
		require.Equal(t, plaintext, opened)
	}
}

// TestCiphertextDoesNotContainTheSecret is the reason the column is encrypted at
// all: a database dump must not leak the key.
func TestCiphertextDoesNotContainTheSecret(t *testing.T) {
	box := newBox(t)
	secret := "sk-live-must-not-appear"

	sealed, err := box.Encrypt(secret)
	require.NoError(t, err)
	require.NotContains(t, string(sealed), secret)
	require.NotContains(t, hex.EncodeToString(sealed), hex.EncodeToString([]byte(secret)))
}

// TestNonceIsFresh guards the property that makes GCM safe under one key: two
// encryptions of the same secret must not share a nonce.
func TestNonceIsFresh(t *testing.T) {
	box := newBox(t)

	first, err := box.Encrypt("same")
	require.NoError(t, err)
	second, err := box.Encrypt("same")
	require.NoError(t, err)

	require.NotEqual(t, first, second)
	require.NotEqual(t, first[:12], second[:12], "the nonce must be random per call")
}

func TestDecryptRejectsAnotherKey(t *testing.T) {
	sealed, err := newBox(t).Encrypt("sk-secret")
	require.NoError(t, err)

	other, err := NewFromString("ffffffffffffffffffffffffffffffff")
	require.NoError(t, err)

	opened, err := other.Decrypt(sealed)
	require.ErrorIs(t, err, ErrInvalidCiphertext)
	require.Empty(t, opened)
	require.Contains(t, err.Error(), other.Fingerprint(), "the failure must name the key in use")
}

func TestDecryptRejectsTamperedCiphertext(t *testing.T) {
	box := newBox(t)
	sealed, err := box.Encrypt("sk-secret")
	require.NoError(t, err)

	tampered := make([]byte, len(sealed))
	copy(tampered, sealed)
	tampered[len(tampered)-1] ^= 0x01

	_, err = box.Decrypt(tampered)
	require.ErrorIs(t, err, ErrInvalidCiphertext)
}

func TestDecryptRejectsShortAndEmptyValues(t *testing.T) {
	box := newBox(t)

	for name, value := range map[string][]byte{
		"nil":      nil,
		"empty":    {},
		"nonce":    make([]byte, 12),
		"tooShort": make([]byte, 15),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := box.Decrypt(value)
			require.ErrorIs(t, err, ErrInvalidCiphertext)
		})
	}
}

func TestNilBoxIsExplicit(t *testing.T) {
	var box *Box

	_, err := box.Encrypt("sk-secret")
	require.ErrorIs(t, err, ErrNoKey)

	_, err = box.Decrypt([]byte("whatever"))
	require.ErrorIs(t, err, ErrNoKey)

	require.Empty(t, box.Fingerprint())
}

func TestParseKeyAcceptsTheDocumentedForms(t *testing.T) {
	raw := []byte(testKey)

	cases := map[string]string{
		"raw":       testKey,
		"rawPadded": "  " + testKey + "\n",
		"base64":    base64.StdEncoding.EncodeToString(raw),
		"base64Raw": base64.RawStdEncoding.EncodeToString(raw),
		"base64URL": base64.URLEncoding.EncodeToString(raw),
		"hex":       hex.EncodeToString(raw),
		"hexUpper":  strings.ToUpper(hex.EncodeToString(raw)),
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			key, err := ParseKey(encoded)
			require.NoError(t, err)
			require.Equal(t, raw, key)
		})
	}
}

func TestParseKeyRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"empty":      "",
		"blank":      "   ",
		"tooShort":   "short",
		"tooLong":    strings.Repeat("a", 33),
		"base64Wron": base64.StdEncoding.EncodeToString([]byte("only-16-bytes!!!")),
	}

	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseKey(encoded)
			require.Error(t, err)
			require.True(t, errors.Is(err, ErrNoKey) || errors.Is(err, ErrInvalidKey))
		})
	}
}

func TestNewRequiresExactlyThirtyTwoBytes(t *testing.T) {
	for _, size := range []int{0, 16, 31, 33, 64} {
		_, err := New(make([]byte, size))
		require.ErrorIs(t, err, ErrInvalidKey, "size %d must be rejected", size)
	}

	box, err := New(make([]byte, 32))
	require.NoError(t, err)
	require.Len(t, box.Fingerprint(), 8)
}

// TestFingerprintIsStableAcrossInstances is what makes it usable in a log: the
// same key must always report the same short id.
func TestFingerprintIsStableAcrossInstances(t *testing.T) {
	first := newBox(t)
	second := newBox(t)

	require.Equal(t, first.Fingerprint(), second.Fingerprint())
	require.NotContains(t, first.Fingerprint(), testKey)
}
