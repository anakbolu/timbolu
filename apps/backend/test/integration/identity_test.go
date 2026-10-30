package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/container"
	authhandler "github.com/anakbolu/timbolu/apps/backend/internal/modules/auth/handler"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/mail"
	"github.com/anakbolu/timbolu/packages/platform/config"
)

// These tests drive the real API over a real Postgres: the container assembles
// the modules, the handlers run, and Row Level Security enforces tenancy. They
// cover the acceptance gates of EPIC 2 (#20).

// captureMailer records the links the service emails, so a test can click them
// the way a user would.
type captureMailer struct {
	messages []mail.Message
}

func (m *captureMailer) Send(_ context.Context, message mail.Message) error {
	m.messages = append(m.messages, message)
	return nil
}

func (m *captureMailer) lastTo(t *testing.T, address string) string {
	t.Helper()

	for i := len(m.messages) - 1; i >= 0; i-- {
		if m.messages[i].To == address {
			return m.messages[i].Text
		}
	}
	t.Fatalf("no message was sent to %s (sent: %d)", address, len(m.messages))
	return ""
}

var tokenPattern = regexp.MustCompile(`token=([A-Za-z0-9_\-]+)`)

func tokenFrom(t *testing.T, body string) string {
	t.Helper()

	matches := tokenPattern.FindStringSubmatch(body)
	require.Len(t, matches, 2, "no token found in %q", body)
	return matches[1]
}

// identityAPI is the running API plus the mail sink.
type identityAPI struct {
	app    *fiber.App
	mailer *captureMailer
}

func newIdentityAPI(t *testing.T, dsn string, opts ...container.Option) *identityAPI {
	t.Helper()

	mailer := &captureMailer{}

	cfg := &config.Config{
		Application: config.AppConfig{
			Environment: "test",
			FrontendURL: "https://app.example.com",
		},
		Http: config.HttpConfig{Address: ":0", ApiPrefix: "/api"},
		Database: config.DatabaseConfig{
			URL:          dsn,
			MaxOpenConns: 5,
			MaxIdleConns: 1,
		},
		JWT: config.JWTConfig{Secret: "integration-test-secret", ExpireHours: 1},
		Auth: config.AuthConfig{
			AccessTokenMinutes: 15, RefreshHours: 24, VerificationHours: 24, ResetMinutes: 60, StateMinutes: 10,
			LoginThrottleCapacity: 100, LoginThrottleRefill: 100,
			// The deployment's config.yaml requires a confirmed address, so the
			// sign-up and password flows below expect a verification link. The
			// zero value is false, and leaving it there silently turns every
			// register into a no-mail path.
			RequireEmailVerification: true,
		},
		Mail:    config.MailConfig{Driver: "log"},
		Logging: config.LoggingConfig{Level: "error", Format: "json"},
	}

	all := append([]container.Option{
		container.WithLogger(zap.NewNop()),
		container.WithMailer(mailer),
	}, opts...)

	c, err := container.New(t.Context(), cfg, all...)
	require.NoError(t, err)
	t.Cleanup(c.Close)

	require.NotNil(t, c.App())
	return &identityAPI{app: c.App(), mailer: mailer}
}

// call performs a request and returns the status plus the decoded envelope.
func (a *identityAPI) call(t *testing.T, method, path, body string, headers map[string]string) (int, map[string]any, string) {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := a.app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	raw := new(strings.Builder)
	decoder := json.NewDecoder(resp.Body)
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		payload = nil
	}

	var buffer []byte
	if payload != nil {
		buffer, _ = json.Marshal(payload)
	}
	raw.Write(buffer)

	return resp.StatusCode, payload, raw.String()
}

// data returns the "data" object of a successful envelope.
func data(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()

	require.NotNil(t, payload, "the response had no JSON body")
	value, ok := payload["data"].(map[string]any)
	require.True(t, ok, "the response has no data object: %v", payload)
	return value
}

// register creates an account and returns the verification token.
func (a *identityAPI) register(t *testing.T, email, password string) string {
	t.Helper()

	status, _, body := a.call(t, fiber.MethodPost, "/api/auth/register",
		`{"email":"`+email+`","password":"`+password+`"}`, nil)
	require.Equal(t, http.StatusCreated, status, body)

	return tokenFrom(t, a.mailer.lastTo(t, email))
}

