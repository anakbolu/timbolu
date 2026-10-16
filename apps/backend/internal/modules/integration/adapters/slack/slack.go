// Package slack is the native adapter for Slack: the tools a Bolu uses to read
// the channels a workspace installed it into and to post into one, and the OAuth
// endpoints that install it.
//
// It speaks Slack's Web API directly rather than through a wrapper library, for
// the same reason the Gmail adapter speaks REST: a tool call is one HTTP request,
// and what matters is that a refusal arrives as a reason rather than as a retry.
//
// Two facts about Slack shape this file. Every Web API answer is an envelope that
// carries `ok`, and a refusal is still answered **200** — so the envelope, not the
// status, is what tells a success from a failure. And the credential is a bot
// token (`xoxb-`), which belongs to the installation rather than to a person: the
// scopes below are bot scopes, and every tool acts as the app the workspace
// installed rather than as whoever installed it.
package slack

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

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
)

// App is the application key, and the prefix of every tool this adapter declares.
const App = "slack"

// Slack's endpoints. They are constants rather than configuration because a
// deployment cannot point Slack anywhere else, and a wrong address here would be a
// silent misconfiguration rather than a feature.
const (
	authEndpoint  = "https://slack.com/oauth/v2/authorize"
	tokenEndpoint = "https://slack.com/api/oauth.v2.access"
	apiBase       = "https://slack.com/api"
)

// Scope is the set the tools need, and no more. Reading the channel list needs
// the first and posting needs the second; both are bot scopes, so they are granted
// to the installation rather than to a person.
//
// Reading what was *said* in a channel is deliberately absent: no tool here reads
// message history yet, and asking for a permission nothing uses is how a
// connection becomes frightening to grant. The day `slack.message_received` is
// delivered rather than only declared, `channels:history` joins this list.
const (
	ScopeChannelsRead = "channels:read"
	ScopeChatWrite    = "chat:write"
)

// maxBody bounds what is read from Slack, so an unexpected answer cannot exhaust
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
	// APIBase overrides the Web API endpoint. It exists for the tests, which
	// cannot reach Slack.
	APIBase string
	// AuthEndpoint and TokenEndpoint are overridden by the tests for the same
	// reason.
	AuthEndpoint  string
	TokenEndpoint string
	// Timeout bounds one request. Zero takes the default.
	Timeout time.Duration
}

// Adapter is the Slack integration.
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
		return nil, fmt.Errorf("slack: an OAuth client id and secret are required")
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

// DefaultScopes is what an installation asks for.
func (a *Adapter) DefaultScopes() []string {
	return []string{ScopeChannelsRead, ScopeChatWrite}
}

// Tools is what a Bolu can do with an installed app.
//
// The labels are the whole governance story: reading the channel list needs no
// person, and posting into a channel is something people read, so it always waits
// for one.
func (a *Adapter) Tools() []domain.ToolSpec {
	return []domain.ToolSpec{
		{
			Name:        domain.ToolName(App, "post_message"),
			Label:       domain.LabelWriteExternal,
			Description: "Kirim pesan ke sebuah kanal Slack. Membutuhkan persetujuan manusia.",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"channel":   {"type": "string", "description": "ID atau nama kanal, mis. C012AB3CD atau #umum"},
					"text":      {"type": "string", "description": "Isi pesan"},
					"thread_ts": {"type": "string", "description": "Timestamp pesan induk, untuk membalas di dalam thread"}
				},
				"required": ["channel", "text"],
				"additionalProperties": false
			}`),
		},
		{
			Name:        domain.ToolName(App, "list_channels"),
			Label:       domain.LabelRead,
			Description: "Daftar kanal publik di workspace Slack yang terhubung",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"limit":  {"type": "integer", "minimum": 1, "maximum": 200},
					"cursor": {"type": "string", "description": "Penanda halaman berikutnya dari jawaban sebelumnya"}
				},
				"additionalProperties": false
			}`),
		},
	}
}

