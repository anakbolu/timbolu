// Package handler exposes the conversations over HTTP: threads, message
// history, sending, attachments, the event replay, and the live stream over
// WebSocket with an SSE fallback.
package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
)

// Handler serves the conversation endpoints.
type Handler struct {
	service chatdomain.Service
	log     *zap.Logger
	content ContentConfig
}

// ContentConfig describes the sandboxed content origin.
type ContentConfig struct {
	// FrameAncestors lists the origins allowed to frame a document. It is the
	// application's origin, and it must be named: the document is served from a
	// different host than the app, so `frame-ancestors 'self'` would refuse every
	// frame.
	FrameAncestors []string
}

// New builds the handler.
func New(service chatdomain.Service, log *zap.Logger) *Handler {
	return &Handler{service: service, log: log, content: ContentConfig{FrameAncestors: []string{"'self'"}}}
}

// WithContent sets the content origin policy.
func (h *Handler) WithContent(config ContentConfig) *Handler {
	if len(config.FrameAncestors) > 0 {
		h.content = config
	}
	return h
}

// Routes registers the endpoints every member of the workspace may use.
//
// The stream endpoints are separate because they are authenticated from the
// query string: a browser's WebSocket and EventSource clients cannot set an
// Authorization header, so the container mounts them behind a middleware that
// reads the token from the query instead.
func Routes(group fiber.Router, h *Handler) {
	group.Get("/conversations", h.List)
	group.Post("/conversations/direct", h.Direct)
	group.Post("/conversations/groups", h.CreateGroup)
	group.Get("/conversations/:conversationID", h.Get)
	group.Get("/conversations/:conversationID/messages", h.History)
	group.Post("/conversations/:conversationID/messages", h.Send)
	group.Post("/conversations/:conversationID/attachments", h.Attach)
	group.Get("/attachments/:attachmentID", h.Download)
	group.Get("/events", h.Events)
}

// ManagerRoutes registers the endpoints that change a group.
func ManagerRoutes(group fiber.Router, h *Handler) {
	group.Post("/conversations/:conversationID/participants", h.AddParticipants)
}

// StreamRoutes registers the live stream. The container applies the middleware
// that authenticates from the query string.
func StreamRoutes(group fiber.Router, h *Handler) {
	group.Get("/events/stream", h.SSE)
	group.Get("/events/socket", h.Socket)
}

// ContentRoutes registers the sandboxed content origin.
//
// It is mounted outside the tenant middleware on purpose: an iframe without
// allow-same-origin cannot send the application's cookies, so the reference in
// the URL is the only capability, and it is a UUID.
func ContentRoutes(group fiber.Router, h *Handler) {
	group.Get("/content/:reference", h.Content)
}

type sendRequest struct {
	Text          string   `json:"text"`
	AttachmentIDs []string `json:"attachment_ids"`
	Reply         *bool    `json:"reply"`
	AgentID       string   `json:"agent_id"`
}

type directRequest struct {
	AgentID string `json:"agent_id"`
}

type groupRequest struct {
	Title    string   `json:"title"`
	AgentIDs []string `json:"agent_ids"`
	UserIDs  []string `json:"user_ids"`
}

type participantsRequest struct {
	AgentIDs []string `json:"agent_ids"`
	UserIDs  []string `json:"user_ids"`
}

type conversationPayload struct {
	ID           string               `json:"id"`
	Kind         string               `json:"kind"`
	Title        string               `json:"title"`
	Participants []participantPayload `json:"participants"`
	MessageCount int                  `json:"message_count"`
	LastActivity string               `json:"last_activity_at"`
	CreatedAt    string               `json:"created_at"`
}

type participantPayload struct {
	ID      string `json:"id"`
	AgentID string `json:"agent_id,omitempty"`
	UserID  string `json:"user_id,omitempty"`
	Name    string `json:"name"`
	Role    string `json:"role"`
	IsAgent bool   `json:"is_agent"`
}