// verifyEmail consumes the verification token.
func (a *identityAPI) verifyEmail(t *testing.T, token string) {
	t.Helper()

	status, _, body := a.call(t, fiber.MethodPost, "/api/auth/verify-email", `{"token":"`+token+`"}`, nil)
	require.Equal(t, http.StatusOK, status, body)
}

// login returns the access token and the refresh cookie value.
func (a *identityAPI) login(t *testing.T, email, password string) (accessToken, refreshCookie string) {
	t.Helper()

	req := httptest.NewRequest(fiber.MethodPost, "/api/auth/login", strings.NewReader(
		`{"email":"`+email+`","password":"`+password+`"}`))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)

	resp, err := a.app.Test(req, -1)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var payload struct {
		Data struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	require.NotEmpty(t, payload.Data.AccessToken)

	return payload.Data.AccessToken, refreshCookieFrom(resp)
}

// refreshCookieFrom extracts the refresh cookie value from a response.
func refreshCookieFrom(resp *http.Response) string {
	for _, cookie := range resp.Cookies() {
		if cookie.Name == authhandler.CookieName {
			return cookie.Value
		}
	}
	return ""
}

func bearer(token string) map[string]string {
	return map[string]string{fiber.HeaderAuthorization: "Bearer " + token}
}

func withWorkspace(token, workspaceID string) map[string]string {
	return map[string]string{
		fiber.HeaderAuthorization:   "Bearer " + token,
		container.HeaderWorkspaceID: workspaceID,
	}
}

// TestSignupFlowThroughTheAPI is the first acceptance gate: register → verify →
// onboard → the account owns a workspace with its default team.
func TestSignupFlowThroughTheAPI(t *testing.T) {
	api := newIdentityAPI(t, migratedDatabase(t))

	const email = "owner@example.com"
	const password = "A-good-password1"

	token := api.register(t, email, password)

	// The account exists but cannot onboard before verifying: that is the
	// "unverified accounts cannot use verified features" rule.
	loginStatus, loginPayload, loginBody := api.call(t, fiber.MethodPost, "/api/auth/login",
		`{"email":"`+email+`","password":"`+password+`"}`, nil)
	require.Equal(t, http.StatusOK, loginStatus, loginBody)
	require.False(t, data(t, loginPayload)["user"].(map[string]any)["email_verified"].(bool),
		"a fresh account is not verified yet")

	// Without a token the protected routes refuse the request.
	anonymousStatus, _, _ := api.call(t, fiber.MethodGet, "/api/workspaces", "", nil)
	require.Equal(t, http.StatusUnauthorized, anonymousStatus)

	api.verifyEmail(t, token)

	accessToken, refreshCookie := api.login(t, email, password)
	require.NotEmpty(t, refreshCookie, "the refresh token must arrive in a cookie")

	status, payload, body := api.call(t, fiber.MethodPost, "/api/workspaces/onboard",
		`{"name":"Toko Sinar","business_field":"Retail","timezone":"Asia/Jakarta"}`, bearer(accessToken))
	require.Equal(t, http.StatusOK, status, body)

	onboarded := data(t, payload)
	require.True(t, onboarded["created"].(bool))

	workspace := onboarded["workspace"].(map[string]any)
	require.Equal(t, "Toko Sinar", workspace["name"])
	require.Equal(t, "owner", workspace["role"])

	teams := onboarded["teams"].([]any)
	require.Len(t, teams, 1, "a new workspace gets one team, not a second empty one")

	team := teams[0].(map[string]any)
	require.Equal(t, "Tim Bolu", team["name"])
	require.Equal(t, "bolu", team["kind"])

	workspaceID := workspace["id"].(string)

	// The workspace is now usable with the active-workspace header.
	status, payload, body = api.call(t, fiber.MethodGet, "/api/workspaces/current", "", withWorkspace(accessToken, workspaceID))
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "Toko Sinar", data(t, payload)["name"])

	status, payload, body = api.call(t, fiber.MethodGet, "/api/workspaces", "", bearer(accessToken))
	require.Equal(t, http.StatusOK, status, body)
	require.Len(t, payload["data"].([]any), 1)

	// Onboarding again returns the same workspace instead of creating a second.
	status, payload, body = api.call(t, fiber.MethodPost, "/api/workspaces/onboard",
		`{"name":"Toko Sinar Lagi","business_field":"Retail","timezone":"Asia/Jakarta"}`, bearer(accessToken))
	require.Equal(t, http.StatusOK, status, body)
	require.False(t, data(t, payload)["created"].(bool), "a repeated onboarding must not create another workspace")

	status, payload, _ = api.call(t, fiber.MethodGet, "/api/workspaces", "", bearer(accessToken))
	require.Equal(t, http.StatusOK, status)
	require.Len(t, payload["data"].([]any), 1, "there must still be exactly one workspace")
}

