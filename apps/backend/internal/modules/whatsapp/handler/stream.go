package handler

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"

	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
	"github.com/robzlabz/employeebot/packages/whatsapp/domain"
)

// The QR code reaches the browser over its own socket.
//
// Why a socket rather than the workspace's event stream: a QR is a capability. It
// is whoever scans it that links the number, so it must be visible to the person
// who started the attempt and to nobody else — and the workspace stream is
// exactly what every member receives. This endpoint is therefore per attempt,
// authenticated as the person who started it, and it stops the moment the attempt
// ends.
//
// The polling read stays as the way a screen recovers: a socket that drops because
// a laptop slept must not mean starting over, and one frame is not worth
// reconnecting for.

// linkFrameInterval is how often the attempt is read. A QR rotates every twenty
// seconds or so, so a second is comfortably ahead of it without being a busy loop
// against the gateway.
const linkFrameInterval = time.Second

// linkFrameLimit bounds one attempt's stream, so a socket left open cannot read the
// gateway forever. Ten minutes is several times the longest a pairing takes.
const linkFrameLimit = 10 * time.Minute

// LinkStreamRoutes registers the per-attempt socket. The container applies the
// middleware that authenticates from the query string, as it does for the event
// stream.
func LinkStreamRoutes(group fiber.Router, h *Handler) {
	group.Get("/whatsapp/links/:linkID/socket", h.LinkSocket)
}

// LinkSocket streams one link attempt's state until it ends.
func (h *Handler) LinkSocket(c *fiber.Ctx) error {
	if !websocket.IsWebSocketUpgrade(c) {
		return response.Error(c, fiber.StatusUpgradeRequired,
			"this endpoint speaks WebSocket", "upgrade_required", nil)
	}

	// The scope and the attempt are read before the upgrade: once the connection
	// is hijacked, Fiber's context no longer holds the request.
	linkScope := scope(c)
	linkID := c.Params("linkID")

	return websocket.New(func(conn *websocket.Conn) {
		ctx, cancel := context.WithTimeout(context.Background(), linkFrameLimit)
		defer cancel()

		// A client that goes away without closing cleanly would leave this loop
		// reading the gateway until the limit; reading in the background is what
		// notices.
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					cancel()
					return
				}
			}
		}()

		ticker := time.NewTicker(linkFrameInterval)
		defer ticker.Stop()

		var last string
		for {
			link, err := h.service.LinkState(ctx, linkScope, linkID)
			if err != nil {
				writeLinkError(conn, err)
				return
			}

			// A frame is written when something changed rather than every tick:
			// the QR is the same string until WhatsApp rotates it, and sending it
			// repeatedly would make a screen re-render a code somebody is midway
			// through scanning.
			frame := wireLink(link)
			encoded, err := json.Marshal(frame)
			if err != nil {
				return
			}
			if string(encoded) != last {
				if err := conn.WriteMessage(websocket.TextMessage, encoded); err != nil {
					return
				}
				last = string(encoded)
			}

			if link.State != domain.LinkWaiting && link.State != domain.LinkCode {
				// The attempt is over, either way. The socket closes rather than
				// waiting for the client to decide it is done.
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, link.State),
					time.Now().Add(5*time.Second))
				return
			}

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})(c)
}

// writeLinkError tells the browser what went wrong in one frame, so a refusal is
// read rather than inferred from a closed socket.
func writeLinkError(conn *websocket.Conn, err error) {
	message := "penautan ini tidak bisa dibaca lagi"
	switch {
	case errors.Is(err, domain.ErrNotFound):
		message = "penautan itu milik workspace lain"
	case errors.Is(err, domain.ErrLinkExpired):
		message = "penautan ini sudah kedaluwarsa; minta kode baru"
	case errors.Is(err, domain.ErrNotConfigured):
		message = "gateway WhatsApp belum dikonfigurasi di deployment ini"
	}

	encoded, marshalErr := json.Marshal(linkPayload{ID: "", State: domain.LinkFailed, Reason: message})
	if marshalErr != nil {
		return
	}
	_ = conn.WriteMessage(websocket.TextMessage, encoded)
}