type sendPayload struct {
	Message        chatdomain.MessagePayload `json:"message"`
	ReplyMessageID string                    `json:"reply_message_id,omitempty"`
	Responder      *participantPayload       `json:"responder,omitempty"`
	Routed         bool                      `json:"routed"`
}

type pagePayload struct {
	Messages []chatdomain.MessagePayload `json:"messages"`
	// NextCursor is what the client sends back to read the following page.
	NextCursor string `json:"next_cursor,omitempty"`
	HasMore    bool   `json:"has_more"`
}

type eventPayload struct {
	ID             int64           `json:"id"`
	Type           string          `json:"type"`
	WorkspaceID    string          `json:"workspace_id"`
	AgentID        string          `json:"agent_id,omitempty"`
	UserID         string          `json:"user_id,omitempty"`
	ConversationID string          `json:"conversation_id,omitempty"`
	TaskID         string          `json:"task_id,omitempty"`
	DraftID        string          `json:"draft_id,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	CreatedAt      string          `json:"created_at"`
}

// List returns the threads of the workspace.
func (h *Handler) List(c *fiber.Ctx) error {
	conversations, err := h.service.Conversations(c.UserContext(), scope(c))
	if err != nil {
		return h.fail(c, "list conversations", err)
	}

	payload := make([]conversationPayload, 0, len(conversations))
	for _, conversation := range conversations {
		payload = append(payload, toConversationPayload(conversation))
	}

	return response.OK(c, "ok", payload)
}

// Get returns one thread.
func (h *Handler) Get(c *fiber.Ctx) error {
	id, err := parseID(c.Params("conversationID"), "conversation id")
	if err != nil {
		return badRequest(c, err.Error())
	}

	conversation, err := h.service.Conversation(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "get conversation", err)
	}

	return response.OK(c, "ok", toConversationPayload(conversation))
}

// Direct returns the 1:1 thread with one Bolu, creating it on first use.
func (h *Handler) Direct(c *fiber.Ctx) error {
	var req directRequest
	if err := c.BodyParser(&req); err != nil {
		return badRequest(c, "invalid request body")
	}

	agentID, err := parseID(req.AgentID, "agent id")
	if err != nil {
		return badRequest(c, err.Error())
	}

	conversation, err := h.service.Direct(c.UserContext(), scope(c), agentID)
	if err != nil {
		return h.fail(c, "open direct conversation", err)
	}

	return response.OK(c, "ok", toConversationPayload(conversation))
}

// CreateGroup makes a group.
func (h *Handler) CreateGroup(c *fiber.Ctx) error {
	var req groupRequest
	if err := c.BodyParser(&req); err != nil {
		return badRequest(c, "invalid request body")
	}

	agentIDs, err := parseIDs(req.AgentIDs, "agent id")
	if err != nil {
		return badRequest(c, err.Error())
	}
	userIDs, err := parseIDs(req.UserIDs, "user id")
	if err != nil {
		return badRequest(c, err.Error())
	}

	conversation, err := h.service.CreateGroup(c.UserContext(), scope(c), chatdomain.CreateGroupRequest{
		Title:    req.Title,
		AgentIDs: agentIDs,
		UserIDs:  userIDs,
	})
	if err != nil {
		return h.fail(c, "create group", err)
	}

	return response.Success(c, fiber.StatusCreated, "group created", toConversationPayload(conversation))
}

// AddParticipants adds Bolu and members to a group.
func (h *Handler) AddParticipants(c *fiber.Ctx) error {
	id, err := parseID(c.Params("conversationID"), "conversation id")
	if err != nil {
		return badRequest(c, err.Error())
	}

	var req participantsRequest
	if err := c.BodyParser(&req); err != nil {
		return badRequest(c, "invalid request body")
	}

	agentIDs, err := parseIDs(req.AgentIDs, "agent id")
	if err != nil {
		return badRequest(c, err.Error())
	}
	userIDs, err := parseIDs(req.UserIDs, "user id")
	if err != nil {
		return badRequest(c, err.Error())
	}

	conversation, err := h.service.AddParticipants(c.UserContext(), scope(c), id, chatdomain.AddParticipantsRequest{
		AgentIDs: agentIDs,
		UserIDs:  userIDs,
	})
	if err != nil {
		return h.fail(c, "add participants", err)
	}

	return response.OK(c, "participants added", toConversationPayload(conversation))
}

// History returns one page of messages, newest first.
func (h *Handler) History(c *fiber.Ctx) error {
	id, err := parseID(c.Params("conversationID"), "conversation id")
	if err != nil {
		return badRequest(c, err.Error())
	}

	cursor, err := parseCursor(c)
	if err != nil {
		return badRequest(c, err.Error())
	}

	page, err := h.service.History(c.UserContext(), scope(c), id, cursor)
	if err != nil {
		return h.fail(c, "read history", err)
	}

	payload := pagePayload{Messages: make([]chatdomain.MessagePayload, 0, len(page.Messages)), HasMore: page.HasMore}
	for _, message := range page.Messages {
		payload.Messages = append(payload.Messages, chatdomain.WireMessage(message))
	}
	if page.HasMore {
		payload.NextCursor = encodeCursor(page.NextBeforeCreatedAtNanos, page.NextBeforeID)
	}

	return response.OK(c, "ok", payload)
}

// Send stores a message and starts the reply.
func (h *Handler) Send(c *fiber.Ctx) error {
	id, err := parseID(c.Params("conversationID"), "conversation id")
	if err != nil {
		return badRequest(c, err.Error())
	}

	var req sendRequest
	if err := c.BodyParser(&req); err != nil {
		return badRequest(c, "invalid request body")
	}

	attachmentIDs, err := parseIDs(req.AttachmentIDs, "attachment id")
	if err != nil {
		return badRequest(c, err.Error())
	}
	agentID, err := parseOptionalID(req.AgentID, "agent id")
	if err != nil {
		return badRequest(c, err.Error())
	}

	// A message is answered by default: sending one and expecting nothing is the
	// exception, and a client that wants to post a note says so explicitly.
	reply := true
	if req.Reply != nil {
		reply = *req.Reply
	}

	result, err := h.service.Send(c.UserContext(), scope(c), chatdomain.SendRequest{
		ConversationID: id,
		Text:           req.Text,
		AttachmentIDs:  attachmentIDs,
		Reply:          reply,
		AgentID:        agentID,
	})
	if err != nil {
		return h.fail(c, "send message", err)
	}

	payload := sendPayload{
		Message:        chatdomain.WireMessage(result.Message),
		ReplyMessageID: idString(result.ReplyMessageID),
		Routed:         result.Routed,
	}
	if result.Responder != nil {
		responder := toParticipantPayloadAgent(*result.Responder)
		payload.Responder = &responder
	}

	return response.Success(c, fiber.StatusCreated, "message sent", payload)
}

// Attach stores an uploaded file.
func (h *Handler) Attach(c *fiber.Ctx) error {
	id, err := parseID(c.Params("conversationID"), "conversation id")
	if err != nil {
		return badRequest(c, err.Error())
	}

	file, err := c.FormFile("file")
	if err != nil {
		return badRequest(c, "a file is required")
	}
	if file.Size > chatdomain.AttachmentMaxBytes {
		return response.Error(c, fiber.StatusRequestEntityTooLarge,
			fmt.Sprintf("the file is larger than %d bytes", chatdomain.AttachmentMaxBytes), "file_too_large", nil)
	}

	messageID, err := parseOptionalID(c.FormValue("message_id"), "message id")
	if err != nil {
		return badRequest(c, err.Error())
	}

	opened, err := file.Open()
	if err != nil {
		return h.fail(c, "open upload", err)
	}
	defer func() { _ = opened.Close() }()

	content, err := readUpload(opened)
	if err != nil {
		return response.Error(c, fiber.StatusRequestEntityTooLarge, err.Error(), "file_too_large", nil)
	}

	// One call for both shapes. Without a message id the service opens the message
	// the file belongs to inside the same transaction that stores it, so an upload
	// that fails leaves no empty bubble behind and a client is never told about a
	// message that then changes. The part's own Content-Type is not passed on: the
	// file's type is read from its extension and its bytes, and a header a client
	// chose is neither.
	attachment, err := h.service.Attach(c.UserContext(), scope(c), chatdomain.AttachRequest{
		MessageID:      messageID,
		ConversationID: id,
		Filename:       file.Filename,
	}, content)
	if err != nil {
		return h.fail(c, "store attachment", err)
	}

	return response.Success(c, fiber.StatusCreated, "file stored", chatdomain.WireAttachment(attachment))
}

// Download streams one attachment's bytes.
func (h *Handler) Download(c *fiber.Ctx) error {
	id, err := parseID(c.Params("attachmentID"), "attachment id")
	if err != nil {
		return badRequest(c, err.Error())
	}

	attachment, content, err := h.service.AttachmentContent(c.UserContext(), scope(c), id)
	if err != nil {
		return h.fail(c, "read attachment", err)
	}

	c.Set(fiber.HeaderContentType, attachment.ContentType)
	c.Set(fiber.HeaderContentDisposition,
		fmt.Sprintf(`inline; filename="%s"`, strings.ReplaceAll(attachment.Filename, `"`, "")))
	// An attachment is served as data, never executed: this is what stops a
	// crafted content type from turning a download into a script the browser
	// runs on the API's origin.
	c.Set("X-Content-Type-Options", "nosniff")
	c.Set("Content-Security-Policy", "default-src 'none'; sandbox")

	return c.Send(content)
}

