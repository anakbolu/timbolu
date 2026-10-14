// Package handler exposes the WhatsApp endpoints. It talks to the domain
// service interface only.
package handler

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
	"github.com/robzlabz/employeebot/packages/whatsapp/domain"
)

// Handler serves the WhatsApp endpoints.
type Handler struct {
	service domain.Service
	log     *zap.Logger
}

// New builds the handler.
func New(service domain.Service, log *zap.Logger) *Handler {
	return &Handler{service: service, log: log}
}

// Routes registers the endpoints every member of the workspace may use.
//
// Linking a number is every member's business for the same reason a routine is:
// the person who notices the shop's WhatsApp is offline is not always the owner,
// and a number that is down is a customer who got no answer.
func Routes(group fiber.Router, h *Handler) {
	group.Get("/whatsapp/numbers", h.List)
	group.Post("/whatsapp/numbers", h.Create)
	group.Get("/whatsapp/numbers/:numberID", h.Get)
	group.Patch("/whatsapp/numbers/:numberID", h.Update)
	group.Delete("/whatsapp/numbers/:numberID", h.Unlink)

	group.Post("/whatsapp/numbers/:numberID/terms", h.AcceptTerms)
	group.Post("/whatsapp/numbers/:numberID/link", h.BeginLink)
	group.Post("/whatsapp/links/:linkID/pair-code", h.PairCode)
	group.Get("/whatsapp/links/:linkID", h.LinkState)
	group.Delete("/whatsapp/links/:linkID", h.CancelLink)

	group.Get("/whatsapp/numbers/:numberID/messages", h.Messages)
	group.Get("/whatsapp/numbers/:numberID/outbox", h.Outbox)
	group.Get("/whatsapp/messages/:messageID/media", h.Media)
}

type createRequest struct {
	Phone string `json:"phone"`
	Label string `json:"label"`
	// AgentID is the Bolu that answers this number. It is required at creation
	// because a number with nobody answering it is a number that loses customers.
	AgentID string `json:"agent_id"`
}

type updateRequest struct {
	Label       *string `json:"label"`
	AgentID     *string `json:"agent_id"`
	HourlyLimit *int    `json:"hourly_limit"`
}

type linkRequest struct {
	Phone string `json:"phone"`
}

type pairCodeRequest struct {
	Phone string `json:"phone"`
}

type numberPayload struct {
	ID          string `json:"id"`
	Phone       string `json:"phone"`
	Label       string `json:"label"`
	AgentID     string `json:"agent_id,omitempty"`
	Status      string `json:"status"`
	Reason      string `json:"reason,omitempty"`
	Linked      bool   `json:"linked"`
	Connected   bool   `json:"connected"`
	TermsOK     bool   `json:"terms_accepted"`
	HourlyLimit int    `json:"hourly_limit"`
	CreatedAt   string `json:"created_at"`
	// LastSeenAt is when the gateway last saw the number, which is what tells a
	// person whether the connection is actually alive.
	LastSeenAt string `json:"last_seen_at,omitempty"`
}

type linkPayload struct {
	ID       string `json:"link_id"`
	NumberID string `json:"number_id,omitempty"`
	State    string `json:"state"`
	// QR is the payload a phone scans, kept alongside the matrix so a client that
	// renders its own code can, and so a debugging session can read what was
	// offered.
	QR string `json:"qr,omitempty"`
	// QRMatrix is the same payload as rows of "1" and "0", which is what the
	// screen draws. It is rendered here because the payload is a capability and
	// this frame is the only place it travels.
	QRMatrix  []string `json:"qr_matrix,omitempty"`
	PairCode  string   `json:"pair_code,omitempty"`
	ExpiresAt string   `json:"expires_at,omitempty"`
	Reason    string   `json:"reason,omitempty"`
}

type messagePayload struct {
	ID          string `json:"id"`
	WaMessageID string `json:"wa_message_id"`
	ChatJID     string `json:"chat"`
	SenderJID   string `json:"from,omitempty"`
	SenderName  string `json:"name,omitempty"`
	IsGroup     bool   `json:"group"`
	IsFromMe    bool   `json:"ours"`
	Kind        string `json:"kind"`
	Text        string `json:"text"`
	MediaKind   string `json:"media_kind,omitempty"`
	MediaStatus string `json:"media_status,omitempty"`
	MediaError  string `json:"media_error,omitempty"`
	TaskID      string `json:"task_id,omitempty"`
	ReceivedAt  string `json:"received_at"`
}

