package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
)

// StateTTL is how long a person has to finish a provider's consent screen.
//
// It is generous because the step that takes the time is not ours: a person has
// to sign in to the provider, possibly pick an account, and read a scope list.
// Ten minutes is the window most providers use for the same reason.
const StateTTL = 10 * time.Minute

// BeginAuth starts a connection flow.
//
// The state it returns is signed and single-use. Signed so the callback knows
// which workspace and which person it belongs to without a session; single-use
// so a replayed callback cannot store a second credential.
func (s *service) BeginAuth(ctx context.Context, scope domain.Scope, app, redirectURI, returnTo string) (domain.AuthURL, error) {
	trimmed := strings.TrimSpace(app)
	if trimmed == "" {
		return domain.AuthURL{}, fmt.Errorf("%w: an app is required", domain.ErrInvalidInput)
	}
	// A redirect the provider cannot resolve is a flow that sends the browser
	// somewhere that is not this deployment, so it is refused here rather than
	// discovered by a person staring at a provider's error page.
	redirect := strings.TrimSpace(redirectURI)
	if redirect == "" || !strings.Contains(redirect, "://") {
		return domain.AuthURL{}, fmt.Errorf("%w: the redirect URI must be absolute", domain.ErrInvalidInput)
	}
	if scope.WorkspaceID == uuid.Nil || scope.UserID == uuid.Nil {
		return domain.AuthURL{}, fmt.Errorf("%w: a workspace and a person are required", domain.ErrInvalidInput)
	}
	if s.deps.States == nil || s.deps.StateStore == nil {
		return domain.AuthURL{}, fmt.Errorf("%w: no OAuth state codec is configured", domain.ErrInvalidInput)
	}

	adapter, ok := s.oauthAdapter(trimmed)
	if !ok {
		return domain.AuthURL{}, fmt.Errorf("%w: %q", domain.ErrUnknownAdapter, trimmed)
	}

	nonce, err := newNonce()
	if err != nil {
		return domain.AuthURL{}, err
	}

	state := domain.OAuthState{
		WorkspaceID: scope.WorkspaceID,
		UserID:      scope.UserID,
		App:         trimmed,
		Nonce:       nonce,
		ReturnTo:    safeReturnTo(returnTo),
		ExpiresAt:   s.now().Add(StateTTL),
	}

	// Stored before the value is handed out, so a callback that arrives
	// immediately still finds the state it is about to spend.
	if err := s.deps.StateStore.SaveState(ctx, state); err != nil {
		return domain.AuthURL{}, err
	}

	signed, err := s.deps.States.Sign(state)
	if err != nil {
		return domain.AuthURL{}, fmt.Errorf("integration: sign state: %w", err)
	}

	return domain.AuthURL{
		URL:   adapter.AuthCodeURL(signed, redirect),
		State: signed,
	}, nil
}

// CompleteAuth finishes a flow.
//
// The order matters and is the whole security story: verify the signature to
// learn the tenant, spend the nonce so a replay cannot also succeed, and only
// then talk to the provider. A code that the provider refuses has already cost
// the state, which is correct — the flow is over either way.
func (s *service) CompleteAuth(ctx context.Context, req domain.CompleteAuthRequest) (domain.Integration, error) {
	if s.deps.States == nil || s.deps.StateStore == nil {
		return domain.Integration{}, fmt.Errorf("%w: no OAuth state codec is configured", domain.ErrInvalidInput)
	}
	if strings.TrimSpace(req.State) == "" {
		return domain.Integration{}, fmt.Errorf("%w: no state", domain.ErrInvalidState)
	}
	if strings.TrimSpace(req.Code) == "" {
		return domain.Integration{}, fmt.Errorf("%w: no authorization code", domain.ErrInvalidState)
	}

	state, err := s.deps.States.Verify(req.State)
	if err != nil {
		return domain.Integration{}, err
	}
	if state.Expired(s.now()) {
		return domain.Integration{}, fmt.Errorf("%w: it has expired", domain.ErrInvalidState)
	}

	adapter, ok := s.oauthAdapter(state.App)
	if !ok {
		return domain.Integration{}, fmt.Errorf("%w: %q", domain.ErrUnknownAdapter, state.App)
	}

	if err := s.deps.StateStore.ConsumeState(ctx, state.WorkspaceID, state.Nonce, s.now()); err != nil {
		return domain.Integration{}, err
	}

	redirect := req.RedirectURI
	if strings.TrimSpace(redirect) == "" {
		redirect = domain.OAuthRedirectURI
	}

	tokens, err := adapter.Exchange(ctx, req.Code, redirect)
	if err != nil {
		return domain.Integration{}, fmt.Errorf("%w: %w", domain.ErrExchangeFailed, err)
	}
	if strings.TrimSpace(tokens.AccessToken) == "" {
		return domain.Integration{}, fmt.Errorf("%w: the provider returned no access token", domain.ErrExchangeFailed)
	}

	scope := domain.Scope{UserID: state.UserID, WorkspaceID: state.WorkspaceID}

	account, err := s.accountLabel(ctx, adapter, tokens)
	if err != nil {
		return domain.Integration{}, err
	}

	return s.Connect(ctx, scope, domain.ConnectInput{
		App:          state.App,
		AccountLabel: account,
		Scopes:       tokens.Scopes,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		ExpiresAt:    tokens.ExpiresAt,
	})
}

