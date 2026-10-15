package integration

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/container"
	approvaldomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/domain"
	approvalrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/repository"
	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	memorydomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/domain"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/mail"
	"github.com/robzlabz/employeebot/packages/platform/config"
	"github.com/robzlabz/employeebot/packages/platform/database"
)

// EPIC 13's first gate is this file: **every sensitive action leaves exactly one line, with
// the actor on it**, and the trail can be read by what a person asks.
//
// It runs through the real container, so what is under test includes the adapters the
// container installs — the translation between each module's entry shape and the trail's.
// Duplicating that translation in a test would prove the test's own copy rather than the
// deployment's, which is the one thing this file exists to avoid.

// auditFixture is one onboarded workspace with the modules reachable through the container's
// own wiring.
type auditFixture struct {
	app    *container.Container
	mailer *capturedMail

	workspace uuid.UUID
	userID    uuid.UUID
	agentID   uuid.UUID
	scope     approvaldomain.Scope
}

// capturedMail keeps what the deployment sent, which is how a test reads the invitation link
// the real mailer would have carried — the same way the person who received it does.
type capturedMail struct {
	mu   sync.Mutex
	sent []string
}

// Send implements mail.Sender.
func (m *capturedMail) Send(_ context.Context, message mail.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, message.Text)
	return nil
}

// invitationToken reads the token out of the last invitation the deployment sent.
//
// It parses the link rather than reaching into the service, because the link is exactly what
// the person clicks: a test that generated its own token would prove nothing about the path
// between them.
func (m *capturedMail) invitationToken(t *testing.T) string {
	t.Helper()

	m.mu.Lock()
	defer m.mu.Unlock()

	require.NotEmpty(t, m.sent, "no invitation was sent")
	const marker = "token="
	text := m.sent[len(m.sent)-1]
	index := strings.LastIndex(text, marker)
	require.GreaterOrEqual(t, index, 0, "the invitation carries no token: %s", text)

	token := text[index+len(marker):]
	if end := strings.IndexAny(token, "\n\r \t"); end >= 0 {
		token = token[:end]
	}
	require.NotEmpty(t, token)
	return token
}

func newAuditFixture(t *testing.T, dsn string) *auditFixture {
	t.Helper()

	ctx := t.Context()
	pool := poolFor(t, dsn)

	user, err := createVerifiedUser(t, pool)
	require.NoError(t, err)

	workspace, err := onboardWorkspace(t, pool, user)
	require.NoError(t, err)

	// The container is built with a signing key so the hub can issue and verify a webhook
	// reference, and with the encryption key so a sealed token can be written.
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
		// The key that seals a provider's API key, so a provider can be stored at all — and
		// so the test that the trail never carries that key is about a real one.
		Llm:          config.LlmConfig{SecretEncryptionKey: "local_test_secret_key_32_charact"},
		Kms:          config.KmsConfig{MasterKey: kmsKey(77), MasterKeyID: "v1"},
		Integrations: config.IntegrationConfig{OAuthStateKey: "an-integration-state-signing-key-32-chars"},
	}

	mailer := &capturedMail{}
	app, err := container.New(ctx, cfg, container.WithLogger(zap.NewNop()), container.WithMailer(mailer))
	require.NoError(t, err)
	t.Cleanup(app.Close)

	require.NotNil(t, app.Repositories.Approval, "the trail must be wired for this file to mean anything")

	// A Bolu exists in this workspace, which the grant lines name.
	agentID := firstAgentOf(t, pool, workspace)

	return &auditFixture{
		app:       app,
		mailer:    mailer,
		workspace: workspace,
		userID:    user,
		agentID:   agentID,
		scope:     approvaldomain.Scope{UserID: user, WorkspaceID: workspace},
	}
}

// trail reads the workspace's audit entries through the module's own read.
func (f *auditFixture) trail(t *testing.T, filter approvaldomain.AuditFilter) []approvaldomain.AuditRecord {
	t.Helper()

	records, err := f.repository().AuditTrail(t.Context(), f.scope, filter)
	require.NoError(t, err)
	return records
}

