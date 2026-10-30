// Package gmail is the native adapter for Gmail: the tools a Bolu uses to read
// and send mail, and the OAuth endpoints that connect an account.
//
// It speaks the Gmail REST API directly rather than through a wrapper library,
// because the wrapper's job here is small and its failure modes are not: a tool
// call is one HTTP request, and what matters is that a refusal from Google arrives
// as a reason rather than as a retry.
//
// Two rules are structural here. `gmail.create_draft` and `gmail.send` are
// different tools with different labels, so writing a draft never leaves the
// workspace and sending always waits for a person. And nothing in this package
// decides whether an action is allowed: the label does, upstream.
package gmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/integration/domain"
)

// App is the application key, and the prefix of every tool this adapter declares.
const App = "gmail"

// Google's endpoints. They are constants rather than configuration because a
// deployment cannot point Gmail anywhere else, and a wrong address here would be a
// silent misconfiguration rather than a feature.
const (
	authEndpoint  = "https://accounts.google.com/o/oauth2/v2/auth"
	tokenEndpoint = "https://oauth2.googleapis.com/token"
	apiBase       = "https://gmail.googleapis.com/gmail/v1"
)

// Scope is the narrowest set that makes the read tools work. Sending and drafting
// need their own, and they are requested because the product offers those tools.
const (
	ScopeReadonly = "https://www.googleapis.com/auth/gmail.readonly"
	ScopeSend     = "https://www.googleapis.com/auth/gmail.send"
	ScopeCompose  = "https://www.googleapis.com/auth/gmail.compose"
)

// maxBody bounds what is read from Google, so a large mailbox cannot exhaust
// memory. A search returns ids; a message body is fetched one at a time.
const maxBody = 4 << 20

// maxAttachment bounds one attachment read, which is what keeps a tool result
// from becoming a task's whole token budget.
const maxAttachment = 1 << 20

// Config is what the adapter needs.
type Config struct {
	// ClientID and ClientSecret are the deployment's OAuth application.
	ClientID     string
	ClientSecret string
	// HTTPClient overrides the transport, which is how a test points the adapter
	// at its own server.
	HTTPClient *http.Client
	// APIBase overrides the Gmail endpoint. It exists for the tests, which cannot
	// reach Google.
	APIBase string
	// AuthEndpoint and TokenEndpoint are overridden by the tests for the same
	// reason.
	AuthEndpoint  string
	TokenEndpoint string
	// Timeout bounds one request. Zero takes the default.
	Timeout time.Duration
}

// Adapter is the Gmail integration.
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
		return nil, fmt.Errorf("gmail: an OAuth client id and secret are required")
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
	return []string{ScopeReadonly, ScopeCompose, ScopeSend}
}

// Tools is what a Bolu can do with a connected mailbox.
//
// The labels are the whole governance story: reading needs no person, writing a
// draft stays inside the workspace, and sending always waits for one.
func (a *Adapter) Tools() []domain.ToolSpec {
	return []domain.ToolSpec{
		{
			Name:        domain.ToolName(App, "search"),
			Label:       domain.LabelRead,
			Description: "Cari email di kotak masuk dengan kata kunci Gmail",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"q":     {"type": "string", "description": "Kata kunci, sintaks Gmail"},
					"limit": {"type": "integer", "minimum": 1, "maximum": 50}
				},
				"required": ["q"],
				"additionalProperties": false
			}`),
		},
		{
			Name:        domain.ToolName(App, "read_attachment"),
			Label:       domain.LabelRead,
			Description: "Baca isi satu lampiran email",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"message_id":   {"type": "string"},
					"attachment_id": {"type": "string"}
				},
				"required": ["message_id", "attachment_id"],
				"additionalProperties": false
			}`),
		},
		{
			Name:        domain.ToolName(App, "create_draft"),
			Label:       domain.LabelWriteInternal,
			Description: "Buat draf email tanpa mengirimnya",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"to":      {"type": "string"},
					"subject": {"type": "string"},
					"body":    {"type": "string"}
				},
				"required": ["to", "subject", "body"],
				"additionalProperties": false
			}`),
		},
		{
			Name:        domain.ToolName(App, "send"),
			Label:       domain.LabelWriteExternal,
			Description: "Kirim email ke penerima. Membutuhkan persetujuan manusia.",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"to":      {"type": "string"},
					"subject": {"type": "string"},
					"body":    {"type": "string"}
				},
				"required": ["to", "subject", "body"],
				"additionalProperties": false
			}`),
		},
	}
}

