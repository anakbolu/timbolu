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

	integrationdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/integration/domain"
)

// minKeySize is the shortest signing key accepted. A short key is a key somebody
// can guess, and a guessable state signature means a forged callback.
const minKeySize = 32

// Codec signs and verifies state values with an HMAC.
type Codec struct {
	key []byte
}

// New builds a codec from a configured key.
//
// An absent key is not an error: a deployment that has not registered an OAuth
// application simply cannot start a connection flow, and the screen says so rather
// than the process refusing to start. A key that is present and too short is a
// mistake, and is reported as one.
func New(key string) (*Codec, error) {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return nil, nil
	}
	if len(trimmed) < minKeySize {
		return nil, fmt.Errorf("oauthstate: the signing key must be at least %d characters", minKeySize)
	}
	return &Codec{key: []byte(trimmed)}, nil
}

// payload is what actually travels, base64url encoded and signed.
type payload struct {
	// Purpose says what this signature is for. Both values are signed with the
	// same key, and one that does not say what it is for can be spent as the
	// other.
	Purpose     string `json:"p"`
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
		Purpose:     statePurpose,
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
	if decoded.Purpose != statePurpose {
		return integrationdomain.OAuthState{}, fmt.Errorf("%w: it was not signed as an authorization state", integrationdomain.ErrInvalidState)
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

// refPayload is what a webhook URL carries.
type refPayload struct {
	WorkspaceID   string `json:"w"`
	IntegrationID string `json:"i"`
	App           string `json:"a"`
	// Purpose keeps a webhook reference from being usable as an OAuth state or
	// the other way round: both are signed with the same key, and a signature
	// that does not say what it is for is a signature that can be spent twice.
	Purpose string `json:"p"`
}

const (
	refPurpose   = "webhook"
	statePurpose = "oauth-state"
)

// SignRef renders a webhook reference as the value that travels in a URL.
func (c *Codec) SignRef(ref integrationdomain.WebhookRef) (string, error) {
	if c == nil {
		return "", errors.New("oauthstate: codec is not configured")
	}

	document, err := json.Marshal(refPayload{
		WorkspaceID:   ref.WorkspaceID.String(),
		IntegrationID: ref.IntegrationID.String(),
		App:           ref.App,
		Purpose:       refPurpose,
	})
	if err != nil {
		return "", fmt.Errorf("oauthstate: encode reference: %w", err)
	}

	encoded := base64.RawURLEncoding.EncodeToString(document)
	return encoded + "." + c.signature(encoded), nil
}

// VerifyRef reads a webhook reference back, refusing anything this codec did not
// sign or that was signed for another purpose.
func (c *Codec) VerifyRef(value string) (integrationdomain.WebhookRef, error) {
	if c == nil {
		return integrationdomain.WebhookRef{}, errors.New("oauthstate: codec is not configured")
	}

	encoded, signature, found := strings.Cut(value, ".")
	if !found || encoded == "" || signature == "" {
		return integrationdomain.WebhookRef{}, integrationdomain.ErrSignatureInvalid
	}
	if !hmac.Equal([]byte(c.signature(encoded)), []byte(signature)) {
		return integrationdomain.WebhookRef{}, fmt.Errorf("%w: the signature does not match", integrationdomain.ErrSignatureInvalid)
	}

	document, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return integrationdomain.WebhookRef{}, fmt.Errorf("%w: it is not readable", integrationdomain.ErrSignatureInvalid)
	}

	var decoded refPayload
	if err := json.Unmarshal(document, &decoded); err != nil {
		return integrationdomain.WebhookRef{}, fmt.Errorf("%w: it is not readable", integrationdomain.ErrSignatureInvalid)
	}
	if decoded.Purpose != refPurpose {
		return integrationdomain.WebhookRef{}, fmt.Errorf("%w: it was not signed as a webhook reference", integrationdomain.ErrSignatureInvalid)
	}

	workspaceID, err := uuid.Parse(decoded.WorkspaceID)
	if err != nil {
		return integrationdomain.WebhookRef{}, fmt.Errorf("%w: it names no workspace", integrationdomain.ErrSignatureInvalid)
	}
	integrationID, err := uuid.Parse(decoded.IntegrationID)
	if err != nil {
		return integrationdomain.WebhookRef{}, fmt.Errorf("%w: it names no connection", integrationdomain.ErrSignatureInvalid)
	}
	if decoded.App == "" {
		return integrationdomain.WebhookRef{}, fmt.Errorf("%w: it names no app", integrationdomain.ErrSignatureInvalid)
	}

	return integrationdomain.WebhookRef{
		WorkspaceID:   workspaceID,
		IntegrationID: integrationID,
		App:           decoded.App,
	}, nil
}

// compile-time checks that the codec satisfies the ports it is wired to.
var (
	_ integrationdomain.StateCodec = (*Codec)(nil)
	_ integrationdomain.RefCodec   = (*Codec)(nil)
)
