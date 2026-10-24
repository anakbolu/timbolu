package ghconnector_test

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

	ghconnector "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/adapters/github"
	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
)

// The adapter's contract with the runtime is what this file tests: the URL it sends
// a person to, the two shapes GitHub's token endpoint answers with (a refusal is a
// 200 carrying `error`), which tools it declares and with which labels, and what a
// call sends and reads back. GitHub itself is a stub, so what is under test is this
// deployment's own behaviour.

// newAdapter points an adapter at one stub server, which answers both the token
// endpoint and the REST API.
func newAdapter(t *testing.T) (*ghconnector.Adapter, *httptest.Server) {
	t.Helper()

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))

	adapter, err := ghconnector.New(ghconnector.Config{
		ClientID:      "client-id",
		ClientSecret:  "client-secret",
		HTTPClient:    server.Client(),
		AuthEndpoint:  "https://github.com/login/oauth/authorize",
		TokenEndpoint: server.URL + "/login/oauth/access_token",
		APIBase:       server.URL,
	})
	require.NoError(t, err)

	t.Cleanup(server.Close)
	return adapter, server
}

// TestNewRequiresAnOAuthApplication: a deployment that has not registered one is
// disabled by the container, and this is the same refusal one level down.
func TestNewRequiresAnOAuthApplication(t *testing.T) {
	for name, cfg := range map[string]ghconnector.Config{
		"no client id":     {ClientSecret: "secret"},
		"no client secret": {ClientID: "id"},
		"blank":            {ClientID: "  ", ClientSecret: "  "},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ghconnector.New(cfg)
			require.Error(t, err)
		})
	}
}

// TestAuthCodeURLIsGitHubsConsentPage: the shape a browser is sent to, including the
// state the flow verifies the callback against and the scopes the app asks for.
func TestAuthCodeURLIsGitHubsConsentPage(t *testing.T) {
	adapter, _ := newAdapter(t)

	raw := adapter.AuthCodeURL("state-value", "https://app.example.com/api/integrations/oauth/callback")

	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	require.Equal(t, "github.com", parsed.Host)
	require.Equal(t, "/login/oauth/authorize", parsed.Path)

	query := parsed.Query()
	require.Equal(t, "client-id", query.Get("client_id"))
	require.Equal(t, "state-value", query.Get("state"))
	require.Equal(t, "https://app.example.com/api/integrations/oauth/callback", query.Get("redirect_uri"))
	require.Equal(t, "repo read:user", query.Get("scope"), "GitHub takes the scopes space separated")
	require.Equal(t, []string{"repo", "read:user"}, adapter.DefaultScopes())
}

// TestExchangeTradesACodeForAToken covers the happy path and the two headers the
// token endpoint needs: GitHub answers form-encoded unless it is asked for JSON.
func TestExchangeTradesACodeForAToken(t *testing.T) {
	adapter, server := newAdapter(t)

	var (
		received url.Values
		accept   string
		agent    string
	)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/login/oauth/access_token", r.URL.Path)
		require.NoError(t, r.ParseForm())
		received, accept, agent = r.PostForm, r.Header.Get("Accept"), r.Header.Get("User-Agent")

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"access_token": "gho_token",
			"token_type": "bearer",
			"scope": "repo,read:user"
		}`))
	})

	tokens, err := adapter.Exchange(context.Background(), "the-code", "https://app.example.com/callback")
	require.NoError(t, err)

	require.Equal(t, "gho_token", tokens.AccessToken)
	require.Empty(t, tokens.RefreshToken, "an OAuth app without expiration issues no refresh token")
	require.Nil(t, tokens.ExpiresAt, "a token that does not expire states no expiry")
	require.Equal(t, []string{"repo", "read:user"}, tokens.Scopes)

	require.Equal(t, "application/json", accept)
	require.NotEmpty(t, agent, "GitHub refuses a request with no user agent")
	require.Equal(t, "authorization_code", received.Get("grant_type"))
	require.Equal(t, "the-code", received.Get("code"))
	require.Equal(t, "https://app.example.com/callback", received.Get("redirect_uri"))
	require.Equal(t, "client-id", received.Get("client_id"))
	require.Equal(t, "client-secret", received.Get("client_secret"))
}

// TestExchangeReportsARefusalGitHubAnswersWithTwoHundred is the failure mode that
// makes the body the only place the refusal is visible.
func TestExchangeReportsARefusalGitHubAnswersWithTwoHundred(t *testing.T) {
	adapter, server := newAdapter(t)

	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"error": "bad_verification_code",
			"error_description": "The code passed is incorrect or expired.",
			"error_uri": "https://docs.github.com/apps/managing-oauth-apps/troubleshooting-oauth-app-access-token-request-errors"
		}`))
	})

	_, err := adapter.Exchange(context.Background(), "spent-code", "https://app.example.com/callback")
	require.Error(t, err)
	require.Contains(t, err.Error(), "bad_verification_code")
	require.Contains(t, err.Error(), "incorrect or expired")
}