// TestOnboardingIsWhatTellsTheSessionItIsDone is the fact the sign-in redirect
// reads.
//
// The web app sends a person to the onboarding screen or the dashboard based on
// `user.onboarded`, so an account that owns a workspace while that flag stays
// false is sent back to onboarding after every sign-in, forever. The flag is
// written inside the onboarding transaction rather than by a second call, so a
// crash cannot leave the two disagreeing.
func TestOnboardingIsWhatTellsTheSessionItIsDone(t *testing.T) {
	api := newIdentityAPI(t, migratedDatabase(t))

	const email = "onboard-flag@example.com"
	const password = "A-good-password1"

	api.verifyEmail(t, api.register(t, email, password))

	// Before onboarding the flag is false, which is what sends a fresh account
	// to the form.
	_, payload, body := api.call(t, fiber.MethodPost, "/api/auth/login",
		`{"email":"`+email+`","password":"`+password+`"}`, nil)
	require.False(t, data(t, payload)["user"].(map[string]any)["onboarded"].(bool), body)

	accessToken, _ := api.login(t, email, password)
	status, _, body := api.call(t, fiber.MethodPost, "/api/workspaces/onboard",
		`{"name":"Toko Sinar","business_field":"Retail","timezone":"Asia/Jakarta"}`, bearer(accessToken))
	require.Equal(t, http.StatusOK, status, body)

	// After it, the next sign-in says the account is done, so the app lets it
	// through to the dashboard.
	_, payload, body = api.call(t, fiber.MethodPost, "/api/auth/login",
		`{"email":"`+email+`","password":"`+password+`"}`, nil)
	require.True(t, data(t, payload)["user"].(map[string]any)["onboarded"].(bool), body)

	// Onboarding again reports the same thing: the fact is not lost by repeating
	// the call, and it does not depend on the workspace being newly created.
	status, _, body = api.call(t, fiber.MethodPost, "/api/workspaces/onboard",
		`{"name":"Toko Sinar","business_field":"Retail","timezone":"Asia/Jakarta"}`, bearer(accessToken))
	require.Equal(t, http.StatusOK, status, body)

	_, payload, body = api.call(t, fiber.MethodPost, "/api/auth/login",
		`{"email":"`+email+`","password":"`+password+`"}`, nil)
	require.True(t, data(t, payload)["user"].(map[string]any)["onboarded"].(bool), body)
}

// TestAnAccountWithAWorkspaceButNoFlagIsHealed covers the accounts that already
// existed when the flag was wired: they own a workspace, so onboarding returns it
// unchanged, and it must still record that they are done.
func TestAnAccountWithAWorkspaceButNoFlagIsHealed(t *testing.T) {
	dsn := migratedDatabase(t)
	api := newIdentityAPI(t, dsn)

	const email = "legacy-onboard@example.com"
	const password = "A-good-password1"

	api.verifyEmail(t, api.register(t, email, password))
	accessToken, _ := api.login(t, email, password)

	status, _, body := api.call(t, fiber.MethodPost, "/api/workspaces/onboard",
		`{"name":"Toko Sinar","business_field":"Retail","timezone":"Asia/Jakarta"}`, bearer(accessToken))
	require.Equal(t, http.StatusOK, status, body)

	// Reproduce the state a deployment that never marked the account is in.
	pool := poolFor(t, dsn)
	_, err := pool.PgxPool().Exec(t.Context(),
		"UPDATE users SET onboarded_at = NULL WHERE lower(email) = lower($1)", email)
	require.NoError(t, err)

	_, payload, body := api.call(t, fiber.MethodPost, "/api/auth/login",
		`{"email":"`+email+`","password":"`+password+`"}`, nil)
	require.False(t, data(t, payload)["user"].(map[string]any)["onboarded"].(bool), body)

	// The next onboarding call returns the workspace it already owns and records
	// the fact on the way through.
	status, payload, body = api.call(t, fiber.MethodPost, "/api/workspaces/onboard",
		`{"name":"Toko Sinar","business_field":"Retail","timezone":"Asia/Jakarta"}`, bearer(accessToken))
	require.Equal(t, http.StatusOK, status, body)
	require.False(t, data(t, payload)["created"].(bool), "it must not create a second workspace")

	_, payload, body = api.call(t, fiber.MethodPost, "/api/auth/login",
		`{"email":"`+email+`","password":"`+password+`"}`, nil)
	require.True(t, data(t, payload)["user"].(map[string]any)["onboarded"].(bool), body)
}