// Triggers is what an installed app can tell us about.
//
// Slack pushes, so the trigger is stated as a webhook rather than as a poll — but
// the declaration is only what a routine screen offers. Delivery is the
// deployment's own Events API subscription pointed at the integration webhook
// route, and reading the event's message is the piece that is still missing: this
// adapter declares the trigger and nothing more.
func (a *Adapter) Triggers() []domain.TriggerSpec {
	return []domain.TriggerSpec{{
		App:         App,
		Kind:        domain.TriggerWebhook,
		Event:       "slack.message_received",
		Description: "Pesan baru masuk ke kanal Slack",
	}}
}

// --------------------------------------------------------------------- OAuth

// AuthCodeURL is Slack's consent page for a state.
func (a *Adapter) AuthCodeURL(state, redirectURI string) string {
	query := url.Values{
		"client_id":    {a.clientID},
		"redirect_uri": {redirectURI},
		"state":        {state},
		// Slack takes the bot scopes as one comma-separated list. There is no
		// `user_scope`: every tool here runs as the installation, so a user token
		// would be a credential nothing uses.
		"scope": {strings.Join(a.DefaultScopes(), ",")},
	}
	return a.authURL + "?" + query.Encode()
}

// Exchange trades a code for the installation's bot token.
func (a *Adapter) Exchange(ctx context.Context, code, redirectURI string) (domain.OAuthTokens, error) {
	return a.token(ctx, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {redirectURI},
	})
}