// repository is the trail's own store, reached through the container.
func (f *auditFixture) repository() *approvalrepo.Repository { return f.app.Repositories.Approval }

// findByAction returns the one entry for an action, failing when there is not exactly one.
func (f *auditFixture) findByAction(t *testing.T, action string) approvaldomain.AuditRecord {
	t.Helper()

	records := f.trail(t, approvaldomain.AuditFilter{Action: action})
	require.Len(t, records, 1, "exactly one line for %s", action)
	return records[0]
}

// rawAuditMetadata is one entry's stored metadata as bytes, so a test can assert a secret is
// absent from the row rather than only from the decoded map.
func (f *auditFixture) rawAuditMetadata(t *testing.T, id int64) string {
	t.Helper()

	var raw string
	require.NoError(t, f.app.DB.InScopeRead(t.Context(), database.Scope{
		UserID:      f.userID,
		WorkspaceID: f.workspace,
	}, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `SELECT metadata::text FROM audit_logs WHERE id = $1`, id).Scan(&raw)
	}))

	return raw
}

// TestAGrantChangeLeavesExactlyOneLineWithTheActor is the outbound permission surface: who
// may act as which connection is what an incident asks about, and the line names both sides.
func TestAGrantChangeLeavesExactlyOneLineWithTheActor(t *testing.T) {
	fixture := newAuditFixture(t, appDatabase(t))
	ctx := t.Context()

	hubScope := integrationdomain.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace}
	hub := fixture.app.Services.Integration
	require.NotNil(t, hub)

	connection, err := hub.Connect(ctx, hubScope, integrationdomain.ConnectInput{
		App:          "gmail",
		AccountLabel: "toko@example.com",
		AccessToken:  "a-real-token",
	})
	require.NoError(t, err)

	_, err = hub.SetGrant(ctx, hubScope, fixture.agentID, connection.ID, integrationdomain.PermissionReadWrite)
	require.NoError(t, err)

	granted := fixture.findByAction(t, "grant.set")
	require.Equal(t, fixture.userID, granted.ActorUserID, "the line names the person who allowed it")
	require.Equal(t, uuid.Nil, granted.ActorAgentID, "no Bolu acted")
	require.Equal(t, "integration", granted.ObjectType)
	require.Equal(t, connection.ID.String(), granted.ObjectID)
	require.Equal(t, fixture.agentID.String(), granted.Metadata["agent_id"])
	require.Equal(t, integrationdomain.PermissionReadWrite, granted.Metadata["permission"])
	require.Equal(t, "gmail", granted.Metadata["app"])
	require.NotZero(t, granted.CreatedAt, "the line carries when it happened")

	// The token is nowhere in the row: a trail that carried a credential would be a second
	// place to leak it from, and the epic forbids it outright.
	raw := fixture.rawAuditMetadata(t, granted.ID)
	require.NotContains(t, raw, "a-real-token")
	require.NotContains(t, granted.Metadata, "access_token")

	// Removing it is its own action, and it says what access was taken away — the question
	// asked after an incident, which a line that only said "removed" would not answer.
	require.NoError(t, hub.DeleteGrant(ctx, hubScope, fixture.agentID, connection.ID))

	removed := fixture.findByAction(t, "grant.removed")
	require.Equal(t, fixture.userID, removed.ActorUserID)
	require.Equal(t, integrationdomain.PermissionReadWrite, removed.Metadata["permission"])

	// Removing it again names something that is not there, so the caller is told rather than
	// answered as though a change happened — and no second line is written.
	require.ErrorIs(t, hub.DeleteGrant(ctx, hubScope, fixture.agentID, connection.ID), integrationdomain.ErrNotFound)
	require.Len(t, fixture.trail(t, approvaldomain.AuditFilter{Action: "grant.removed"}), 1)
}