type outboxPayload struct {
	ID        string `json:"id"`
	ToJID     string `json:"to"`
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	Status    string `json:"status"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error,omitempty"`
	SentAt    string `json:"sent_at,omitempty"`
	CreatedAt string `json:"created_at"`
}

// List returns the workspace's numbers.
func (h *Handler) List(c *fiber.Ctx) error {
	numbers, err := h.service.Numbers(c.UserContext(), scope(c))
	if err != nil {
		return h.fail(c, "list numbers", err)
	}

	payload := make([]numberPayload, 0, len(numbers))
	for _, number := range numbers {
		payload = append(payload, wireNumber(number))
	}

	return response.OKWithMeta(c, "ok", payload, fiber.Map{"count": len(payload)})
}

// Get returns one number.
func (h *Handler) Get(c *fiber.Ctx) error {
	id, err := parseID(c.Params("numberID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "number id is not a uuid", "invalid_input", nil)
	}

	number, err := h.service.Number(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "get number", err)
	}

	return response.OK(c, "ok", wireNumber(number))
}

// Create prepares one number to be linked.
//
// It does not link anything: it records which number is about to be linked and
// who answers it, so the warning can be shown against a row and the link has
// somewhere to land.
func (h *Handler) Create(c *fiber.Ctx) error {
	var body createRequest
	if err := c.BodyParser(&body); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "the body is not valid JSON", "invalid_request", nil)
	}

	agentID, err := parseID(body.AgentID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "agent_id is not a uuid", "invalid_input", nil)
	}

	number, err := h.service.Prepare(c.UserContext(), scope(c), domain.LinkRequest{
		Phone:   body.Phone,
		Label:   body.Label,
		AgentID: agentID,
	})
	if err != nil {
		return h.fail(c, "prepare number", err)
	}

	return response.Created(c, "number prepared", wireNumber(number))
}

// Update changes the label, the Bolu that answers, or the hourly ceiling.
func (h *Handler) Update(c *fiber.Ctx) error {
	id, err := parseID(c.Params("numberID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "number id is not a uuid", "invalid_input", nil)
	}

	var body updateRequest
	if err := c.BodyParser(&body); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "the body is not valid JSON", "invalid_request", nil)
	}

	current, err := h.service.Number(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "get number", err)
	}

	// Each field is its own statement, so a screen that changes one thing does
	// not have to send the rest back and risk overwriting a change made in
	// another tab with a stale copy.
	if body.Label != nil {
		current, err = h.service.SetLabel(c.UserContext(), scope(c), id, *body.Label)
		if err != nil {
			return h.fail(c, "rename number", err)
		}
	}
	if body.AgentID != nil {
		agentID, err := parseID(*body.AgentID)
		if err != nil {
			return response.Error(c, fiber.StatusBadRequest, "agent_id is not a uuid", "invalid_input", nil)
		}
		current, err = h.service.AssignAgent(c.UserContext(), scope(c), id, agentID)
		if err != nil {
			return h.fail(c, "assign agent", err)
		}
	}
	if body.HourlyLimit != nil {
		current, err = h.service.SetLimit(c.UserContext(), scope(c), id, *body.HourlyLimit)
		if err != nil {
			return h.fail(c, "set limit", err)
		}
	}

	return response.OK(c, "number updated", wireNumber(current))
}

// Unlink ends a session and clears what was stored.
func (h *Handler) Unlink(c *fiber.Ctx) error {
	id, err := parseID(c.Params("numberID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "number id is not a uuid", "invalid_input", nil)
	}

	number, err := h.service.Unlink(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "unlink number", err)
	}

	return response.OK(c, "number unlinked", wireNumber(number))
}

// AcceptTerms records that the workspace accepted the warning.
func (h *Handler) AcceptTerms(c *fiber.Ctx) error {
	id, err := parseID(c.Params("numberID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "number id is not a uuid", "invalid_input", nil)
	}

	number, err := h.service.AcceptTerms(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "accept terms", err)
	}

	return response.OK(c, "terms accepted", wireNumber(number))
}

