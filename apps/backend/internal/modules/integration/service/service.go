// Package service implements the integration use cases: connecting an account,
// opening its token where the token is about to be used, and saying which Bolu
// may use it.
//
// Two rules the package exists for. A token is sealed on the way in and opened
// only on the path that is about to use it, so nothing else in the process ever
// holds a plaintext one. And a grant is checked on the way out: a read-only grant
// keeps the tools that cannot change anything outside Bolu, and drops the rest.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
)

// RefreshMargin is how long before its stated expiry a token is treated as
// expired. A token that expires during a slow call is worth refreshing early;
// the cost is one refresh, and the alternative is a call that fails halfway.
const RefreshMargin = 2 * time.Minute

// Deps are the service dependencies.
type Deps struct {
	Repository domain.Repository
	// Cipher seals and opens tokens. Optional: without it the module still lists
	// and disconnects, and refuses to store a token rather than writing one in
	// clear text.
	Cipher domain.TokenCipher
	Logger *zap.Logger
	// Clock is the source of "now", so a test can age a token without sleeping.
	Clock func() time.Time
	// Registry holds the adapters this deployment knows. Optional: without one no
	// integration tool can run, which is what a deployment with no adapters is.
	Registry domain.Registry
	// Validator checks a call's arguments against the tool's schema. Optional:
	// without one the schema is not enforced, which is stated in the log rather
	// than silently assumed.
	Validator domain.SchemaValidator
	// States signs and verifies the OAuth state value. Optional: without it no
	// connection flow can start, which is what a deployment with no signing key is.
	States domain.StateCodec
	// StateStore remembers which state nonces are still unspent. Optional, for
	// the same reason as States.
	StateStore domain.StateStore
	// Refs signs and verifies a webhook URL's reference. Optional: without it no
	// webhook address can be issued or trusted.
	Refs domain.RefCodec
	// Queue is where a verified delivery waits to be acted on. Optional, for the
	// same reason as Refs.
	Queue domain.Queue
	// Triggers starts the routines watching an event a delivery announced.
	// Optional: without it a delivery is queued and nothing sooner acts on it, so
	// the routines a workspace set up for "when an email arrives" would never run.
	Triggers domain.EventTriggers
	// Limiter paces the calls a workspace makes on one integration. Optional:
	// without one nothing is paced, which is what a deployment with no Redis is.
	Limiter domain.CallLimiter
	// MCP stores the servers a workspace added. Optional: without it this
	// deployment offers only the adapters it was built with.
	MCP domain.MCPRepository
	// MCPClients builds a client for one server. Optional, for the same reason.
	MCPClients domain.MCPClientFactory
}

type service struct {
	deps Deps
}

// New builds the service.
func New(deps Deps) domain.Service {
	if deps.Clock == nil {
		deps.Clock = time.Now
	}
	if deps.Logger == nil {
		deps.Logger = zap.NewNop()
	}
	return &service{deps: deps}
}

// List returns the workspace's connections.
func (s *service) List(ctx context.Context, scope domain.Scope) ([]domain.Integration, error) {
	return s.deps.Repository.List(ctx, scope)
}

// Get returns one connection.
func (s *service) Get(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Integration, error) {
	if id == uuid.Nil {
		return domain.Integration{}, fmt.Errorf("%w: an integration id is required", domain.ErrInvalidInput)
	}
	return s.deps.Repository.Get(ctx, scope, id)
}

