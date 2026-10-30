// Package ghconnector is the native adapter for GitHub: the tools a Bolu uses to
// see the repositories a connection reaches and to file an issue, and the OAuth
// endpoints that connect an account.
//
// It speaks GitHub's REST API directly rather than through a wrapper library, for
// the same reason the Gmail adapter speaks REST: a tool call is one HTTP request,
// and what matters is that a refusal arrives as a reason rather than as a retry.
//
// The package is named `ghconnector` rather than `github` so that a file here can
// still import `github.com/...` without the two reading alike; the directory keeps
// the application's own name.
package ghconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/integration/domain"
)

// App is the application key, and the prefix of every tool this adapter declares.
const App = "github"

// GitHub's endpoints. They are constants rather than configuration because a
// deployment cannot point GitHub anywhere else, and a wrong address here would be a
// silent misconfiguration rather than a feature. GitHub Enterprise is a different
// host, which is why every one of them can be overridden.
const (
	authEndpoint  = "https://github.com/login/oauth/authorize"
	tokenEndpoint = "https://github.com/login/oauth/access_token"
	apiBase       = "https://api.github.com"
)

// Scope is what a connection asks for. `repo` is the narrowest *classic* scope
// that lets `github.create_issue` reach a private repository, and `read:user` is
// what names the connection after the account. Fine-grained tokens are narrower,
// but the OAuth app flow issues classic ones; a deployment that only ever files
// into public repositories can narrow this to `public_repo`.
const (
	ScopeRepo     = "repo"
	ScopeReadUser = "read:user"
)

// GitHub refuses a request without a user agent, so every call carries one.
const userAgent = "timbolu-backend"

// apiVersion pins the REST shape, so a deployment cannot silently start reading a
// different one when GitHub changes its default.
const apiVersion = "2022-11-28"

// maxBody bounds what is read from GitHub, so an unexpected answer cannot exhaust
// memory.
const maxBody = 4 << 20

// Config is what the adapter needs.
type Config struct {
	// ClientID and ClientSecret are the deployment's OAuth application.
	ClientID     string
	ClientSecret string
	// HTTPClient overrides the transport, which is how a test points the adapter
	// at its own server.
	HTTPClient *http.Client
	// APIBase overrides the REST endpoint. It exists for the tests, which cannot
	// reach GitHub, and for GitHub Enterprise.
	APIBase string
	// AuthEndpoint and TokenEndpoint are overridden by the tests for the same
	// reason.
	AuthEndpoint  string
	TokenEndpoint string
	// Timeout bounds one request. Zero takes the default.
	Timeout time.Duration
}

// Adapter is the GitHub integration.
type Adapter struct {
	clientID     string
	clientSecret string
	http         *http.Client
	apiBase      string
	authURL      string
	tokenURL     string
}

// New builds the adapter.
func New(cfg Config) (*Adapter, error) {
	if strings.TrimSpace(cfg.ClientID) == "" || strings.TrimSpace(cfg.ClientSecret) == "" {
		return nil, fmt.Errorf("github: an OAuth client id and secret are required")
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}

	return &Adapter{
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		http:         client,
		apiBase:      orDefault(cfg.APIBase, apiBase),
		authURL:      orDefault(cfg.AuthEndpoint, authEndpoint),
		tokenURL:     orDefault(cfg.TokenEndpoint, tokenEndpoint),
	}, nil
}

// App is the application key.
func (a *Adapter) App() string { return App }

// DefaultScopes is what a connection asks for.
func (a *Adapter) DefaultScopes() []string {
	return []string{ScopeRepo, ScopeReadUser}
}