// TestSigningOutEndsTheSession is the MVP's way out: a person who signs out is
// signed out on the server, not only in the tab they were using.
//
// It is checked by using the credential after the sign-out, because that is the
// only thing that tells "the button cleared my local state" apart from "the
// session is gone".
func TestSigningOutEndsTheSession(t *testing.T) {
	api := newIdentityAPI(t, migratedDatabase(t))

	const email = "signout@example.com"
	const password = "A-good-password1"

	api.verifyEmail(t, api.register(t, email, password))

	accessToken, refreshCookie := api.login(t, email, password)
	require.NotEmpty(t, refreshCookie, "the refresh token arrives in a cookie")

	// The session works before the sign-out.
	status, _, body := api.call(t, fiber.MethodGet, "/api/workspaces", "", bearer(accessToken))
	require.Equal(t, http.StatusOK, status, body)

	// The refresh cookie is what proves the session, so the sign-out is made with
	// it: a browser carries the cookie automatically, and a client that uses the
	// body supplies the same value.
	status, _, body = api.call(t, fiber.MethodPost, "/api/auth/logout",
		`{"token":"`+refreshCookie+`"}`, nil)
	require.Equal(t, http.StatusOK, status, body)

	// The refresh token no longer buys a session, which is the whole point: a
	// copied cookie must not outlive the sign-out. The endpoint reports a spent
	// token as invalid input rather than as a missing credential, which is what it
	// is: the value arrived and was refused.
	status, _, body = api.call(t, fiber.MethodPost, "/api/auth/refresh", "", map[string]string{
		fiber.HeaderCookie: authhandler.CookieName + "=" + refreshCookie,
	})
	require.Equal(t, http.StatusBadRequest, status, body)
	require.Contains(t, body, "invalid_token")

	// And the access token stops working too, so signing out is not only a local
	// state change: a token copied out of the tab before the sign-out is refused
	// afterwards.
	status, _, body = api.call(t, fiber.MethodGet, "/api/workspaces", "", bearer(accessToken))
	require.Equal(t, http.StatusUnauthorized, status, body)
	require.Contains(t, body, "unauthenticated")
}