// Connect stores a completed authorisation.
//
// The tokens are sealed before they reach the repository, so a row that is
// written is a row nobody can read without the key. Re-authorising the same
// account replaces the stored token rather than adding a second connection.
func (s *service) Connect(ctx context.Context, scope domain.Scope, input domain.ConnectInput) (domain.Integration, error) {
	if err := input.Validate(); err != nil {
		return domain.Integration{}, fmt.Errorf("%w: %w", domain.ErrInvalidInput, err)
	}
	if s.deps.Cipher == nil {
		return domain.Integration{}, domain.ErrNoCipher
	}

	access, err := s.deps.Cipher.Seal(ctx, scope.WorkspaceID, input.AccessToken)
	if err != nil {
		return domain.Integration{}, fmt.Errorf("integration: seal access token: %w", err)
	}

	var refresh []byte
	if input.RefreshToken != "" {
		refresh, err = s.deps.Cipher.Seal(ctx, scope.WorkspaceID, input.RefreshToken)
		if err != nil {
			return domain.Integration{}, fmt.Errorf("integration: seal refresh token: %w", err)
		}
	}

	stored, err := s.deps.Repository.Upsert(ctx, scope, input, domain.StoredToken{
		AccessTokenEncrypted:  access,
		RefreshTokenEncrypted: refresh,
		ExpiresAt:             input.ExpiresAt,
		Scopes:                input.Scopes,
	})
	if err != nil {
		return domain.Integration{}, err
	}

	// The app and the account, never the token: this line is what an operator
	// reads when a connection changes.
	s.deps.Logger.Info("integration connected",
		zap.String("app", stored.App),
		zap.String("account", stored.AccountLabel),
		zap.Int("scopes", len(stored.Scopes)),
	)
	return stored, nil
}

// Token opens the stored token of a connection.
//
// This is the only path that decrypts, and it is called where the token is about
// to be used rather than when a connection is read.
func (s *service) Token(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Token, error) {
	if id == uuid.Nil {
		return domain.Token{}, fmt.Errorf("%w: an integration id is required", domain.ErrInvalidInput)
	}

	integration, err := s.deps.Repository.Get(ctx, scope, id)
	if err != nil {
		return domain.Token{}, err
	}

	sealed, err := s.deps.Repository.Sealed(ctx, scope, id)
	if err != nil {
		return domain.Token{}, err
	}
	if len(sealed.AccessTokenEncrypted) == 0 {
		// Nothing usable is the fact a caller acts on, and the status is the
		// reason it can show: a connection waiting for a fresh login and one
		// somebody disconnected are the same answer with different advice.
		return domain.Token{}, fmt.Errorf("%w (status %s)", domain.ErrNoToken, integration.Status)
	}
	if s.deps.Cipher == nil {
		return domain.Token{}, domain.ErrNoCipher
	}

	access, err := s.deps.Cipher.Open(ctx, scope.WorkspaceID, sealed.AccessTokenEncrypted)
	if err != nil {
		return domain.Token{}, fmt.Errorf("integration: open access token: %w", err)
	}

	var refresh string
	if len(sealed.RefreshTokenEncrypted) > 0 {
		refresh, err = s.deps.Cipher.Open(ctx, scope.WorkspaceID, sealed.RefreshTokenEncrypted)
		if err != nil {
			return domain.Token{}, fmt.Errorf("integration: open refresh token: %w", err)
		}
	}

	return domain.Token{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresAt:    sealed.ExpiresAt,
		Scopes:       sealed.Scopes,
	}, nil
}

// Refresh stores the result of a refresh the caller performed.
//
// The provider call is the caller's: this module stores credentials and says
// whether they work, and a provider-specific HTTP exchange belongs to the
// adapter that speaks that provider.
func (s *service) Refresh(ctx context.Context, scope domain.Scope, id uuid.UUID, input domain.RefreshInput) (domain.Integration, error) {
	if id == uuid.Nil {
		return domain.Integration{}, fmt.Errorf("%w: an integration id is required", domain.ErrInvalidInput)
	}
	if strings.TrimSpace(input.AccessToken) == "" {
		return domain.Integration{}, fmt.Errorf("%w: a refreshed access token is required", domain.ErrInvalidInput)
	}
	if s.deps.Cipher == nil {
		return domain.Integration{}, domain.ErrNoCipher
	}

	access, err := s.deps.Cipher.Seal(ctx, scope.WorkspaceID, input.AccessToken)
	if err != nil {
		return domain.Integration{}, fmt.Errorf("integration: seal access token: %w", err)
	}

	var refresh []byte
	if input.RefreshToken != "" {
		refresh, err = s.deps.Cipher.Seal(ctx, scope.WorkspaceID, input.RefreshToken)
		if err != nil {
			return domain.Integration{}, fmt.Errorf("integration: seal refresh token: %w", err)
		}
	}

	if err := s.deps.Repository.SaveRefreshed(ctx, scope, id, domain.StoredToken{
		AccessTokenEncrypted:  access,
		RefreshTokenEncrypted: refresh,
		ExpiresAt:             input.ExpiresAt,
		Scopes:                input.Scopes,
	}); err != nil {
		return domain.Integration{}, err
	}

	return s.deps.Repository.Get(ctx, scope, id)
}