// BeginLink starts a link attempt and answers the first state to show.
func (h *Handler) BeginLink(c *fiber.Ctx) error {
	numberID, err := parseID(c.Params("numberID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "number id is not a uuid", "invalid_input", nil)
	}

	var body linkRequest
	if err := c.BodyParser(&body); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "the body is not valid JSON", "invalid_request", nil)
	}

	number, err := h.service.Number(c.UserContext(), scope(c), numberID)
	if err != nil {
		return h.fail(c, "get number", err)
	}

	link, err := h.service.BeginLink(c.UserContext(), scope(c), domain.LinkRequest{
		Phone:   phoneOr(body.Phone, number.Phone),
		Label:   number.Label,
		AgentID: number.AgentID,
	})
	if err != nil {
		return h.fail(c, "begin link", err)
	}

	return response.OK(c, "link started", wireLink(link))
}

// PairCode asks for the phone-code alternative to scanning.
func (h *Handler) PairCode(c *fiber.Ctx) error {
	var body pairCodeRequest
	if err := c.BodyParser(&body); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "the body is not valid JSON", "invalid_request", nil)
	}

	link, err := h.service.PairCode(c.UserContext(), scope(c), c.Params("linkID"), body.Phone)
	if err != nil {
		return h.fail(c, "pair by phone", err)
	}

	return response.OK(c, "pair code issued", wireLink(link))
}

// LinkState reports where an attempt stands.
//
// It is polled rather than streamed. A link lasts a couple of minutes and answers
// a handful of polls; a second stream would be a second authentication path and a
// second thing to break, for a screen that is open for one minute of a person's
// day.
func (h *Handler) LinkState(c *fiber.Ctx) error {
	link, err := h.service.LinkState(c.UserContext(), scope(c), c.Params("linkID"))
	if err != nil {
		return h.fail(c, "read link", err)
	}

	return response.OK(c, "ok", wireLink(link))
}

// CancelLink abandons an attempt.
func (h *Handler) CancelLink(c *fiber.Ctx) error {
	if err := h.service.CancelLink(c.UserContext(), scope(c), c.Params("linkID")); err != nil {
		return h.fail(c, "cancel link", err)
	}

	return response.OK(c, "link cancelled", nil)
}