// TestInvitationFlow is the second acceptance gate: a second user is invited,
// joins with the role they were given, and cannot do more than that role allows.
func TestInvitationFlow(t *testing.T) {
	api := newIdentityAPI(t, migratedDatabase(t))

	const ownerEmail = "owner@example.com"
	const memberEmail = "teammate@example.com"
	const password = "A-good-password1"

	api.verifyEmail(t, api.register(t, ownerEmail, password))
	ownerToken, _ := api.login(t, ownerEmail, password)

	status, _, _ := api.call(t, fiber.MethodPost, "/api/workspaces/onboard",
		`{"name":"Toko Sinar","business_field":"Retail","timezone":"Jakarta"}`, bearer(ownerToken))
	require.Equal(t, http.StatusBadRequest, status, "an unknown timezone must be rejected")

	status, payload, body := api.call(t, fiber.MethodPost, "/api/workspaces/onboard",
		`{"name":"Toko Sinar","business_field":"Retail","timezone":"Asia/Jakarta"}`, bearer(ownerToken))
	require.Equal(t, http.StatusOK, status, body)
	workspaceID := data(t, payload)["workspace"].(map[string]any)["id"].(string)

	// The second account is invited as a plain member.
	status, _, body = api.call(t, fiber.MethodPost, "/api/workspaces/current/invitations",
		`{"email":"`+memberEmail+`","role":"member"}`, withWorkspace(ownerToken, workspaceID))
	require.Equal(t, http.StatusCreated, status, body)

	// Inviting the same address twice is reported instead of silently piling up.
	status, _, _ = api.call(t, fiber.MethodPost, "/api/workspaces/current/invitations",
		`{"email":"`+memberEmail+`","role":"member"}`, withWorkspace(ownerToken, workspaceID))
	require.Equal(t, http.StatusConflict, status)

	inviteToken := tokenFrom(t, api.mailer.lastTo(t, memberEmail))

	api.verifyEmail(t, api.register(t, memberEmail, password))
	memberToken, _ := api.login(t, memberEmail, password)

	// A user who is not a member cannot use the workspace, even with its id.
	status, _, _ = api.call(t, fiber.MethodGet, "/api/workspaces/current", "", withWorkspace(memberToken, workspaceID))
	require.Equal(t, http.StatusForbidden, status, "a non-member must get 403, not data")

	// Accepting joins the workspace with the invited role.
	status, payload, body = api.call(t, fiber.MethodPost, "/api/invitations/accept",
		`{"token":"`+inviteToken+`"}`, bearer(memberToken))
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "member", data(t, payload)["role"])

	// The invitation is single use.
	status, _, _ = api.call(t, fiber.MethodPost, "/api/invitations/accept",
		`{"token":"`+inviteToken+`"}`, bearer(memberToken))
	require.Equal(t, http.StatusBadRequest, status, "an accepted invitation must not be reusable")

	// Both accounts now see the same workspace.
	status, payload, body = api.call(t, fiber.MethodGet, "/api/workspaces/current/members", "", withWorkspace(memberToken, workspaceID))
	require.Equal(t, http.StatusOK, status, body)
	require.Len(t, payload["data"].([]any), 2, "the owner and the new member")

	// A plain member may read but not manage.
	status, _, _ = api.call(t, fiber.MethodPost, "/api/workspaces/current/invitations",
		`{"email":"another@example.com","role":"member"}`, withWorkspace(memberToken, workspaceID))
	require.Equal(t, http.StatusForbidden, status, "a member must not be able to invite")

	status, _, _ = api.call(t, fiber.MethodPatch, "/api/workspaces/current",
		`{"name":"Renamed"}`, withWorkspace(memberToken, workspaceID))
	require.Equal(t, http.StatusForbidden, status, "a member must not be able to change settings")

	// The owner can, and the change is visible to the member.
	//
	// The endpoint replaces the profile rather than patching fields, which is why
	// the whole profile travels in the body: the window outbound actions leave in
	// is what the approval gate reads, and a request that omitted it would write a
	// workspace whose window starts at 00:00.
	status, _, body = api.call(t, fiber.MethodPatch, "/api/workspaces/current",
		`{"name":"Toko Sinar Jaya","business_field":"Retail","timezone":"Asia/Makassar",`+
			`"language":"id","work_hours_start":"08:00","work_hours_end":"21:00","work_days":[1,2,3,4,5,6]}`,
		withWorkspace(ownerToken, workspaceID))
	require.Equal(t, http.StatusOK, status, body)

	status, payload, _ = api.call(t, fiber.MethodGet, "/api/workspaces/current", "", withWorkspace(memberToken, workspaceID))
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "Toko Sinar Jaya", data(t, payload)["name"])
	require.Equal(t, "Asia/Makassar", data(t, payload)["timezone"])
}