// Events returns the workspace stream after an ID.
func (h *Handler) Events(c *fiber.Ctx) error {
	afterID := int64(c.QueryInt("after_id", 0))
	limit := c.QueryInt("limit", 0)

	events, err := h.service.Events(c.UserContext(), scope(c), afterID, limit)
	if err != nil {
		return h.fail(c, "read events", err)
	}

	return response.OK(c, "ok", toEventPayloads(events))
}

// SSE streams the workspace events as Server-Sent Events.
//
// It is the fallback for a client that cannot open a WebSocket. Every frame
// carries the event ID, so a client that reconnects with the `Last-Event-ID`
// header (which the browser sends by itself) resumes exactly where it stopped.
func (h *Handler) SSE(c *fiber.Ctx) error {
	ctx := c.UserContext()
	events, err := h.service.Stream(ctx, scope(c), lastEventID(c))
	if err != nil {
		return h.fail(c, "open stream", err)
	}

	c.Set(fiber.HeaderContentType, "text/event-stream")
	c.Set("Cache-Control", "no-cache, no-transform")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		// The head goes out on its own, before there is an event to carry it:
		// fasthttp sends the status line and the headers only when the first
		// bytes are flushed, so a client that resumes with nothing to replay
		// (the ordinary reconnect, whose cursor is already the newest event)
		// would otherwise wait for the first heartbeat before its EventSource
		// reported itself open. A comment is not an event, so a client ignores
		// the line and a listener that only knows named events is unaffected.
		if _, err := w.WriteString(": connected\n\n"); err != nil {
			return
		}
		if err := w.Flush(); err != nil {
			return
		}

		heartbeat := time.NewTicker(chatdomain.Heartbeat)
		defer heartbeat.Stop()

		encoder := json.NewEncoder(w)
		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeat.C:
				// A comment keeps an idle connection alive through a proxy
				// without being an event the client must ignore by type.
				if _, err := w.Write([]byte(": keep-alive\n\n")); err != nil {
					return
				}
				_ = w.Flush()
			case event, ok := <-events:
				if !ok {
					return
				}
				if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: ", event.ID, event.Type); err != nil {
					return
				}
				if err := encoder.Encode(toEventPayload(event)); err != nil {
					return
				}
				if _, err := w.Write([]byte("\n")); err != nil {
					return
				}
				_ = w.Flush()
			}
		}
	})

	return nil
}