// TestExchangeRefusesAnAnswerWithNoToken: an answer that is readable and carries
// nothing must not become an empty connection.
func TestExchangeRefusesAnAnswerWithNoToken(t *testing.T) {
	adapter, server := newAdapter(t)

	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token_type": "bearer"}`))
	})

	_, err := adapter.Exchange(context.Background(), "the-code", "https://app.example.com/callback")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no access token")
}

// TestRefreshWithoutARefreshTokenIsTheFailureAPersonActsOn: an OAuth app without
// user-to-server token expiration issues none, so the connection is reported as
// needing a fresh login rather than sent to an endpoint that would refuse it.
func TestRefreshWithoutARefreshTokenIsTheFailureAPersonActsOn(t *testing.T) {
	adapter, server := newAdapter(t)

	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the token endpoint must not be called without a refresh token")
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})

	_, err := adapter.Refresh(context.Background(), "  ")
	require.ErrorIs(t, err, integrationdomain.ErrRefreshFailed)
	require.Contains(t, err.Error(), "no refresh token")
}

// TestRefreshRotatesATokenWhenGitHubIssuedOne covers the grant an OAuth app with
// token expiration turned on answers.
func TestRefreshRotatesATokenWhenGitHubIssuedOne(t *testing.T) {
	adapter, server := newAdapter(t)

	var received url.Values
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		received = r.PostForm

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"access_token": "gho_rotated",
			"refresh_token": "ghr_new",
			"expires_in": 28800,
			"scope": "repo,read:user"
		}`))
	})

	tokens, err := adapter.Refresh(context.Background(), "ghr_old")
	require.NoError(t, err)

	require.Equal(t, "gho_rotated", tokens.AccessToken)
	require.Equal(t, "ghr_new", tokens.RefreshToken)
	require.NotNil(t, tokens.ExpiresAt)
	require.Equal(t, "refresh_token", received.Get("grant_type"))
	require.Equal(t, "ghr_old", received.Get("refresh_token"))
}

// TestARefusedRefreshBecomesErrRefreshFailed: a grant GitHub no longer accepts is
// the one refresh failure a person has to act on.
func TestARefusedRefreshBecomesErrRefreshFailed(t *testing.T) {
	adapter, server := newAdapter(t)

	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error": "bad_refresh_token", "error_description": "The refresh token is incorrect."}`))
	})

	_, err := adapter.Refresh(context.Background(), "revoked")
	require.ErrorIs(t, err, integrationdomain.ErrRefreshFailed)
	require.Contains(t, err.Error(), "bad_refresh_token")
}

// TestToolsAreNamedAndLabelled: every tool is `github.action`, every spec passes the
// registry's own validation, and the labels are the governance story — reading needs
// nobody, filing an issue is read by other people and always waits for one.
func TestToolsAreNamedAndLabelled(t *testing.T) {
	adapter, _ := newAdapter(t)

	tools := adapter.Tools()
	require.NotEmpty(t, tools)

	byName := map[string]integrationdomain.ToolSpec{}
	for _, tool := range tools {
		require.NoError(t, tool.Valid(), tool.Name)
		require.Equal(t, ghconnector.App, tool.App(), tool.Name)
		byName[tool.Name] = tool
	}

	require.Equal(t, integrationdomain.LabelRead, byName["github.list_repos"].Label)
	require.Equal(t, integrationdomain.LabelWriteExternal, byName["github.create_issue"].Label)
	require.Equal(t, integrationdomain.LabelWriteExternal, byName["github.create_pull_request"].Label)
}

