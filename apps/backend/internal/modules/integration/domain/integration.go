// Package domain defines the integration contract: the account a workspace
// connected, the token that lets a Bolu act on it, and the grant that says which
// Bolu may.
//
// Two rules live here. A token is only ever handled sealed — the domain has no
// field for a plaintext one, so a caller cannot hold it by accident. And a tool
// is only offered to a Bolu whose grant covers it: a read-only grant keeps the
// tools that cannot change anything outside Bolu, and nothing else.
package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Connection statuses, matching the check constraint on `integrations.status`.
const (
	// StatusConnected is an account whose token works.
	StatusConnected = "connected"
	// StatusNeedsRelogin is an account whose refresh failed. The stored data is
	// kept: the person re-authorises and the connection is usable again, rather
	// than losing what the integration already knows.
	StatusNeedsRelogin = "needs_relogin"
	// StatusDisconnected is an account somebody removed or never finished
	// connecting.
	StatusDisconnected = "disconnected"
)

// Grant permissions, matching the check constraint on `agent_grants.permission`.
const (
	// PermissionRead is read-only: the Bolu may look, never change.
	PermissionRead = "read"
	// PermissionReadWrite lets the Bolu act, subject to the approval gate.
	PermissionReadWrite = "read_write"
)

// Errors the service and the handlers map onto responses.
var (
	ErrNotFound = errors.New("integration: not found")
	// ErrNoCipher means the deployment configured no key-management key, so no
	// token could be sealed. It is a deployment mistake, not a user error.
	ErrNoCipher = errors.New("integration: no encryption key configured, so tokens cannot be stored")
	// ErrInvalidInput means a caller supplied something the schema would refuse.
	ErrInvalidInput = errors.New("integration: invalid input")
	// ErrUnknownStatus means a caller asked for a status the schema does not have.
	ErrUnknownStatus = errors.New("integration: unknown connection status")
	// ErrUnknownPermission means a caller asked for a permission the schema does
	// not have.
	ErrUnknownPermission = errors.New("integration: unknown grant permission")
	// ErrRateLimited means the workspace has spent the calls this integration is
	// allowed for now. It is retryable: the work is held, not refused.
	ErrRateLimited = errors.New("integration: the call limit for this integration is reached")
	// ErrNoToken means the connection has nothing usable stored, which is what a
	// connection waiting for a fresh login looks like. The caller asks the person
	// to reconnect rather than retrying.
	ErrNoToken = errors.New("integration: the connection has no usable token")
	// ErrPlanLimit means the workspace's package does not allow another connection.
	// It is a refusal to buy out of rather than one to retry.
	ErrPlanLimit = errors.New("integration: paket workspace tidak mengizinkan koneksi baru")
)

// Auditor records what a person did, for the trail the product answers "who changed
// this" with. The approval module owns the table.
//
// It is a port rather than a call into that module for the reason the plan ports are: this
// module does not know what a draft is. Changing who a Bolu may act as is one of the
// sensitive actions the epic lists, and it is recorded through here.
type Auditor interface {
	Record(ctx context.Context, scope Scope, entry AuditEntry) error
}

// AuditEntry is one thing that happened, for the record.
type AuditEntry struct {
	Action     string
	ObjectType string
	ObjectID   string
	// ActorUserID is who did it, when a person did.
	ActorUserID uuid.UUID
	// Metadata is the detail: which Bolu, which connection, which permission. Never a
	// token, and never anything read out of one.
	Metadata map[string]any
}

// PlanLimits reports how many connections a workspace's package allows.
//
// It is a port for the reason the other plan ports are: this module does not know
// what a subscription is, and the container hands it the module that does. Unwired
// means the deployment sells no packages, and nothing is capped.
type PlanLimits interface {
	MaxIntegrations(ctx context.Context, workspaceID uuid.UUID) (int, error)
}

// Scope is the tenant every call is bound to. It mirrors the approval module's
// scope: the user is who is asking, the workspace is what they may see.
type Scope struct {
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
	// AgentID and TaskID are who the call is being made for, when it is made for somebody. They are
	// not part of the tenancy — a call is scoped by the workspace above — but an adapter that records
	// what it did needs to attribute it: an order a Bolu took belongs to that Bolu and to the task it
	// was working on, and reading them off the scope is what lets the tool write them without the hub
	// having to know what they are for.
	AgentID uuid.UUID
	TaskID  uuid.UUID
}

