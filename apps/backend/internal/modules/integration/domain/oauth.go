package domain

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// This file is the OAuth half of the adapter contract. The flow itself belongs
// to the integration module — issuing and consuming a single-use state, storing
// what the provider returned, and marking a connection that stopped working —
// while the provider's endpoints belong to the adapter that speaks it.

// Errors the flow reports.
var (
	// ErrInvalidState means the state the provider sent back is not one this
	// deployment issued, or was already spent, or has expired.
	ErrInvalidState = errors.New("integration: the authorization state is not valid")
	// ErrExchangeFailed means the provider refused the code. A code that was
	// already used, or that expired while the person was deciding, lands here.
	ErrExchangeFailed = errors.New("integration: the provider refused the authorization code")
	// ErrNoOAuth means an application that is not connected through OAuth was
	// asked to start an authorization flow.
	ErrNoOAuth = errors.New("integration: this application is not connected through OAuth")
	// ErrRefreshFailed means the provider refused the refresh token, which is
	// what marks a connection as needing a fresh login.
	ErrRefreshFailed = errors.New("integration: the provider refused the refresh token")
)

// OAuthTokens is what a provider returns from an exchange or a refresh.
type OAuthTokens struct {
	AccessToken  string
	RefreshToken string
	// ExpiresAt is when the access token stops working. A provider that does not
	// state one leaves this nil, and the token is used until it is refused.
	ExpiresAt *time.Time
	Scopes    []string
}

// OAuthAdapter is an adapter whose application is connected through OAuth.
//
// The split is deliberate: this half knows the provider's authorization
// endpoint, its token endpoint, and the shape of its answer. It knows nothing
// about workspaces, states, or storage.
type OAuthAdapter interface {
	Adapter
	// AuthCodeURL is the provider's consent URL for a state and a redirect.
	AuthCodeURL(state, redirectURI string) string
	// Exchange trades a code for tokens.
	Exchange(ctx context.Context, code, redirectURI string) (OAuthTokens, error)
	// Refresh trades a refresh token for a new access token. A provider that
	// refuses returns an error wrapping ErrRefreshFailed.
	Refresh(ctx context.Context, refreshToken string) (OAuthTokens, error)
	// DefaultScopes is the narrowest scope set that makes the adapter's read
	// tools work. It is what a deployment asks for unless it says otherwise,
	// because asking for more than the product uses is how a connection becomes
	// frightening to grant.
	DefaultScopes() []string
}

// OAuthState is the payload a state value carries.
//
// It is signed rather than looked up, so the tenant is known before any query
// runs and the single-use row can be consumed inside the ordinary tenant scope.
type OAuthState struct {
	WorkspaceID uuid.UUID
	UserID      uuid.UUID
	App         string
	Nonce       string
	ReturnTo    string
	ExpiresAt   time.Time
}

// Expired reports whether the state is past its life.
func (s OAuthState) Expired(now time.Time) bool { return !s.ExpiresAt.After(now) }

// StateCodec signs and verifies a state payload.
//
// Signing lives behind a port because it is a keyed operation: the container
// holds the key, and the domain layer holds none.
type StateCodec interface {
	// Sign renders a payload as the value handed to the provider.
	Sign(state OAuthState) (string, error)
	// Verify reads a value back, reporting ErrInvalidState for anything that was
	// not signed here, or that has expired.
	Verify(value string) (OAuthState, error)
}

// StateStore remembers which nonces are still unspent.
//
// It is the narrow view of the repository that the flow needs, declared so the
// service depends on two operations rather than on everything the module stores.
type StateStore interface {
	// SaveState records a state as usable until it expires.
	SaveState(ctx context.Context, state OAuthState) error
	// ConsumeState spends a nonce, returning ErrInvalidState when it was already
	// spent, never existed, or has expired. It must be atomic: two callbacks
	// arriving together cannot both succeed.
	ConsumeState(ctx context.Context, workspaceID uuid.UUID, nonce string, now time.Time) error
}

// AuthURL is where a connection flow starts.
type AuthURL struct {
	// URL is the provider's consent page.
	URL string
	// State is the value the provider will return, kept so a client can check
	// that the callback it receives belongs to the flow it started.
	State string
}

// CompleteAuthRequest is a provider's callback.
type CompleteAuthRequest struct {
	Code  string
	State string
	// RedirectURI is the same value the authorization request used, because a
	// provider requires them to match.
	RedirectURI string
}

// OAuthRedirectURI is the path this deployment registers with providers. It is
// one path for every application: the app travels inside the signed state, so a
// new integration does not need a new callback URL registered.
const OAuthRedirectURI = "/api/integrations/oauth/callback"

// TriggerEvent is one thing an application reported.
//
// It is what a webhook or a poll turns into, and what a routine is started by.
// The payload is the provider's own body, kept whole so a routine reads the
// field it needs rather than the subset somebody guessed at.
type TriggerEvent struct {
	App           string
	Event         string
	IntegrationID uuid.UUID
	// EventID is the provider's own id for this delivery. It is what makes a
	// redelivery harmless.
	EventID string
	Payload json.RawMessage
}
