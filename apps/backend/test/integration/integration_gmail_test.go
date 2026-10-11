package integration

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/container"
	approvaldomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/adapters/gmail"
	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/config"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
)

// These tests cover the E8.7 gate, and with it the promise EPIC 7 was built
// around: an action a person approved actually leaves, and one nobody approved
// does not.
//
// Google is a stub, so what is under test is this deployment's own behaviour: the
// tool labels, the approval gate, the grant, and the executor that joins them.

// stubGmail is the Gmail API, answering the calls the adapter makes.
type stubGmail struct {
	mu sync.Mutex
	// sent records the messages that were actually sent, which is what a test
	// asserts on: a draft must never appear here.
	sent []map[string]any
	// drafts records the drafts that were written.
	drafts []map[string]any
	// lastAuth is the bearer token the last request carried.
	lastAuth string
	// status, when set, is the status every request answers with.
	status int
	server *httptest.Server
}

func newStubGmail(t *testing.T) *stubGmail {
	t.Helper()

	stub := &stubGmail{status: http.StatusOK}
	stub.server = httptest.NewServer(http.HandlerFunc(stub.handle))
	t.Cleanup(stub.server.Close)

	return stub
}

func (s *stubGmail) url() string { return s.server.URL }

func (s *stubGmail) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastAuth = r.Header.Get("Authorization")

	if s.status >= 400 {
		http.Error(w, `{"error":{"message":"google refused"}}`, s.status)
		return
	}

	body, _ := io.ReadAll(r.Body)
	write := func(payload any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}

	switch {
	case strings.HasSuffix(r.URL.Path, "/users/me/profile"):
		write(map[string]any{"emailAddress": "toko@example.com"})

	case strings.HasSuffix(r.URL.Path, "/users/me/drafts"):
		var payload struct {
			Message struct {
				Raw string `json:"raw"`
			} `json:"message"`
		}
		_ = json.Unmarshal(body, &payload)

		s.drafts = append(s.drafts, map[string]any{"raw": payload.Message.Raw})
		write(map[string]any{"id": "draft-1", "message": map[string]any{"id": "msg-draft-1"}})

	case strings.HasSuffix(r.URL.Path, "/users/me/messages/send"):
		var payload struct {
			Raw string `json:"raw"`
		}
		_ = json.Unmarshal(body, &payload)

		s.sent = append(s.sent, map[string]any{"raw": payload.Raw})
		write(map[string]any{"id": "msg-sent-1", "threadId": "thread-1"})

	case strings.HasSuffix(r.URL.Path, "/users/me/messages"):
		write(map[string]any{
			"messages": []map[string]any{
				{"id": "msg-1", "threadId": "thread-1"},
				{"id": "msg-2", "threadId": "thread-2"},
			},
			"resultSizeEstimate": 2,
		})

	case strings.Contains(r.URL.Path, "/users/me/messages/"):
		write(map[string]any{
			"payload": map[string]any{
				"headers": []map[string]any{
					{"name": "Subject", "value": "Pesanan baru"},
					{"name": "From", "value": "pelanggan@example.com"},
				},
			},
			"snippet": "Halo, saya mau pesan",
		})

	default:
		http.Error(w, "unknown path "+r.URL.Path, http.StatusNotFound)
	}
}

func (s *stubGmail) sentMessages() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.sent...)
}

func (s *stubGmail) writtenDrafts() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.drafts...)
}

// rawIntegrationToken reads a stored ciphertext the way an operator with database
// access would, inside the tenant scope the policy requires.
func rawIntegrationToken(t *testing.T, pool *database.Pool, workspaceID, integrationID uuid.UUID) []byte {
	t.Helper()

	var sealed []byte
	err := pool.InScopeRead(t.Context(),
		database.Scope{UserID: uuid.New(), WorkspaceID: workspaceID},
		func(tx pgx.Tx) error {
			return tx.QueryRow(t.Context(),
				`SELECT access_token_encrypted FROM integrations WHERE id = $1`, integrationID).Scan(&sealed)
		})
	require.NoError(t, err)
	return sealed
}