// Refresh trades a refresh token for a new access token.
//
// A bot token from the plain exchange does not expire and comes with no refresh
// token, so a connection made that way has nothing to refresh; only an app with
// token rotation turned on reaches this with a value. A connection that has none is
// reported as the failure a person has to act on — re-installing the app — rather
// than as a transport error.
func (a *Adapter) Refresh(ctx context.Context, refreshToken string) (domain.OAuthTokens, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return domain.OAuthTokens{}, fmt.Errorf(
			"%w: slack issued no refresh token for this installation", domain.ErrRefreshFailed)
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

// AccountLabel names the workspace the app was installed into, so the connection
// is named after the workspace rather than after the moment it was added.
func (a *Adapter) AccountLabel(ctx context.Context, tokens domain.OAuthTokens) (string, error) {
	var profile struct {
		Team string `json:"team"`
		URL  string `json:"url"`
	}

	if err := a.get(ctx, tokens.AccessToken, "auth.test", nil, &profile); err != nil {
		return "", fmt.Errorf("slack: read the workspace name: %w", err)
	}

	// The name is what a person recognises, and the workspace's own domain is what
	// is unique: it is the fallback, so two installations cannot end up sharing a
	// label and overwriting each other on the upsert key.
	label := strings.TrimSpace(profile.Team)
	if label == "" {
		label = strings.TrimSpace(profile.URL)
	}
	if label == "" {
		return "", fmt.Errorf("slack: the installation has no workspace name")
	}
	return label, nil
}

// errTokenRefused is Slack's own refusal, told apart from a transport failure so
// it can become ErrRefreshFailed.
var errTokenRefused = errors.New("slack: slack refused the grant")

// token performs the token-endpoint exchange, for both a code and a refresh.
func (a *Adapter) token(ctx context.Context, form url.Values) (domain.OAuthTokens, error) {
	form.Set("client_id", a.clientID)
	form.Set("client_secret", a.clientSecret)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return domain.OAuthTokens{}, fmt.Errorf("slack: build token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := a.http.Do(request)
	if err != nil {
		return domain.OAuthTokens{}, fmt.Errorf("slack: token request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody))
	if err != nil {
		return domain.OAuthTokens{}, fmt.Errorf("slack: read token answer: %w", err)
	}

	if response.StatusCode >= 400 {
		return domain.OAuthTokens{}, fmt.Errorf("%w (status %d): %s",
			errTokenRefused, response.StatusCode, truncate(string(body), 200))
	}

	var decoded struct {
		OK           bool   `json:"ok"`
		Error        string `json:"error"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return domain.OAuthTokens{}, fmt.Errorf("slack: token answer is not readable: %w", err)
	}

	if !decoded.OK {
		// Slack answers a refused exchange with 200 and `ok:false`, so the envelope
		// is the only place the refusal is visible. A code that was already used,
		// one that expired while the person was deciding, and a revoked refresh
		// token all land here.
		return domain.OAuthTokens{}, fmt.Errorf("%w: %s",
			errTokenRefused, orDefault(decoded.Error, "unknown_error"))
	}

	tokens := domain.OAuthTokens{
		AccessToken:  decoded.AccessToken,
		RefreshToken: decoded.RefreshToken,
		Scopes:       splitScopes(decoded.Scope),
	}
	if decoded.ExpiresIn > 0 {
		// A minute is taken off, so a token that expires while a slow call is in
		// flight is treated as expired rather than used. A bot token without
		// rotation states no expiry, and the token is used until it is refused.
		tokens.ExpiresAt = new(time.Now().Add(time.Duration(decoded.ExpiresIn)*time.Second - time.Minute))
	}
	return tokens, nil
}

// --------------------------------------------------------------------- tools

// Execute runs one tool.
func (a *Adapter) Execute(ctx context.Context, call domain.Call) (domain.Result, error) {
	switch call.Tool {
	case domain.ToolName(App, "post_message"):
		return a.postMessage(ctx, call)
	case domain.ToolName(App, "list_channels"):
		return a.listChannels(ctx, call)
	default:
		return domain.Result{}, fmt.Errorf("%w: %q", domain.ErrUnknownTool, call.Tool)
	}
}

// postMessageInput is the arguments of `slack.post_message`.
type postMessageInput struct {
	Channel  string `json:"channel"`
	Text     string `json:"text"`
	ThreadTS string `json:"thread_ts"`
}

// postMessage posts into a channel.
//
// It is only ever reached after a person approved it: the label is
// `write_external`, so the approval gate holds the task until somebody decides.
func (a *Adapter) postMessage(ctx context.Context, call domain.Call) (domain.Result, error) {
	var input postMessageInput
	if err := json.Unmarshal(call.Arguments, &input); err != nil {
		return domain.Result{}, fmt.Errorf("slack: read post_message arguments: %w", err)
	}
	if strings.TrimSpace(input.Channel) == "" {
		return domain.Result{}, fmt.Errorf("%w: a channel is required", domain.ErrInvalidInput)
	}
	if strings.TrimSpace(input.Text) == "" {
		return domain.Result{}, fmt.Errorf("%w: a message body is required", domain.ErrInvalidInput)
	}

	payload := map[string]any{"channel": input.Channel, "text": input.Text}
	if strings.TrimSpace(input.ThreadTS) != "" {
		payload["thread_ts"] = input.ThreadTS
	}

	var posted struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	if err := a.post(ctx, call.Token.AccessToken, "chat.postMessage", payload, &posted); err != nil {
		return domain.Result{}, err
	}

	content, err := json.Marshal(map[string]any{
		"channel": orDefault(posted.Channel, input.Channel),
		"ts":      posted.TS,
		"posted":  true,
	})
	if err != nil {
		return domain.Result{}, fmt.Errorf("slack: encode post_message answer: %w", err)
	}

	return domain.Result{
		Content: content,
		Summary: fmt.Sprintf("pesan terkirim ke %s", input.Channel),
	}, nil
}

// listChannelsInput is the arguments of `slack.list_channels`.
type listChannelsInput struct {
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor"`
}

// listChannels lists the public channels of the workspace.
//
// Only public channels: reading the private ones needs `groups:read`, which no tool
// here asks for. A page is returned with the cursor that reads the next one, rather
// than the whole workspace at once, because the answer is a model's context.
func (a *Adapter) listChannels(ctx context.Context, call domain.Call) (domain.Result, error) {
	var input listChannelsInput
	if err := json.Unmarshal(call.Arguments, &input); err != nil {
		return domain.Result{}, fmt.Errorf("slack: read list_channels arguments: %w", err)
	}

	limit := input.Limit
	if limit <= 0 || limit > 200 {
		limit = 100
	}

	query := url.Values{
		"types":            {"public_channel"},
		"exclude_archived": {"true"},
		"limit":            {fmt.Sprint(limit)},
	}
	if strings.TrimSpace(input.Cursor) != "" {
		query.Set("cursor", input.Cursor)
	}

	var listing struct {
		Channels []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			IsPrivate  bool   `json:"is_private"`
			NumMembers int    `json:"num_members"`
		} `json:"channels"`
		Metadata struct {
			NextCursor string `json:"next_cursor"`
		} `json:"response_metadata"`
	}
	if err := a.get(ctx, call.Token.AccessToken, "conversations.list", query, &listing); err != nil {
		return domain.Result{}, err
	}

	channels := make([]map[string]any, 0, len(listing.Channels))
	for _, channel := range listing.Channels {
		channels = append(channels, map[string]any{
			"id":          channel.ID,
			"name":        channel.Name,
			"private":     channel.IsPrivate,
			"num_members": channel.NumMembers,
		})
	}

	content, err := json.Marshal(map[string]any{
		"channels":    channels,
		"next_cursor": listing.Metadata.NextCursor,
	})
	if err != nil {
		return domain.Result{}, fmt.Errorf("slack: encode list_channels answer: %w", err)
	}

	return domain.Result{
		Content: content,
		Summary: fmt.Sprintf("%d kanal Slack", len(channels)),
	}, nil
}

// --------------------------------------------------------------- HTTP helpers

// get performs a GET and decodes the answer.
func (a *Adapter) get(ctx context.Context, token, path string, query url.Values, target any) error {
	address := a.apiBase + "/" + path
	if len(query) > 0 {
		address += "?" + query.Encode()
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return fmt.Errorf("slack: build %s request: %w", path, err)
	}
	return a.do(request, token, path, target)
}

// post performs a POST with a JSON body and decodes the answer.
func (a *Adapter) post(ctx context.Context, token, path string, body any, target any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("slack: encode %s body: %w", path, err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiBase+"/"+path, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("slack: build %s request: %w", path, err)
	}
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	return a.do(request, token, path, target)
}

// do sends a request and unwraps the envelope Slack answers everything with.
//
// The status is checked first because a transport-level failure is not a refusal,
// and then the envelope's own `ok` is checked because Slack reports a refusal —
// an unknown channel, a missing scope — as **200** with `ok:false`. Reading only
// the status would turn "the channel does not exist" into a success with empty
// fields.
func (a *Adapter) do(request *http.Request, token, path string, target any) error {
	request.Header.Set("Authorization", "Bearer "+token)

	response, err := a.http.Do(request)
	if err != nil {
		return fmt.Errorf("slack: %s: %w", path, err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody))
	if err != nil {
		return fmt.Errorf("slack: read %s answer: %w", path, err)
	}

	if response.StatusCode >= 400 {
		return fmt.Errorf("slack: %s failed (status %d): %s",
			path, response.StatusCode, truncate(string(body), 300))
	}

	var envelope struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("slack: %s answered something unreadable: %w", path, err)
	}
	if !envelope.OK {
		// The reason is passed on, so the model can fix its own call instead of
		// retrying the same one.
		return fmt.Errorf("slack: %s was refused: %s", path, orDefault(envelope.Error, "unknown_error"))
	}

	if target == nil {
		return nil
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("slack: %s answered something unreadable: %w", path, err)
	}
	return nil
}

// splitScopes reads the scope list Slack answers with, which is comma separated —
// unlike Google's space separated one.
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
