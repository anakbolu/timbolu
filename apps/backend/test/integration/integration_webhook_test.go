package integration

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/container"
	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/config"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
)

// These tests cover the E8.4 gate: a delivery that is not signed by the provider
// is refused and queued nowhere, a genuine one is queued and answered fast, and a
// redelivery of the same event stays one row.
//
// The endpoint is driven over the real router, so what is under test is the HTTP
// path a provider actually takes.

// webhookSecret is the shared secret the stub provider signs with.
const webhookSecret = "a-webhook-signing-secret"

// webhookAdapter is a provider that pushes events, signing them the way a real
// one does: an HMAC over the raw body in a header.
type webhookAdapter struct {
	// seen counts the deliveries that reached verification, so a test can prove a
	// refused request never got that far.
	seen int
	mu   sync.Mutex
}

func (w *webhookAdapter) App() string { return "gmail" }

func (w *webhookAdapter) Tools() []integrationdomain.ToolSpec {
	return []integrationdomain.ToolSpec{{
		Name: "gmail.search", Label: integrationdomain.LabelRead, Description: "Cari email",
		Schema: json.RawMessage(`{"type":"object"}`),
	}}
}

func (w *webhookAdapter) Triggers() []integrationdomain.TriggerSpec {
	return []integrationdomain.TriggerSpec{
		{App: "gmail", Kind: integrationdomain.TriggerWebhook, Event: "gmail.email_received", Description: "Email masuk"},
	}
}

func (w *webhookAdapter) Execute(context.Context, integrationdomain.Call) (integrationdomain.Result, error) {
	return integrationdomain.Result{Content: json.RawMessage(`{}`)}, nil
}

func (w *webhookAdapter) VerifyWebhook(_ context.Context, request integrationdomain.WebhookRequest) ([]integrationdomain.TriggerEvent, error) {
	w.mu.Lock()
	w.seen++
	w.mu.Unlock()

	signature := request.Header("x-webhook-signature")
	if !validSignature(request.Body, signature) {
		return nil, integrationdomain.ErrSignatureInvalid
	}

	var body struct {
		MessageID string `json:"message_id"`
		Subject   string `json:"subject"`
	}
	if err := json.Unmarshal(request.Body, &body); err != nil {
		return nil, err
	}

	return []integrationdomain.TriggerEvent{{
		App:     "gmail",
		Event:   "gmail.email_received",
		EventID: body.MessageID,
		Payload: request.Body,
	}}, nil
}

// pullOnlyAdapter is an application that offers tools but pushes nothing. It is
// what makes "this app does not send webhooks" reachable with a reference this
// deployment actually issued.
type pullOnlyAdapter struct{}

func (pullOnlyAdapter) App() string { return "notion" }

func (pullOnlyAdapter) Tools() []integrationdomain.ToolSpec {
	return []integrationdomain.ToolSpec{{
		Name: "notion.search", Label: integrationdomain.LabelRead, Description: "Cari halaman",
		Schema: json.RawMessage(`{"type":"object"}`),
	}}
}

func (pullOnlyAdapter) Triggers() []integrationdomain.TriggerSpec { return nil }

func (pullOnlyAdapter) Execute(context.Context, integrationdomain.Call) (integrationdomain.Result, error) {
	return integrationdomain.Result{Content: json.RawMessage(`{}`)}, nil
}

func (w *webhookAdapter) deliveries() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen
}