// TestAMembershipChangeLeavesExactlyOneLineWithTheActor: who is in the workspace and what
// they may do decides who can act inside it, so both are recorded.
func TestAMembershipChangeLeavesExactlyOneLineWithTheActor(t *testing.T) {
	fixture := newAuditFixture(t, appDatabase(t))
	ctx := t.Context()

	workspaceScope := workspacedomain.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace}
	workspaces := fixture.app.Services.Workspace
	require.NotNil(t, workspaces)

	// A second member joins the way a person does: invited with a token, then accepted with
	// the one the mailer would have carried.
	invited, err := workspaces.Invite(ctx, workspaceScope, workspacedomain.RoleOwner, fixture.userID,
		"teammate@example.com", workspacedomain.RoleMember)
	require.NoError(t, err)

	sent := fixture.findByAction(t, "member.invited")
	require.Equal(t, fixture.userID, sent.ActorUserID)
	require.Equal(t, "invitation", sent.ObjectType)
	require.Equal(t, invited.ID.String(), sent.ObjectID)
	require.Equal(t, workspacedomain.RoleMember, sent.Metadata["role"])

	// The address is deliberately absent: the line names the invitation and the role it
	// grants, and an address is personal data nothing here needs.
	raw := fixture.rawAuditMetadata(t, sent.ID)
	require.NotContains(t, raw, "teammate@example.com")

	token := fixture.mailer.invitationToken(t)

	memberID := createUser(t, fixture.app.DB.PgxPool().Config().ConnString(), "teammate@example.com")
	_, err = workspaces.AcceptInvitation(ctx, token, memberID, "teammate@example.com")
	require.NoError(t, err)

	// Promoting them is its own line.
	_, err = workspaces.SetRole(ctx, workspaceScope, workspacedomain.RoleOwner, memberID, workspacedomain.RoleAdmin)
	require.NoError(t, err)

	promoted := fixture.findByAction(t, "member.role_changed")
	require.Equal(t, fixture.userID, promoted.ActorUserID)
	require.Equal(t, "member", promoted.ObjectType)
	require.Equal(t, memberID.String(), promoted.ObjectID)
	require.Equal(t, workspacedomain.RoleAdmin, promoted.Metadata["role"])

	// Removing them records the access they held, read before the row went — after it the
	// trail could only say that somebody left.
	require.NoError(t, workspaces.RemoveMember(ctx, workspaceScope, workspacedomain.RoleOwner, fixture.userID, memberID))

	dropped := fixture.findByAction(t, "member.removed")
	require.Equal(t, fixture.userID, dropped.ActorUserID)
	require.Equal(t, memberID.String(), dropped.ObjectID)
	require.Equal(t, workspacedomain.RoleAdmin, dropped.Metadata["had_role"])

	// Revoking an open invitation is access withdrawn before it was taken.
	second, err := workspaces.Invite(ctx, workspaceScope, workspacedomain.RoleOwner, fixture.userID,
		"third@example.com", workspacedomain.RoleMember)
	require.NoError(t, err)
	require.NoError(t, workspaces.RevokeInvitation(ctx, workspaceScope, workspacedomain.RoleOwner, second.ID))

	revoked := fixture.findByAction(t, "member.invitation_revoked")
	require.Equal(t, fixture.userID, revoked.ActorUserID)
	require.Equal(t, second.ID.String(), revoked.ObjectID)
}

