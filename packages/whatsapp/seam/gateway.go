package seam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	whatsappdomain "github.com/robzlabz/employeebot/packages/whatsapp/domain"
)

// The HTTP seam between the two processes.
//
// The gateway owns the connection and the API owns the product, so they talk
// over one small surface: the nine things a number can be asked to do. The
// surface is the Transport interface in both directions — the API holds a client
// that implements it, and the gateway serves it — which is what keeps the API
// process from importing whatsmeow and the gateway from importing a router.
//
// It is an internal address. The gateway is not publicly reachable: it speaks for
// every tenant it serves, and a caller who reached it could link a number into
// any workspace.

// TokenHeader is the shared secret that authenticates the API to the gateway.
const TokenHeader = "X-Wa-Gateway-Token"

// HeaderToken is what a deployment sets to let the API reach the gateway. It is a
// shared secret rather than a user credential because both ends are ours and the
// gateway has no user of its own.
type GatewayConfig struct {
	// Token authenticates the caller. Empty refuses every request, which is what
	// a deployment that has not set one looks like.
	Token string
	// MaxBody bounds a request. The largest thing that crosses this seam is a
	// message body, so a megabyte is generous.
	MaxBody int64
}

// Gateway serves one runtime over HTTP for the API process.
//
// It takes the Transport rather than the engine so the surface can be tested
// without a WhatsApp connection: the routes are the contract, and a fake that
// answers them proves the contract without a phone.
type Gateway struct {
	engine whatsappdomain.Transport
	cfg    GatewayConfig
	log    *zap.Logger
}

// NewGateway builds the server.
func NewGateway(transport whatsappdomain.Transport, cfg GatewayConfig, log *zap.Logger) *Gateway {
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = 1 << 20
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Gateway{engine: transport, cfg: cfg, log: log}
}

// Routes is the gateway's whole surface.
//
// The paths name the number they act on because the engine has to know which
// session to use, and the workspace travels in a header rather than in the path
// because it is the tenant the operation runs in rather than part of the
// resource.
func (g *Gateway) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", g.health)
	mux.HandleFunc("POST /numbers/{numberID}/link", g.link)
	mux.HandleFunc("GET /links/{linkID}", g.linkState)
	mux.HandleFunc("POST /links/{linkID}/pair-code", g.pairCode)
	mux.HandleFunc("DELETE /links/{linkID}", g.cancelLink)
	mux.HandleFunc("POST /numbers/{numberID}/send", g.send)
	mux.HandleFunc("POST /numbers/{numberID}/typing", g.typing)
	mux.HandleFunc("POST /numbers/{numberID}/unlink", g.unlink)
	mux.HandleFunc("GET /numbers/{numberID}/online", g.online)
}

// health answers whether the process is up. It does not claim any number is
// online: that is per number and read from the database.
func (g *Gateway) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// link starts one attempt.
func (g *Gateway) link(w http.ResponseWriter, r *http.Request) {
	scope, ok := g.scoped(w, r)
	if !ok {
		return
	}

	numberID, ok := g.numberID(w, r)
	if !ok {
		return
	}

	var body struct {
		Phone string `json:"phone"`
	}
	if !g.decode(w, r, &body) {
		return
	}

	link, err := g.engine.Link(r.Context(), scope, numberID, body.Phone)
	if err != nil {
		g.fail(w, "start link", err)
		return
	}

	writeJSON(w, http.StatusOK, link)
}

// linkState reports where an attempt stands.
func (g *Gateway) linkState(w http.ResponseWriter, r *http.Request) {
	link, err := g.engine.LinkState(r.Context(), r.PathValue("linkID"))
	if err != nil {
		g.fail(w, "read link", err)
		return
	}

	writeJSON(w, http.StatusOK, link)
}

// pairCode asks for the phone-code alternative.
func (g *Gateway) pairCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
	}
	if !g.decode(w, r, &body) {
		return
	}

	link, err := g.engine.PairCode(r.Context(), r.PathValue("linkID"), body.Phone)
	if err != nil {
		g.fail(w, "pair by phone", err)
		return
	}

	writeJSON(w, http.StatusOK, link)
}

// cancelLink abandons an attempt.
func (g *Gateway) cancelLink(w http.ResponseWriter, r *http.Request) {
	if err := g.engine.Cancel(r.Context(), r.PathValue("linkID")); err != nil {
		g.fail(w, "cancel link", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "cancelled"})
}

// sendPayload is one message to deliver.
type sendPayload struct {
	ToJID    string    `json:"to"`
	Kind     string    `json:"kind"`
	Text     string    `json:"text"`
	MediaKey string    `json:"media_key"`
	ID       uuid.UUID `json:"id"`
}

