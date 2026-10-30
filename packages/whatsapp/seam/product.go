package seam

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	whatsappdomain "github.com/anakbolu/timbolu/packages/whatsapp/domain"
)

// The second half of the seam: what the gateway asks the product to do.
//
// The gateway owns the socket and the backend owns the product, so the four
// things the engine does with the product — it records what arrived, pulls what
// is due, reports what left, and says how a number is doing — travel back over
// the same internal address the other direction uses. The two directions share
// one secret, one status table, and one sentence: a refusal reads the same
// whichever way it crosses.
//
// It is a plain HTTP surface rather than a router the API already has, because
// it answers the gateway and nobody else: no user, no tenant middleware, no
// browser. The address it is served on is never published.

// ProductConfig is how the product side authenticates the gateway.
type ProductConfig struct {
	// Token is the same shared secret the gateway is reached with. Empty refuses
	// every call, which is what a deployment that has not set one looks like.
	Token string
	// MaxBody bounds a request. A message body and a media record are the largest
	// things that cross, so a megabyte is generous.
	MaxBody int64
}

// ProductDeps are the ports the product side serves.
//
// They are the four the engine holds, named here rather than taking the whole
// service so the surface is exactly what a gateway may ask for and nothing else.
type ProductDeps struct {
	Inbox  whatsappdomain.Inbox
	Drain  whatsappdomain.Drain
	Status whatsappdomain.Status
	Links  whatsappdomain.LinkSink
}

// Product serves the gateway's requests to the backend.
type Product struct {
	deps ProductDeps
	cfg  ProductConfig
	log  *zap.Logger
}

// NewProduct builds the server.
func NewProduct(deps ProductDeps, cfg ProductConfig, log *zap.Logger) *Product {
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = 1 << 20
	}
	if log == nil {
		log = zap.NewNop()
	}

	return &Product{deps: deps, cfg: cfg, log: log}
}

// Routes is the product surface's whole route table.
//
// The paths name the number or the message they act on, and the workspace
// travels in the same header the other direction uses: it is the tenant the
// operation runs in rather than part of the resource.
func (p *Product) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /internal/whatsapp/numbers/{numberID}/inbound", p.receive)
	mux.HandleFunc("POST /internal/whatsapp/media/pending", p.pendingMedia)
	mux.HandleFunc("POST /internal/whatsapp/messages/{messageID}/media", p.storeMedia)
	mux.HandleFunc("POST /internal/whatsapp/numbers/{numberID}/outbox/claim", p.claim)
	mux.HandleFunc("POST /internal/whatsapp/outbox/reclaim", p.reclaim)
	mux.HandleFunc("POST /internal/whatsapp/outbox/{messageID}/sent", p.sent)
	mux.HandleFunc("POST /internal/whatsapp/outbox/{messageID}/retry", p.retry)
	mux.HandleFunc("POST /internal/whatsapp/outbox/{messageID}/failed", p.failed)
	mux.HandleFunc("GET /internal/whatsapp/numbers/{numberID}", p.number)
	mux.HandleFunc("POST /internal/whatsapp/numbers/{numberID}/status", p.report)
	mux.HandleFunc("POST /internal/whatsapp/numbers/{numberID}/paired", p.paired)
}

// receive records one inbound message.
func (p *Product) receive(w http.ResponseWriter, r *http.Request) {
	scope, ok := p.scoped(w, r)
	if !ok {
		return
	}
	numberID, ok := p.pathID(w, r, "numberID", "number")
	if !ok {
		return
	}

	var body inboundWire
	if !p.decode(w, r, &body) {
		return
	}

	stored, created, err := p.deps.Inbox.Receive(r.Context(), scope, numberID, body.toDomain())
	if err != nil {
		p.fail(w, "receive", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"message": fromInbound(stored),
		"created": created,
	})
}

