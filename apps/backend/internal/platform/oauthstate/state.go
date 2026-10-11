// Package oauthstate signs the value an integration's authorization flow carries
// between the app and the provider.
//
// It is a signed payload rather than a lookup, so the process that receives the
// callback learns which workspace and which person the flow belongs to without a
// session and without a query. The signature is what makes that trustworthy: the
// value travels through a browser, so anybody can hand one back.
//
// Being signed is not the same as being single-use, and this package does not
// pretend otherwise: replay within the value's life is refused by the state store
// the integration module consumes from.
package oauthstate

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
)

// minKeySize is the shortest signing key accepted. A short key is a key somebody
// can guess, and a guessable state signature means a forged callback.
const minKeySize = 32

// Codec signs and verifies state values with an HMAC.
type Codec struct {
	key []byte
}

// New builds a codec from a configured key.
func New(key string) (*Codec, error) {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return nil, errors.New("oauthstate: a signing key is required")
	}
	if len(trimmed) < minKeySize {
		return nil, fmt.Errorf("oauthstate: the signing key must be at least %d characters", minKeySize)
	}
	return &Codec{key: []byte(trimmed)}, nil
}

// payload is what actually travels, base64url encoded and signed.
type payload struct {
	WorkspaceID string `json:"w"`
	UserID      string `json:"u"`
	App         string `json:"a"`
	Nonce       string `json:"n"`
	ReturnTo    string `json:"r,omitempty"`
	ExpiresAt   int64  `json:"e"`
}

// Sign renders a state value: the payload, then its signature.
func (c *Codec) Sign(state integrationdomain.OAuthState) (string, error) {
	if c == nil {
		return "", errors.New("oauthstate: codec is not configured")
	}

	document, err := json.Marshal(payload{
		WorkspaceID: state.WorkspaceID.String(),
		UserID:      state.UserID.String(),
		App:         state.App,
		Nonce:       state.Nonce,
		ReturnTo:    state.ReturnTo,
		ExpiresAt:   state.ExpiresAt.Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("oauthstate: encode state: %w", err)
	}

	encoded := base64.RawURLEncoding.EncodeToString(document)
	return encoded + "." + c.signature(encoded), nil
}

// Verify reads a value back, refusing anything this codec did not sign.
func (c *Codec) Verify(value string) (integrationdomain.OAuthState, error) {
	if c == nil {
		return integrationdomain.OAuthState{}, errors.New("oauthstate: codec is not configured")
	}

	encoded, signature, found := strings.Cut(value, ".")
	if !found || encoded == "" || signature == "" {
		return integrationdomain.OAuthState{}, integrationdomain.ErrInvalidState
	}

	// The signature is checked before the payload is parsed, so a forged value is
	// refused without the decoder ever seeing attacker-controlled structure.
	if !hmac.Equal([]byte(c.signature(encoded)), []byte(signature)) {
		return integrationdomain.OAuthState{}, fmt.Errorf("%w: the signature does not match", integrationdomain.ErrInvalidState)
	}

	document, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return integrationdomain.OAuthState{}, fmt.Errorf("%w: it is not readable", integrationdomain.ErrInvalidState)
	}

	var decoded payload
	if err := json.Unmarshal(document, &decoded); err != nil {
		return integrationdomain.OAuthState{}, fmt.Errorf("%w: it is not readable", integrationdomain.ErrInvalidState)
	}

	workspaceID, err := uuid.Parse(decoded.WorkspaceID)
	if err != nil {
		return integrationdomain.OAuthState{}, fmt.Errorf("%w: it names no workspace", integrationdomain.ErrInvalidState)
	}
	userID, err := uuid.Parse(decoded.UserID)
	if err != nil {
		return integrationdomain.OAuthState{}, fmt.Errorf("%w: it names no person", integrationdomain.ErrInvalidState)
	}
	if decoded.App == "" || decoded.Nonce == "" {
		return integrationdomain.OAuthState{}, fmt.Errorf("%w: it is incomplete", integrationdomain.ErrInvalidState)
	}

	return integrationdomain.OAuthState{
		WorkspaceID: workspaceID,
		UserID:      userID,
		App:         decoded.App,
		Nonce:       decoded.Nonce,
		ReturnTo:    decoded.ReturnTo,
		ExpiresAt:   time.Unix(decoded.ExpiresAt, 0).UTC(),
	}, nil
}

// signature is the HMAC over the encoded payload.
func (c *Codec) signature(encoded string) string {
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// compile-time check that the codec satisfies the port it is wired to.
var _ integrationdomain.StateCodec = (*Codec)(nil)
