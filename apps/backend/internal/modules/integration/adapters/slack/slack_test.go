package slack_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/adapters/slack"
	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
)

// The adapter's contract with the runtime is what this file tests: the URL it sends
// a person to, the two answers Slack gives that look alike (a refusal is a 200 with
// `ok:false`), which tools it declares and with which labels, and what a call sends
// and reads back. Slack itself is a stub, so what is under test is this
// deployment's own behaviour.

// newAdapter points an adapter at one stub server, which answers both the token
// endpoint and the Web API.
func newAdapter(t *testing.T) (*slack.Adapter, *httptest.Server) {
	t.Helper()

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))

	adapter, err := slack.New(slack.Config{
		ClientID:      "client-id",
		ClientSecret:  "client-secret",
		HTTPClient:    server.Client(),
		AuthEndpoint:  "https://slack.com/oauth/v2/authorize",
		TokenEndpoint: server.URL + "/oauth.v2.access",
		APIBase:       server.URL + "/api",
	})
	require.NoError(t, err)

	t.Cleanup(server.Close)
	return adapter, server
}

// TestNewRequiresAnOAuthApplication: a deployment that has not registered one is
// disabled by the container, and this is the same refusal one level down.
func TestNewRequiresAnOAuthApplication(t *testing.T) {
	for name, cfg := range map[string]slack.Config{
		"no client id":     {ClientSecret: "secret"},
		"no client secret": {ClientID: "id"},
		"blank":            {ClientID: "  ", ClientSecret: "  "},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := slack.New(cfg)
			require.Error(t, err)
		})
	}
}

// TestAuthCodeURLIsSlacksConsentPage: the shape a browser is sent to, including the
// state the flow verifies the callback against and the scopes the app asks for.
func TestAuthCodeURLIsSlacksConsentPage(t *testing.T) {
	adapter, _ := newAdapter(t)

	raw := adapter.AuthCodeURL("state-value", "https://app.example.com/api/integrations/oauth/callback")

	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	require.Equal(t, "slack.com", parsed.Host)
	require.Equal(t, "/oauth/v2/authorize", parsed.Path)

	query := parsed.Query()
	require.Equal(t, "client-id", query.Get("client_id"))
	require.Equal(t, "state-value", query.Get("state"))
	require.Equal(t, "https://app.example.com/api/integrations/oauth/callback", query.Get("redirect_uri"))

	scopes := strings.Split(query.Get("scope"), ",")
	require.ElementsMatch(t, []string{"channels:read", "chat:write"}, scopes,
		"Slack takes bot scopes as one comma-separated list")
	require.Equal(t, []string{"channels:read", "chat:write"}, adapter.DefaultScopes())
}

// TestExchangeTradesACodeForTheBotToken covers the happy path, and the form Slack
// is asked with: the credentials travel in the body rather than in a header.
func TestExchangeTradesACodeForTheBotToken(t *testing.T) {
	adapter, server := newAdapter(t)

	var received url.Values
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/oauth.v2.access", r.URL.Path)
		require.NoError(t, r.ParseForm())
		received = r.PostForm

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"ok": true,
			"access_token": "xoxb-bot-token",
			"token_type": "bot",
			"scope": "channels:read,chat:write",
			"bot_user_id": "U0KRQLJ9H",
			"team": {"id": "T9TK3CUKW", "name": "Toko Demo"}
		}`))
	})

	tokens, err := adapter.Exchange(context.Background(), "the-code", "https://app.example.com/callback")
	require.NoError(t, err)

	require.Equal(t, "xoxb-bot-token", tokens.AccessToken)
	require.Empty(t, tokens.RefreshToken, "a bot token without rotation has nothing to refresh with")
	require.Nil(t, tokens.ExpiresAt, "an expiring bot token states expires_in; this one does not")
	require.Equal(t, []string{"channels:read", "chat:write"}, tokens.Scopes)

	require.Equal(t, "authorization_code", received.Get("grant_type"))
	require.Equal(t, "the-code", received.Get("code"))
	require.Equal(t, "https://app.example.com/callback", received.Get("redirect_uri"))
	require.Equal(t, "client-id", received.Get("client_id"))
	require.Equal(t, "client-secret", received.Get("client_secret"))
}

// TestExchangeReportsARefusalSlackAnswersWithTwoHundred is the failure mode that
// makes this adapter different from Google's: the status says success and only the
// envelope carries the refusal.
func TestExchangeReportsARefusalSlackAnswersWithTwoHundred(t *testing.T) {
	adapter, server := newAdapter(t)

	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok": false, "error": "invalid_code"}`))
	})

	_, err := adapter.Exchange(context.Background(), "spent-code", "https://app.example.com/callback")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid_code")
}

