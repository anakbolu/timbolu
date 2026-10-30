// Package handler exposes the operator surface over HTTP: the accounts a
// deployment has, and the one decision an admin makes about them.
//
// It is mounted on a group the container guards with the platform-admin
// middleware, so every request that reaches it is already known to be an admin.
// The handler still takes the caller's id from the session rather than from the
// body — a request that names its own actor could promote itself — and the
// service re-reads that account's role, so the surface has two locks rather than
// one.
package handler

import (
	"errors"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/admin/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/middleware"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/response"
)

// Handler serves the operator surface.
type Handler struct {
	service domain.Service
	log     *zap.Logger
}

// New builds the handler.
func New(service domain.Service, log *zap.Logger) *Handler {
	return &Handler{service: service, log: log}
}

// Routes registers the endpoints. The container applies the auth middleware and
// the platform-admin check to this group.
func Routes(group fiber.Router, h *Handler) {
	group.Get("/admin/overview", h.Overview)
	group.Get("/admin/users", h.Users)
	group.Post("/admin/users/:userID/role", h.ChangeRole)
}

// Overview answers what the deployment's accounts add up to.
func (h *Handler) Overview(c *fiber.Ctx) error {
	stats, err := h.service.Overview(c.UserContext())
	if err != nil {
		return h.fail(c, "overview", err)
	}

	return response.OK(c, "ok", statsPayload{
		Total:          stats.Total,
		Admins:         stats.Admins,
		Users:          stats.Users,
		Verified:       stats.Verified,
		Onboarded:      stats.Onboarded,
		GoogleAccounts: stats.GoogleAccounts,
		NewLast7Days:   stats.NewLast7Days,
	})
}

// Users answers a page of accounts.
func (h *Handler) Users(c *fiber.Ctx) error {
	accounts, total, err := h.service.List(c.UserContext(), domain.Filter{
		Search: c.Query("q"),
		Role:   c.Query("role"),
		Limit:  c.QueryInt("limit", 0),
		Offset: c.QueryInt("offset", 0),
	})
	if err != nil {
		return h.fail(c, "list users", err)
	}

	payload := make([]accountPayload, 0, len(accounts))
	for _, account := range accounts {
		payload = append(payload, toAccountPayload(account))
	}

	// The filter comes back with the page, so a screen that shows "3 of 41" does
	// not have to remember what it asked for.
	normalised := domain.Filter{
		Search: c.Query("q"),
		Role:   c.Query("role"),
		Limit:  c.QueryInt("limit", 0),
		Offset: c.QueryInt("offset", 0),
	}.Normalised()

	return response.OKWithMeta(c, "ok", payload, fiber.Map{
		"count":  len(payload),
		"total":  total,
		"limit":  normalised.Limit,
		"offset": normalised.Offset,
		"role":   normalised.Role,
		"search": normalised.Search,
	})
}

// ChangeRole promotes or demotes an account.
func (h *Handler) ChangeRole(c *fiber.Ctx) error {
	targetID, err := uuid.Parse(c.Params("userID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "user id is not a uuid", "invalid_input", nil)
	}

	var req roleRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	account, err := h.service.ChangeRole(c.UserContext(), domain.ChangeRoleRequest{
		ActorID:  middleware.CurrentUserID(c),
		TargetID: targetID,
		Role:     req.Role,
	})
	if err != nil {
		return h.fail(c, "change role", err)
	}

	message := "peran diubah menjadi user"
	if account.Admin() {
		message = "peran diubah menjadi admin"
	}

	return response.OK(c, message, toAccountPayload(account))
}

// fail maps the module's errors onto responses.
func (h *Handler) fail(c *fiber.Ctx, operation string, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotAdmin):
		return response.Error(c, fiber.StatusForbidden, "hanya admin platform yang boleh membuka halaman ini", "forbidden", nil)
	case errors.Is(err, domain.ErrAccountNotFound):
		return response.Error(c, fiber.StatusNotFound, "akun tidak ditemukan", "user_not_found", nil)
	case errors.Is(err, domain.ErrLastAdmin):
		return response.Error(c, fiber.StatusConflict, "admin terakhir tidak bisa diturunkan", "last_admin", nil)
	case errors.Is(err, domain.ErrSelfDemotion):
		return response.Error(c, fiber.StatusConflict, "tidak bisa menurunkan peran akun sendiri", "self_demotion", nil)
	case errors.Is(err, domain.ErrNotConfigured):
		return response.Unavailable(c, "halaman admin tidak dikonfigurasi di deployment ini", "admin_not_configured")
	case errors.Is(err, domain.ErrInvalidInput):
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "invalid_input", nil)
	default:
		h.log.Error("admin request failed", zap.String("operation", operation), zap.Error(err))
		return response.Error(c, fiber.StatusInternalServerError, "internal server error", "internal_error", nil)
	}
}

// ---------------------------------------------------------------- the wire

// statsPayload is the overview's numbers.
type statsPayload struct {
	Total          int64 `json:"total"`
	Admins         int64 `json:"admins"`
	Users          int64 `json:"users"`
	Verified       int64 `json:"verified"`
	Onboarded      int64 `json:"onboarded"`
	GoogleAccounts int64 `json:"google_accounts"`
	NewLast7Days   int64 `json:"new_last_7_days"`
}

// accountPayload is one account as the operator screen reads it.
type accountPayload struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	Role          string `json:"role"`
	EmailVerified bool   `json:"email_verified"`
	Onboarded     bool   `json:"onboarded"`
	HasGoogle     bool   `json:"has_google"`
	HasPassword   bool   `json:"has_password"`
	Workspaces    int64  `json:"workspaces"`
	CreatedAt     string `json:"created_at"`
}

// roleRequest carries the role to set.
type roleRequest struct {
	Role string `json:"role"`
}

func toAccountPayload(account domain.Account) accountPayload {
	payload := accountPayload{
		ID:            account.ID.String(),
		Email:         account.Email,
		Role:          account.Role,
		EmailVerified: account.EmailVerified,
		Onboarded:     account.Onboarded,
		HasGoogle:     account.HasGoogle,
		HasPassword:   account.HasPassword,
		Workspaces:    account.Workspaces,
	}
	if !account.CreatedAt.IsZero() {
		payload.CreatedAt = account.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return payload
}