// NeedsRelogin marks a connection whose refresh failed.
//
// The stored token is left alone: a refresh that failed is a reason to ask for a
// login, not a reason to forget what the connection already knows. A person who
// re-authorises finds the integration as it was.
func (s *service) NeedsRelogin(ctx context.Context, scope domain.Scope, id uuid.UUID, reason string) (domain.Integration, error) {
	if id == uuid.Nil {
		return domain.Integration{}, fmt.Errorf("%w: an integration id is required", domain.ErrInvalidInput)
	}

	trimmed := strings.TrimSpace(reason)
	if trimmed == "" {
		trimmed = "penyedia menolak token; sambungkan ulang"
	}

	if err := s.deps.Repository.SetStatus(ctx, scope, id, domain.StatusNeedsRelogin, trimmed); err != nil {
		return domain.Integration{}, err
	}

	s.deps.Logger.Warn("integration needs a fresh login",
		zap.String("integration", id.String()),
		zap.String("reason", trimmed),
	)
	return s.deps.Repository.Get(ctx, scope, id)
}

// Disconnect clears the stored tokens.
func (s *service) Disconnect(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	if id == uuid.Nil {
		return fmt.Errorf("%w: an integration id is required", domain.ErrInvalidInput)
	}
	return s.deps.Repository.Disconnect(ctx, scope, id)
}

// Delete removes the connection.
func (s *service) Delete(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	if id == uuid.Nil {
		return fmt.Errorf("%w: an integration id is required", domain.ErrInvalidInput)
	}
	return s.deps.Repository.Delete(ctx, scope, id)
}

// RotateDataKey re-seals the workspace's data key under the active
// key-management key.
//
// It is what makes a rotation converge. Until it runs, an old wrap still opens
// because the retired key is configured; after it, the stored wrap names the
// active key and the retired one can be dropped. The tokens are not touched:
// only the layer around the data key changes.
func (s *service) RotateDataKey(ctx context.Context, scope domain.Scope) error {
	if s.deps.Cipher == nil {
		return domain.ErrNoCipher
	}
	if err := s.deps.Cipher.Rotate(ctx, scope.WorkspaceID); err != nil {
		return fmt.Errorf("integration: rotate data key: %w", err)
	}
	return nil
}

// Execute runs one tool call against one connection.
//
// The order is the point: look the tool up, check the connection speaks that
// application, validate the arguments, and only then open the token. Nothing is
// decrypted for a call that was never going to be made.
func (s *service) Execute(ctx context.Context, scope domain.Scope, integrationID uuid.UUID, tool string, arguments json.RawMessage) (domain.Result, error) {
	// Which path a tool takes is decided by who offers it. A native adapter is
	// reached through a connection, so it needs one; a server a workspace added is
	// the connection, so it has no integration row to name. Looking the tool up
	// first is what keeps the requirement where it applies.
	spec, adapter, native := domain.ToolSpec{}, domain.Adapter(nil), false
	if s.deps.Registry != nil {
		spec, adapter, native = s.deps.Registry.Lookup(tool)
	}

	if !native {
		// No native adapter offers it, so it may belong to a server this workspace
		// added. That path is scoped, which is why it is reached here rather than
		// from the registry.
		return s.executeMCP(ctx, scope, tool, arguments)
	}

	if integrationID == uuid.Nil {
		return domain.Result{}, fmt.Errorf("%w: an integration id is required", domain.ErrInvalidInput)
	}

	// The tool has to belong to the application the connection is for: running a
	// gmail tool against a jira connection would send the wrong credential to the
	// wrong provider.
	integration, err := s.deps.Repository.Get(ctx, scope, integrationID)
	if err != nil {
		return domain.Result{}, err
	}
	if integration.App != spec.App() {
		return domain.Result{}, fmt.Errorf("%w: %q is not a %s tool",
			domain.ErrInvalidInput, tool, integration.App)
	}

	if s.deps.Validator != nil {
		if err := s.deps.Validator.Validate(spec.Schema, arguments); err != nil {
			return domain.Result{}, err
		}
	}

	// The limit is checked before the token is opened: a call that is going to be
	// held should not decrypt a credential on its way to waiting.
	if s.deps.Limiter != nil {
		decision, err := s.deps.Limiter.Allow(ctx, spec.App(), integrationID)
		if err != nil {
			return domain.Result{}, err
		}
		if !decision.Allowed {
			s.deps.Logger.Info("an integration call was held",
				zap.String("app", spec.App()),
				zap.String("integration", integrationID.String()),
				zap.Int("per_minute", decision.PerMinute),
				zap.Duration("retry_after", decision.RetryAfter),
			)
			return domain.Result{}, &domain.RateLimitedError{
				App:        spec.App(),
				PerMinute:  decision.PerMinute,
				RetryAfter: decision.RetryAfter,
			}
		}
	}

	// Opened here, used immediately, and never stored on the result. An adapter
	// that declares it needs no token is called without one: it carries its own
	// credential, and refusing the call because the token columns are empty would
	// make a linked number permanently unusable.
	var token domain.Token
	if needsToken(adapter) {
		opened, err := s.Token(ctx, scope, integrationID)
		if err != nil {
			return domain.Result{}, err
		}
		token = opened
	}

	result, err := adapter.Execute(ctx, domain.Call{
		Scope:         scope,
		IntegrationID: integrationID,
		Tool:          spec.Name,
		Arguments:     arguments,
		Token:         token,
	})
	if err != nil {
		return domain.Result{}, fmt.Errorf("integration: %s: %w", spec.Name, err)
	}
	return result, nil
}