// TestRefreshRotatesATokenWhenSlackIssuedOne covers the grant an app with token
// rotation on answers.
func TestRefreshRotatesATokenWhenSlackIssuedOne(t *testing.T) {
	adapter, server := newAdapter(t)

	var received url.Values
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		received = r.PostForm

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"ok": true,
			"access_token": "xoxe.xoxb-rotated",
			"refresh_token": "xoxe-1-new",
			"expires_in": 43200,
			"scope": "channels:read,chat:write"
		}`))
	})

	tokens, err := adapter.Refresh(context.Background(), "xoxe-1-old")
	require.NoError(t, err)

	require.Equal(t, "xoxe.xoxb-rotated", tokens.AccessToken)
	require.Equal(t, "xoxe-1-new", tokens.RefreshToken)
	require.NotNil(t, tokens.ExpiresAt)
	require.Equal(t, "refresh_token", received.Get("grant_type"))
	require.Equal(t, "xoxe-1-old", received.Get("refresh_token"))
}

// TestRefreshWithoutARefreshTokenIsTheFailureAPersonActsOn: a plain bot token does
// not expire, so a connection with no refresh token is reported as needing a fresh
// install rather than sent to an endpoint that would refuse it.
func TestRefreshWithoutARefreshTokenIsTheFailureAPersonActsOn(t *testing.T) {
	adapter, server := newAdapter(t)

	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the token endpoint must not be called without a refresh token")
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})

	_, err := adapter.Refresh(context.Background(), "  ")
	require.ErrorIs(t, err, integrationdomain.ErrRefreshFailed)
}

// TestARefusedRefreshBecomesErrRefreshFailed: a rotation Slack no longer accepts is
// the one refresh failure a person has to act on.
func TestARefusedRefreshBecomesErrRefreshFailed(t *testing.T) {
	adapter, server := newAdapter(t)

	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok": false, "error": "invalid_refresh_token"}`))
	})

	_, err := adapter.Refresh(context.Background(), "revoked")
	require.ErrorIs(t, err, integrationdomain.ErrRefreshFailed)
	require.Contains(t, err.Error(), "invalid_refresh_token")
}

// TestToolsAreNamedAndLabelled: every tool is `slack.action`, every spec passes the
// registry's own validation, and the labels are the governance story — reading needs
// nobody, posting into a channel is read by people and always waits for one.
func TestToolsAreNamedAndLabelled(t *testing.T) {
	adapter, _ := newAdapter(t)

	tools := adapter.Tools()
	require.NotEmpty(t, tools)

	byName := map[string]integrationdomain.ToolSpec{}
	for _, tool := range tools {
		require.NoError(t, tool.Valid(), tool.Name)
		require.Equal(t, slack.App, tool.App(), tool.Name)
		byName[tool.Name] = tool
	}

	require.Equal(t, integrationdomain.LabelWriteExternal, byName["slack.post_message"].Label)
	require.Equal(t, integrationdomain.LabelRead, byName["slack.list_channels"].Label)
}

// TestTriggersDeclareTheMessageEvent: the trigger a routine screen offers, declared
// as the push it is rather than as a poll.
func TestTriggersDeclareTheMessageEvent(t *testing.T) {
	adapter, _ := newAdapter(t)

	triggers := adapter.Triggers()
	require.Len(t, triggers, 1)

	trigger := triggers[0]
	require.NoError(t, trigger.Valid())
	require.Equal(t, slack.App, trigger.App)
	require.Equal(t, integrationdomain.TriggerWebhook, trigger.Kind)
	require.Equal(t, "slack.message_received", trigger.Event)
}

// call builds one tool invocation with the token the runtime would have opened.
func call(t *testing.T, tool string, arguments string) integrationdomain.Call {
	t.Helper()

	return integrationdomain.Call{
		IntegrationID: uuid.New(),
		Tool:          tool,
		Arguments:     json.RawMessage(arguments),
		Token:         integrationdomain.Token{AccessToken: "xoxb-bot-token"},
	}
}

// TestPostMessageSendsTheTokenAndTheBody: the write tool's request, including the
// bot token in the header and the channel, text, and thread it posts into.
func TestPostMessageSendsTheTokenAndTheBody(t *testing.T) {
	adapter, server := newAdapter(t)

	var (
		method string
		path   string
		auth   string
		body   map[string]any
	)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok": true, "channel": "C012AB3CD", "ts": "1503435956.000247"}`))
	})

	result, err := adapter.Execute(context.Background(), call(t, "slack.post_message",
		`{"channel": "C012AB3CD", "text": "Pesanan baru", "thread_ts": "1503435956.000100"}`))
	require.NoError(t, err)

	require.Equal(t, http.MethodPost, method)
	require.Equal(t, "/api/chat.postMessage", path)
	require.Equal(t, "Bearer xoxb-bot-token", auth)
	require.Equal(t, map[string]any{
		"channel":   "C012AB3CD",
		"text":      "Pesanan baru",
		"thread_ts": "1503435956.000100",
	}, body)

	require.Contains(t, result.Summary, "C012AB3CD")
	require.Contains(t, string(result.Content), "1503435956.000247")
}