// Socket streams the workspace events over a WebSocket.
func (h *Handler) Socket(c *fiber.Ctx) error {
	if !websocket.IsWebSocketUpgrade(c) {
		return response.Error(c, fiber.StatusUpgradeRequired,
			"this endpoint speaks WebSocket; use /events/stream for SSE", "upgrade_required", nil)
	}

	// The scope and the resume point are read before the upgrade: once the
	// connection is hijacked, Fiber's context no longer holds the request.
	streamScope := scope(c)
	afterID := lastEventID(c)

	return websocket.New(func(conn *websocket.Conn) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		events, err := h.service.Stream(ctx, streamScope, afterID)
		if err != nil {
			_ = conn.WriteJSON(map[string]string{"type": "error", "message": err.Error()})
			return
		}

		// A client may send a resume request on the open socket, which is what
		// makes a reconnect cheap: no new connection, no missed events.
		go func() {
			for {
				var request struct {
					AfterID int64 `json:"after_id"`
				}
				if err := conn.ReadJSON(&request); err != nil {
					cancel()
					return
				}
			}
		}()

		heartbeat := time.NewTicker(chatdomain.Heartbeat)
		defer heartbeat.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeat.C:
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			case event, ok := <-events:
				if !ok {
					return
				}
				if err := conn.WriteJSON(toEventPayload(event)); err != nil {
					return
				}
			}
		}
	})(c)
}