// LinkInput is a connection this deployment holds the credential for.
//
// It is not an authorisation the provider performed: pairing a WhatsApp number
// gives us a session that lives in our own gateway, and there is no token to
// store. What the hub still owns is everything else — the connection a screen
// shows, the grant a Bolu holds, and the approval gate an action passes through.
type LinkInput struct {
	App          string
	AccountLabel string
	Scopes       []string
}

// Validate refuses an input the schema would reject, so the failure is a message
// rather than a constraint violation.
func (in LinkInput) Validate() error {
	if strings.TrimSpace(in.App) == "" {
		return errors.New("integration: an app is required")
	}
	if strings.TrimSpace(in.AccountLabel) == "" {
		return errors.New("integration: an account label is required")
	}
	return nil
}

// Integration is one connected account. It carries no token: the sealed values
// stay in the repository and are opened one at a time, when they are used.
type Integration struct {
	ID           uuid.UUID
	App          string
	AccountLabel string
	Status       string
	// StatusReason explains a status a person has to act on, in their words.
	StatusReason string
	Scopes       []string
	// ExpiresAt is when the access token stops working, so a screen can warn
	// before it does.
	ExpiresAt       *time.Time
	LastRefreshedAt *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Connected reports whether the account can be used right now.
func (i Integration) Connected() bool { return i.Status == StatusConnected }

// NeedsRelogin reports whether a person has to re-authorise.
func (i Integration) NeedsRelogin() bool { return i.Status == StatusNeedsRelogin }

// Token is a usable credential pair. It only ever exists in memory, for as long
// as one call needs it.
//
// It redacts itself when printed. A struct with a secret in it eventually reaches
// a log line — through a debug print, a wrapped error, a `%v` in a message — so
// the redaction lives on the type rather than in a rule somebody has to remember.
type Token struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    *time.Time
	Scopes       []string
}

// String renders the token without its secrets.
func (t Token) String() string {
	return fmt.Sprintf("integration.Token{access: %s, refresh: %s, expires: %s, scopes: %v}",
		redact(t.AccessToken), redact(t.RefreshToken), expiryOf(t.ExpiresAt), t.Scopes)
}

// LogValue is what `slog` uses, so a structured log redacts it too.
func (t Token) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("access", redact(t.AccessToken)),
		slog.String("refresh", redact(t.RefreshToken)),
		slog.String("expires", expiryOf(t.ExpiresAt)),
	)
}

// redact keeps enough of a secret to tell two apart while never showing one.
// An empty value stays visibly empty, because "there is no refresh token" and
// "there is one I am hiding" are different facts.
func redact(secret string) string {
	if secret == "" {
		return "(empty)"
	}
	if len(secret) <= 4 {
		return "(redacted)"
	}
	return "(redacted:" + secret[:2] + "…" + secret[len(secret)-2:] + ")"
}

// expiryOf renders an expiry, or says there is none.
func expiryOf(at *time.Time) string {
	if at == nil {
		return "(none)"
	}
	return at.UTC().Format(time.RFC3339)
}

// Expired reports whether the access token is past its expiry, with a margin so
// a token that expires during a slow call is treated as expired now.
func (t Token) Expired(now time.Time, margin time.Duration) bool {
	if t.ExpiresAt == nil {
		// No stated expiry means the provider does not use them, so the token is
		// taken at its word rather than refreshed on every call.
		return false
	}
	return !t.ExpiresAt.After(now.Add(margin))
}

// ConnectInput is what a completed authorisation produces.
type ConnectInput struct {
	App          string
	AccountLabel string
	Scopes       []string
	AccessToken  string
	RefreshToken string
	ExpiresAt    *time.Time
}

// Validate refuses an input the schema or the flow would reject, so the failure
// is a message rather than a constraint violation.
func (in ConnectInput) Validate() error {
	if strings.TrimSpace(in.App) == "" {
		return errors.New("integration: an app is required")
	}
	if strings.TrimSpace(in.AccessToken) == "" {
		return errors.New("integration: an access token is required")
	}
	if strings.TrimSpace(in.AccountLabel) == "" {
		return errors.New("integration: an account label is required")
	}
	return nil
}