// Registry exposes the adapters this deployment knows.
func (s *service) Registry() domain.Registry { return s.deps.Registry }

// Grants lists the grants of one Bolu.
func (s *service) Grants(ctx context.Context, scope domain.Scope, agentID uuid.UUID) ([]domain.Grant, error) {
	if agentID == uuid.Nil {
		return nil, fmt.Errorf("%w: a Bolu id is required", domain.ErrInvalidInput)
	}
	return s.deps.Repository.Grants(ctx, scope, agentID)
}

// SetGrant grants or updates one Bolu's permission on one integration.
func (s *service) SetGrant(ctx context.Context, scope domain.Scope, agentID, integrationID uuid.UUID, permission string) (domain.Grant, error) {
	if agentID == uuid.Nil || integrationID == uuid.Nil {
		return domain.Grant{}, fmt.Errorf("%w: a Bolu id and an integration id are required", domain.ErrInvalidInput)
	}
	if !domain.ValidPermission(permission) {
		return domain.Grant{}, fmt.Errorf("%w: %q", domain.ErrUnknownPermission, permission)
	}

	// The integration must be in this workspace and must actually work, because
	// granting a Bolu a connection nobody finished authorising is a grant that
	// silently does nothing.
	integration, err := s.deps.Repository.Get(ctx, scope, integrationID)
	if err != nil {
		return domain.Grant{}, err
	}
	if integration.Status == domain.StatusDisconnected {
		return domain.Grant{}, fmt.Errorf("%w: the connection is not authorised yet", domain.ErrInvalidInput)
	}

	return s.deps.Repository.SetGrant(ctx, scope, agentID, integrationID, permission)
}

// DeleteGrant removes one grant.
func (s *service) DeleteGrant(ctx context.Context, scope domain.Scope, agentID, integrationID uuid.UUID) error {
	if agentID == uuid.Nil || integrationID == uuid.Nil {
		return fmt.Errorf("%w: a Bolu id and an integration id are required", domain.ErrInvalidInput)
	}
	return s.deps.Repository.DeleteGrant(ctx, scope, agentID, integrationID)
}

// AllowedPermissions returns the permission a Bolu holds on each integration it
// may use, which is what the tool filter reads.
func (s *service) AllowedPermissions(ctx context.Context, scope domain.Scope, agentID uuid.UUID) (map[uuid.UUID]string, error) {
	grants, err := s.deps.Repository.Grants(ctx, scope, agentID)
	if err != nil {
		return nil, err
	}

	out := make(map[uuid.UUID]string, len(grants))
	for _, grant := range grants {
		out[grant.IntegrationID] = grant.Permission
	}
	return out, nil
}

// compile-time check that the service satisfies its own contract.
var _ domain.Service = (*service)(nil)