// Tools is what a Bolu can do with a connected account.
//
// The labels are the whole governance story: listing repositories needs no person,
// and filing an issue is something other people read, so it always waits for one.
func (a *Adapter) Tools() []domain.ToolSpec {
	return []domain.ToolSpec{
		{
			Name:        domain.ToolName(App, "list_repos"),
			Label:       domain.LabelRead,
			Description: "Daftar repositori GitHub yang dapat diakses akun terhubung",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"limit": {"type": "integer", "minimum": 1, "maximum": 100},
					"page":  {"type": "integer", "minimum": 1, "description": "Halaman ke berapa, mulai dari 1"}
				},
				"additionalProperties": false
			}`),
		},
		{
			Name:        domain.ToolName(App, "create_issue"),
			Label:       domain.LabelWriteExternal,
			Description: "Buat issue baru di sebuah repositori GitHub. Membutuhkan persetujuan manusia.",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"owner": {"type": "string", "description": "Pemilik repositori, mis. robzlabz"},
					"repo":  {"type": "string", "description": "Nama repositori"},
					"title": {"type": "string"},
					"body":  {"type": "string", "description": "Isi issue, dalam Markdown"}
				},
				"required": ["owner", "repo", "title"],
				"additionalProperties": false
			}`),
		},
		{
			Name:        domain.ToolName(App, "create_pull_request"),
			Label:       domain.LabelWriteExternal,
			Description: "Buka pull request dari sebuah branch di repositori GitHub. Membutuhkan persetujuan manusia.",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"owner": {"type": "string", "description": "Pemilik repositori, mis. robzlabz"},
					"repo":  {"type": "string", "description": "Nama repositori"},
					"title": {"type": "string"},
					"head":  {"type": "string", "description": "Branch yang berisi perubahan"},
					"base":  {"type": "string", "description": "Branch tujuan, mis. main"},
					"body":  {"type": "string", "description": "Deskripsi pull request, dalam Markdown. Sebut issue-nya, mis. Closes #12"},
					"draft": {"type": "boolean"}
				},
				"required": ["owner", "repo", "title", "head", "base"],
				"additionalProperties": false
			}`),
		},
	}
}

// Triggers is what a connected account can tell us about.
//
// Nothing is declared, and that is the honest answer: GitHub's push events arrive
// through an app installation's webhook, which is a different credential than the
// OAuth token this adapter stores, so a trigger here would be a routine screen
// offering something no delivery could ever fire. A repository poll is the shape
// that would fit, and it is not written yet.
func (a *Adapter) Triggers() []domain.TriggerSpec {
	return nil
}

// --------------------------------------------------------------------- OAuth

// AuthCodeURL is GitHub's consent page for a state.
func (a *Adapter) AuthCodeURL(state, redirectURI string) string {
	query := url.Values{
		"client_id":    {a.clientID},
		"redirect_uri": {redirectURI},
		"state":        {state},
		// GitHub takes the scopes as one space-separated list, which is also the
		// shape DefaultScopes answers in.
		"scope": {strings.Join(a.DefaultScopes(), " ")},
	}
	return a.authURL + "?" + query.Encode()
}

// Exchange trades a code for an access token.
func (a *Adapter) Exchange(ctx context.Context, code, redirectURI string) (domain.OAuthTokens, error) {
	return a.token(ctx, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {redirectURI},
	})
}

// Refresh trades a refresh token for a new access token.
//
// An OAuth app only issues a refresh token when user-to-server token expiration is
// turned on for it; without one the access token is valid until it is revoked and
// there is nothing to refresh. A connection that has none is reported as the
// failure a person has to act on — connecting again — rather than as a transport
// error, and the token endpoint is not called with an empty grant.
func (a *Adapter) Refresh(ctx context.Context, refreshToken string) (domain.OAuthTokens, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return domain.OAuthTokens{}, fmt.Errorf(
			"%w: github issued no refresh token for this connection", domain.ErrRefreshFailed)
	}

	tokens, err := a.token(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
	if err != nil {
		if errors.Is(err, errTokenRefused) {
			return domain.OAuthTokens{}, fmt.Errorf("%w: %w", domain.ErrRefreshFailed, err)
		}
		return domain.OAuthTokens{}, err
	}
	return tokens, nil
}

// AccountLabel reads the login the account belongs to, so the connection is named
// after the account rather than after the moment it was added.
func (a *Adapter) AccountLabel(ctx context.Context, tokens domain.OAuthTokens) (string, error) {
	var profile struct {
		Login string `json:"login"`
	}

	if err := a.get(ctx, tokens.AccessToken, "/user", nil, &profile); err != nil {
		return "", fmt.Errorf("github: read the account login: %w", err)
	}
	if strings.TrimSpace(profile.Login) == "" {
		return "", fmt.Errorf("github: the account has no login")
	}
	return profile.Login, nil
}

// errTokenRefused is GitHub's own refusal, told apart from a transport failure so
// it can become ErrRefreshFailed.
var errTokenRefused = errors.New("github: github refused the grant")

// token performs the token-endpoint exchange, for both a code and a refresh.
//
// GitHub answers the token endpoint with a form-encoded body unless it is asked for
// JSON, so the Accept header is what makes the answer readable at all.
func (a *Adapter) token(ctx context.Context, form url.Values) (domain.OAuthTokens, error) {
	form.Set("client_id", a.clientID)
	form.Set("client_secret", a.clientSecret)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return domain.OAuthTokens{}, fmt.Errorf("github: build token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", userAgent)

	response, err := a.http.Do(request)
	if err != nil {
		return domain.OAuthTokens{}, fmt.Errorf("github: token request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody))
	if err != nil {
		return domain.OAuthTokens{}, fmt.Errorf("github: read token answer: %w", err)
	}

	if response.StatusCode >= 400 {
		return domain.OAuthTokens{}, fmt.Errorf("%w (status %d): %s",
			errTokenRefused, response.StatusCode, truncate(string(body), 200))
	}

	var decoded struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int    `json:"expires_in"`
		Scope            string `json:"scope"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return domain.OAuthTokens{}, fmt.Errorf("github: token answer is not readable: %w", err)
	}

	if decoded.Error != "" {
		// GitHub answers a refused exchange with 200 and an `error` field, so the
		// body is the only place the refusal is visible. A code that was already
		// used, one that expired while the person was deciding, and a revoked
		// refresh token all land here.
		return domain.OAuthTokens{}, fmt.Errorf("%w: %s %s",
			errTokenRefused, decoded.Error, truncate(decoded.ErrorDescription, 200))
	}
	if strings.TrimSpace(decoded.AccessToken) == "" {
		return domain.OAuthTokens{}, fmt.Errorf("%w: github answered no access token", errTokenRefused)
	}

	tokens := domain.OAuthTokens{
		AccessToken:  decoded.AccessToken,
		RefreshToken: decoded.RefreshToken,
		Scopes:       splitScopes(decoded.Scope),
	}
	if decoded.ExpiresIn > 0 {
		// A minute is taken off, so a token that expires while a slow call is in
		// flight is treated as expired rather than used. A token without expiration
		// states none, and is used until it is refused.
		tokens.ExpiresAt = new(time.Now().Add(time.Duration(decoded.ExpiresIn)*time.Second - time.Minute))
	}
	return tokens, nil
}