// validSignature is the provider's own check, restated so the stub signs and
// verifies the same way.
func validSignature(body []byte, signature string) bool {
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

// sign renders what the provider sends.
func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// webhookFixture is the running API with a stub provider that pushes events.
type webhookFixture struct {
	container *container.Container
	app       *fiber.App
	service   integrationdomain.Service
	provider  *webhookAdapter
	pool      *database.Pool
	scope     integrationdomain.Scope
	workspace string
	// integration is the connection the webhook URL belongs to.
	integration integrationdomain.Integration
	// signed caches the reference the service issued, so each request reuses it.
	signed string
}

func newWebhookFixture(t *testing.T, dsn string) *webhookFixture {
	t.Helper()

	ctx := t.Context()
	pool := poolFor(t, dsn)

	user, err := createVerifiedUser(t, pool)
	require.NoError(t, err)
	workspace, err := onboardWorkspace(t, pool, user)
	require.NoError(t, err)

	provider := &webhookAdapter{}

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
		Kms:          config.KmsConfig{MasterKey: kmsKey(30), MasterKeyID: "v1"},
		Integrations: config.IntegrationConfig{OAuthStateKey: "an-integration-state-signing-key-32-chars"},
	}

	app, err := container.New(ctx, cfg,
		container.WithLogger(zap.NewNop()),
		container.WithIntegrationAdapters(provider, pullOnlyAdapter{}),
	)
	require.NoError(t, err)
	t.Cleanup(app.Close)

	scope := integrationdomain.Scope{UserID: user, WorkspaceID: workspace}

	// A connected account, so the webhook URL has something to belong to.
	connection, err := app.Services.Integration.Connect(ctx, scope, integrationdomain.ConnectInput{
		App:          "gmail",
		AccountLabel: "toko@example.com",
		AccessToken:  "a-real-token",
		Scopes:       []string{"gmail.readonly"},
	})
	require.NoError(t, err)

	return &webhookFixture{
		container:   app,
		app:         app.App(),
		service:     app.Services.Integration,
		provider:    provider,
		pool:        pool,
		scope:       scope,
		workspace:   workspace.String(),
		integration: connection,
	}
}

// post sends a delivery to the endpoint.
func (f *webhookFixture) post(t *testing.T, body string, headers map[string]string) (int, string) {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, "/api/webhooks/gmail"+f.query(t), strings.NewReader(body))
	request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	for key, value := range headers {
		request.Header.Set(key, value)
	}

	response, err := f.app.Test(request, -1)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()

	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, string(raw)
}

// query returns the signed reference the endpoint needs, taken from the URL the
// service issues.
func (f *webhookFixture) query(t *testing.T) string {
	t.Helper()

	if f.signed == "" {
		url, err := f.service.WebhookURL(t.Context(), f.scope, f.integration.ID, "https://app.example.com")
		require.NoError(t, err)

		_, after, found := strings.Cut(url, "?")
		require.True(t, found, url)
		f.signed = "?" + after
	}
	return f.signed
}

// TestASignedDeliveryIsQueued is the happy path: the provider's signature and the
// URL's reference are both checked, and the event waits in the queue.
func TestASignedDeliveryIsQueued(t *testing.T) {
	fixture := newWebhookFixture(t, migratedDatabase(t))
	ctx := t.Context()

	body := `{"message_id":"msg-1","subject":"Pesanan baru"}`

	status, raw := fixture.post(t, body, map[string]string{"X-Webhook-Signature": sign([]byte(body))})
	require.Equal(t, http.StatusOK, status, raw)

	// It is queued, unprocessed, and carries the provider's body whole.
	pending, err := fixture.service.PendingWebhooks(ctx, fixture.scope, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "gmail.email_received", pending[0].Event)
	require.Equal(t, "msg-1", pending[0].DeliveryID)
	require.Equal(t, fixture.integration.ID, pending[0].IntegrationID)
	require.JSONEq(t, body, string(pending[0].Payload))
	require.Zero(t, pending[0].Attempts)

	// The worker marks it done, and it leaves the queue.
	require.NoError(t, fixture.service.WebhookProcessed(ctx, fixture.scope, pending[0].ID))

	pending, err = fixture.service.PendingWebhooks(ctx, fixture.scope, 10)
	require.NoError(t, err)
	require.Empty(t, pending)
}

// TestAnUnsignedDeliveryIsRefused is the first acceptance criterion: a request
// that is not from the provider is rejected and queued nowhere.
func TestAnUnsignedDeliveryIsRefused(t *testing.T) {
	fixture := newWebhookFixture(t, migratedDatabase(t))
	ctx := t.Context()

	body := `{"message_id":"msg-2","subject":"Palsu"}`

	cases := map[string]map[string]string{
		"no signature at all":  {},
		"an empty signature":   {"X-Webhook-Signature": ""},
		"a wrong signature":    {"X-Webhook-Signature": "deadbeef"},
		"a signature of bytes": {"X-Webhook-Signature": sign([]byte("different body"))},
	}

	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			status, raw := fixture.post(t, body, headers)
			require.Equal(t, http.StatusUnauthorized, status, raw)
			require.Contains(t, raw, "invalid_signature")
		})
	}

	// Nothing reached the queue, and the provider's own check was the one that
	// refused: the deliveries counted are exactly the ones that got that far.
	pending, err := fixture.service.PendingWebhooks(ctx, fixture.scope, 10)
	require.NoError(t, err)
	require.Empty(t, pending)
	require.Equal(t, len(cases), fixture.provider.deliveries())
}