// send delivers one message.
func (g *Gateway) send(w http.ResponseWriter, r *http.Request) {
	scope, ok := g.scoped(w, r)
	if !ok {
		return
	}

	numberID, ok := g.numberID(w, r)
	if !ok {
		return
	}

	var body sendPayload
	if !g.decode(w, r, &body) {
		return
	}

	id, err := g.engine.Send(r.Context(), whatsappdomain.Snapshot{
		NumberID:    numberID,
		WorkspaceID: scope.WorkspaceID,
	}, whatsappdomain.ToSend{
		ID:       body.ID,
		ToJID:    body.ToJID,
		Kind:     body.Kind,
		Text:     body.Text,
		MediaKey: body.MediaKey,
	})
	if err != nil {
		g.fail(w, "send", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"message_id": id})
}

// typing shows the typing indicator.
func (g *Gateway) typing(w http.ResponseWriter, r *http.Request) {
	scope, ok := g.scoped(w, r)
	if !ok {
		return
	}

	numberID, ok := g.numberID(w, r)
	if !ok {
		return
	}

	var body struct {
		To string `json:"to"`
	}
	if !g.decode(w, r, &body) {
		return
	}

	if err := g.engine.Typing(r.Context(), whatsappdomain.Snapshot{
		NumberID:    numberID,
		WorkspaceID: scope.WorkspaceID,
	}, body.To); err != nil {
		g.fail(w, "typing", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// unlink ends a session.
func (g *Gateway) unlink(w http.ResponseWriter, r *http.Request) {
	scope, ok := g.scoped(w, r)
	if !ok {
		return
	}

	numberID, ok := g.numberID(w, r)
	if !ok {
		return
	}

	if err := g.engine.Unlink(r.Context(), whatsappdomain.Snapshot{
		NumberID:    numberID,
		WorkspaceID: scope.WorkspaceID,
	}); err != nil {
		g.fail(w, "unlink", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "unlinked"})
}

// online reports whether this instance holds the number.
//
// It needs the token but no tenant: the answer is about this process rather than
// about a workspace's data. The token is still checked, because an unauthenticated
// caller could otherwise enumerate how many numbers a deployment serves.
func (g *Gateway) online(w http.ResponseWriter, r *http.Request) {
	if !g.authorized(w, r) {
		return
	}

	numberID, ok := g.numberID(w, r)
	if !ok {
		return
	}

	online, err := g.engine.Online(r.Context(), numberID)
	if err != nil {
		g.fail(w, "read online", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"online": online})
}

// authorized checks the shared secret.
//
// The gateway speaks for every tenant it serves, so an unauthenticated caller
// could link a number into any workspace. There is no read-only path worth leaving
// open.
func (g *Gateway) authorized(w http.ResponseWriter, r *http.Request) bool {
	if g.cfg.Token == "" || r.Header.Get(TokenHeader) != g.cfg.Token {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return false
	}
	return true
}

// scoped reads and checks the tenant a request runs in.
func (g *Gateway) scoped(w http.ResponseWriter, r *http.Request) (whatsappdomain.Scope, bool) {
	if !g.authorized(w, r) {
		return whatsappdomain.Scope{}, false
	}

	raw := strings.TrimSpace(r.Header.Get("X-Workspace-Id"))
	if raw == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "workspace is required"})
		return whatsappdomain.Scope{}, false
	}

	workspaceID, err := uuid.Parse(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "workspace is not a uuid"})
		return whatsappdomain.Scope{}, false
	}

	return whatsappdomain.Scope{WorkspaceID: workspaceID}, true
}

// numberID reads the number a path names.
func (g *Gateway) numberID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("numberID"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "number is not a uuid"})
		return uuid.Nil, false
	}
	return id, true
}

// decode reads a JSON body.
func (g *Gateway) decode(w http.ResponseWriter, r *http.Request, into any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, g.cfg.MaxBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the body could not be read"})
		return false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return true
	}
	if err := json.Unmarshal(body, into); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the body is not valid JSON"})
		return false
	}
	return true
}