// TestNoTriggerIsDeclared: a webhook here would need an app installation, which is a
// different credential than the token this adapter stores, so a trigger would be a
// routine screen offering something no delivery could fire.
func TestNoTriggerIsDeclared(t *testing.T) {
	adapter, _ := newAdapter(t)

	require.Empty(t, adapter.Triggers())
}

// call builds one tool invocation with the token the runtime would have opened.
func call(t *testing.T, tool string, arguments string) integrationdomain.Call {
	t.Helper()

	return integrationdomain.Call{
		IntegrationID: uuid.New(),
		Tool:          tool,
		Arguments:     json.RawMessage(arguments),
		Token:         integrationdomain.Token{AccessToken: "gho_token"},
	}
}

// TestListReposSendsTheTokenAndThePinnedHeaders: the read tool's request, including
// the user agent GitHub requires and the page it asks for.
func TestListReposSendsTheTokenAndThePinnedHeaders(t *testing.T) {
	adapter, server := newAdapter(t)

	var (
		path    string
		query   url.Values
		headers http.Header
	)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, query, headers = r.URL.Path, r.URL.Query(), r.Header

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{
				"full_name": "robzlabz/timbolu",
				"name": "timbolu",
				"private": true,
				"html_url": "https://github.com/robzlabz/timbolu",
				"default_branch": "main",
				"updated_at": "2026-10-16T03:00:00Z",
				"description": "Bolu untuk toko"
			}
		]`))
	})

	result, err := adapter.Execute(context.Background(), call(t, "github.list_repos", `{"limit": 10, "page": 2}`))
	require.NoError(t, err)

	require.Equal(t, "/user/repos", path)
	require.Equal(t, "10", query.Get("per_page"))
	require.Equal(t, "2", query.Get("page"))
	require.Equal(t, "Bearer gho_token", headers.Get("Authorization"))
	require.Equal(t, "application/vnd.github+json", headers.Get("Accept"))
	require.NotEmpty(t, headers.Get("User-Agent"), "GitHub refuses a request with no user agent")

	var decoded struct {
		Repos []struct {
			FullName string `json:"full_name"`
			Private  bool   `json:"private"`
		} `json:"repos"`
	}
	require.NoError(t, json.Unmarshal(result.Content, &decoded))
	require.Len(t, decoded.Repos, 1)
	require.Equal(t, "robzlabz/timbolu", decoded.Repos[0].FullName)
	require.True(t, decoded.Repos[0].Private)
}

// TestCreateIssuePostsIntoTheNamedRepository: the write tool's request, and the
// number and link that come back so a person can follow what was filed.
func TestCreateIssuePostsIntoTheNamedRepository(t *testing.T) {
	adapter, server := newAdapter(t)

	var (
		method string
		path   string
		body   map[string]any
	)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"number": 178,
			"html_url": "https://github.com/robzlabz/timbolu/issues/178",
			"state": "open",
			"title": "Stok gula habis"
		}`))
	})

	result, err := adapter.Execute(context.Background(), call(t, "github.create_issue",
		`{"owner": "robzlabz", "repo": "timbolu", "title": "Stok gula habis", "body": "Sisa 2 kg."}`))
	require.NoError(t, err)

	require.Equal(t, http.MethodPost, method)
	require.Equal(t, "/repos/robzlabz/timbolu/issues", path)
	require.Equal(t, map[string]any{"title": "Stok gula habis", "body": "Sisa 2 kg."}, body)
	require.Contains(t, result.Summary, "robzlabz/timbolu#178")

	var decoded struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	require.NoError(t, json.Unmarshal(result.Content, &decoded))
	require.Equal(t, 178, decoded.Number)
	require.Equal(t, "https://github.com/robzlabz/timbolu/issues/178", decoded.HTMLURL)
}

// TestCreateIssueRefusesAnIncompleteCall: a call the schema would have caught is
// refused here too, so the adapter is safe to call directly.
func TestCreateIssueRefusesAnIncompleteCall(t *testing.T) {
	adapter, _ := newAdapter(t)

	_, err := adapter.Execute(context.Background(), call(t, "github.create_issue",
		`{"owner": "robzlabz", "repo": "timbolu", "title": "  "}`))
	require.ErrorIs(t, err, integrationdomain.ErrInvalidInput)
}