// RefreshInput is what a provider returned for a refresh.
type RefreshInput struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    *time.Time
	Scopes       []string
}

// Grant says which Bolu may use an integration, and how far.
type Grant struct {
	AgentID       uuid.UUID
	IntegrationID uuid.UUID
	Permission    string
}

// Allows reports whether a grant covers a tool of the given label.
//
// A read-only grant keeps the tools that cannot change anything outside Bolu —
// `read` and `write_internal` both stay inside. Anything that reaches out is
// dropped, which is the whole point of granting read-only.
func (g Grant) Allows(label string) bool {
	if g.Permission == PermissionReadWrite {
		return true
	}
	return label == LabelRead || label == LabelWriteInternal
}

// Tool labels, mirroring the task runtime's. They are repeated here rather than
// imported so the integration module does not depend on the task module; the
// container is what reconciles the two.
const (
	LabelRead          = "read"
	LabelWriteInternal = "write_internal"
	LabelWriteExternal = "write_external"
)

// ValidPermission reports whether a permission is one the schema stores.
func ValidPermission(permission string) bool {
	return permission == PermissionRead || permission == PermissionReadWrite
}

// ValidStatus reports whether a status is one the schema stores.
func ValidStatus(status string) bool {
	switch status {
	case StatusConnected, StatusNeedsRelogin, StatusDisconnected:
		return true
	default:
		return false
	}
}

// WrappedKey is a data key sealed by the key-management key, with the id of the
// key that sealed it.
type WrappedKey struct {
	KeyID string
	Bytes []byte
}

// KeyManager seals and opens the per-workspace data keys. The platform package
// implements it; the container hands it over.
type KeyManager interface {
	Wrap(ctx context.Context, workspaceID uuid.UUID, dataKey []byte) (WrappedKey, error)
	Unwrap(ctx context.Context, workspaceID uuid.UUID, wrapped WrappedKey) ([]byte, error)
	ActiveKeyID() string
}

// TokenCipher seals and opens a token with the workspace's data key.
//
// The domain declares this rather than calling the crypto package directly for
// the usual reason: the sealing is infrastructure, and the module boundary rules
// keep a service out of the platform packages that hold it. It is also the only
// place a data key is resolved, so "decrypt only when used" is one function to
// read rather than a rule to remember.
type TokenCipher interface {
	Seal(ctx context.Context, workspaceID uuid.UUID, plaintext string) ([]byte, error)
	Open(ctx context.Context, workspaceID uuid.UUID, ciphertext []byte) (string, error)
	// Rotate re-seals the workspace's data key under the active key-management
	// key, leaving the sealed tokens alone.
	Rotate(ctx context.Context, workspaceID uuid.UUID) error
}

// StoredToken is what the repository holds: sealed values and their metadata.
type StoredToken struct {
	AccessTokenEncrypted  []byte
	RefreshTokenEncrypted []byte
	ExpiresAt             *time.Time
	Scopes                []string
}