// Messages returns what customers wrote, newest first.
func (h *Handler) Messages(c *fiber.Ctx) error {
	numberID, err := parseID(c.Params("numberID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "number id is not a uuid", "invalid_input", nil)
	}

	messages, err := h.service.Messages(c.UserContext(), scope(c), domain.MessageFilter{
		NumberID: numberID,
		ChatJID:  strings.TrimSpace(c.Query("chat")),
		Limit:    c.QueryInt("limit", 0),
	})
	if err != nil {
		return h.fail(c, "list messages", err)
	}

	payload := make([]messagePayload, 0, len(messages))
	for _, message := range messages {
		payload = append(payload, wireMessage(message))
	}

	return response.OKWithMeta(c, "ok", payload, fiber.Map{"count": len(payload)})
}

// Outbox returns what the workspace asked this number to send.
func (h *Handler) Outbox(c *fiber.Ctx) error {
	numberID, err := parseID(c.Params("numberID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "number id is not a uuid", "invalid_input", nil)
	}

	items, err := h.service.Outbox(c.UserContext(), scope(c), domain.OutboxFilter{
		NumberID: numberID,
		Status:   strings.TrimSpace(c.Query("status")),
		Limit:    c.QueryInt("limit", 0),
	})
	if err != nil {
		return h.fail(c, "list outbox", err)
	}

	payload := make([]outboxPayload, 0, len(items))
	for _, item := range items {
		payload = append(payload, wireOutbox(item))
	}

	return response.OKWithMeta(c, "ok", payload, fiber.Map{"count": len(payload)})
}

// Media serves one message's attachment.
//
// It is on the API rather than on the gateway because the bytes are stored here
// and the person asking is a member of the workspace: the gateway holds a
// connection, not a file server.
func (h *Handler) Media(c *fiber.Ctx) error {
	messageID, err := parseID(c.Params("messageID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "message id is not a uuid", "invalid_input", nil)
	}

	message, err := h.service.MessageByID(c.UserContext(), scope(c), messageID)
	if err != nil {
		return h.fail(c, "get message", err)
	}
	if message.Media.Status != domain.MediaReady || message.Media.Key == "" {
		return response.Error(c, fiber.StatusNotFound,
			"lampiran pesan ini tidak tersedia", "media_not_found", nil)
	}

	content, err := h.service.MediaBytes(c.UserContext(), message.Media.Key)
	if err != nil {
		return h.fail(c, "read media", err)
	}

	c.Set(fiber.HeaderContentType, contentTypeFor(message.Media.Kind))
	c.Set(fiber.HeaderContentDisposition, fmt.Sprintf("inline; filename=%q", filenameFor(message)))
	c.Set("Cache-Control", "private, max-age=3600")

	// The body is written raw rather than through Send, because Send sniffs the
	// bytes and would replace the type with whatever it guessed: a JPEG that
	// starts with an unusual marker would be served as octet-stream and downloaded
	// instead of shown.
	c.Response().SetBodyRaw(content)
	return nil
}

// fail maps the module's errors onto responses.
func (h *Handler) fail(c *fiber.Ctx, operation string, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		// Two things answer 404: a row that is not in this workspace, and a number
		// WhatsApp does not know. The second is the caller's input, and the module
		// already wrote a sentence about it — so it is passed through rather than
		// replaced by one about a missing row.
		message := "nomor atau pesan itu tidak ada di workspace ini"
		if user := userMessage(err, domain.ErrNotFound); strings.HasPrefix(user, "WhatsApp") {
			message = user
		}
		return response.Error(c, fiber.StatusNotFound, message, "whatsapp_not_found", nil)
	case errors.Is(err, domain.ErrTermsNotAccepted):
		return response.Error(c, fiber.StatusConflict,
			"peringatan tentang jalur tidak resmi belum diterima", "terms_not_accepted", nil)
	case errors.Is(err, domain.ErrAlreadyLinked):
		return response.Error(c, fiber.StatusConflict,
			"nomor ini sudah tertaut", "already_linked", nil)
	case errors.Is(err, domain.ErrNoAgent):
		return response.Error(c, fiber.StatusConflict,
			"nomor ini belum punya Bolu yang menjawab", "no_agent", nil)
	case errors.Is(err, domain.ErrNotConnected):
		return response.Error(c, fiber.StatusConflict,
			"nomornya belum tertaut atau sedang offline", "not_connected", nil)
	case errors.Is(err, domain.ErrLinkExpired):
		return response.Error(c, fiber.StatusGone,
			"penautan ini sudah kedaluwarsa; minta kode baru", "link_expired", nil)
	case errors.Is(err, domain.ErrNotConfigured):
		return response.Error(c, fiber.StatusServiceUnavailable,
			"gateway WhatsApp belum dikonfigurasi di deployment ini", "whatsapp_unavailable", nil)
	case errors.Is(err, domain.ErrUnavailable):
		// The module knows why WhatsApp could not be reached, and the difference
		// matters to the person reading it: being throttled means waiting a few
		// minutes, while a network that is down means trying later. What it wrote is
		// passed through, and the generic sentence is only the fallback.
		message := "WhatsApp sedang tidak bisa dihubungi dari deployment ini; coba lagi sebentar lagi"
		if user := userMessage(err, domain.ErrUnavailable); user != "" {
			message = user
		}
		return response.Error(c, fiber.StatusServiceUnavailable, message, "whatsapp_unreachable", nil)
	case errors.Is(err, domain.ErrInvalidInput):
		return response.Error(c, fiber.StatusBadRequest, userMessage(err, domain.ErrInvalidInput), "invalid_input", nil)
	default:
		h.log.Error("whatsapp request failed", zap.String("operation", operation), zap.Error(err))
		return response.Error(c, fiber.StatusInternalServerError, "internal server error", "internal_error", nil)
	}
}

// wireNumber renders one number for a screen.
func wireNumber(number domain.Number) numberPayload {
	payload := numberPayload{
		ID:          number.ID.String(),
		Phone:       number.Phone,
		Label:       number.Label,
		Status:      number.Status,
		Reason:      number.StatusReason,
		Linked:      number.Linked(),
		Connected:   number.Connected(),
		TermsOK:     number.TOSAcceptedAt != nil,
		HourlyLimit: number.HourlyLimit,
		CreatedAt:   number.CreatedAt.UTC().Format(time.RFC3339),
	}
	if number.AgentID != uuid.Nil {
		payload.AgentID = number.AgentID.String()
	}
	if number.LastSeenAt != nil {
		payload.LastSeenAt = number.LastSeenAt.UTC().Format(time.RFC3339)
	}
	return payload
}

// wireLink renders one attempt for a screen.
func wireLink(link domain.Link) linkPayload {
	payload := linkPayload{
		ID:       link.ID,
		State:    link.State,
		QR:       link.Code,
		QRMatrix: qrMatrix(link.Code),
		PairCode: link.PairCode,
		Reason:   link.Reason,
	}
	if link.NumberID != uuid.Nil {
		payload.NumberID = link.NumberID.String()
	}
	if !link.ExpiresAt.IsZero() {
		payload.ExpiresAt = link.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return payload
}

// wireMessage renders one message for a screen.
func wireMessage(message domain.Inbound) messagePayload {
	payload := messagePayload{
		ID:          message.ID.String(),
		WaMessageID: message.WaMessageID,
		ChatJID:     message.ChatJID,
		SenderJID:   message.SenderJID,
		SenderName:  message.SenderName,
		IsGroup:     message.IsGroup,
		IsFromMe:    message.IsFromMe,
		Kind:        message.Kind,
		Text:        message.Text,
		MediaKind:   message.Media.Kind,
		MediaStatus: message.Media.Status,
		MediaError:  message.Media.Error,
		ReceivedAt:  message.ReceivedAt.UTC().Format(time.RFC3339),
	}
	if message.TaskID != uuid.Nil {
		payload.TaskID = message.TaskID.String()
	}
	return payload
}

// wireOutbox renders one queued message for a screen.
func wireOutbox(item domain.Outbox) outboxPayload {
	payload := outboxPayload{
		ID:        item.ID.String(),
		ToJID:     item.ToJID,
		Kind:      item.Kind,
		Text:      item.Text,
		Status:    item.Status,
		Attempts:  item.Attempts,
		LastError: item.LastError,
		CreatedAt: item.CreatedAt.UTC().Format(time.RFC3339),
	}
	if item.SentAt != nil {
		payload.SentAt = item.SentAt.UTC().Format(time.RFC3339)
	}
	return payload
}

// contentTypeFor names what an attachment is, so a browser shows it rather than
// downloading it.
func contentTypeFor(kind string) string {
	return map[string]string{
		domain.KindImage:    "image/jpeg",
		domain.KindVideo:    "video/mp4",
		domain.KindAudio:    "audio/ogg",
		domain.KindSticker:  "image/webp",
		domain.KindDocument: "application/octet-stream",
	}[kind]
}

// filenameFor is what the browser calls a downloaded attachment.
func filenameFor(message domain.Inbound) string {
	extension := map[string]string{
		domain.KindImage:   ".jpg",
		domain.KindVideo:   ".mp4",
		domain.KindAudio:   ".ogg",
		domain.KindSticker: ".webp",
	}[message.Media.Kind]

	return "whatsapp-" + message.WaMessageID + extension
}

// phoneOr falls back to the number already on the row.
func phoneOr(given, stored string) string {
	if strings.TrimSpace(given) != "" {
		return given
	}
	return stored
}

// parseID reads a uuid from the request.
func parseID(raw string) (uuid.UUID, error) {
	return uuid.Parse(strings.TrimSpace(raw))
}

// scope reads the tenant the request runs in, from the identity the container's
// middlewares resolved.
func scope(c *fiber.Ctx) domain.Scope {
	return domain.Scope{
		UserID:      middleware.CurrentUserID(c),
		WorkspaceID: middleware.WorkspaceUUID(c),
	}
}

// userMessage strips this module's own prefix from a refusal, so what a person
// reads is about what they typed rather than about which package noticed.
func userMessage(err error, prefix error) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(err.Error(), prefix.Error()), ":"))
}