// TestAReferenceThisDeploymentNeverIssuedIsRefused keeps the URL from being
// usable as a way in: without it, a delivery cannot name a tenant at all.
func TestAReferenceThisDeploymentNeverIssuedIsRefused(t *testing.T) {
	fixture := newWebhookFixture(t, migratedDatabase(t))

	body := `{"message_id":"msg-3"}`
	good := sign([]byte(body))

	for name, query := range map[string]string{
		"no reference":        "",
		"a made up reference": "?ref=not.a.reference",
		"a tampered one":      "?ref=AAAA.BBBB",
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/webhooks/gmail"+query, strings.NewReader(body))
			request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
			request.Header.Set("X-Webhook-Signature", good)

			response, err := fixture.app.Test(request, -1)
			require.NoError(t, err)
			defer func() { _ = response.Body.Close() }()

			require.Equal(t, http.StatusUnauthorized, response.StatusCode)
		})
	}

	// The reference is what failed, so the provider's check was never reached.
	require.Zero(t, fixture.provider.deliveries())
}

// TestARedeliveryIsProcessedOnce is the second acceptance criterion: providers
// retry, and a retry is not a second event.
func TestARedeliveryIsProcessedOnce(t *testing.T) {
	fixture := newWebhookFixture(t, migratedDatabase(t))
	ctx := t.Context()

	body := `{"message_id":"msg-4","subject":"Pesanan baru"}`
	headers := map[string]string{"X-Webhook-Signature": sign([]byte(body))}

	status, _ := fixture.post(t, body, headers)
	require.Equal(t, http.StatusOK, status)

	// The provider did not get its answer and tries again, twice.
	for i := 0; i < 2; i++ {
		status, raw := fixture.post(t, body, headers)
		require.Equal(t, http.StatusOK, status,
			"a duplicate must still be accepted, or the provider retries forever: %s", raw)
	}

	pending, err := fixture.service.PendingWebhooks(ctx, fixture.scope, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1, "three deliveries of one event are one row")

	// A different event from the same provider is a second row.
	other := `{"message_id":"msg-5","subject":"Pesanan lain"}`
	status, _ = fixture.post(t, other, map[string]string{"X-Webhook-Signature": sign([]byte(other))})
	require.Equal(t, http.StatusOK, status)

	pending, err = fixture.service.PendingWebhooks(ctx, fixture.scope, 10)
	require.NoError(t, err)
	require.Len(t, pending, 2)
}

// TestADeliveryIsAnsweredWithoutWaitingForTheWork is why the queue exists: the
// provider is on a timeout, so the endpoint only decides and remembers.
func TestADeliveryIsAnsweredWithoutWaitingForTheWork(t *testing.T) {
	fixture := newWebhookFixture(t, migratedDatabase(t))

	body := `{"message_id":"msg-6","subject":"Cepat"}`

	started := time.Now()
	status, _ := fixture.post(t, body, map[string]string{"X-Webhook-Signature": sign([]byte(body))})
	elapsed := time.Since(started)

	require.Equal(t, http.StatusOK, status)
	require.Less(t, elapsed, time.Second,
		"the endpoint must not do the work a provider is waiting on")
}