// fail maps the engine's errors onto statuses.
//
// What crosses the wire is the *sentence* the module wrote, with its own prefix
// removed, rather than the rendered error. That is not cosmetic: the API turns the
// status back into the same sentinel and wraps it, so a message that still carried
// its prefix would arrive wrapped twice and the caller's own explanation would end up
// buried behind two layers of "whatsapp: not found".
func (g *Gateway) fail(w http.ResponseWriter, operation string, err error) {
	status := StatusFor(err)

	// A failure nobody classified is logged with its own words, because what crosses
	// the seam is one sentence and the detail is the only way to find out what
	// happened. Without this line an unclassified error is a 500 with no trace at all
	// — which is exactly what it became while this function was being rewritten.
	if status == http.StatusInternalServerError {
		g.log.Error("a gateway request failed", zap.String("operation", operation), zap.Error(err))
	}

	writeJSON(w, status, map[string]any{"error": Sentence(err)})
}

// StatusFor is the status one error becomes, which is the mapping the client
// reverses. The two are one table written twice, so a change to either is a change to
// the seam.
func StatusFor(err error) int {
	switch {
	case errors.Is(err, whatsappdomain.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, whatsappdomain.ErrInvalidInput),
		errors.Is(err, whatsappdomain.ErrTermsNotAccepted):
		return http.StatusBadRequest
	case errors.Is(err, whatsappdomain.ErrAlreadyLinked), errors.Is(err, whatsappdomain.ErrNoAgent):
		// Both are a state that refuses the request rather than a failure to
		// serve it: a number that is already linked, and a number nobody answers
		// yet. The product's own handler says the same with 409, and the gateway
		// must not read either as an outage to retry.
		return http.StatusConflict
	case errors.Is(err, whatsappdomain.ErrNotConnected), errors.Is(err, whatsappdomain.ErrUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, whatsappdomain.ErrLinkExpired):
		return http.StatusGone
	default:
		return http.StatusInternalServerError
	}
}

// Sentence is the module's own words for a refusal: the error with its sentinel's
// text removed, so what travels is what a person should read.
func Sentence(err error) string {
	if err == nil {
		return ""
	}

	text := err.Error()
	for _, sentinel := range []error{
		whatsappdomain.ErrNotFound,
		whatsappdomain.ErrInvalidInput,
		whatsappdomain.ErrTermsNotAccepted,
		whatsappdomain.ErrAlreadyLinked,
		whatsappdomain.ErrNotConnected,
		whatsappdomain.ErrUnavailable,
		whatsappdomain.ErrLinkExpired,
		whatsappdomain.ErrNoAgent,
		whatsappdomain.ErrNotConfigured,
	} {
		text = strings.TrimSpace(strings.TrimPrefix(text, sentinel.Error()+":"))
	}

	// A failure nobody classified is not something to hand a caller: it is the
	// gateway's own business, and what crosses the seam is one sentence.
	if strings.TrimSpace(text) == "" || StatusFor(err) == http.StatusInternalServerError {
		return "internal error"
	}

	return text
}

// SentinelForStatus maps a gateway status back to the error the module uses.
//
// The two ends are ours, so this is a contract rather than a guess: `fail` decides
// which status each sentinel becomes, and this is the other direction of the same
// table. A status nobody mapped is an unexpected failure, which is what it is.
func SentinelForStatus(status int) error {
	switch status {
	case http.StatusNotFound:
		return whatsappdomain.ErrNotFound
	case http.StatusBadRequest, http.StatusConflict:
		// Both are the caller's input: one is a request that cannot be acted on,
		// the other is a state that refuses it. The message says which.
		return whatsappdomain.ErrInvalidInput
	case http.StatusGone:
		return whatsappdomain.ErrLinkExpired
	case http.StatusServiceUnavailable:
		return whatsappdomain.ErrUnavailable
	default:
		return errors.New("whatsapp: the gateway failed")
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// Client is the API's side of the seam: it implements the Transport the module
// declares by calling the gateway.
type Client struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewClient builds the client. An empty base URL or token leaves it
// unconfigured, and every call reports that rather than reaching a URL nobody
// meant.
func NewClient(baseURL, token string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: timeout},
	}
}

// Compile-time check that the client is the Transport the module declares.
var _ whatsappdomain.Transport = (*Client)(nil)

// Link starts one attempt.
func (c *Client) Link(ctx context.Context, scope whatsappdomain.Scope, numberID uuid.UUID, phone string) (whatsappdomain.Link, error) {
	var link whatsappdomain.Link
	err := c.post(ctx, scope, fmt.Sprintf("/numbers/%s/link", numberID), map[string]any{"phone": phone}, &link)
	return link, err
}

// LinkState reports where an attempt stands.
func (c *Client) LinkState(ctx context.Context, id string) (whatsappdomain.Link, error) {
	var link whatsappdomain.Link
	err := c.get(ctx, whatsappdomain.Scope{}, "/links/"+id, &link)
	return link, err
}