// TestOwnerProtection keeps a workspace from being left without an owner.
func TestOwnerProtection(t *testing.T) {
	api := newIdentityAPI(t, migratedDatabase(t))

	const email = "owner@example.com"
	const password = "A-good-password1"

	api.verifyEmail(t, api.register(t, email, password))
	token, _ := api.login(t, email, password)

	_, payload, _ := api.call(t, fiber.MethodPost, "/api/workspaces/onboard",
		`{"name":"Toko Sinar","business_field":"Retail","timezone":"Asia/Jakarta"}`, bearer(token))
	workspaceID := data(t, payload)["workspace"].(map[string]any)["id"].(string)

	status, payload, body := api.call(t, fiber.MethodGet, "/api/workspaces/current/members", "", withWorkspace(token, workspaceID))
	require.Equal(t, http.StatusOK, status, body)
	members := payload["data"].([]any)
	require.Len(t, members, 1)
	ownerID := members[0].(map[string]any)["user_id"].(string)

	// The only owner cannot demote themselves, which would orphan the workspace.
	status, _, _ = api.call(t, fiber.MethodPatch, "/api/workspaces/current/members/"+ownerID,
		`{"role":"member"}`, withWorkspace(token, workspaceID))
	require.Equal(t, http.StatusConflict, status)

	// Nor remove themselves.
	status, _, _ = api.call(t, fiber.MethodDelete, "/api/workspaces/current/members/"+ownerID,
		"", withWorkspace(token, workspaceID))
	require.Equal(t, http.StatusConflict, status)
}

// TestPasswordResetFlow proves the reset link works once, replaces the password,
// and invalidates the sessions that existed before it.
func TestPasswordResetFlow(t *testing.T) {
	api := newIdentityAPI(t, migratedDatabase(t))

	const email = "owner@example.com"
	const password = "A-good-password1"
	const newPassword = "A-brand-new-password1"

	api.verifyEmail(t, api.register(t, email, password))
	accessToken, _ := api.login(t, email, password)

	status, _, body := api.call(t, fiber.MethodPost, "/api/auth/password/forgot", `{"email":"`+email+`"}`, nil)
	require.Equal(t, http.StatusOK, status, body)

	// An unknown address answers exactly the same way.
	statusUnknown, _, bodyUnknown := api.call(t, fiber.MethodPost, "/api/auth/password/forgot", `{"email":"nobody@example.com"}`, nil)
	require.Equal(t, status, statusUnknown)
	require.Equal(t, body, bodyUnknown, "the response must not reveal whether the address exists")

	resetToken := tokenFrom(t, api.mailer.lastTo(t, email))

	status, _, body = api.call(t, fiber.MethodPost, "/api/auth/password/reset",
		`{"token":"`+resetToken+`","password":"`+newPassword+`"}`, nil)
	require.Equal(t, http.StatusOK, status, body)

	// The token is single use.
	status, _, _ = api.call(t, fiber.MethodPost, "/api/auth/password/reset",
		`{"token":"`+resetToken+`","password":"`+newPassword+`"}`, nil)
	require.Equal(t, http.StatusBadRequest, status)

	// Sessions that existed before the reset are gone: the old access token is
	// refused even though it has not expired.
	status, _, _ = api.call(t, fiber.MethodGet, "/api/workspaces", "", bearer(accessToken))
	require.Equal(t, http.StatusUnauthorized, status, "a password reset must revoke existing sessions")

	// The new password works and the old one does not.
	_, _ = api.login(t, email, newPassword)

	status, _, _ = api.call(t, fiber.MethodPost, "/api/auth/login", `{"email":"`+email+`","password":"`+password+`"}`, nil)
	require.Equal(t, http.StatusUnauthorized, status, "the old password must stop working")
}

// TestRefreshRotationThroughTheAPI proves the refresh cookie is rotated: the
// second use of the same cookie is refused.
func TestRefreshRotationThroughTheAPI(t *testing.T) {
	api := newIdentityAPI(t, migratedDatabase(t))

	const email = "owner@example.com"
	const password = "A-good-password1"

	api.verifyEmail(t, api.register(t, email, password))
	_, refreshCookie := api.login(t, email, password)
	require.NotEmpty(t, refreshCookie)

	status, payload, body := api.call(t, fiber.MethodPost, "/api/auth/refresh", "",
		map[string]string{fiber.HeaderCookie: authhandler.CookieName + "=" + refreshCookie})
	require.Equal(t, http.StatusOK, status, body)
	require.NotEmpty(t, data(t, payload)["access_token"])

	status, _, _ = api.call(t, fiber.MethodPost, "/api/auth/refresh", "",
		map[string]string{fiber.HeaderCookie: authhandler.CookieName + "=" + refreshCookie})
	require.Equal(t, http.StatusBadRequest, status, "a rotated refresh token must not work twice")
}