// TestAModelConfigurationChangeLeavesExactlyOneLineAndNoKey is the one action whose payload
// is a secret, so it is also the test that the secret cannot reach the trail.
func TestAModelConfigurationChangeLeavesExactlyOneLineAndNoKey(t *testing.T) {
	fixture := newAuditFixture(t, appDatabase(t))
	ctx := t.Context()

	modelScope := llmdomain.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace}
	gateway := fixture.app.Services.LLM
	require.NotNil(t, gateway)

	const secret = "sk-live-the-actual-provider-key"

	saved, err := gateway.Upsert(ctx, modelScope, llmdomain.UpsertRequest{
		ProviderConfig: llmdomain.ProviderConfig{
			Name:    "[OI] utama",
			Adapter: "openai",
			BaseURL: "https://api.openai.com/v1",
			Model:   "gpt-4o-mini",
			APIKey:  secret,
		},
	})
	require.NoError(t, err)

	created := fixture.findByAction(t, "provider.created")
	require.Equal(t, fixture.userID, created.ActorUserID)
	require.Equal(t, "provider", created.ObjectType)
	require.Equal(t, saved.ID.String(), created.ObjectID)
	require.Equal(t, "[OI] utama", created.Metadata["name"])
	require.Equal(t, "openai", created.Metadata["adapter"])
	require.Equal(t, "gpt-4o-mini", created.Metadata["model"])
	require.Equal(t, true, created.Metadata["has_api_key"], "the line says a key is stored, not what it is")

	// The key itself is nowhere in the row, which is the epic's "never store secrets here"
	// stated as an assertion rather than as a comment.
	raw := fixture.rawAuditMetadata(t, created.ID)
	require.NotContains(t, raw, secret)
	require.NotContains(t, raw, "sk-live")

	// An edit is its own action, so the trail says when a model was changed rather than only
	// that one was created. The key is left out, which is how an edit keeps the stored one.
	_, err = gateway.Upsert(ctx, modelScope, llmdomain.UpsertRequest{
		ProviderConfig: llmdomain.ProviderConfig{
			ID:      saved.ID,
			Name:    saved.Name,
			Adapter: saved.Adapter,
			BaseURL: saved.BaseURL,
			Model:   "gpt-4o",
		},
	})
	require.NoError(t, err)

	require.Len(t, fixture.trail(t, approvaldomain.AuditFilter{Action: "provider.updated"}), 1)

	// Deleting it records the name, read before the row went.
	require.NoError(t, gateway.Delete(ctx, modelScope, saved.ID))

	deleted := fixture.findByAction(t, "provider.deleted")
	require.Equal(t, "[OI] utama", deleted.Metadata["name"])

	// Which model a Bolu thinks with is a cost decision, so the per-Bolu override is
	// recorded too — and its own key is not.
	agentScope := llmdomain.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace, AgentID: fixture.agentID}
	require.NoError(t, gateway.SetAgentModel(ctx, agentScope, llmdomain.AgentOverride{
		Model:     "gpt-4o",
		APIKey:    secret,
		MaxTokens: 4096,
	}))

	override := fixture.findByAction(t, "agent_model.changed")
	require.Equal(t, fixture.agentID.String(), override.ObjectID)
	require.Equal(t, "gpt-4o", override.Metadata["model"])
	require.Equal(t, true, override.Metadata["has_own_key"])
	require.EqualValues(t, 4096, override.Metadata["max_tokens"],
		"metadata is decoded from JSON, so a number arrives as a float")
	require.NotContains(t, fixture.rawAuditMetadata(t, override.ID), secret)
}

// auditVector is a vector of the module's own width, for a memory the trail test writes directly.
//
// The embedding is field 0, which is what the memory integration tests use to tell two vectors
// apart by distance: this test never searches, so any well-formed vector serves.
func auditVector() []float32 {
	out := make([]float32, llmdomain.EmbeddingDimensions)
	out[0] = 1

	return out
}