// PairCode asks for the phone-code alternative.
func (c *Client) PairCode(ctx context.Context, id string, phone string) (whatsappdomain.Link, error) {
	var link whatsappdomain.Link
	err := c.post(ctx, whatsappdomain.Scope{}, "/links/"+id+"/pair-code", map[string]any{"phone": phone}, &link)
	return link, err
}

// Cancel abandons an attempt.
func (c *Client) Cancel(ctx context.Context, id string) error {
	return c.delete(ctx, "/links/"+id)
}

// Send delivers one message.
func (c *Client) Send(ctx context.Context, snapshot whatsappdomain.Snapshot, message whatsappdomain.ToSend) (string, error) {
	var response struct {
		MessageID string `json:"message_id"`
	}
	err := c.post(ctx, whatsappdomain.Scope{WorkspaceID: snapshot.WorkspaceID},
		fmt.Sprintf("/numbers/%s/send", snapshot.NumberID), map[string]any{
			"id":        message.ID,
			"to":        message.ToJID,
			"kind":      message.Kind,
			"text":      message.Text,
			"media_key": message.MediaKey,
		}, &response)
	if err != nil {
		return "", err
	}

	return response.MessageID, nil
}

// Typing shows the typing indicator.
func (c *Client) Typing(ctx context.Context, snapshot whatsappdomain.Snapshot, toJID string) error {
	return c.post(ctx, whatsappdomain.Scope{WorkspaceID: snapshot.WorkspaceID},
		fmt.Sprintf("/numbers/%s/typing", snapshot.NumberID), map[string]any{"to": toJID}, nil)
}

// Unlink ends a session.
func (c *Client) Unlink(ctx context.Context, snapshot whatsappdomain.Snapshot) error {
	return c.post(ctx, whatsappdomain.Scope{WorkspaceID: snapshot.WorkspaceID},
		fmt.Sprintf("/numbers/%s/unlink", snapshot.NumberID), nil, nil)
}

// Online reports whether the gateway holds the number.
func (c *Client) Online(ctx context.Context, numberID uuid.UUID) (bool, error) {
	var response struct {
		Online bool `json:"online"`
	}
	if err := c.get(ctx, whatsappdomain.Scope{}, fmt.Sprintf("/numbers/%s/online", numberID), &response); err != nil {
		return false, err
	}
	return response.Online, nil
}

// post sends a body and reads an answer.
func (c *Client) post(ctx context.Context, scope whatsappdomain.Scope, path string, body, into any) error {
	return c.do(ctx, http.MethodPost, scope, path, body, into)
}

// get reads.
func (c *Client) get(ctx context.Context, scope whatsappdomain.Scope, path string, into any) error {
	return c.do(ctx, http.MethodGet, scope, path, nil, into)
}

// delete removes.
func (c *Client) delete(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodDelete, whatsappdomain.Scope{}, path, nil, nil)
}

// do performs one call.
func (c *Client) do(ctx context.Context, method string, scope whatsappdomain.Scope, path string, body, into any) error {
	if c.baseURL == "" || c.token == "" {
		return fmt.Errorf("%w: the gateway address and token are not configured", whatsappdomain.ErrNotConfigured)
	}

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("whatsapp: encode request: %w", err)
		}
		reader = strings.NewReader(string(encoded))
	}

	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("whatsapp: build request: %w", err)
	}
	request.Header.Set(TokenHeader, c.token)
	request.Header.Set("Content-Type", "application/json")
	if !scope.IsZero() {
		request.Header.Set("X-Workspace-Id", scope.WorkspaceID.String())
	}

	response, err := c.client.Do(request)
	if err != nil {
		// An unreachable gateway is not a refusal by the product: the message it
		// was asked to send is still queued, and the answer says the platform was
		// the problem.
		return fmt.Errorf("%w: %w", whatsappdomain.ErrNotConfigured, err)
	}
	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("whatsapp: read answer: %w", err)
	}

	if response.StatusCode >= 400 {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(payload, &failure)

		message := strings.TrimSpace(failure.Error)
		if message == "" {
			message = response.Status
		}

		// The status is mapped back to the sentinel the other end put it in, and
		// that mapping is the whole reason this seam keeps the classification: a
		// 404 from the gateway means "no such number", and flattening it into a
		// string is how the API came to answer 500 for a number WhatsApp does not
		// recognise. The sentence the caller needs is already in the message, and
		// it is carried through rather than replaced.
		return fmt.Errorf("%w: %s", SentinelForStatus(response.StatusCode), message)
	}

	if into == nil || len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, into); err != nil {
		return fmt.Errorf("whatsapp: read gateway answer: %w", err)
	}
	return nil
}