// TestAnAppThatPushesNothingIsRefused keeps a URL from being aimed at an
// application that cannot answer it.
func TestAnAppThatPushesNothingIsRefused(t *testing.T) {
	fixture := newWebhookFixture(t, migratedDatabase(t))
	ctx := t.Context()

	// A connection to an application that offers tools but pushes nothing, with
	// a reference this deployment really issued for it.
	pull, err := fixture.service.Connect(ctx, fixture.scope, integrationdomain.ConnectInput{
		App:          "notion",
		AccountLabel: "catatan@example.com",
		AccessToken:  "a-real-token",
	})
	require.NoError(t, err)

	url, err := fixture.service.WebhookURL(ctx, fixture.scope, pull.ID, "https://app.example.com")
	require.NoError(t, err)
	_, query, found := strings.Cut(url, "?")
	require.True(t, found)

	body := `{"message_id":"msg-7"}`
	request := httptest.NewRequest(http.MethodPost, "/api/webhooks/notion?"+query, strings.NewReader(body))
	request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	request.Header.Set("X-Webhook-Signature", sign([]byte(body)))

	response, err := fixture.app.Test(request, -1)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()

	require.Equal(t, http.StatusBadRequest, response.StatusCode)

	// A reference issued for one application cannot be aimed at another: the
	// mismatch is refused before anything is read.
	request = httptest.NewRequest(http.MethodPost, "/api/webhooks/gmail?"+query, strings.NewReader(body))
	request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	request.Header.Set("X-Webhook-Signature", sign([]byte(body)))

	response, err = fixture.app.Test(request, -1)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()

	require.Equal(t, http.StatusUnauthorized, response.StatusCode,
		"a reference names the application it was issued for")
}

// TestAFailedAttemptIsRecorded keeps a delivery that keeps failing visible rather
// than silently retried forever.
func TestAFailedAttemptIsRecorded(t *testing.T) {
	fixture := newWebhookFixture(t, migratedDatabase(t))
	ctx := t.Context()

	body := `{"message_id":"msg-8"}`
	status, _ := fixture.post(t, body, map[string]string{"X-Webhook-Signature": sign([]byte(body))})
	require.Equal(t, http.StatusOK, status)

	pending, err := fixture.service.PendingWebhooks(ctx, fixture.scope, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)

	require.NoError(t, fixture.service.WebhookFailed(ctx, fixture.scope, pending[0].ID, "penerima tidak dikenal"))

	pending, err = fixture.service.PendingWebhooks(ctx, fixture.scope, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1, "a failed delivery stays queued for another attempt")
	require.Equal(t, 1, pending[0].Attempts)

	// A delivery of another workspace is invisible, so one tenant cannot mark
	// another's work done.
	otherUser, err := createVerifiedUser(t, fixture.pool)
	require.NoError(t, err)
	otherWorkspace, err := onboardWorkspace(t, fixture.pool, otherUser)
	require.NoError(t, err)

	otherScope := integrationdomain.Scope{UserID: otherUser, WorkspaceID: otherWorkspace}
	require.ErrorIs(t, fixture.service.WebhookProcessed(ctx, otherScope, pending[0].ID), integrationdomain.ErrNotFound)
	require.ErrorIs(t, fixture.service.WebhookFailed(ctx, otherScope, pending[0].ID, "x"), integrationdomain.ErrNotFound)
}

// TestAWebhookReferenceCannotBeUsedAsAStateAndTheOtherWayRound is why both values
// carry a purpose: they are signed with the same key.
func TestAWebhookReferenceCannotBeUsedAsAStateAndTheOtherWayRound(t *testing.T) {
	fixture := newWebhookFixture(t, migratedDatabase(t))
	ctx := t.Context()

	url, err := fixture.service.WebhookURL(ctx, fixture.scope, fixture.integration.ID, "https://app.example.com")
	require.NoError(t, err)
	_, reference, found := strings.Cut(url, "?ref=")
	require.True(t, found)

	// The reference is not a state, so a flow cannot be finished with it.
	_, err = fixture.service.CompleteAuth(ctx, integrationdomain.CompleteAuthRequest{
		Code:  "a-good-code",
		State: reference,
	})
	require.ErrorIs(t, err, integrationdomain.ErrInvalidState)

	// And a state is not a reference, so it cannot name a tenant for a delivery.
	body := `{"message_id":"msg-9"}`
	request := httptest.NewRequest(http.MethodPost, "/api/webhooks/gmail?ref="+reference+"x", strings.NewReader(body))
	request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	request.Header.Set("X-Webhook-Signature", sign([]byte(body)))

	response, err := fixture.app.Test(request, -1)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
}