// --------------------------------------------------------------------- tools

// Execute runs one tool.
func (a *Adapter) Execute(ctx context.Context, call domain.Call) (domain.Result, error) {
	switch call.Tool {
	case domain.ToolName(App, "list_repos"):
		return a.listRepos(ctx, call)
	case domain.ToolName(App, "create_issue"):
		return a.createIssue(ctx, call)
	case domain.ToolName(App, "create_pull_request"):
		return a.createPullRequest(ctx, call)
	default:
		return domain.Result{}, fmt.Errorf("%w: %q", domain.ErrUnknownTool, call.Tool)
	}
}

// listReposInput is the arguments of `github.list_repos`.
type listReposInput struct {
	Limit int `json:"limit"`
	Page  int `json:"page"`
}

// listRepos lists the repositories the connection can reach.
//
// It returns one page of names rather than everything: an account with a thousand
// repositories would otherwise spend a task's whole token budget on one call, and
// the model can ask for the next page.
func (a *Adapter) listRepos(ctx context.Context, call domain.Call) (domain.Result, error) {
	var input listReposInput
	if err := json.Unmarshal(call.Arguments, &input); err != nil {
		return domain.Result{}, fmt.Errorf("github: read list_repos arguments: %w", err)
	}

	limit := input.Limit
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	page := input.Page
	if page <= 0 {
		page = 1
	}

	query := url.Values{
		"per_page": {fmt.Sprint(limit)},
		"page":     {fmt.Sprint(page)},
		"sort":     {"updated"},
	}

	var repos []struct {
		FullName      string `json:"full_name"`
		Name          string `json:"name"`
		Private       bool   `json:"private"`
		HTMLURL       string `json:"html_url"`
		DefaultBranch string `json:"default_branch"`
		UpdatedAt     string `json:"updated_at"`
		Description   string `json:"description"`
	}
	if err := a.get(ctx, call.Token.AccessToken, "/user/repos", query, &repos); err != nil {
		return domain.Result{}, err
	}

	listed := make([]map[string]any, 0, len(repos))
	for _, repo := range repos {
		listed = append(listed, map[string]any{
			"full_name":      repo.FullName,
			"name":           repo.Name,
			"private":        repo.Private,
			"html_url":       repo.HTMLURL,
			"default_branch": repo.DefaultBranch,
			"updated_at":     repo.UpdatedAt,
			"description":    repo.Description,
		})
	}

	content, err := json.Marshal(map[string]any{
		"repos": listed,
		"page":  page,
	})
	if err != nil {
		return domain.Result{}, fmt.Errorf("github: encode list_repos answer: %w", err)
	}

	return domain.Result{
		Content: content,
		Summary: fmt.Sprintf("%d repositori GitHub", len(listed)),
	}, nil
}