// pendingMedia returns the messages whose bytes have not been fetched.
func (p *Product) pendingMedia(w http.ResponseWriter, r *http.Request) {
	scope, ok := p.scoped(w, r)
	if !ok {
		return
	}

	var body struct {
		Limit int `json:"limit"`
	}
	if !p.decode(w, r, &body) {
		return
	}

	pending, err := p.deps.Inbox.PendingMedia(r.Context(), scope, body.Limit)
	if err != nil {
		p.fail(w, "pending media", err)
		return
	}

	wire := make([]inboundWire, 0, len(pending))
	for _, message := range pending {
		wire = append(wire, fromInbound(message))
	}

	writeJSON(w, http.StatusOK, map[string]any{"messages": wire})
}

// storeMedia records where a message's bytes were stored.
func (p *Product) storeMedia(w http.ResponseWriter, r *http.Request) {
	scope, ok := p.scoped(w, r)
	if !ok {
		return
	}
	messageID, ok := p.pathID(w, r, "messageID", "message")
	if !ok {
		return
	}

	var body mediaWire
	if !p.decode(w, r, &body) {
		return
	}

	if err := p.deps.Inbox.StoreMedia(r.Context(), scope, messageID, body.toDomain()); err != nil {
		p.fail(w, "store media", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "stored"})
}

// claim returns the messages of one number that may go out now.
func (p *Product) claim(w http.ResponseWriter, r *http.Request) {
	scope, ok := p.scoped(w, r)
	if !ok {
		return
	}
	numberID, ok := p.pathID(w, r, "numberID", "number")
	if !ok {
		return
	}

	var body struct {
		Limit int `json:"limit"`
	}
	if !p.decode(w, r, &body) {
		return
	}

	claimed, err := p.deps.Drain.Claim(r.Context(), scope, numberID, body.Limit)
	if err != nil {
		p.fail(w, "claim", err)
		return
	}

	ready := make([]outboxWire, 0, len(claimed.Ready))
	for _, item := range claimed.Ready {
		ready = append(ready, fromOutbox(item))
	}

	writeJSON(w, http.StatusOK, map[string]any{"ready": ready, "held": claimed.Held})
}

// reclaim returns the messages a dead gateway left mid-send to the queue.
func (p *Product) reclaim(w http.ResponseWriter, r *http.Request) {
	scope, ok := p.scoped(w, r)
	if !ok {
		return
	}

	var body struct {
		OlderThan time.Time `json:"older_than"`
	}
	if !p.decode(w, r, &body) {
		return
	}

	reclaimed, err := p.deps.Drain.Reclaim(r.Context(), scope, body.OlderThan)
	if err != nil {
		p.fail(w, "reclaim", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"reclaimed": reclaimed})
}

// sent records that a message left.
func (p *Product) sent(w http.ResponseWriter, r *http.Request) {
	scope, messageID, ok := p.outboxRequest(w, r)
	if !ok {
		return
	}

	var body struct {
		WaMessageID string    `json:"wa_message_id"`
		At          time.Time `json:"at"`
	}
	if !p.decode(w, r, &body) {
		return
	}

	if err := p.deps.Drain.Sent(r.Context(), scope, messageID, body.WaMessageID, body.At); err != nil {
		p.fail(w, "mark sent", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "sent"})
}

// retry puts a message back in the queue with a later time.
func (p *Product) retry(w http.ResponseWriter, r *http.Request) {
	scope, messageID, ok := p.outboxRequest(w, r)
	if !ok {
		return
	}

	var body struct {
		Reason string    `json:"reason"`
		At     time.Time `json:"at"`
	}
	if !p.decode(w, r, &body) {
		return
	}

	if err := p.deps.Drain.Retry(r.Context(), scope, messageID, body.Reason, body.At); err != nil {
		p.fail(w, "retry", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "retried"})
}