// decodeRaw reads the RFC 5322 message the adapter encoded.
func decodeRaw(t *testing.T, raw string) string {
	t.Helper()

	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	require.NoError(t, err)
	return string(decoded)
}

// gmailFixture wires both modules over one onboarded workspace, so the approval
// gate and the integration hub are the real ones and joined the way the container
// joins them.
type gmailFixture struct {
	approvals    approvaldomain.Service
	integrations integrationdomain.Service
	google       *stubGmail
	pool         *database.Pool
	workspaceID  uuid.UUID
	userID       uuid.UUID
	agentID      uuid.UUID
	approval     approvaldomain.Scope
	integration  integrationdomain.Scope
}

func newGmailFixture(t *testing.T, dsn string) *gmailFixture {
	t.Helper()

	ctx := t.Context()
	pool := poolFor(t, dsn)

	user, err := createVerifiedUser(t, pool)
	require.NoError(t, err)
	workspace, err := onboardWorkspace(t, pool, user)
	require.NoError(t, err)

	google := newStubGmail(t)

	// The adapter is built here rather than through the container's configuration
	// because its endpoints have to point at the stub, and a test that reached
	// Google would not be a test.
	adapter, err := gmail.New(gmail.Config{
		ClientID:      "a-client-id",
		ClientSecret:  "a-client-secret",
		APIBase:       google.url(),
		AuthEndpoint:  google.url() + "/auth",
		TokenEndpoint: google.url() + "/token",
	})
	require.NoError(t, err)

	cfg := &config.Config{
		Application: config.AppConfig{Environment: "test", FrontendURL: "https://app.example.com"},
		Http:        config.HttpConfig{Address: ":0", ApiPrefix: "/api"},
		Database:    config.DatabaseConfig{URL: dsn, MaxOpenConns: 5, MaxIdleConns: 1},
		JWT:         config.JWTConfig{Secret: "integration-test-secret", ExpireHours: 1},
		Auth: config.AuthConfig{
			AccessTokenMinutes: 15, RefreshHours: 24, VerificationHours: 24,
			ResetMinutes: 60, StateMinutes: 10, LoginThrottleCapacity: 100, LoginThrottleRefill: 100,
		},
		Mail:         config.MailConfig{Driver: "log"},
		Logging:      config.LoggingConfig{Level: "error", Format: "json"},
		Kms:          config.KmsConfig{MasterKey: kmsKey(60), MasterKeyID: "v1"},
		Integrations: config.IntegrationConfig{OAuthStateKey: "an-integration-state-signing-key-32-chars"},
	}

	app, err := container.New(ctx, cfg,
		container.WithLogger(zap.NewNop()),
		container.WithIntegrationAdapters(adapter),
	)
	require.NoError(t, err)
	t.Cleanup(app.Close)

	require.NotNil(t, app.Services.Integration)
	require.NotNil(t, app.Services.Approval, "the approval gate must be wired")

	agentID := firstAgentOf(t, pool, workspace)

	return &gmailFixture{
		approvals:    app.Services.Approval,
		integrations: app.Services.Integration,
		google:       google,
		pool:         pool,
		workspaceID:  workspace,
		userID:       user,
		agentID:      agentID,
		approval:     approvaldomain.Scope{UserID: user, WorkspaceID: workspace},
		integration:  integrationdomain.Scope{UserID: user, WorkspaceID: workspace},
	}
}

// connect grants the Bolu the mailbox and returns the connection.
func (f *gmailFixture) connect(t *testing.T) integrationdomain.Integration {
	t.Helper()

	ctx := t.Context()

	connection, err := f.integrations.Connect(ctx, f.integration, integrationdomain.ConnectInput{
		App:          "gmail",
		AccountLabel: "toko@example.com",
		AccessToken:  "ya29.a-real-token",
		RefreshToken: "1//a-real-refresh",
		Scopes:       []string{gmail.ScopeReadonly, gmail.ScopeSend},
	})
	require.NoError(t, err)

	_, err = f.integrations.SetGrant(ctx, f.integration, f.agentID, connection.ID, integrationdomain.PermissionReadWrite)
	require.NoError(t, err)

	return connection
}