// TestCreatePullRequestPostsTheBranches: the pull request is opened from the
// named head into the named base, and its number and link come back.
func TestCreatePullRequestPostsTheBranches(t *testing.T) {
	adapter, server := newAdapter(t)

	var (
		method string
		path   string
		body   map[string]any
	)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"number": 290,
			"html_url": "https://github.com/robzlabz/timbolu/pull/290",
			"state": "open",
			"title": "Panel thread",
			"draft": true
		}`))
	})

	result, err := adapter.Execute(context.Background(), call(t, "github.create_pull_request",
		`{"owner": "robzlabz", "repo": "timbolu", "title": "Panel thread", "head": "fitur/thread", "base": "main", "body": "Closes #289", "draft": true}`))
	require.NoError(t, err)

	require.Equal(t, http.MethodPost, method)
	require.Equal(t, "/repos/robzlabz/timbolu/pulls", path)
	require.Equal(t, map[string]any{
		"title": "Panel thread", "head": "fitur/thread", "base": "main", "body": "Closes #289", "draft": true,
	}, body)
	require.Contains(t, result.Summary, "robzlabz/timbolu#290")

	var decoded struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Draft   bool   `json:"draft"`
	}
	require.NoError(t, json.Unmarshal(result.Content, &decoded))
	require.Equal(t, 290, decoded.Number)
	require.Equal(t, "https://github.com/robzlabz/timbolu/pull/290", decoded.HTMLURL)
	require.True(t, decoded.Draft)
}

// TestCreatePullRequestNeedsBothBranches: a pull request with no head or base
// would ask GitHub to guess, so it is refused before it is sent.
func TestCreatePullRequestNeedsBothBranches(t *testing.T) {
	adapter, _ := newAdapter(t)

	_, err := adapter.Execute(context.Background(), call(t, "github.create_pull_request",
		`{"owner": "robzlabz", "repo": "timbolu", "title": "Panel thread", "head": "fitur/thread"}`))
	require.ErrorIs(t, err, integrationdomain.ErrInvalidInput)
}

// TestARefusalCarriesGitHubsOwnSentence: the reason is what the model reads, so the
// `message` field is unwrapped rather than the whole body being pasted in.
func TestARefusalCarriesGitHubsOwnSentence(t *testing.T) {
	adapter, server := newAdapter(t)

	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message": "Not Found", "documentation_url": "https://docs.github.com/rest"}`))
	})

	_, err := adapter.Execute(context.Background(), call(t, "github.create_issue",
		`{"owner": "robzlabz", "repo": "hilang", "title": "hai"}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "Not Found")
	require.False(t, strings.Contains(err.Error(), "documentation_url"),
		"the sentence is passed on; the documentation link is not")
}

// TestUnknownToolIsRefused keeps a typo in a tool name from reaching GitHub.
func TestUnknownToolIsRefused(t *testing.T) {
	adapter, _ := newAdapter(t)

	_, err := adapter.Execute(context.Background(), call(t, "github.delete_repo", `{}`))
	require.ErrorIs(t, err, integrationdomain.ErrUnknownTool)
}

// TestAccountLabelNamesTheAccount: the connection is named after the login rather
// than after the moment it was added.
func TestAccountLabelNamesTheAccount(t *testing.T) {
	adapter, server := newAdapter(t)

	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/user", r.URL.Path)
		require.Equal(t, "Bearer gho_token", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login": "robzlabz"}`))
	})

	label, err := adapter.AccountLabel(context.Background(), integrationdomain.OAuthTokens{AccessToken: "gho_token"})
	require.NoError(t, err)
	require.Equal(t, "robzlabz", label)
}

// TestATransportFailureIsNotARefusal: a connection that never happened must not read
// as GitHub saying no, because only one of the two is worth acting on.
func TestATransportFailureIsNotARefusal(t *testing.T) {
	adapter, server := newAdapter(t)
	server.Close()

	_, err := adapter.Execute(context.Background(), call(t, "github.list_repos", `{}`))
	require.Error(t, err)
	require.False(t, errors.Is(err, integrationdomain.ErrRefreshFailed))
	require.Contains(t, err.Error(), "/user/repos")
}