// failed records that a message will not be retried.
func (p *Product) failed(w http.ResponseWriter, r *http.Request) {
	scope, messageID, ok := p.outboxRequest(w, r)
	if !ok {
		return
	}

	var body struct {
		Reason   string `json:"reason"`
		Attempts int    `json:"attempts"`
	}
	if !p.decode(w, r, &body) {
		return
	}

	if err := p.deps.Drain.Failed(r.Context(), scope, messageID, body.Reason, body.Attempts); err != nil {
		p.fail(w, "mark failed", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "failed"})
}

// number returns one number, which the engine reads for its phone and ceiling.
func (p *Product) number(w http.ResponseWriter, r *http.Request) {
	scope, ok := p.scoped(w, r)
	if !ok {
		return
	}
	numberID, ok := p.pathID(w, r, "numberID", "number")
	if !ok {
		return
	}

	number, err := p.deps.Status.Number(r.Context(), scope, numberID)
	if err != nil {
		p.fail(w, "read number", err)
		return
	}

	writeJSON(w, http.StatusOK, fromNumber(number))
}

// report records what the transport knows about a number.
func (p *Product) report(w http.ResponseWriter, r *http.Request) {
	scope, ok := p.scoped(w, r)
	if !ok {
		return
	}
	numberID, ok := p.pathID(w, r, "numberID", "number")
	if !ok {
		return
	}

	var body struct {
		Status string    `json:"status"`
		Reason string    `json:"reason"`
		At     time.Time `json:"at"`
	}
	if !p.decode(w, r, &body) {
		return
	}

	number, err := p.deps.Status.Report(r.Context(), scope, whatsappdomain.ReportRequest{
		NumberID: numberID,
		Status:   body.Status,
		Reason:   body.Reason,
		At:       body.At,
	})
	if err != nil {
		p.fail(w, "report", err)
		return
	}

	writeJSON(w, http.StatusOK, fromNumber(number))
}

// paired records that a phone finished pairing.
func (p *Product) paired(w http.ResponseWriter, r *http.Request) {
	scope, ok := p.scoped(w, r)
	if !ok {
		return
	}
	numberID, ok := p.pathID(w, r, "numberID", "number")
	if !ok {
		return
	}

	var body struct {
		Phone string `json:"phone"`
		JID   string `json:"jid"`
	}
	if !p.decode(w, r, &body) {
		return
	}

	if err := p.deps.Links.Paired(r.Context(), scope, whatsappdomain.PairedLink{
		NumberID: numberID,
		Phone:    body.Phone,
		JID:      body.JID,
	}); err != nil {
		p.fail(w, "paired", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "paired"})
}

// outboxRequest reads the tenant and the message a call names.
func (p *Product) outboxRequest(w http.ResponseWriter, r *http.Request) (whatsappdomain.Scope, uuid.UUID, bool) {
	scope, ok := p.scoped(w, r)
	if !ok {
		return whatsappdomain.Scope{}, uuid.Nil, false
	}

	messageID, ok := p.pathID(w, r, "messageID", "message")
	if !ok {
		return whatsappdomain.Scope{}, uuid.Nil, false
	}

	return scope, messageID, true
}

// scoped reads and checks the tenant a request runs in.
func (p *Product) scoped(w http.ResponseWriter, r *http.Request) (whatsappdomain.Scope, bool) {
	if !p.authorized(w, r) {
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

// authorized checks the shared secret.
//
// The product side speaks for every tenant it serves, so an unauthenticated
// caller could open a task in any workspace. There is no read-only path worth
// leaving open.
func (p *Product) authorized(w http.ResponseWriter, r *http.Request) bool {
	if p.cfg.Token == "" || r.Header.Get(TokenHeader) != p.cfg.Token {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return false
	}

	return true
}

// pathID reads one uuid the path names.
func (p *Product) pathID(w http.ResponseWriter, r *http.Request, name, what string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": what + " is not a uuid"})
		return uuid.Nil, false
	}

	return id, true
}

// decode reads a JSON body.
func (p *Product) decode(w http.ResponseWriter, r *http.Request, into any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, p.cfg.MaxBody))
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