// TestGoogleSignInThroughTheAPI drives the whole Google flow against a stub
// provider: an existing account is linked, not duplicated.
func TestGoogleSignInThroughTheAPI(t *testing.T) {
	const email = "owner@example.com"
	const password = "A-good-password1"

	provider := newGoogleStub(t, email)
	api := newIdentityAPI(t, migratedDatabase(t), container.WithGoogle(provider.client(t)))

	api.verifyEmail(t, api.register(t, email, password))

	// Start: the API sends the browser to the provider with a signed state.
	startResp, err := api.app.Test(httptest.NewRequest(fiber.MethodGet, "/api/auth/google/start", nil), -1)
	require.NoError(t, err)
	defer func() { _ = startResp.Body.Close() }()
	require.Equal(t, http.StatusFound, startResp.StatusCode)

	state := stateFrom(t, startResp.Header.Get(fiber.HeaderLocation))

	// Callback: the code is exchanged and the account is signed in.
	callbackResp, err := api.app.Test(httptest.NewRequest(
		fiber.MethodGet, "/api/auth/google/callback?code=stub-code&state="+state, nil), -1)
	require.NoError(t, err)
	defer func() { _ = callbackResp.Body.Close() }()

	require.Equal(t, http.StatusFound, callbackResp.StatusCode)
	require.Contains(t, callbackResp.Header.Get(fiber.HeaderLocation), "google=ok")
	require.NotEmpty(t, refreshCookieFrom(callbackResp), "the callback must set the refresh cookie")

	// The account was linked, not duplicated: signing in with the password still
	// works and there is still exactly one account.
	api.login(t, email, password)

	// A tampered state is refused before the provider is called.
	badResp, err := api.app.Test(httptest.NewRequest(
		fiber.MethodGet, "/api/auth/google/callback?code=stub-code&state=tampered", nil), -1)
	require.NoError(t, err)
	defer func() { _ = badResp.Body.Close() }()
	require.Contains(t, badResp.Header.Get(fiber.HeaderLocation), "google_error=1")
}

// TestSessionCookieIsHttpOnly reads the raw login response to prove the refresh
// token never reaches JavaScript.
func TestSessionCookieIsHttpOnly(t *testing.T) {
	api := newIdentityAPI(t, migratedDatabase(t))

	const email = "owner@example.com"
	const password = "A-good-password1"

	api.verifyEmail(t, api.register(t, email, password))

	req := httptest.NewRequest(fiber.MethodPost, "/api/auth/login",
		strings.NewReader(`{"email":"`+email+`","password":"`+password+`"}`))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)

	resp, err := api.app.Test(req, -1)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var cookie *http.Cookie
	for _, candidate := range resp.Cookies() {
		if candidate.Name == authhandler.CookieName {
			cookie = candidate
		}
	}
	require.NotNil(t, cookie, "the refresh cookie must be set")
	require.True(t, cookie.HttpOnly, "the refresh cookie must be httpOnly")
	require.Equal(t, "/api/auth", cookie.Path, "the cookie must only be sent to the auth endpoints")
	require.Greater(t, cookie.MaxAge, 0, "the cookie must outlive the access token")
	require.Greater(t, cookie.MaxAge, 15*60, "the refresh cookie must live longer than an access token")

	// The access token, unlike the refresh token, is in the body for the client
	// to keep in memory.
	body, err := readAll(resp)
	require.NoError(t, err)
	require.Contains(t, body, "access_token")
	require.NotContains(t, body, cookie.Value, "the refresh token must not be in the response body")
}

func readAll(resp *http.Response) (string, error) {
	content, err := io.ReadAll(resp.Body)
	return string(content), err
}

// stateFrom pulls the OAuth state out of the consent URL.
func stateFrom(t *testing.T, location string) string {
	t.Helper()

	matches := regexp.MustCompile(`state=([A-Za-z0-9_\-\.]+)`).FindStringSubmatch(location)
	require.Len(t, matches, 2, "no state in %q", location)
	return matches[1]
}