// TestTheTrailIsReadableByWhatAPersonAsks is E13.1's read half: a trail nobody can filter is a
// table, not an answer. Each axis is a different question an incident asks.
func TestTheTrailIsReadableByWhatAPersonAsks(t *testing.T) {
	fixture := newAuditFixture(t, appDatabase(t))
	ctx := t.Context()

	modelScope := llmdomain.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace}
	hubScope := integrationdomain.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace}
	workspaceScope := workspacedomain.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace}
	memoryScope := memorydomain.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace}

	// A spread of actions to read back.
	saved, err := fixture.app.Services.LLM.Upsert(ctx, modelScope, llmdomain.UpsertRequest{
		ProviderConfig: llmdomain.ProviderConfig{Name: "Utama", Adapter: "openai", Model: "gpt-4o-mini"},
	})
	require.NoError(t, err)

	connection, err := fixture.app.Services.Integration.Connect(ctx, hubScope, integrationdomain.ConnectInput{
		App: "gmail", AccountLabel: "toko@example.com", AccessToken: "a-token",
	})
	require.NoError(t, err)
	_, err = fixture.app.Services.Integration.SetGrant(ctx, hubScope, fixture.agentID, connection.ID, integrationdomain.PermissionRead)
	require.NoError(t, err)

	invited, err := fixture.app.Services.Workspace.Invite(ctx, workspaceScope, workspacedomain.RoleOwner, fixture.userID,
		"teammate@example.com", workspacedomain.RoleMember)
	require.NoError(t, err)

	// The fourth family the epic names: a memory a person removed. It is recorded by the memory
	// module through the same trail, through its own adapter — which is what makes "every sensitive
	// action leaves a line" a claim about the deployment rather than about three of its modules.
	//
	// The row is written through the repository and the *deletion* goes through the service, because
	// the service embeds on a write and this deployment has no provider configured: what is under
	// test here is the adapter, and a memory nobody can delete would be an assertion about the
	// embedding path instead.
	remembered, err := fixture.app.Repositories.Memory.CreateMemory(ctx, memoryScope, memorydomain.NewMemory{
		AgentID:   fixture.agentID,
		Content:   "Ongkir Bandung 15 ribu",
		Kind:      memorydomain.KindFact,
		Embedding: auditVector(),
		CreatedBy: memorydomain.SourceUser,
	})
	require.NoError(t, err)

	require.NoError(t, fixture.app.Services.Memory.Forget(ctx, memoryScope, remembered.ID))

	// Newest first, which is what a screen shows.
	everything := fixture.trail(t, approvaldomain.AuditFilter{Limit: 50})
	require.Len(t, everything, 4, "four actions by a person so far")
	require.Equal(t, "memory.deleted", everything[0].Action)
	require.Equal(t, "member.invited", everything[1].Action)
	require.Equal(t, "grant.set", everything[2].Action)
	require.Equal(t, "provider.created", everything[3].Action)

	// Every line names the person who made it — the audit's own gate, asserted over the whole trail
	// rather than one family at a time: a line that says what changed but not who changed it cannot
	// answer the question the trail exists for, and the four modules have four separate adapters
	// where a dropped actor is exactly the mistake that would go unnoticed.
	for _, entry := range everything {
		require.Equal(t, fixture.userID, entry.ActorUserID,
			"%s must name the person who made it", entry.Action)
		require.False(t, entry.CreatedAt.IsZero(), "%s must record when", entry.Action)
	}

	// The memory line names the memory, so "who removed this fact" is answerable from the trail.
	byMemory := fixture.trail(t, approvaldomain.AuditFilter{ObjectID: remembered.ID.String()})
	require.Len(t, byMemory, 1)
	require.Equal(t, "memory.deleted", byMemory[0].Action)
	require.Equal(t, fixture.userID, byMemory[0].ActorUserID)
	require.Equal(t, "memory", byMemory[0].ObjectType)

	// By action: one verb, and by prefix: every verb of one family — which is what "show me
	// the grant history" asks, and what would otherwise need the caller to enumerate verbs.
	require.Len(t, fixture.trail(t, approvaldomain.AuditFilter{Action: "grant.set"}), 1)
	require.Len(t, fixture.trail(t, approvaldomain.AuditFilter{Action: "grant."}), 1)

	// By the object that was touched, which is the question the trail exists to answer.
	byProvider := fixture.trail(t, approvaldomain.AuditFilter{ObjectType: "provider"})
	require.Len(t, byProvider, 1)
	require.Equal(t, saved.ID.String(), byProvider[0].ObjectID)

	byInvitation := fixture.trail(t, approvaldomain.AuditFilter{ObjectID: invited.ID.String()})
	require.Len(t, byInvitation, 1)
	require.Equal(t, "member.invited", byInvitation[0].Action)

	// By the person who acted. A grant change is a person's act — no Bolu may widen its own
	// permissions — so this read finds every line a person made.
	byActor := fixture.trail(t, approvaldomain.AuditFilter{ActorUserID: fixture.userID})
	require.Len(t, byActor, 4, "every action so far was a person's")

	granted := fixture.trail(t, approvaldomain.AuditFilter{Action: "grant.set"})
	require.Len(t, granted, 1)
	require.Equal(t, fixture.userID, granted[0].ActorUserID, "a person widened the permission")
	require.Equal(t, fixture.agentID.String(), granted[0].Metadata["agent_id"],
		"the Bolu it concerns is named in the detail, not claimed as the actor")

	// The Bolu axis is the first question of an incident, and it finds the lines a Bolu
	// caused. One is recorded here because in this workspace nothing else has yet: a draft a
	// Bolu proposes is the action that carries an agent actor, and the approval module's own
	// test covers that path end to end.
	require.NoError(t, fixture.repository().RecordAudit(ctx, fixture.scope, approvaldomain.AuditEntry{
		Action:       "draft.created",
		ObjectType:   "draft",
		ObjectID:     uuid.NewString(),
		ActorAgentID: fixture.agentID,
		Metadata:     map[string]any{"action_kind": "gmail.send"},
	}))

	byAgent := fixture.trail(t, approvaldomain.AuditFilter{ActorAgentID: fixture.agentID})
	require.Len(t, byAgent, 1)
	require.Equal(t, "draft.created", byAgent[0].Action)
	require.Equal(t, uuid.Nil, byAgent[0].ActorUserID, "no person acted")
	require.Len(t, fixture.trail(t, approvaldomain.AuditFilter{Limit: 50}), 5,
		"four a person made, and the one a Bolu proposed")

	// By window. Until is exclusive, so two windows that abut hold every entry exactly once —
	// which is what makes paging by time safe.
	now := time.Now()
	require.Empty(t, fixture.trail(t, approvaldomain.AuditFilter{Since: now.Add(time.Hour)}))
	require.Len(t, fixture.trail(t, approvaldomain.AuditFilter{Until: now.Add(time.Hour)}), 5)

	// Two abutting windows hold every entry exactly once.
	past := fixture.trail(t, approvaldomain.AuditFilter{Until: now.Add(-time.Second)})
	present := fixture.trail(t, approvaldomain.AuditFilter{Since: now.Add(-time.Second), Until: now.Add(time.Hour)})
	require.Len(t, append(append([]approvaldomain.AuditRecord{}, past...), present...), 5,
		"the window read holds every entry the unfiltered read does")

	// The read is this workspace's alone, whatever the filter asks for.
	other := newAuditFixture(t, appDatabase(t))
	require.Empty(t, other.trail(t, approvaldomain.AuditFilter{Limit: 50}))
	require.Empty(t, other.trail(t, approvaldomain.AuditFilter{ObjectID: saved.ID.String()}),
		"naming another workspace's object by id does not reach it")

	// A limit nobody should be able to ask for is replaced rather than sent on.
	require.LessOrEqual(t, len(fixture.trail(t, approvaldomain.AuditFilter{Limit: 100000})), 200)
}

// TestAnEntryThatWillNotEncodeIsStillRecorded: the fact that something happened is the
// load-bearing part of a trail line, so detail that cannot be rendered costs the detail and
// never the line.
func TestAnEntryThatWillNotEncodeIsStillRecorded(t *testing.T) {
	fixture := newAuditFixture(t, appDatabase(t))
	ctx := t.Context()

	require.NoError(t, fixture.repository().RecordAudit(ctx, fixture.scope, approvaldomain.AuditEntry{
		Action:     "grant.set",
		ObjectType: "integration",
		ObjectID:   uuid.NewString(),
		// A channel is not JSON-encodable, which is what a caller passing a live object by
		// mistake would look like.
		Metadata: map[string]any{"unencodable": make(chan int)},
	}))

	recorded := fixture.findByAction(t, "grant.set")
	require.Equal(t, "integration", recorded.ObjectType)
	require.Empty(t, recorded.Metadata, "the detail was dropped and the line was not")
}