// fail maps the product's errors onto statuses with the one shared table.
func (p *Product) fail(w http.ResponseWriter, operation string, err error) {
	status := StatusFor(err)
	if status == http.StatusInternalServerError {
		p.log.Error("a product request failed", zap.String("operation", operation), zap.Error(err))
	}

	writeJSON(w, status, map[string]any{"error": Sentence(err)})
}

// --------------------------------------------------------------------------
// The client the gateway holds.
// --------------------------------------------------------------------------

// ProductClient is the gateway's side: it implements the four ports the engine
// declares by calling the backend.
type ProductClient struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewProductClient builds the client. An empty base URL or token leaves it
// unconfigured, and every call reports that rather than reaching a URL nobody
// meant.
func NewProductClient(baseURL, token string, timeout time.Duration) *ProductClient {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	return &ProductClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: timeout},
	}
}

// Compile-time checks that the client is every port the engine declares.
var (
	_ whatsappdomain.Inbox    = (*ProductClient)(nil)
	_ whatsappdomain.Drain    = (*ProductClient)(nil)
	_ whatsappdomain.Status   = (*ProductClient)(nil)
	_ whatsappdomain.LinkSink = (*ProductClient)(nil)
)

// Receive records one inbound message.
func (c *ProductClient) Receive(ctx context.Context, scope whatsappdomain.Scope, numberID uuid.UUID, message whatsappdomain.Inbound) (whatsappdomain.Inbound, bool, error) {
	var response struct {
		Message inboundWire `json:"message"`
		Created bool        `json:"created"`
	}
	err := c.post(ctx, scope, fmt.Sprintf("/internal/whatsapp/numbers/%s/inbound", numberID), fromInbound(message), &response)
	if err != nil {
		return whatsappdomain.Inbound{}, false, err
	}

	return response.Message.toDomain(), response.Created, nil
}

// PendingMedia returns the messages whose bytes have not been fetched.
func (c *ProductClient) PendingMedia(ctx context.Context, scope whatsappdomain.Scope, limit int) ([]whatsappdomain.Inbound, error) {
	var response struct {
		Messages []inboundWire `json:"messages"`
	}
	if err := c.post(ctx, scope, "/internal/whatsapp/media/pending", map[string]any{"limit": limit}, &response); err != nil {
		return nil, err
	}

	messages := make([]whatsappdomain.Inbound, 0, len(response.Messages))
	for _, message := range response.Messages {
		messages = append(messages, message.toDomain())
	}

	return messages, nil
}

// StoreMedia records where a message's bytes were stored.
func (c *ProductClient) StoreMedia(ctx context.Context, scope whatsappdomain.Scope, id uuid.UUID, media whatsappdomain.Media) error {
	return c.post(ctx, scope, fmt.Sprintf("/internal/whatsapp/messages/%s/media", id), fromMedia(media), nil)
}

// Claim returns the messages of one number that may go out now.
func (c *ProductClient) Claim(ctx context.Context, scope whatsappdomain.Scope, numberID uuid.UUID, limit int) (whatsappdomain.Claimed, error) {
	var response struct {
		Ready []outboxWire `json:"ready"`
		Held  int          `json:"held"`
	}
	if err := c.post(ctx, scope, fmt.Sprintf("/internal/whatsapp/numbers/%s/outbox/claim", numberID), map[string]any{"limit": limit}, &response); err != nil {
		return whatsappdomain.Claimed{}, err
	}

	ready := make([]whatsappdomain.Outbox, 0, len(response.Ready))
	for _, item := range response.Ready {
		ready = append(ready, item.toDomain())
	}

	return whatsappdomain.Claimed{Ready: ready, Held: response.Held}, nil
}

// Sent records that a message left.
func (c *ProductClient) Sent(ctx context.Context, scope whatsappdomain.Scope, id uuid.UUID, waMessageID string, at time.Time) error {
	return c.post(ctx, scope, fmt.Sprintf("/internal/whatsapp/outbox/%s/sent", id),
		map[string]any{"wa_message_id": waMessageID, "at": at}, nil)
}