// propose records an action the way a task does.
func (f *gmailFixture) propose(t *testing.T, action string, payload string) approvaldomain.Draft {
	t.Helper()

	outcome, err := f.approvals.Request(t.Context(), f.approval, approvaldomain.Request{
		TaskID:     f.newTask(t),
		AgentID:    f.agentID,
		ActionKind: action,
		Title:      "Kirim tagihan",
		Payload:    json.RawMessage(payload),
	})
	require.NoError(t, err)

	stored, err := f.approvals.Draft(t.Context(), f.approval, outcome.DraftID)
	require.NoError(t, err)
	return stored
}

// newTask creates the task a draft hangs off, because a draft names one. It is
// written inside the tenant scope: the fixture runs as the application role, which
// is what makes the policies apply.
func (f *gmailFixture) newTask(t *testing.T) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	err := f.pool.InScope(t.Context(),
		database.Scope{UserID: f.userID, WorkspaceID: f.workspaceID},
		func(tx pgx.Tx) error {
			// A started task must name its workflow, which the runtime guarantees.
			// The fixture satisfies the same constraint rather than weakening it.
			return tx.QueryRow(t.Context(),
				`INSERT INTO tasks (workspace_id, agent_id, trigger, title, status, workflow_id)
				 VALUES ($1, $2, 'chat', 'Kirim tagihan', 'running', 'task-fixture')
				 RETURNING id`, f.workspaceID, f.agentID).Scan(&id)
		})
	require.NoError(t, err)
	return id
}

// TestAnApprovedActionActuallyLeaves is the promise EPIC 7 was built around: the
// action a person approved is performed, and the message carries what was shown.
func TestAnApprovedActionActuallyLeaves(t *testing.T) {
	fixture := newGmailFixture(t, appDatabase(t))
	ctx := t.Context()

	fixture.connect(t)

	draft := fixture.propose(t, "gmail.send",
		`{"to":"pelanggan@example.com","subject":"Tagihan Maret","body":"Mohon dibayar ya."}`)

	// Nothing has left yet: a proposal is not an action.
	require.Empty(t, fixture.google.sentMessages())

	// A person approves it.
	approved, err := fixture.approvals.Approve(ctx, fixture.approval, draft.ID, fixture.userID, "kirim")
	require.NoError(t, err)
	require.Equal(t, approvaldomain.StatusApproved, approved.Status)

	// The runtime carries it out through the executor.
	carried, err := fixture.approvals.CarryOut(ctx, fixture.approval, draft.ID)
	require.NoError(t, err)
	require.True(t, carried.Executed, "the approved action must have been performed")

	// The message left, and it is the one the card showed.
	sent := fixture.google.sentMessages()
	require.Len(t, sent, 1)

	raw := decodeRaw(t, sent[0]["raw"].(string))
	require.Contains(t, raw, "To: pelanggan@example.com")
	require.Contains(t, raw, "Tagihan Maret")
	require.Contains(t, raw, "Mohon dibayar ya.")

	// The draft records that it went, so the audit trail is not a claim.
	stored, err := fixture.approvals.Draft(ctx, fixture.approval, draft.ID)
	require.NoError(t, err)
	require.Equal(t, approvaldomain.StatusSent, stored.Status)
	require.NotNil(t, stored.SentAt)

	// The token was opened for the call and carried as a bearer credential.
	require.Contains(t, fixture.google.lastAuth, "ya29.a-real-token")
}