// Content serves one sandboxed HTML document from the content origin.
//
// The reference is a storage key, so nothing is looked up in the database and no
// session is needed: this route exists to be framed, not to be authenticated.
//
// The policy is the security boundary, not the sandbox attribute alone: with
// connect-src 'none' a script inside the document cannot reach the network at
// all, and default-src 'none' leaves it no way to load anything but an inline
// script and its own styles. The document is served with a content type of
// text/html from a path that never carries a cookie-backed session, so a script
// that somehow ran outside the sandbox would still have nothing to read.
func (h *Handler) Content(c *fiber.Ctx) error {
	reference := strings.TrimSpace(c.Params("reference"))
	if reference == "" {
		return badRequest(c, "a content reference is required")
	}

	document, err := h.service.Content(c.UserContext(), reference)
	if err != nil {
		return h.fail(c, "read content", err)
	}

	c.Set(fiber.HeaderContentType, "text/html; charset=utf-8")
	c.Set("X-Content-Type-Options", "nosniff")
	c.Set("Content-Security-Policy", h.contentPolicy())
	c.Set("Referrer-Policy", "no-referrer")
	// The frame is the only place this document may appear, and it may not
	// navigate the application.
	c.Set("Cross-Origin-Resource-Policy", "same-site")

	return c.Send(document)
}

// contentPolicy is the CSP the sandboxed documents are served under.
//
// connect-src 'none' is the one that matters: it is what stops an agent-written
// script from exfiltrating anything it can read, whether or not the sandbox
// attribute holds. `script-src` names the CDNs a document may load from and
// nothing else, and `frame-ancestors` names the application, because the content
// origin is a different host from the app by design.
func (h *Handler) contentPolicy() string {
	ancestors := strings.Join(h.content.FrameAncestors, " ")

	return "default-src 'none'; " +
		"script-src 'unsafe-inline' https://cdn.jsdelivr.net https://unpkg.com; " +
		"style-src 'unsafe-inline'; " +
		"img-src data: blob:; " +
		"font-src data:; " +
		"media-src data: blob:; " +
		"connect-src 'none'; " +
		"form-action 'none'; " +
		"frame-ancestors " + ancestors + "; " +
		"base-uri 'none'"
}