// RefreshConnection trades a stored refresh token for a new access token.
//
// A provider that refuses is not an error the caller has to handle: the
// connection is marked as needing a fresh login, the stored token is left alone,
// and the returned integration says so. That is the difference between a
// connection somebody has to fix and one the product silently broke.
func (s *service) RefreshConnection(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Integration, error) {
	integration, err := s.deps.Repository.Get(ctx, scope, id)
	if err != nil {
		return domain.Integration{}, err
	}

	adapter, ok := s.oauthAdapter(integration.App)
	if !ok {
		return domain.Integration{}, fmt.Errorf("%w: %q", domain.ErrUnknownAdapter, integration.App)
	}

	token, err := s.Token(ctx, scope, id)
	if err != nil {
		return domain.Integration{}, err
	}
	if strings.TrimSpace(token.RefreshToken) == "" {
		// Nothing to trade. A provider that issues no refresh token means the
		// person re-authorises when the access token dies, and saying so is the
		// only honest answer.
		return s.NeedsRelogin(ctx, scope, id, "penyedia tidak memberi refresh token")
	}

	refreshed, err := adapter.Refresh(ctx, token.RefreshToken)
	if err != nil {
		s.deps.Logger.Warn("refreshing an integration failed",
			zap.String("app", integration.App),
			zap.String("account", integration.AccountLabel),
			zap.Error(err),
		)
		return s.NeedsRelogin(ctx, scope, id, refreshReason(err))
	}

	// A provider that rotates the refresh token sends a new one; one that does
	// not sends an empty field, and the stored value stays.
	nextRefresh := refreshed.RefreshToken
	if strings.TrimSpace(nextRefresh) == "" {
		nextRefresh = token.RefreshToken
	}

	return s.Refresh(ctx, scope, id, domain.RefreshInput{
		AccessToken:  refreshed.AccessToken,
		RefreshToken: nextRefresh,
		ExpiresAt:    refreshed.ExpiresAt,
		Scopes:       refreshed.Scopes,
	})
}

// RefreshExpiring refreshes every connection whose token is close to expiring.
//
// It is what a schedule calls. One connection failing does not stop the others:
// each is independent, and the failure is already recorded on the row.
//
// The returned list is every connection it touched, including the ones a provider
// refused. That is deliberate: a refusal is a state the screen shows rather than
// an error, so a caller reads NeedsRelogin off the rows instead of catching
// something. The sweep itself only fails when it cannot read the workspace.
func (s *service) RefreshExpiring(ctx context.Context, scope domain.Scope) ([]domain.Integration, error) {
	connections, err := s.deps.Repository.List(ctx, scope)
	if err != nil {
		return nil, err
	}

	now := s.now()
	out := make([]domain.Integration, 0, len(connections))

	for _, connection := range connections {
		if !connection.Connected() {
			continue
		}
		if !expiresSoon(connection.ExpiresAt, now, RefreshMargin) {
			continue
		}

		refreshed, err := s.RefreshConnection(ctx, scope, connection.ID)
		if err != nil {
			// A single connection that cannot be refreshed is already marked as
			// needing a login. The rest of the workspace's connections still get
			// their turn.
			s.deps.Logger.Warn("refreshing one connection failed",
				zap.String("integration", connection.ID.String()),
				zap.Error(err),
			)
			continue
		}
		out = append(out, refreshed)
	}

	return out, nil
}

// oauthAdapter finds an adapter that is connected through OAuth.
func (s *service) oauthAdapter(app string) (domain.OAuthAdapter, bool) {
	if s.deps.Registry == nil {
		return nil, false
	}

	adapter, ok := s.deps.Registry.Adapter(app)
	if !ok {
		return nil, false
	}

	oauth, ok := adapter.(domain.OAuthAdapter)
	return oauth, ok
}

// accountLabel names the account a flow connected.
//
// The adapter decides, because only it knows where the provider states the
// address. A provider that does not is named after the app and the moment, which
// is a label a person can still tell apart from another.
func (s *service) accountLabel(ctx context.Context, adapter domain.OAuthAdapter, tokens domain.OAuthTokens) (string, error) {
	if namer, ok := adapter.(interface {
		AccountLabel(ctx context.Context, tokens domain.OAuthTokens) (string, error)
	}); ok {
		label, err := namer.AccountLabel(ctx, tokens)
		if err != nil {
			return "", fmt.Errorf("integration: read the account label: %w", err)
		}
		if trimmed := strings.TrimSpace(label); trimmed != "" {
			return trimmed, nil
		}
	}

	// A fallback that is still unique per app, so two connections of the same
	// app cannot collide on the upsert key and overwrite each other.
	return fmt.Sprintf("%s@%d", adapter.App(), s.now().Unix()), nil
}

// now reads the clock, which a test can freeze.
func (s *service) now() time.Time {
	if s.deps.Clock == nil {
		return time.Now()
	}
	return s.deps.Clock()
}

// expiresSoon reports whether an expiry is within the margin, or has none.
func expiresSoon(at *time.Time, now time.Time, margin time.Duration) bool {
	if at == nil {
		return false
	}
	return !at.After(now.Add(margin))
}

// refreshReason turns a provider's refusal into something a person can read.
func refreshReason(err error) string {
	if errors.Is(err, domain.ErrRefreshFailed) {
		return "penyedia menolak refresh token; sambungkan ulang"
	}
	return "penyegaran token gagal; sambungkan ulang"
}

// newNonce returns the unguessable part of a state value.
func newNonce() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("integration: generate state nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// safeReturnTo keeps the callback from being usable as an open redirect.
//
// Only a path within the app is allowed: an absolute URL would let a crafted
// authorization request send somebody to another site after they granted access,
// which is exactly the shape a phishing flow wants.
func safeReturnTo(returnTo string) string {
	trimmed := strings.TrimSpace(returnTo)
	if trimmed == "" {
		return "/dashboard"
	}
	if strings.HasPrefix(trimmed, "/") && !strings.HasPrefix(trimmed, "//") {
		return trimmed
	}
	return "/dashboard"
}