// Retry puts a message back in the queue with a later time.
func (c *ProductClient) Retry(ctx context.Context, scope whatsappdomain.Scope, id uuid.UUID, reason string, at time.Time) error {
	return c.post(ctx, scope, fmt.Sprintf("/internal/whatsapp/outbox/%s/retry", id),
		map[string]any{"reason": reason, "at": at}, nil)
}

// Failed records that a message will not be retried.
func (c *ProductClient) Failed(ctx context.Context, scope whatsappdomain.Scope, id uuid.UUID, reason string, attempts int) error {
	return c.post(ctx, scope, fmt.Sprintf("/internal/whatsapp/outbox/%s/failed", id),
		map[string]any{"reason": reason, "attempts": attempts}, nil)
}

// Reclaim puts messages a dead gateway left mid-send back in the queue.
func (c *ProductClient) Reclaim(ctx context.Context, scope whatsappdomain.Scope, olderThan time.Time) (int, error) {
	var response struct {
		Reclaimed int `json:"reclaimed"`
	}
	if err := c.post(ctx, scope, "/internal/whatsapp/outbox/reclaim", map[string]any{"older_than": olderThan}, &response); err != nil {
		return 0, err
	}

	return response.Reclaimed, nil
}

// Number returns one number.
func (c *ProductClient) Number(ctx context.Context, scope whatsappdomain.Scope, id uuid.UUID) (whatsappdomain.Number, error) {
	var number numberWire
	if err := c.get(ctx, scope, fmt.Sprintf("/internal/whatsapp/numbers/%s", id), &number); err != nil {
		return whatsappdomain.Number{}, err
	}

	return number.toDomain(), nil
}

// Report records what the transport knows about a number.
func (c *ProductClient) Report(ctx context.Context, scope whatsappdomain.Scope, request whatsappdomain.ReportRequest) (whatsappdomain.Number, error) {
	var number numberWire
	err := c.post(ctx, scope, fmt.Sprintf("/internal/whatsapp/numbers/%s/status", request.NumberID), map[string]any{
		"status": request.Status,
		"reason": request.Reason,
		"at":     request.At,
	}, &number)
	if err != nil {
		return whatsappdomain.Number{}, err
	}

	return number.toDomain(), nil
}

// Paired records that a phone finished pairing.
func (c *ProductClient) Paired(ctx context.Context, scope whatsappdomain.Scope, paired whatsappdomain.PairedLink) error {
	return c.post(ctx, scope, fmt.Sprintf("/internal/whatsapp/numbers/%s/paired", paired.NumberID), map[string]any{
		"phone": paired.Phone,
		"jid":   paired.JID,
	}, nil)
}

// post sends a body and reads an answer.
func (c *ProductClient) post(ctx context.Context, scope whatsappdomain.Scope, path string, body, into any) error {
	return c.do(ctx, http.MethodPost, scope, path, body, into)
}

// get reads.
func (c *ProductClient) get(ctx context.Context, scope whatsappdomain.Scope, path string, into any) error {
	return c.do(ctx, http.MethodGet, scope, path, nil, into)
}

// do performs one call.
func (c *ProductClient) do(ctx context.Context, method string, scope whatsappdomain.Scope, path string, body, into any) error {
	if c.baseURL == "" || c.token == "" {
		return fmt.Errorf("%w: the backend address and token are not configured", whatsappdomain.ErrNotConfigured)
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
		// An unreachable backend is not a refusal by the product: the message is
		// still queued, and the answer says the platform was the problem.
		return fmt.Errorf("%w: %w", whatsappdomain.ErrUnavailable, err)
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

		return fmt.Errorf("%w: %s", SentinelForStatus(response.StatusCode), message)
	}

	if into == nil || len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, into); err != nil {
		return fmt.Errorf("whatsapp: read backend answer: %w", err)
	}

	return nil
}