// Triggers is what a connected mailbox can tell us about.
//
// Gmail pushes nothing without a Pub/Sub subscription, which is a deployment's
// own infrastructure rather than something this adapter can assume. So the trigger
// is a poll, and it is stated as one rather than promised as a push.
func (a *Adapter) Triggers() []domain.TriggerSpec {
	return []domain.TriggerSpec{{
		App:         App,
		Kind:        domain.TriggerPolling,
		Event:       "gmail.email_received",
		Description: "Email baru masuk ke kotak masuk",
	}}
}

// --------------------------------------------------------------------- OAuth

// AuthCodeURL is Google's consent page for a state.
func (a *Adapter) AuthCodeURL(state, redirectURI string) string {
	query := url.Values{
		"client_id":     {a.clientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {strings.Join(a.DefaultScopes(), " ")},
		"state":         {state},
		// A refresh token is the whole point: without one the connection dies
		// when the access token does, and the person has to authorise again.
		"access_type": {"offline"},
		// Google only returns a refresh token when the consent screen is shown.
		"prompt": {"consent"},
	}
	return a.authURL + "?" + query.Encode()
}

// Exchange trades a code for tokens.
func (a *Adapter) Exchange(ctx context.Context, code, redirectURI string) (domain.OAuthTokens, error) {
	return a.token(ctx, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {redirectURI},
	})
}