// Repository is the storage the service needs. Every method is scoped to a
// workspace, and the row level security policy enforces it as well.
type Repository interface {
	// List returns the workspace's connections, newest app first.
	List(ctx context.Context, scope Scope) ([]Integration, error)
	// Get returns one connection.
	Get(ctx context.Context, scope Scope, id uuid.UUID) (Integration, error)
	// Upsert stores a connection, keyed by app and account label, so
	// re-authorising the same account updates it rather than adding a second.
	Upsert(ctx context.Context, scope Scope, input ConnectInput, sealed StoredToken) (Integration, error)
	// Sealed returns the stored token of a connection, still sealed.
	Sealed(ctx context.Context, scope Scope, id uuid.UUID) (StoredToken, error)
	// SaveRefreshed replaces the sealed token after a refresh.
	SaveRefreshed(ctx context.Context, scope Scope, id uuid.UUID, sealed StoredToken) error
	// SetStatus records a status a person may have to act on, with the reason.
	SetStatus(ctx context.Context, scope Scope, id uuid.UUID, status, reason string) error
	// Disconnect clears the stored token and marks the row disconnected.
	Disconnect(ctx context.Context, scope Scope, id uuid.UUID) error
	// Delete removes the row.
	Delete(ctx context.Context, scope Scope, id uuid.UUID) error

	// DataKey returns the workspace's wrapped data key, or ErrNotFound when the
	// workspace has none yet.
	DataKey(ctx context.Context, scope Scope) (WrappedKey, error)
	// SaveDataKey stores the wrapped data key, replacing an existing wrap. It is
	// how a rotation converges.
	SaveDataKey(ctx context.Context, scope Scope, wrapped WrappedKey) error

	// SaveState records an OAuth state as usable until it expires.
	SaveState(ctx context.Context, state OAuthState) error
	// ConsumeState spends a state's nonce. It returns ErrInvalidState when the
	// nonce was already spent, never existed, or has expired, and it must be
	// atomic so two callbacks arriving together cannot both succeed.
	ConsumeState(ctx context.Context, workspaceID uuid.UUID, nonce string, now time.Time) error

	// Grants lists the grants of one Bolu.
	Grants(ctx context.Context, scope Scope, agentID uuid.UUID) ([]Grant, error)
	// SetGrant adds or updates one Bolu's permission on one integration.
	SetGrant(ctx context.Context, scope Scope, agentID, integrationID uuid.UUID, permission string) (Grant, error)
	// DeleteGrant removes one grant.
	DeleteGrant(ctx context.Context, scope Scope, agentID, integrationID uuid.UUID) error
}