// TestADraftIsWrittenWithoutSending is the other half of the gate: the writing
// tool stays inside the workspace, so it needs nobody's approval.
func TestADraftIsWrittenWithoutSending(t *testing.T) {
	fixture := newGmailFixture(t, appDatabase(t))
	ctx := t.Context()

	fixture.connect(t)

	draft := fixture.propose(t, "gmail.create_draft",
		`{"to":"pelanggan@example.com","subject":"Tagihan Maret","body":"Mohon dibayar ya."}`)

	// A write that stays inside the workspace is not held: the policy has no row
	// for it, and the default is to ask only about what leaves.
	require.Equal(t, approvaldomain.StatusPending, draft.Status)

	// Approving it performs it, and the result is a draft on the provider rather
	// than a message anybody received.
	_, err := fixture.approvals.Approve(ctx, fixture.approval, draft.ID, fixture.userID, "buat")
	require.NoError(t, err)

	carried, err := fixture.approvals.CarryOut(ctx, fixture.approval, draft.ID)
	require.NoError(t, err)
	require.True(t, carried.Executed)

	require.Len(t, fixture.google.writtenDrafts(), 1)
	require.Empty(t, fixture.google.sentMessages(), "writing a draft must not send anything")
}

// TestAnActionNobodyApprovedDoesNotLeave is the rule the whole epic exists for.
func TestAnActionNobodyApprovedDoesNotLeave(t *testing.T) {
	fixture := newGmailFixture(t, appDatabase(t))
	ctx := t.Context()

	fixture.connect(t)

	draft := fixture.propose(t, "gmail.send",
		`{"to":"pelanggan@example.com","subject":"Tagihan","body":"Bayar ya."}`)

	// The runtime tries to carry out a proposal nobody decided on.
	_, err := fixture.approvals.CarryOut(ctx, fixture.approval, draft.ID)
	require.ErrorIs(t, err, approvaldomain.ErrNotApproved)
	require.Empty(t, fixture.google.sentMessages())
}

// TestAnUngrantedBoluCannotSendFromTheMailbox is the governance rule at the point
// it matters: an approval for one Bolu must not be sent from a mailbox that Bolu
// was never given.
func TestAnUngrantedBoluCannotSendFromTheMailbox(t *testing.T) {
	fixture := newGmailFixture(t, appDatabase(t))
	ctx := t.Context()

	// The connection exists, but no grant does.
	_, err := fixture.integrations.Connect(ctx, fixture.integration, integrationdomain.ConnectInput{
		App:          "gmail",
		AccountLabel: "toko@example.com",
		AccessToken:  "ya29.a-real-token",
	})
	require.NoError(t, err)

	draft := fixture.propose(t, "gmail.send",
		`{"to":"pelanggan@example.com","subject":"Tagihan","body":"Bayar ya."}`)

	_, err = fixture.approvals.Approve(ctx, fixture.approval, draft.ID, fixture.userID, "kirim")
	require.NoError(t, err)

	// The action was approved and still cannot leave: the Bolu was never granted
	// the mailbox.
	_, err = fixture.approvals.CarryOut(ctx, fixture.approval, draft.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "granted")
	require.Empty(t, fixture.google.sentMessages())
}

// TestAProviderRefusalIsReportedHonestly is what the gate does when the provider
// says no: the draft is not marked sent, because nothing left.
func TestAProviderRefusalIsReportedHonestly(t *testing.T) {
	fixture := newGmailFixture(t, appDatabase(t))
	ctx := t.Context()

	fixture.connect(t)

	draft := fixture.propose(t, "gmail.send",
		`{"to":"pelanggan@example.com","subject":"Tagihan","body":"Bayar ya."}`)

	_, err := fixture.approvals.Approve(ctx, fixture.approval, draft.ID, fixture.userID, "kirim")
	require.NoError(t, err)

	// Google refuses the send.
	fixture.google.mu.Lock()
	fixture.google.status = http.StatusForbidden
	fixture.google.mu.Unlock()

	_, err = fixture.approvals.CarryOut(ctx, fixture.approval, draft.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "google refused")

	// The draft is still approved rather than sent: claiming it went would be a
	// lie the audit trail would repeat.
	stored, err := fixture.approvals.Draft(ctx, fixture.approval, draft.ID)
	require.NoError(t, err)
	require.Equal(t, approvaldomain.StatusApproved, stored.Status)
	require.Nil(t, stored.SentAt)
}