func (h *Handler) fail(c *fiber.Ctx, operation string, err error) error {
	var requestErr requestError
	if errors.As(err, &requestErr) {
		return response.Error(c, requestErr.status, requestErr.message, requestErr.code, nil)
	}

	switch {
	case errors.Is(err, chatdomain.ErrConversationNotFound):
		return response.Error(c, fiber.StatusNotFound, "conversation not found", "conversation_not_found", nil)
	case errors.Is(err, chatdomain.ErrMessageNotFound):
		return response.Error(c, fiber.StatusNotFound, "message not found", "message_not_found", nil)
	case errors.Is(err, chatdomain.ErrAttachmentNotFound):
		return response.Error(c, fiber.StatusNotFound, "attachment not found", "attachment_not_found", nil)
	case errors.Is(err, chatdomain.ErrAgentNotInConversation):
		return response.Error(c, fiber.StatusBadRequest, "that Bolu is not in this conversation", "agent_not_in_conversation", nil)
	case errors.Is(err, chatdomain.ErrNoResponder):
		return response.Error(c, fiber.StatusConflict, err.Error(), "no_responder", nil)
	case errors.Is(err, chatdomain.ErrNoBudget):
		// 402 rather than 403: nothing is forbidden about who is asking, and a screen
		// that showed "ask an admin" would send somebody to the wrong place. What fixes
		// this is a payment.
		return response.Error(c, fiber.StatusPaymentRequired, err.Error(), "no_budget", nil)
	case errors.Is(err, chatdomain.ErrInvalidBlock):
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "invalid_block", nil)
	case errors.Is(err, chatdomain.ErrBlockTooLarge):
		return response.Error(c, fiber.StatusRequestEntityTooLarge, err.Error(), "block_too_large", nil)
	case errors.Is(err, chatdomain.ErrFileTypeNotAllowed):
		// 415 rather than 400: the request is well formed and the file is real, and
		// what is wrong is its media type — which is what a client checks for to tell
		// the user to pick another file.
		return response.Error(c, fiber.StatusUnsupportedMediaType, err.Error(), "file_type_not_allowed", nil)
	case errors.Is(err, chatdomain.ErrInvalidInput):
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "invalid_input", nil)
	case errors.Is(err, chatdomain.ErrStorageUnavailable):
		return response.Unavailable(c, err.Error(), "storage_not_configured")
	default:
		h.log.Error("chat request failed", zap.String("operation", operation), zap.Error(err))
		return response.Error(c, fiber.StatusInternalServerError, "internal server error", "internal_error", nil)
	}
}

// requestError is a failure the handler itself detected, carrying the status and
// code the client sees.
type requestError struct {
	status  int
	code    string
	message string
}

func (e requestError) Error() string { return e.message }

func badRequest(c *fiber.Ctx, message string) error {
	return response.Error(c, fiber.StatusBadRequest, message, "invalid_request", nil)
}

// scope builds the module scope from the identity the middleware stored.
func scope(c *fiber.Ctx) chatdomain.Scope {
	return chatdomain.Scope{
		UserID:      middleware.CurrentUserID(c),
		WorkspaceID: middleware.WorkspaceUUID(c),
	}
}

// lastEventID reads the resume point: the Last-Event-ID header the browser sends
// on its own, then the query parameter an explicit reconnect uses.
func lastEventID(c *fiber.Ctx) int64 {
	if raw := strings.TrimSpace(c.Get("Last-Event-ID")); raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
			return id
		}
	}
	if raw := strings.TrimSpace(c.Query("last_event_id")); raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
			return id
		}
	}
	return 0
}