// Service is the module's public contract.
type Service interface {
	// List returns the workspace's connections.
	List(ctx context.Context, scope Scope) ([]Integration, error)
	// Get returns one connection.
	Get(ctx context.Context, scope Scope, id uuid.UUID) (Integration, error)
	// Connect stores a completed authorisation, sealing the tokens.
	Connect(ctx context.Context, scope Scope, input ConnectInput) (Integration, error)
	// Link records a connection whose credential this deployment holds itself,
	// with nothing in the token columns. A screen shows it, a Bolu is granted it,
	// and an action on it still passes through the approval gate — what changes is
	// only where the credential lives.
	Link(ctx context.Context, scope Scope, input LinkInput) (Integration, error)
	// Token opens the stored token of a connection. This is the only path that
	// decrypts, and it is called where the token is about to be used.
	Token(ctx context.Context, scope Scope, id uuid.UUID) (Token, error)
	// Refresh stores the result of a refresh the caller performed.
	Refresh(ctx context.Context, scope Scope, id uuid.UUID, input RefreshInput) (Integration, error)
	// NeedsRelogin marks a connection whose refresh failed, keeping its data.
	NeedsRelogin(ctx context.Context, scope Scope, id uuid.UUID, reason string) (Integration, error)
	// Disconnect clears the stored tokens.
	Disconnect(ctx context.Context, scope Scope, id uuid.UUID) error
	// Delete removes the connection.
	Delete(ctx context.Context, scope Scope, id uuid.UUID) error

	// RotateDataKey re-seals the workspace's data key under the active
	// key-management key, which is what makes a rotation converge.
	RotateDataKey(ctx context.Context, scope Scope) error

	// Grants lists the grants of one Bolu.
	Grants(ctx context.Context, scope Scope, agentID uuid.UUID) ([]Grant, error)
	// SetGrant grants or updates one Bolu's permission on one integration.
	SetGrant(ctx context.Context, scope Scope, agentID, integrationID uuid.UUID, permission string) (Grant, error)
	// DeleteGrant removes one grant.
	DeleteGrant(ctx context.Context, scope Scope, agentID, integrationID uuid.UUID) error
	// AllowedPermissions returns the integration ids a Bolu may use, and the
	// permission on each, which is what the tool filter reads.
	AllowedPermissions(ctx context.Context, scope Scope, agentID uuid.UUID) (map[uuid.UUID]string, error)

	// Execute runs one tool call against one connection.
	//
	// It is the whole path in one place: the tool is looked up in the registry,
	// its arguments are validated against the tool's own schema, the connection's
	// token is opened, and the adapter runs the call. Validation happens first, so
	// a malformed call costs nothing and never reaches a provider.
	Execute(ctx context.Context, scope Scope, integrationID uuid.UUID, tool string, arguments json.RawMessage) (Result, error)

	// Registry exposes the adapters this deployment knows, so a screen can list
	// what an application offers before anything is connected.
	Registry() Registry

	// BeginAuth starts a connection flow for an application, returning where to
	// send the person and the state the callback will carry back.
	//
	// The redirect URI is the caller's because only it knows the public origin, and
	// it must be the same value in both steps: a provider checks that the exchange
	// names the address the authorization request did, so a path here and an
	// absolute URL there is a flow that is refused.
	BeginAuth(ctx context.Context, scope Scope, app, redirectURI, returnTo string) (AuthURL, error)
	// CompleteAuth finishes a flow. It reads the tenant from the signed state
	// rather than from a session, because the callback arrives from the
	// provider's browser redirect and carries no session of its own.
	CompleteAuth(ctx context.Context, req CompleteAuthRequest) (Integration, error)
	// RefreshConnection trades a stored refresh token for a new access token. A
	// provider that refuses marks the connection as needing a fresh login rather
	// than discarding what it already knows.
	RefreshConnection(ctx context.Context, scope Scope, id uuid.UUID) (Integration, error)
	// RefreshExpiring refreshes every connection of a workspace whose token is
	// close to expiring, and reports what it did.
	RefreshExpiring(ctx context.Context, scope Scope) ([]Integration, error)

	// WebhookURL returns the address a provider should call for one connection.
	// It is signed, so it authenticates its own tenant and a delivery for one
	// workspace cannot be replayed against another.
	WebhookURL(ctx context.Context, scope Scope, integrationID uuid.UUID, baseURL string) (string, error)
	// ReceiveWebhook verifies a delivery and queues it, reporting how many events
	// were newly accepted. Zero means the delivery was already queued, which is a
	// success: a provider that retries must be told to stop.
	ReceiveWebhook(ctx context.Context, app string, request WebhookRequest) (int, error)
	// PendingWebhooks returns the oldest deliveries a workspace has not acted on.
	PendingWebhooks(ctx context.Context, scope Scope, limit int) ([]QueuedEvent, error)
	// ClaimPendingEvents reads and claims the deliveries of one event kind, which is
	// what a routine watching that event consumes. Claiming in the same step is what
	// stops two routines from acting on one delivery.
	ClaimPendingEvents(ctx context.Context, scope Scope, event string, limit int) ([]QueuedEvent, error)
	// WebhookProcessed marks a delivery as acted on.
	WebhookProcessed(ctx context.Context, scope Scope, id uuid.UUID) error
	// WebhookFailed records an attempt that did not work.
	WebhookFailed(ctx context.Context, scope Scope, id uuid.UUID, reason string) error

	// AddMCPServer registers a server a workspace wants to use and asks it what it
	// offers, so a mistyped address is found while the person is still looking at
	// the form.
	AddMCPServer(ctx context.Context, scope Scope, input MCPServerInput) (MCPServer, error)
	// ListMCPServers returns the workspace's servers.
	ListMCPServers(ctx context.Context, scope Scope) ([]MCPServer, error)
	// RefreshMCPServer asks one server what it offers and records the answer.
	RefreshMCPServer(ctx context.Context, scope Scope, id uuid.UUID) (MCPServer, error)
	// RemoveMCPServer removes one.
	RemoveMCPServer(ctx context.Context, scope Scope, id uuid.UUID) error
	// MCPTools lists the tools the workspace's servers contribute, read from what
	// they last reported rather than by reaching them.
	MCPTools(ctx context.Context, scope Scope) ([]ToolSpec, error)
}

// EventTriggers starts the routines watching an event.
//
// It is a port rather than a call into the routine module because the routine
// module is the one that knows what a routine is: the hub delivers an event and
// says which event it was, and who acts on it is not the hub's business. A
// deployment with no routines is wired with nothing here, and its deliveries are
// simply queued for the consumer that will exist.
type EventTriggers interface {
	// Fire starts the routines watching one event, and reports how many started.
	Fire(ctx context.Context, scope Scope, event string) (int, error)
}