// TestPostMessageReportsARefusalSlackAnswersWithTwoHundred is the reason the
// envelope is checked: a 200 with `ok:false` is a failure, and the reason has to
// reach the model rather than becoming an empty success.
func TestPostMessageReportsARefusalSlackAnswersWithTwoHundred(t *testing.T) {
	adapter, server := newAdapter(t)

	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok": false, "error": "channel_not_found"}`))
	})

	_, err := adapter.Execute(context.Background(), call(t, "slack.post_message",
		`{"channel": "C0MISSING", "text": "hai"}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "channel_not_found")
}

// TestPostMessageRefusesAnEmptyMessage: a call the schema would have caught is
// refused here too, so the adapter is safe to call directly.
func TestPostMessageRefusesAnEmptyMessage(t *testing.T) {
	adapter, _ := newAdapter(t)

	_, err := adapter.Execute(context.Background(), call(t, "slack.post_message",
		`{"channel": "C012AB3CD", "text": "   "}`))
	require.ErrorIs(t, err, integrationdomain.ErrInvalidInput)
}

// TestListChannelsAsksForPublicChannelsOnly: the granted scope is `channels:read`,
// so the query names public channels rather than asking for everything and having
// Slack refuse the private ones.
func TestListChannelsAsksForPublicChannelsOnly(t *testing.T) {
	adapter, server := newAdapter(t)

	var query url.Values
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		require.Equal(t, "Bearer xoxb-bot-token", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"ok": true,
			"channels": [
				{"id": "C012AB3CD", "name": "umum", "is_private": false, "num_members": 12},
				{"id": "C061EG9T2", "name": "pesanan", "is_private": false, "num_members": 4}
			],
			"response_metadata": {"next_cursor": "dGVhbTpDMDYxRkE1UEI="}
		}`))
	})

	result, err := adapter.Execute(context.Background(), call(t, "slack.list_channels", `{"limit": 50}`))
	require.NoError(t, err)

	require.Equal(t, "public_channel", query.Get("types"))
	require.Equal(t, "50", query.Get("limit"))
	require.Equal(t, "true", query.Get("exclude_archived"))

	var decoded struct {
		Channels []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			NumMembers int    `json:"num_members"`
		} `json:"channels"`
		NextCursor string `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(result.Content, &decoded))
	require.Len(t, decoded.Channels, 2)
	require.Equal(t, "umum", decoded.Channels[0].Name)
	require.Equal(t, 12, decoded.Channels[0].NumMembers)
	require.Equal(t, "dGVhbTpDMDYxRkE1UEI=", decoded.NextCursor,
		"the cursor is what reads the next page, so it survives to the model")
}

// TestUnknownToolIsRefused keeps a typo in a tool name from reaching Slack.
func TestUnknownToolIsRefused(t *testing.T) {
	adapter, _ := newAdapter(t)

	_, err := adapter.Execute(context.Background(), call(t, "slack.delete_everything", `{}`))
	require.ErrorIs(t, err, integrationdomain.ErrUnknownTool)
}

// TestAccountLabelNamesTheWorkspace: the connection is named after the workspace
// rather than after the moment it was installed.
func TestAccountLabelNamesTheWorkspace(t *testing.T) {
	adapter, server := newAdapter(t)

	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/auth.test", r.URL.Path)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok": true, "team": "Toko Demo", "url": "https://toko-demo.slack.com/"}`))
	})

	label, err := adapter.AccountLabel(context.Background(), integrationdomain.OAuthTokens{AccessToken: "xoxb-bot-token"})
	require.NoError(t, err)
	require.Equal(t, "Toko Demo", label)
}

// TestAccountLabelFallsBackToTheWorkspaceDomain: a workspace whose name is empty is
// still named uniquely, so two installations cannot overwrite each other's row.
func TestAccountLabelFallsBackToTheWorkspaceDomain(t *testing.T) {
	adapter, server := newAdapter(t)

	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok": true, "team": "", "url": "https://toko-demo.slack.com/"}`))
	})

	label, err := adapter.AccountLabel(context.Background(), integrationdomain.OAuthTokens{AccessToken: "xoxb-bot-token"})
	require.NoError(t, err)
	require.Equal(t, "https://toko-demo.slack.com/", label)
}

// TestATransportFailureIsNotARefusal: a connection that never happened must not read
// as Slack saying no, because only one of the two is worth acting on.
func TestATransportFailureIsNotARefusal(t *testing.T) {
	adapter, server := newAdapter(t)
	server.Close()

	_, err := adapter.Execute(context.Background(), call(t, "slack.list_channels", `{}`))
	require.Error(t, err)
	require.False(t, errors.Is(err, integrationdomain.ErrRefreshFailed))
	require.Contains(t, err.Error(), "conversations.list")
}