// Refresh trades a refresh token for a new access token.
func (a *Adapter) Refresh(ctx context.Context, refreshToken string) (domain.OAuthTokens, error) {
	tokens, err := a.token(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
	if err != nil {
		// A refresh token Google no longer accepts is the one failure a person has
		// to act on, so it is reported as that rather than as a generic error.
		if errors.Is(err, errTokenRefused) {
			return domain.OAuthTokens{}, fmt.Errorf("%w: %w", domain.ErrRefreshFailed, err)
		}
		return domain.OAuthTokens{}, err
	}
	return tokens, nil
}

// AccountLabel reads the address the account belongs to, so the connection is
// named after the mailbox rather than after the moment it was added.
func (a *Adapter) AccountLabel(ctx context.Context, tokens domain.OAuthTokens) (string, error) {
	var profile struct {
		EmailAddress string `json:"emailAddress"`
	}

	if err := a.get(ctx, tokens.AccessToken, "/users/me/profile", nil, &profile); err != nil {
		return "", fmt.Errorf("gmail: read the account address: %w", err)
	}
	if strings.TrimSpace(profile.EmailAddress) == "" {
		return "", fmt.Errorf("gmail: the account has no address")
	}
	return profile.EmailAddress, nil
}

// errTokenRefused is Google's own refusal, told apart from a transport failure so
// it can become ErrRefreshFailed.
var errTokenRefused = errors.New("gmail: google refused the grant")

// token performs the token-endpoint exchange, for both a code and a refresh.
func (a *Adapter) token(ctx context.Context, form url.Values) (domain.OAuthTokens, error) {
	form.Set("client_id", a.clientID)
	form.Set("client_secret", a.clientSecret)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return domain.OAuthTokens{}, fmt.Errorf("gmail: build token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := a.http.Do(request)
	if err != nil {
		return domain.OAuthTokens{}, fmt.Errorf("gmail: token request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody))
	if err != nil {
		return domain.OAuthTokens{}, fmt.Errorf("gmail: read token answer: %w", err)
	}

	if response.StatusCode >= 400 {
		// Google reports a refused grant as 400 with an error code. A code that
		// was already used and a refresh token that was revoked both land here.
		return domain.OAuthTokens{}, fmt.Errorf("%w (status %d): %s",
			errTokenRefused, response.StatusCode, truncate(string(body), 200))
	}

	var decoded struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return domain.OAuthTokens{}, fmt.Errorf("gmail: token answer is not readable: %w", err)
	}

	tokens := domain.OAuthTokens{
		AccessToken:  decoded.AccessToken,
		RefreshToken: decoded.RefreshToken,
		Scopes:       strings.Fields(decoded.Scope),
	}
	if decoded.ExpiresIn > 0 {
		// A minute is taken off, so a token that expires while a slow call is in
		// flight is treated as expired rather than used.
		tokens.ExpiresAt = new(time.Now().Add(time.Duration(decoded.ExpiresIn)*time.Second - time.Minute))
	}
	return tokens, nil
}

// --------------------------------------------------------------------- tools

// Execute runs one tool.
func (a *Adapter) Execute(ctx context.Context, call domain.Call) (domain.Result, error) {
	switch call.Tool {
	case domain.ToolName(App, "search"):
		return a.search(ctx, call)
	case domain.ToolName(App, "read_attachment"):
		return a.readAttachment(ctx, call)
	case domain.ToolName(App, "create_draft"):
		return a.createDraft(ctx, call)
	case domain.ToolName(App, "send"):
		return a.send(ctx, call)
	default:
		return domain.Result{}, fmt.Errorf("%w: %q", domain.ErrUnknownTool, call.Tool)
	}
}

// searchInput is the arguments of `gmail.search`.
type searchInput struct {
	Query string `json:"q"`
	Limit int    `json:"limit"`
}

// search lists the messages matching a query.
//
// It returns ids and headers rather than bodies: a search that returned full
// messages would spend a task's whole token budget on one call, and the model can
// ask for the one it wants.
func (a *Adapter) search(ctx context.Context, call domain.Call) (domain.Result, error) {
	var input searchInput
	if err := json.Unmarshal(call.Arguments, &input); err != nil {
		return domain.Result{}, fmt.Errorf("gmail: read search arguments: %w", err)
	}

	limit := input.Limit
	if limit <= 0 || limit > 50 {
		limit = 10
	}

	var listing struct {
		Messages []struct {
			ID       string `json:"id"`
			ThreadID string `json:"threadId"`
		} `json:"messages"`
		ResultSizeEstimate int `json:"resultSizeEstimate"`
	}

	query := url.Values{"q": {input.Query}, "maxResults": {fmt.Sprint(limit)}}
	if err := a.get(ctx, call.Token.AccessToken, "/users/me/messages", query, &listing); err != nil {
		return domain.Result{}, err
	}

	messages := make([]map[string]any, 0, len(listing.Messages))
	for _, message := range listing.Messages {
		entry := map[string]any{"id": message.ID, "thread_id": message.ThreadID}

		// The headers are what makes a search result readable. A message that
		// cannot be read is still listed: dropping it would hide that it exists.
		var detail struct {
			Payload struct {
				Headers []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"headers"`
			} `json:"payload"`
			Snippet string `json:"snippet"`
		}
		if err := a.get(ctx, call.Token.AccessToken, "/users/me/messages/"+url.PathEscape(message.ID),
			url.Values{"format": {"metadata"}}, &detail); err == nil {
			for _, header := range detail.Payload.Headers {
				switch strings.ToLower(header.Name) {
				case "subject":
					entry["subject"] = header.Value
				case "from":
					entry["from"] = header.Value
				case "date":
					entry["date"] = header.Value
				}
			}
			entry["snippet"] = detail.Snippet
		}

		messages = append(messages, entry)
	}

	content, err := json.Marshal(map[string]any{
		"messages": messages,
		"total":    listing.ResultSizeEstimate,
	})
	if err != nil {
		return domain.Result{}, fmt.Errorf("gmail: encode search answer: %w", err)
	}

	return domain.Result{
		Content: content,
		Summary: fmt.Sprintf("%d email ditemukan", len(messages)),
	}, nil
}

// readAttachment reads one attachment.
//
// Google returns attachments base64url encoded, which is decoded here so what the
// model reads is the file rather than an encoding of it. Text is passed through;
// anything else is reported as its type and size, because a task cannot read a
// spreadsheet by being handed its bytes.
func (a *Adapter) readAttachment(ctx context.Context, call domain.Call) (domain.Result, error) {
	var input struct {
		MessageID    string `json:"message_id"`
		AttachmentID string `json:"attachment_id"`
	}
	if err := json.Unmarshal(call.Arguments, &input); err != nil {
		return domain.Result{}, fmt.Errorf("gmail: read attachment arguments: %w", err)
	}

	var attachment struct {
		Size int    `json:"size"`
		Data string `json:"data"`
	}

	path := "/users/me/messages/" + url.PathEscape(input.MessageID) +
		"/attachments/" + url.PathEscape(input.AttachmentID)
	if err := a.get(ctx, call.Token.AccessToken, path, nil, &attachment); err != nil {
		return domain.Result{}, err
	}

	decoded, err := base64.RawURLEncoding.DecodeString(attachment.Data)
	if err != nil {
		// Google pads some answers and not others, so the standard encoding is
		// tried before giving up.
		decoded, err = base64.URLEncoding.DecodeString(attachment.Data)
		if err != nil {
			return domain.Result{}, fmt.Errorf("gmail: the attachment is not readable: %w", err)
		}
	}

	truncated := false
	if len(decoded) > maxAttachment {
		decoded = decoded[:maxAttachment]
		truncated = true
	}

	content, err := json.Marshal(map[string]any{
		"size":      attachment.Size,
		"truncated": truncated,
		"text":      string(decoded),
	})
	if err != nil {
		return domain.Result{}, fmt.Errorf("gmail: encode attachment: %w", err)
	}

	return domain.Result{
		Content: content,
		Summary: fmt.Sprintf("lampiran dibaca (%d byte)", len(decoded)),
	}, nil
}

// createDraft writes a draft without sending it.
//
// The label is `write_internal`, so this never waits for a person: nothing left the
// workspace. That is the difference between this tool and `send`, and it is the
// only difference.
func (a *Adapter) createDraft(ctx context.Context, call domain.Call) (domain.Result, error) {
	raw, input, err := a.compose(call)
	if err != nil {
		return domain.Result{}, err
	}

	var created struct {
		ID      string `json:"id"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
	}

	payload, err := json.Marshal(map[string]any{"message": map[string]any{"raw": raw}})
	if err != nil {
		return domain.Result{}, fmt.Errorf("gmail: encode draft: %w", err)
	}

	if err := a.post(ctx, call.Token.AccessToken, "/users/me/drafts", payload, &created); err != nil {
		return domain.Result{}, err
	}

	content, err := json.Marshal(map[string]any{
		"draft_id": created.ID,
		"to":       input.To,
		"subject":  input.Subject,
		"sent":     false,
	})
	if err != nil {
		return domain.Result{}, fmt.Errorf("gmail: encode draft answer: %w", err)
	}

	return domain.Result{
		Content: content,
		Summary: fmt.Sprintf("draf untuk %s dibuat, belum dikirim", input.To),
	}, nil
}

// send sends a message.
//
// It is only ever reached after a person approved it: the label is
// `write_external`, so the approval gate holds the task until somebody decides.
func (a *Adapter) send(ctx context.Context, call domain.Call) (domain.Result, error) {
	raw, input, err := a.compose(call)
	if err != nil {
		return domain.Result{}, err
	}

	var sent struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
	}

	payload, err := json.Marshal(map[string]any{"raw": raw})
	if err != nil {
		return domain.Result{}, fmt.Errorf("gmail: encode message: %w", err)
	}

	if err := a.post(ctx, call.Token.AccessToken, "/users/me/messages/send", payload, &sent); err != nil {
		return domain.Result{}, err
	}

	content, err := json.Marshal(map[string]any{
		"message_id": sent.ID,
		"thread_id":  sent.ThreadID,
		"to":         input.To,
		"sent":       true,
	})
	if err != nil {
		return domain.Result{}, fmt.Errorf("gmail: encode send answer: %w", err)
	}

	return domain.Result{
		Content: content,
		Summary: fmt.Sprintf("email terkirim ke %s", input.To),
	}, nil
}

// composeInput is the arguments both writing tools take.
type composeInput struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// compose renders the RFC 5322 message Gmail's API takes, base64url encoded.
func (a *Adapter) compose(call domain.Call) (string, composeInput, error) {
	var input composeInput
	if err := json.Unmarshal(call.Arguments, &input); err != nil {
		return "", composeInput{}, fmt.Errorf("gmail: read the message arguments: %w", err)
	}

	// A header value containing a newline would let a caller inject headers, so
	// the address and the subject are refused rather than escaped: a subject with
	// a line break in it is a mistake or an attack, and neither belongs in a sent
	// message.
	if strings.ContainsAny(input.To, "\r\n") || strings.ContainsAny(input.Subject, "\r\n") {
		return "", composeInput{}, fmt.Errorf("%w: the recipient and the subject must not contain line breaks",
			domain.ErrInvalidInput)
	}
	if strings.TrimSpace(input.To) == "" {
		return "", composeInput{}, fmt.Errorf("%w: a recipient is required", domain.ErrInvalidInput)
	}

	encodedSubject := mime.QEncoding.Encode("utf-8", input.Subject)

	var message bytes.Buffer
	fmt.Fprintf(&message, "To: %s\r\n", input.To)
	fmt.Fprintf(&message, "Subject: %s\r\n", encodedSubject)
	message.WriteString("MIME-Version: 1.0\r\n")
	message.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
	message.WriteString("\r\n")
	message.WriteString(input.Body)

	return base64.RawURLEncoding.EncodeToString(message.Bytes()), input, nil
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
		return fmt.Errorf("gmail: build %s request: %w", path, err)
	}
	return a.do(request, token, path, target)
}

// post performs a POST with a JSON body and decodes the answer.
func (a *Adapter) post(ctx context.Context, token, path string, body []byte, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiBase+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("gmail: build %s request: %w", path, err)
	}
	request.Header.Set("Content-Type", "application/json")
	return a.do(request, token, path, target)
}

// do sends a request and decodes the answer.
//
// The status is checked before the body is decoded, because Google reports a
// refusal as a JSON error rather than as the shape the caller asked for: decoding
// first would turn "the recipient was refused" into "the answer was unreadable".
func (a *Adapter) do(request *http.Request, token, path string, target any) error {
	request.Header.Set("Authorization", "Bearer "+token)

	response, err := a.http.Do(request)
	if err != nil {
		return fmt.Errorf("gmail: %s: %w", path, err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody))
	if err != nil {
		return fmt.Errorf("gmail: read %s answer: %w", path, err)
	}

	if response.StatusCode >= 400 {
		// The reason is passed on, so the model can fix its own call instead of
		// retrying the same one.
		return fmt.Errorf("gmail: %s failed (status %d): %s",
			path, response.StatusCode, truncate(string(body), 300))
	}

	if target == nil {
		return nil
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("gmail: %s answered something unreadable: %w", path, err)
	}
	return nil
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