// createIssueInput is the arguments of `github.create_issue`.
type createIssueInput struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// createIssue files an issue.
//
// It is only ever reached after a person approved it: the label is
// `write_external`, so the approval gate holds the task until somebody decides.
func (a *Adapter) createIssue(ctx context.Context, call domain.Call) (domain.Result, error) {
	var input createIssueInput
	if err := json.Unmarshal(call.Arguments, &input); err != nil {
		return domain.Result{}, fmt.Errorf("github: read create_issue arguments: %w", err)
	}
	if strings.TrimSpace(input.Owner) == "" || strings.TrimSpace(input.Repo) == "" {
		return domain.Result{}, fmt.Errorf("%w: an owner and a repository are required", domain.ErrInvalidInput)
	}
	if strings.TrimSpace(input.Title) == "" {
		return domain.Result{}, fmt.Errorf("%w: an issue title is required", domain.ErrInvalidInput)
	}

	// The owner and the repository are escaped rather than concatenated: a name
	// with a slash in it would otherwise address a different endpoint than the one
	// the person approved.
	path := "/repos/" + url.PathEscape(input.Owner) + "/" + url.PathEscape(input.Repo) + "/issues"

	var created struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
		Title   string `json:"title"`
	}
	if err := a.post(ctx, call.Token.AccessToken, path, map[string]any{
		"title": input.Title,
		"body":  input.Body,
	}, &created); err != nil {
		return domain.Result{}, err
	}

	content, err := json.Marshal(map[string]any{
		"number":   created.Number,
		"html_url": created.HTMLURL,
		"state":    created.State,
		"title":    created.Title,
	})
	if err != nil {
		return domain.Result{}, fmt.Errorf("github: encode create_issue answer: %w", err)
	}

	return domain.Result{
		Content: content,
		Summary: fmt.Sprintf("issue %s#%d dibuat", input.Owner+"/"+input.Repo, created.Number),
	}, nil
}

// createPullRequestInput is the arguments of `github.create_pull_request`.
type createPullRequestInput struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Body  string `json:"body"`
	Draft bool   `json:"draft"`
}

