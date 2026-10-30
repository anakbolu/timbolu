// Package handler exposes the device registry over HTTP: registering a phone's push address, and
// removing it when the phone signs out.
//
// Both routes are the caller's own rows. The user comes from the bearer token and the workspace from
// the tenant middleware, so a client never names either — and a token belonging to another account is
// answered as "not found" rather than as "forbidden", because whether it exists is not this endpoint's
// to tell.
package handler

import (
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/device/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/middleware"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/response"
)

// Handler serves the device endpoints.
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
// A device belongs to the person holding it rather than to the workspace, so there is no role to
// check: a member registering their phone is the only thing this surface is for.
func Routes(group fiber.Router, h *Handler) {
	group.Post("/devices", h.Register)
	group.Delete("/devices/:token", h.Unregister)
}

type registerRequest struct {
	Token      string `json:"token"`
	Platform   string `json:"platform"`
	AppVersion string `json:"app_version"`
}

// devicePayload is a registered device as a client reads it.
type devicePayload struct {
	DeviceID    string `json:"device_id"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	Platform    string `json:"platform"`
	Token       string `json:"token"`
	AppVersion  string `json:"app_version,omitempty"`
	LastSeenAt  string `json:"last_seen_at"`
	CreatedAt   string `json:"created_at"`
}

// Register stores a phone's push address, updating the row a repeated token already has.
func (h *Handler) Register(c *fiber.Ctx) error {
	var body registerRequest
	if err := c.BodyParser(&body); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	device, err := h.service.Register(c.UserContext(), scope(c), domain.RegisterRequest{
		Token:      body.Token,
		Platform:   body.Platform,
		AppVersion: body.AppVersion,
	})
	if err != nil {
		return h.fail(c, "register device", err)
	}

	return response.Created(c, "perangkat terdaftar", toPayload(device))
}

// Unregister removes one of the caller's tokens.
//
// 204 rather than an envelope: the row is gone, and there is nothing about it left to say. A token
// that is not the caller's is 404, which is the same answer as a token nobody registered.
func (h *Handler) Unregister(c *fiber.Ctx) error {
	if err := h.service.Delete(c.UserContext(), scope(c), c.Params("token")); err != nil {
		return h.fail(c, "delete device", err)
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// fail maps the module's errors onto responses.
func (h *Handler) fail(c *fiber.Ctx, operation string, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return response.Error(c, fiber.StatusNotFound, "perangkat tidak ditemukan", "not_found", nil)
	case errors.Is(err, domain.ErrInvalidInput):
		return response.Error(c, fiber.StatusUnprocessableEntity, userMessage(err, domain.ErrInvalidInput), "validation_failed", nil)
	default:
		h.log.Error("device request failed", zap.String("operation", operation), zap.Error(err))

		return response.Error(c, fiber.StatusInternalServerError, "permintaan gagal", "internal_error", nil)
	}
}

// scope is the tenant the request runs as.
func scope(c *fiber.Ctx) domain.Scope {
	return domain.Scope{
		UserID:      middleware.CurrentUserID(c),
		WorkspaceID: middleware.WorkspaceUUID(c),
	}
}

// toPayload renders a registered device.
func toPayload(device domain.Device) devicePayload {
	payload := devicePayload{
		DeviceID:   device.ID.String(),
		Platform:   device.Platform,
		Token:      device.Token,
		AppVersion: device.AppVersion,
		LastSeenAt: device.LastSeenAt.UTC().Format(time.RFC3339),
		CreatedAt:  device.CreatedAt.UTC().Format(time.RFC3339),
	}
	if device.WorkspaceID != uuid.Nil {
		payload.WorkspaceID = device.WorkspaceID.String()
	}

	return payload
}

// userMessage renders a validation error for a person: the detail after the sentinel's own text,
// because everything before it is this module's name.
func userMessage(err error, sentinel error) string {
	message := err.Error()
	prefix := sentinel.Error() + ": "

	if strings.HasPrefix(message, prefix) {
		return strings.TrimPrefix(message, prefix)
	}

	return message
}
