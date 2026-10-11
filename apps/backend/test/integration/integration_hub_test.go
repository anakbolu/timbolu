package integration

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/container"
	agentrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/repository"
	approvalrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/repository"
	authrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/repository"
	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	workspacerepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/repository"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/config"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/crypto"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
)

// These tests cover the E8.1 gate: a stored token is never readable from the
// database, is opened only where it is used, and survives a rotation of the key
// that protects it.

// kmsKey renders a 32-byte key the way a deployment writes one in the
// environment.
func kmsKey(seed byte) string {
	raw := make([]byte, crypto.KeySize)
	for i := range raw {
		raw[i] = seed*7 + byte(i)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// createVerifiedUser makes an account that may onboard, which is the state the
// sign-up flow leaves behind once the emailed link is followed.
func createVerifiedUser(t *testing.T, pool *database.Pool) (uuid.UUID, error) {
	t.Helper()

	users := authrepo.New(pool.PgxPool())
	user, err := users.CreateUser(t.Context(), uuid.NewString()+"@example.com", "$argon2id$hash")
	if err != nil {
		return uuid.Nil, err
	}
	if _, err := users.MarkEmailVerified(t.Context(), user.ID); err != nil {
		return uuid.Nil, err
	}
	return user.ID, nil
}

// onboardWorkspace gives the account its workspace, which is what the
// integration hub is scoped to.
func onboardWorkspace(t *testing.T, pool *database.Pool, userID uuid.UUID) (uuid.UUID, error) {
	t.Helper()

	agents := agentrepo.New(pool)
	workspaces := workspacerepo.New(pool, agents)
	workspaces.SetPolicyProvisioner(approvalrepo.New(pool))

	onboarded, err := workspaces.Onboard(t.Context(), userID, workspacedomain.OnboardRequest{
		Name:          "Toko Sinar",
		BusinessField: "Retail",
		Timezone:      "Asia/Jakarta",
	})
	if err != nil {
		return uuid.Nil, err
	}
	return onboarded.Workspace.ID, nil
}

// integrationFixture is one onboarded workspace with the integration hub wired
// through the container, so what is under test is the real path from the
// configured key to the stored row.
type integrationFixture struct {
	app         *container.Container
	service     integrationdomain.Service
	pool        *database.Pool
	dsn         string
	workspaceID uuid.UUID
	userID      uuid.UUID
	scope       integrationdomain.Scope
}

func newIntegrationFixture(t *testing.T, dsn string, kmsConfig config.KmsConfig) *integrationFixture {
	t.Helper()

	user, err := createVerifiedUser(t, poolFor(t, dsn))
	require.NoError(t, err)

	workspace, err := onboardWorkspace(t, poolFor(t, dsn), user)
	require.NoError(t, err)

	return newIntegrationFixtureFor(t, dsn, kmsConfig, user, workspace)
}

// newIntegrationFixtureFor wires the hub over an existing workspace, which is
// what a rotation test needs: the same stored rows read under a different key
// configuration, rather than a second workspace with none of them.
func newIntegrationFixtureFor(t *testing.T, dsn string, kmsConfig config.KmsConfig, userID, workspaceID uuid.UUID) *integrationFixture {
	t.Helper()

	ctx := t.Context()
	pool := poolFor(t, dsn)

	cfg := &config.Config{
		Application: config.AppConfig{Environment: "test", FrontendURL: "https://app.example.com"},
		Http:        config.HttpConfig{Address: ":0", ApiPrefix: "/api"},
		Database:    config.DatabaseConfig{URL: dsn, MaxOpenConns: 5, MaxIdleConns: 1},
		JWT:         config.JWTConfig{Secret: "integration-test-secret", ExpireHours: 1},
		Auth: config.AuthConfig{
			AccessTokenMinutes: 15, RefreshHours: 24, VerificationHours: 24,
			ResetMinutes: 60, StateMinutes: 10, LoginThrottleCapacity: 100, LoginThrottleRefill: 100,
		},
		Mail:    config.MailConfig{Driver: "log"},
		Logging: config.LoggingConfig{Level: "error", Format: "json"},
		Kms:     kmsConfig,
	}

	app, err := container.New(ctx, cfg, container.WithLogger(zap.NewNop()))
	require.NoError(t, err)
	t.Cleanup(app.Close)

	require.NotNil(t, app.Services.Integration, "the integration hub must be wired")

	return &integrationFixture{
		app:         app,
		service:     app.Services.Integration,
		pool:        pool,
		dsn:         dsn,
		workspaceID: workspaceID,
		userID:      userID,
		scope:       integrationdomain.Scope{UserID: userID, WorkspaceID: workspaceID},
	}
}

// rawTokenColumn reads the stored ciphertext the way an operator with database
// access would: straight from the column, with no application code in the way.
func rawTokenColumn(t *testing.T, pool *database.Pool, workspaceID uuid.UUID, app, account string) []byte {
	t.Helper()

	var sealed []byte
	err := pool.PgxPool().QueryRow(t.Context(),
		`SELECT access_token_encrypted FROM integrations
		 WHERE workspace_id = $1 AND app = $2 AND account_label = $3`,
		workspaceID, app, account).Scan(&sealed)
	require.NoError(t, err)
	return sealed
}

// TestATokenIsSealedAtRestAndOpensOnlyWhenUsed is the E8.1 acceptance gate.
func TestATokenIsSealedAtRestAndOpensOnlyWhenUsed(t *testing.T) {
	fixture := newIntegrationFixture(t, migratedDatabase(t), config.KmsConfig{
		MasterKey:   kmsKey(1),
		MasterKeyID: "v1",
	})
	ctx := t.Context()

	const access = "ya29.a0AfH6SM-super-secret-access-token"
	const refresh = "1//0g-super-secret-refresh-token"

	connected, err := fixture.service.Connect(ctx, fixture.scope, integrationdomain.ConnectInput{
		App:          "gmail",
		AccountLabel: "toko@example.com",
		Scopes:       []string{"gmail.readonly", "gmail.send"},
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresAt:    new(time.Now().Add(time.Hour)),
	})
	require.NoError(t, err)
	require.Equal(t, integrationdomain.StatusConnected, connected.Status)
	require.True(t, connected.Connected())

	// The column holds ciphertext, and it holds neither secret in any form.
	sealed := rawTokenColumn(t, fixture.pool, fixture.workspaceID, "gmail", "toko@example.com")
	require.NotEmpty(t, sealed)
	require.NotContains(t, string(sealed), access)
	require.NotContains(t, string(sealed), refresh)
	require.NotContains(t, string(sealed), "super-secret")
	require.NotContains(t, string(sealed), "ya29.")

	// The workspace's data key is stored wrapped, not in the clear.
	var wrapped []byte
	var keyID string
	require.NoError(t, fixture.pool.PgxPool().QueryRow(ctx,
		`SELECT wrapped_data_key, kms_key_id FROM workspace_keys WHERE workspace_id = $1`,
		fixture.workspaceID).Scan(&wrapped, &keyID))
	require.NotEmpty(t, wrapped)
	require.Equal(t, "v1", keyID)
	require.Len(t, wrapped, crypto.KeySize+28, "the wrap is a nonce, a 32-byte key, and a GCM tag")

	// And the token opens where it is used.
	token, err := fixture.service.Token(ctx, fixture.scope, connected.ID)
	require.NoError(t, err)
	require.Equal(t, access, token.AccessToken)
	require.Equal(t, refresh, token.RefreshToken)
	require.Equal(t, []string{"gmail.readonly", "gmail.send"}, token.Scopes)

	// The stored row does not carry the secret into a response either.
	listed, err := fixture.service.List(ctx, fixture.scope)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.NotContains(t, listed[0].StatusReason, access)
}

// TestRotatingTheKeyKeepsAConnectionWorking is the second half of the gate: a
// rotation of the key-management key must not break what is already stored.
func TestRotatingTheKeyKeepsAConnectionWorking(t *testing.T) {
	// One database, two containers: the rotation is about the same stored rows
	// being read under a different key configuration, so a second database would
	// prove nothing.
	dsn := migratedDatabase(t)

	// The connection is made while v1 is the key.
	before := newIntegrationFixture(t, dsn, config.KmsConfig{
		MasterKey:   kmsKey(2),
		MasterKeyID: "v1",
	})
	ctx := t.Context()

	const access = "token-sealed-under-v1"

	connected, err := before.service.Connect(ctx, before.scope, integrationdomain.ConnectInput{
		App:          "gmail",
		AccountLabel: "toko@example.com",
		Scopes:       []string{"gmail.readonly"},
		AccessToken:  access,
	})
	require.NoError(t, err)

	// The operator rotates: v2 becomes active and v1 is kept as retired.
	after := newIntegrationFixtureFor(t, dsn, config.KmsConfig{
		MasterKey:   kmsKey(3),
		MasterKeyID: "v2",
		RetiredKeys: "v1:" + kmsKey(2),
	}, before.userID, before.workspaceID)

	// The stored token still opens, because the data key inside is unchanged.
	token, err := after.service.Token(ctx, after.scope, connected.ID)
	require.NoError(t, err)
	require.Equal(t, access, token.AccessToken, "a rotation must not break an existing connection")

	// And the wrap now names the active key, so the rotation converged and the
	// retired key can eventually be dropped.
	var keyID string
	require.NoError(t, after.pool.PgxPool().QueryRow(ctx,
		`SELECT kms_key_id FROM workspace_keys WHERE workspace_id = $1`,
		after.workspaceID).Scan(&keyID))
	require.Equal(t, "v2", keyID, "opening a token re-seals the data key under the active key")

	// The token ciphertext itself was not rewritten: only the layer around the
	// data key changed, which is the whole point of an envelope.
	sealedBefore := rawTokenColumn(t, before.pool, before.workspaceID, "gmail", "toko@example.com")
	sealedAfter := rawTokenColumn(t, after.pool, after.workspaceID, "gmail", "toko@example.com")
	require.Equal(t, sealedBefore, sealedAfter)
}

// TestADroppedKeyIsReportedRatherThanGuessed covers the operator mistake of
// rotating and removing the old key: the connection is unreadable, and the error
// says so instead of returning garbage.
func TestADroppedKeyIsReportedRatherThanGuessed(t *testing.T) {
	// One database, two containers, for the same reason as the rotation above.
	dsn := migratedDatabase(t)

	before := newIntegrationFixture(t, dsn, config.KmsConfig{MasterKey: kmsKey(4), MasterKeyID: "v1"})
	ctx := t.Context()

	connected, err := before.service.Connect(ctx, before.scope, integrationdomain.ConnectInput{
		App:          "gmail",
		AccountLabel: "toko@example.com",
		AccessToken:  "token-under-a-key-nobody-kept",
	})
	require.NoError(t, err)

	// v2 is active and v1 was not carried over.
	after := newIntegrationFixtureFor(t, dsn, config.KmsConfig{MasterKey: kmsKey(5), MasterKeyID: "v2"},
		before.userID, before.workspaceID)

	_, err = after.service.Token(ctx, after.scope, connected.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "v1", "the error must name the key that is missing")
}

// TestDisconnectingClearsTheStoredTokens keeps a disconnected account from
// leaving a usable credential behind.
func TestDisconnectingClearsTheStoredTokens(t *testing.T) {
	fixture := newIntegrationFixture(t, migratedDatabase(t), config.KmsConfig{MasterKey: kmsKey(6), MasterKeyID: "v1"})
	ctx := t.Context()

	connected, err := fixture.service.Connect(ctx, fixture.scope, integrationdomain.ConnectInput{
		App:          "gmail",
		AccountLabel: "toko@example.com",
		AccessToken:  "a-token-to-forget",
		RefreshToken: "a-refresh-to-forget",
	})
	require.NoError(t, err)

	require.NoError(t, fixture.service.Disconnect(ctx, fixture.scope, connected.ID))

	sealed := rawTokenColumn(t, fixture.pool, fixture.workspaceID, "gmail", "toko@example.com")
	require.Empty(t, sealed, "a disconnected connection keeps no token")

	_, err = fixture.service.Token(ctx, fixture.scope, connected.ID)
	require.ErrorIs(t, err, integrationdomain.ErrNoToken)

	// The row stays, so the screen can say the account was disconnected rather
	// than forgetting it existed.
	got, err := fixture.service.Get(ctx, fixture.scope, connected.ID)
	require.NoError(t, err)
	require.Equal(t, integrationdomain.StatusDisconnected, got.Status)
}

// TestReconnectingUpdatesTheSameRow covers re-authorising an account: a second
// connection to the same mailbox would show up twice on the screen.
func TestReconnectingUpdatesTheSameRow(t *testing.T) {
	fixture := newIntegrationFixture(t, migratedDatabase(t), config.KmsConfig{MasterKey: kmsKey(7), MasterKeyID: "v1"})
	ctx := t.Context()

	first, err := fixture.service.Connect(ctx, fixture.scope, integrationdomain.ConnectInput{
		App:          "gmail",
		AccountLabel: "toko@example.com",
		AccessToken:  "first-token",
	})
	require.NoError(t, err)

	second, err := fixture.service.Connect(ctx, fixture.scope, integrationdomain.ConnectInput{
		App:          "gmail",
		AccountLabel: "toko@example.com",
		AccessToken:  "second-token",
		Scopes:       []string{"gmail.send"},
	})
	require.NoError(t, err)

	require.Equal(t, first.ID, second.ID, "re-authorising must not add a second connection")

	list, err := fixture.service.List(ctx, fixture.scope)
	require.NoError(t, err)
	require.Len(t, list, 1)

	token, err := fixture.service.Token(ctx, fixture.scope, first.ID)
	require.NoError(t, err)
	require.Equal(t, "second-token", token.AccessToken, "the newest authorisation is the one that counts")
}

// TestATokenCannotBeMovedToAnotherWorkspace is the tenant boundary applied to a
// ciphertext: the workspace is bound into the seal, so a row copied to another
// workspace does not open there even though it is a real row.
func TestATokenCannotBeMovedToAnotherWorkspace(t *testing.T) {
	fixture := newIntegrationFixture(t, migratedDatabase(t), config.KmsConfig{MasterKey: kmsKey(8), MasterKeyID: "v1"})
	ctx := t.Context()

	_, err := fixture.service.Connect(ctx, fixture.scope, integrationdomain.ConnectInput{
		App:          "gmail",
		AccountLabel: "toko@example.com",
		AccessToken:  "belongs-to-one-workspace",
	})
	require.NoError(t, err)

	// A second workspace in the same deployment.
	otherUser, err := createVerifiedUser(t, fixture.pool)
	require.NoError(t, err)
	otherWorkspace, err := onboardWorkspace(t, fixture.pool, otherUser)
	require.NoError(t, err)

	sealed := rawTokenColumn(t, fixture.pool, fixture.workspaceID, "gmail", "toko@example.com")

	// The ciphertext is planted in the other workspace, as a database-level copy
	// would leave it. The superuser pool is used deliberately: this test is about
	// the seal binding the workspace, which holds even when a policy is bypassed.
	_, err = fixture.pool.PgxPool().Exec(ctx,
		`INSERT INTO integrations (workspace_id, app, account_label, status, access_token_encrypted)
		 VALUES ($1, 'gmail', 'stolen@example.com', 'connected', $2)`,
		otherWorkspace, sealed)
	require.NoError(t, err)

	otherScope := integrationdomain.Scope{UserID: otherUser, WorkspaceID: otherWorkspace}
	otherList, err := fixture.service.List(ctx, otherScope)
	require.NoError(t, err)
	require.Len(t, otherList, 1)

	_, err = fixture.service.Token(ctx, otherScope, otherList[0].ID)
	require.Error(t, err, "a ciphertext bound to one workspace must not open in another")
}

// TestGrantsDecideWhichToolsABoluMayUse is the governance rule: a read-only
// grant keeps the tools that cannot change anything outside Bolu.
func TestGrantsDecideWhichToolsABoluMayUse(t *testing.T) {
	fixture := newIntegrationFixture(t, appDatabase(t), config.KmsConfig{MasterKey: kmsKey(9), MasterKeyID: "v1"})
	ctx := t.Context()

	agentID := firstAgentOf(t, fixture.pool, fixture.workspaceID)

	connected, err := fixture.service.Connect(ctx, fixture.scope, integrationdomain.ConnectInput{
		App:          "gmail",
		AccountLabel: "toko@example.com",
		AccessToken:  "a-real-token",
	})
	require.NoError(t, err)

	grant, err := fixture.service.SetGrant(ctx, fixture.scope, agentID, connected.ID, integrationdomain.PermissionRead)
	require.NoError(t, err)
	require.Equal(t, integrationdomain.PermissionRead, grant.Permission)

	readOnly, err := fixture.service.AllowedPermissions(ctx, fixture.scope, agentID)
	require.NoError(t, err)
	require.Equal(t, integrationdomain.PermissionRead, readOnly[connected.ID])

	// Read-only keeps the tools that stay inside Bolu, and drops the one that
	// leaves it.
	require.True(t, grant.Allows(integrationdomain.LabelRead))
	require.True(t, grant.Allows(integrationdomain.LabelWriteInternal))
	require.False(t, grant.Allows(integrationdomain.LabelWriteExternal))

	// Upgrading the grant lets the external tool through.
	upgraded, err := fixture.service.SetGrant(ctx, fixture.scope, agentID, connected.ID, integrationdomain.PermissionReadWrite)
	require.NoError(t, err)
	require.True(t, upgraded.Allows(integrationdomain.LabelWriteExternal))

	// A permission the schema does not have is refused rather than stored.
	_, err = fixture.service.SetGrant(ctx, fixture.scope, agentID, connected.ID, "superuser")
	require.ErrorIs(t, err, integrationdomain.ErrUnknownPermission)

	// Removing the grant takes the integration away from that Bolu.
	require.NoError(t, fixture.service.DeleteGrant(ctx, fixture.scope, agentID, connected.ID))

	after, err := fixture.service.AllowedPermissions(ctx, fixture.scope, agentID)
	require.NoError(t, err)
	require.NotContains(t, after, connected.ID)
}

// TestAnotherWorkspaceCannotSeeOrGrantAConnection is the tenant boundary on the
// grant path, which is what keeps one workspace's mailbox out of another's.
func TestAnotherWorkspaceCannotSeeOrGrantAConnection(t *testing.T) {
	fixture := newIntegrationFixture(t, appDatabase(t), config.KmsConfig{MasterKey: kmsKey(10), MasterKeyID: "v1"})
	ctx := t.Context()

	connected, err := fixture.service.Connect(ctx, fixture.scope, integrationdomain.ConnectInput{
		App:          "gmail",
		AccountLabel: "toko@example.com",
		AccessToken:  "a-real-token",
	})
	require.NoError(t, err)

	otherUser, err := createVerifiedUser(t, fixture.pool)
	require.NoError(t, err)
	otherWorkspace, err := onboardWorkspace(t, fixture.pool, otherUser)
	require.NoError(t, err)
	otherScope := integrationdomain.Scope{UserID: otherUser, WorkspaceID: otherWorkspace}
	otherAgent := firstAgentOf(t, fixture.pool, otherWorkspace)

	_, err = fixture.service.Get(ctx, otherScope, connected.ID)
	require.ErrorIs(t, err, integrationdomain.ErrNotFound)

	_, err = fixture.service.SetGrant(ctx, otherScope, otherAgent, connected.ID, integrationdomain.PermissionReadWrite)
	require.ErrorIs(t, err, integrationdomain.ErrNotFound)

	list, err := fixture.service.List(ctx, otherScope)
	require.NoError(t, err)
	require.Empty(t, list)
}

// TestAConnectionWithoutAKeyIsRefusedRatherThanStoredInClearText is what a
// deployment that configured no key does: it says so.
func TestAConnectionWithoutAKeyIsRefusedRatherThanStoredInClearText(t *testing.T) {
	fixture := newIntegrationFixture(t, migratedDatabase(t), config.KmsConfig{})
	ctx := t.Context()

	_, err := fixture.service.Connect(ctx, fixture.scope, integrationdomain.ConnectInput{
		App:          "gmail",
		AccountLabel: "toko@example.com",
		AccessToken:  "would-be-plaintext",
	})
	require.ErrorIs(t, err, integrationdomain.ErrNoCipher)

	// Nothing was written, so there is no half-connected account to clean up.
	list, err := fixture.service.List(ctx, fixture.scope)
	require.NoError(t, err)
	require.Empty(t, list)
}

// TestNeedsReloginKeepsWhatTheConnectionKnows is the failure a person can act on:
// a refresh that failed asks for a login without forgetting the account.
func TestNeedsReloginKeepsWhatTheConnectionKnows(t *testing.T) {
	fixture := newIntegrationFixture(t, migratedDatabase(t), config.KmsConfig{MasterKey: kmsKey(11), MasterKeyID: "v1"})
	ctx := t.Context()

	connected, err := fixture.service.Connect(ctx, fixture.scope, integrationdomain.ConnectInput{
		App:          "gmail",
		AccountLabel: "toko@example.com",
		AccessToken:  "a-token-that-stopped-working",
	})
	require.NoError(t, err)

	marked, err := fixture.service.NeedsRelogin(ctx, fixture.scope, connected.ID, "penyedia menolak refresh token")
	require.NoError(t, err)
	require.True(t, marked.NeedsRelogin())
	require.Equal(t, "penyedia menolak refresh token", marked.StatusReason)

	// The stored token is still there: re-authorising finds the connection as it
	// was rather than starting over.
	sealed := rawTokenColumn(t, fixture.pool, fixture.workspaceID, "gmail", "toko@example.com")
	require.NotEmpty(t, sealed)

	// A refresh that succeeds clears the status again.
	refreshed, err := fixture.service.Refresh(ctx, fixture.scope, connected.ID, integrationdomain.RefreshInput{
		AccessToken: "a-fresh-token",
		ExpiresAt:   new(time.Now().Add(time.Hour)),
	})
	require.NoError(t, err)
	require.True(t, refreshed.Connected())
	require.Empty(t, refreshed.StatusReason)

	token, err := fixture.service.Token(ctx, fixture.scope, connected.ID)
	require.NoError(t, err)
	require.Equal(t, "a-fresh-token", token.AccessToken)
}