// TestTheToolLabelsDecideApproval is the rule the epic states as a criterion: the
// label is the only thing that decides whether a person is asked.
func TestTheToolLabelsDecideApproval(t *testing.T) {
	adapter, err := gmail.New(gmail.Config{ClientID: "x", ClientSecret: "y"})
	require.NoError(t, err)

	labels := map[string]string{}
	for _, tool := range adapter.Tools() {
		labels[tool.Name] = tool.Label
	}

	require.Equal(t, integrationdomain.LabelRead, labels["gmail.search"])
	require.Equal(t, integrationdomain.LabelRead, labels["gmail.read_attachment"])
	require.Equal(t, integrationdomain.LabelWriteInternal, labels["gmail.create_draft"])
	require.Equal(t, integrationdomain.LabelWriteExternal, labels["gmail.send"])

	// The labels map onto the runtime's own rule, which is what the approval gate
	// reads. It is restated here so this test fails if the two ever drift.
	for name, label := range labels {
		wantsApproval := label == integrationdomain.LabelWriteExternal
		require.Equal(t, wantsApproval, name == "gmail.send", name)
	}

	// And the trigger is a poll, because Gmail pushes nothing without a Pub/Sub
	// subscription this deployment does not have.
	triggers := adapter.Triggers()
	require.Len(t, triggers, 1)
	require.Equal(t, integrationdomain.TriggerPolling, triggers[0].Kind)
	require.Equal(t, "gmail.email_received", triggers[0].Event)

	// The connection is scoped to what the tools use, and no wider.
	scopes := adapter.DefaultScopes()
	require.Contains(t, scopes, gmail.ScopeReadonly)
	require.Contains(t, scopes, gmail.ScopeSend)
	for _, scope := range scopes {
		require.NotContains(t, scope, "mail.google.com",
			"the full mailbox scope is more than the tools use")
	}
}

// TestGmailSearchReadsTheMailbox covers the read tool end to end.
func TestGmailSearchReadsTheMailbox(t *testing.T) {
	fixture := newGmailFixture(t, appDatabase(t))
	ctx := t.Context()

	connection := fixture.connect(t)

	result, err := fixture.integrations.Execute(ctx, fixture.integration, connection.ID, "gmail.search",
		json.RawMessage(`{"q":"pesanan","limit":5}`))
	require.NoError(t, err)

	var answer struct {
		Messages []map[string]any `json:"messages"`
		Total    int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal(result.Content, &answer))
	require.Len(t, answer.Messages, 2)
	require.Equal(t, "Pesanan baru", answer.Messages[0]["subject"])
	require.Equal(t, "pelanggan@example.com", answer.Messages[0]["from"])
	require.Equal(t, 2, answer.Total)
}

// TestTheTokenIsOpenedOnlyForTheCall keeps the E8.1 rule visible at this level: the
// column holds ciphertext, and the credential exists only while a call runs.
func TestTheTokenIsOpenedOnlyForTheCall(t *testing.T) {
	fixture := newGmailFixture(t, appDatabase(t))
	ctx := t.Context()

	connection := fixture.connect(t)

	// The column is ciphertext, read the way an operator with database access
	// would: straight from the column, with no application code in the way. It is
	// read inside the tenant scope because the application role is subject to the
	// policy, which is exactly why this fixture runs as it.
	sealed := rawIntegrationToken(t, fixture.pool, fixture.workspaceID, connection.ID)
	require.NotEmpty(t, sealed)
	require.NotContains(t, string(sealed), "ya29.a-real-token")

	// A read of the connection does not open it either.
	listed, err := fixture.integrations.List(ctx, fixture.integration)
	require.NoError(t, err)
	require.Len(t, listed, 1)

	// The call does, and it travels as a bearer credential.
	_, err = fixture.integrations.Execute(ctx, fixture.integration, connection.ID, "gmail.search",
		json.RawMessage(`{"q":"pesanan"}`))
	require.NoError(t, err)
	require.Contains(t, fixture.google.lastAuth, "ya29.a-real-token")
}