// createPullRequest opens a pull request from a branch that already holds the
// change.
//
// Like an issue it is only reached after a person approved it. It does not write
// the change: the branch is the work, and opening the pull request is asking
// people to read it.
func (a *Adapter) createPullRequest(ctx context.Context, call domain.Call) (domain.Result, error) {
	var input createPullRequestInput
	if err := json.Unmarshal(call.Arguments, &input); err != nil {
		return domain.Result{}, fmt.Errorf("github: read create_pull_request arguments: %w", err)
	}
	if strings.TrimSpace(input.Owner) == "" || strings.TrimSpace(input.Repo) == "" {
		return domain.Result{}, fmt.Errorf("%w: an owner and a repository are required", domain.ErrInvalidInput)
	}
	if strings.TrimSpace(input.Title) == "" {
		return domain.Result{}, fmt.Errorf("%w: a pull request title is required", domain.ErrInvalidInput)
	}
	if strings.TrimSpace(input.Head) == "" || strings.TrimSpace(input.Base) == "" {
		return domain.Result{}, fmt.Errorf("%w: a head and a base branch are required", domain.ErrInvalidInput)
	}

	path := "/repos/" + url.PathEscape(input.Owner) + "/" + url.PathEscape(input.Repo) + "/pulls"

	var created struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
		Title   string `json:"title"`
		Draft   bool   `json:"draft"`
	}
	if err := a.post(ctx, call.Token.AccessToken, path, map[string]any{
		"title": input.Title,
		"head":  input.Head,
		"base":  input.Base,
		"body":  input.Body,
		"draft": input.Draft,
	}, &created); err != nil {
		return domain.Result{}, err
	}

	content, err := json.Marshal(map[string]any{
		"number":   created.Number,
		"html_url": created.HTMLURL,
		"state":    created.State,
		"title":    created.Title,
		"draft":    created.Draft,
	})
	if err != nil {
		return domain.Result{}, fmt.Errorf("github: encode create_pull_request answer: %w", err)
	}

	return domain.Result{
		Content: content,
		Summary: fmt.Sprintf("pull request %s#%d dibuka", input.Owner+"/"+input.Repo, created.Number),
	}, nil
}

// --------------------------------------------------------------- HTTP helpers

// get performs a GET and decodes the answer.
func (a *Adapter) get(ctx context.Context, token, path string, query url.Values, target any) error {
	address := a.apiBase + path
	if len(query) > 0 {
		address += "?" + query.Encode()
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return fmt.Errorf("github: build %s request: %w", path, err)
	}
	return a.do(request, token, path, target)
}

// post performs a POST with a JSON body and decodes the answer.
func (a *Adapter) post(ctx context.Context, token, path string, body any, target any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("github: encode %s body: %w", path, err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiBase+path, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("github: build %s request: %w", path, err)
	}
	request.Header.Set("Content-Type", "application/json")
	return a.do(request, token, path, target)
}

// do sends a request and decodes the answer.
//
// The status is checked before the body is decoded, because GitHub reports a
// refusal as `{"message": "..."}` rather than as the shape the caller asked for:
// decoding first would turn "the repository is not there" into "the answer was
// unreadable".
func (a *Adapter) do(request *http.Request, token, path string, target any) error {
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", apiVersion)
	// GitHub refuses a request with no user agent, so this is a requirement rather
	// than a courtesy.
	request.Header.Set("User-Agent", userAgent)

	response, err := a.http.Do(request)
	if err != nil {
		return fmt.Errorf("github: %s: %w", path, err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody))
	if err != nil {
		return fmt.Errorf("github: read %s answer: %w", path, err)
	}

	if response.StatusCode >= 400 {
		// The reason is passed on, so the model can fix its own call instead of
		// retrying the same one.
		return fmt.Errorf("github: %s failed (status %d): %s",
			path, response.StatusCode, truncate(apiReason(body), 300))
	}

	if target == nil {
		return nil
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("github: %s answered something unreadable: %w", path, err)
	}
	return nil
}

// apiReason reads the sentence GitHub puts on a refusal, falling back to the body
// when the answer is not the shape it documents.
func apiReason(body []byte) string {
	var refusal struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &refusal); err == nil && strings.TrimSpace(refusal.Message) != "" {
		return refusal.Message
	}
	return string(body)
}

// splitScopes reads the scope list GitHub answers with, which is comma separated.
func splitScopes(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ' '
	})

	scopes := make([]string, 0, len(fields))
	for _, field := range fields {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			scopes = append(scopes, trimmed)
		}
	}
	return scopes
}

// orDefault takes a configured value or the constant.
func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// truncate bounds what an error message carries.
func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// compile-time check that the adapter satisfies the ports it is wired to.
var (
	_ domain.Adapter      = (*Adapter)(nil)
	_ domain.OAuthAdapter = (*Adapter)(nil)
)