// --------------------------------------------------------------------------
// The wire shapes.
//
// They are declared here rather than reusing the domain structs so the two
// processes agree on a document rather than on a Go type: a field renamed in the
// domain is a compile error on both sides of one repository, and the JSON tags
// are what actually cross.
// --------------------------------------------------------------------------

type mediaWire struct {
	Ref    string `json:"ref"`
	Kind   string `json:"kind"`
	Size   int64  `json:"size"`
	Status string `json:"status"`
	Key    string `json:"key"`
	Error  string `json:"error"`
}

func fromMedia(media whatsappdomain.Media) mediaWire {
	return mediaWire{
		Ref:    media.Ref,
		Kind:   media.Kind,
		Size:   media.Size,
		Status: media.Status,
		Key:    media.Key,
		Error:  media.Error,
	}
}

func (m mediaWire) toDomain() whatsappdomain.Media {
	return whatsappdomain.Media{
		Ref:    m.Ref,
		Kind:   m.Kind,
		Size:   m.Size,
		Status: m.Status,
		Key:    m.Key,
		Error:  m.Error,
	}
}

type inboundWire struct {
	ID          uuid.UUID `json:"id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	NumberID    uuid.UUID `json:"number_id"`
	WaMessageID string    `json:"wa_message_id"`
	ChatJID     string    `json:"chat_jid"`
	SenderJID   string    `json:"sender_jid"`
	SenderName  string    `json:"sender_name"`
	IsGroup     bool      `json:"is_group"`
	IsFromMe    bool      `json:"is_from_me"`
	Kind        string    `json:"kind"`
	Text        string    `json:"text"`
	Media       mediaWire `json:"media"`
	ReceivedAt  time.Time `json:"received_at"`
	TaskID      uuid.UUID `json:"task_id"`
	CreatedAt   time.Time `json:"created_at"`
}

func fromInbound(message whatsappdomain.Inbound) inboundWire {
	return inboundWire{
		ID:          message.ID,
		WorkspaceID: message.WorkspaceID,
		NumberID:    message.NumberID,
		WaMessageID: message.WaMessageID,
		ChatJID:     message.ChatJID,
		SenderJID:   message.SenderJID,
		SenderName:  message.SenderName,
		IsGroup:     message.IsGroup,
		IsFromMe:    message.IsFromMe,
		Kind:        message.Kind,
		Text:        message.Text,
		Media:       fromMedia(message.Media),
		ReceivedAt:  message.ReceivedAt,
		TaskID:      message.TaskID,
		CreatedAt:   message.CreatedAt,
	}
}

func (m inboundWire) toDomain() whatsappdomain.Inbound {
	return whatsappdomain.Inbound{
		ID:          m.ID,
		WorkspaceID: m.WorkspaceID,
		NumberID:    m.NumberID,
		WaMessageID: m.WaMessageID,
		ChatJID:     m.ChatJID,
		SenderJID:   m.SenderJID,
		SenderName:  m.SenderName,
		IsGroup:     m.IsGroup,
		IsFromMe:    m.IsFromMe,
		Kind:        m.Kind,
		Text:        m.Text,
		Media:       m.Media.toDomain(),
		ReceivedAt:  m.ReceivedAt,
		TaskID:      m.TaskID,
		CreatedAt:   m.CreatedAt,
	}
}

type outboxWire struct {
	ID          uuid.UUID  `json:"id"`
	WorkspaceID uuid.UUID  `json:"workspace_id"`
	NumberID    uuid.UUID  `json:"number_id"`
	ToJID       string     `json:"to_jid"`
	Kind        string     `json:"kind"`
	Text        string     `json:"text"`
	MediaKey    string     `json:"media_key"`
	DraftID     uuid.UUID  `json:"draft_id"`
	Status      string     `json:"status"`
	Attempts    int        `json:"attempts"`
	LastError   string     `json:"last_error"`
	NotBefore   time.Time  `json:"not_before"`
	ClaimedAt   *time.Time `json:"claimed_at"`
	WaMessageID string     `json:"wa_message_id"`
	SentAt      *time.Time `json:"sent_at"`
	CreatedAt   time.Time  `json:"created_at"`
}

func fromOutbox(item whatsappdomain.Outbox) outboxWire {
	return outboxWire{
		ID:          item.ID,
		WorkspaceID: item.WorkspaceID,
		NumberID:    item.NumberID,
		ToJID:       item.ToJID,
		Kind:        item.Kind,
		Text:        item.Text,
		MediaKey:    item.MediaKey,
		DraftID:     item.DraftID,
		Status:      item.Status,
		Attempts:    item.Attempts,
		LastError:   item.LastError,
		NotBefore:   item.NotBefore,
		ClaimedAt:   item.ClaimedAt,
		WaMessageID: item.WaMessageID,
		SentAt:      item.SentAt,
		CreatedAt:   item.CreatedAt,
	}
}

func (m outboxWire) toDomain() whatsappdomain.Outbox {
	return whatsappdomain.Outbox{
		ID:          m.ID,
		WorkspaceID: m.WorkspaceID,
		NumberID:    m.NumberID,
		ToJID:       m.ToJID,
		Kind:        m.Kind,
		Text:        m.Text,
		MediaKey:    m.MediaKey,
		DraftID:     m.DraftID,
		Status:      m.Status,
		Attempts:    m.Attempts,
		LastError:   m.LastError,
		NotBefore:   m.NotBefore,
		ClaimedAt:   m.ClaimedAt,
		WaMessageID: m.WaMessageID,
		SentAt:      m.SentAt,
		CreatedAt:   m.CreatedAt,
	}
}

type numberWire struct {
	ID            uuid.UUID  `json:"id"`
	WorkspaceID   uuid.UUID  `json:"workspace_id"`
	IntegrationID uuid.UUID  `json:"integration_id"`
	Phone         string     `json:"phone"`
	Label         string     `json:"label"`
	AgentID       uuid.UUID  `json:"agent_id"`
	Status        string     `json:"status"`
	StatusReason  string     `json:"status_reason"`
	TOSAcceptedAt *time.Time `json:"tos_accepted_at"`
	HourlyLimit   int        `json:"hourly_limit"`
	ConnectedAt   *time.Time `json:"connected_at"`
	LastSeenAt    *time.Time `json:"last_seen_at"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

func fromNumber(number whatsappdomain.Number) numberWire {
	return numberWire{
		ID:            number.ID,
		WorkspaceID:   number.WorkspaceID,
		IntegrationID: number.IntegrationID,
		Phone:         number.Phone,
		Label:         number.Label,
		AgentID:       number.AgentID,
		Status:        number.Status,
		StatusReason:  number.StatusReason,
		TOSAcceptedAt: number.TOSAcceptedAt,
		HourlyLimit:   number.HourlyLimit,
		ConnectedAt:   number.ConnectedAt,
		LastSeenAt:    number.LastSeenAt,
		CreatedAt:     number.CreatedAt,
		UpdatedAt:     number.UpdatedAt,
	}
}

func (m numberWire) toDomain() whatsappdomain.Number {
	return whatsappdomain.Number{
		ID:            m.ID,
		WorkspaceID:   m.WorkspaceID,
		IntegrationID: m.IntegrationID,
		Phone:         m.Phone,
		Label:         m.Label,
		AgentID:       m.AgentID,
		Status:        m.Status,
		StatusReason:  m.StatusReason,
		TOSAcceptedAt: m.TOSAcceptedAt,
		HourlyLimit:   m.HourlyLimit,
		ConnectedAt:   m.ConnectedAt,
		LastSeenAt:    m.LastSeenAt,
		CreatedAt:     m.CreatedAt,
		UpdatedAt:     m.UpdatedAt,
	}
}