// parseCursor reads the page cursor: the two parts of the last message of the
// previous page.
func parseCursor(c *fiber.Ctx) (chatdomain.Cursor, error) {
	cursor := chatdomain.Cursor{Limit: c.QueryInt("limit", 0)}

	raw := strings.TrimSpace(c.Query("cursor"))
	if raw == "" {
		return cursor, nil
	}

	createdAt, id, ok := decodeCursor(raw)
	if !ok {
		return chatdomain.Cursor{}, errors.New("invalid cursor")
	}
	cursor.BeforeCreatedAtNanos = createdAt
	cursor.BeforeID = id

	return cursor, nil
}

// encodeCursor packs a page boundary into one opaque string, so a client cannot
// be tempted to build one by hand and get the ordering wrong.
func encodeCursor(createdAtNanos int64, id uuid.UUID) string {
	return strconv.FormatInt(createdAtNanos, 10) + "." + id.String()
}

func decodeCursor(raw string) (int64, uuid.UUID, bool) {
	parts := strings.SplitN(raw, ".", 2)
	if len(parts) != 2 {
		return 0, uuid.Nil, false
	}

	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, uuid.Nil, false
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return 0, uuid.Nil, false
	}
	return nanos, id, true
}

func parseID(raw, field string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid %s", field)
	}
	return id, nil
}

func parseOptionalID(raw, field string) (uuid.UUID, error) {
	if strings.TrimSpace(raw) == "" {
		return uuid.Nil, nil
	}
	return parseID(raw, field)
}

func parseIDs(raw []string, field string) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, 0, len(raw))
	for _, value := range raw {
		id, err := parseID(value, field)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func readUpload(reader io.Reader) ([]byte, error) {
	buffer := &bytes.Buffer{}
	written, err := io.Copy(buffer, io.LimitReader(reader, chatdomain.AttachmentMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read upload: %w", err)
	}
	if written > chatdomain.AttachmentMaxBytes {
		return nil, fmt.Errorf("the file is larger than %d bytes", chatdomain.AttachmentMaxBytes)
	}
	return buffer.Bytes(), nil
}

func idString(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

func toConversationPayload(conversation chatdomain.Conversation) conversationPayload {
	participants := make([]participantPayload, 0, len(conversation.Participants))
	for _, participant := range conversation.Participants {
		participants = append(participants, participantPayload{
			ID:      participant.ID.String(),
			AgentID: idString(participant.AgentID),
			UserID:  idString(participant.UserID),
			Name:    participant.DisplayName,
			Role:    participant.Role,
			IsAgent: participant.IsAgent(),
		})
	}

	return conversationPayload{
		ID:           conversation.ID.String(),
		Kind:         conversation.Kind,
		Title:        conversation.Title,
		Participants: participants,
		MessageCount: conversation.MessageCount,
		LastActivity: conversation.LastActivityAt.Format(time.RFC3339),
		CreatedAt:    conversation.CreatedAt.Format(time.RFC3339),
	}
}

func toParticipantPayloadAgent(agent chatdomain.AgentRef) participantPayload {
	return participantPayload{
		AgentID: agent.ID.String(),
		Name:    agent.Name,
		Role:    agent.Role,
		IsAgent: true,
	}
}

func toEventPayloads(events []chatdomain.Event) []eventPayload {
	payload := make([]eventPayload, 0, len(events))
	for _, event := range events {
		payload = append(payload, toEventPayload(event))
	}
	return payload
}

func toEventPayload(event chatdomain.Event) eventPayload {
	return eventPayload{
		ID:             event.ID,
		Type:           event.Type,
		WorkspaceID:    event.WorkspaceID.String(),
		AgentID:        idString(event.ActorAgentID),
		UserID:         idString(event.ActorUserID),
		ConversationID: idString(event.ConversationID),
		TaskID:         idString(event.TaskID),
		DraftID:        idString(event.DraftID),
		Payload:        event.Payload,
		CreatedAt:      event.CreatedAt.Format(time.RFC3339Nano),
	}
}
