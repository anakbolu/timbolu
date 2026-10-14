package container

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/domain"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
	"github.com/robzlabz/employeebot/packages/platform/database"
)

// HeaderWorkspaceID is the header that carries the active workspace. The
// frontend sends the workspace the user selected, and the tenant middleware
// verifies the caller is a member of it before any query runs.
const HeaderWorkspaceID = "X-Workspace-Id"

// requireUser authenticates the request from the bearer access token and stores
// the identity on the request context.
//
// An identity that is already resolved is left alone rather than resolved again.
// That is what lets the live stream authenticate from the query string first
// (a browser cannot set a header on a WebSocket) and this middleware then accept
// the identity it set, instead of rejecting the request for having no header.
func (c *Container) requireUser() fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		if c.Services.Auth == nil {
			return response.Unavailable(ctx, "authentication is not configured", "auth_not_configured")
		}
		if middleware.CurrentUserID(ctx) != uuid.Nil {
			return ctx.Next()
		}

		token := bearerToken(ctx)
		if token == "" {
			return response.Error(ctx, fiber.StatusUnauthorized, "access token is required", "unauthenticated", nil)
		}

		user, err := c.Services.Auth.Authenticate(ctx.UserContext(), token)
		if err != nil {
			if errors.Is(err, authdomain.ErrInvalidToken) {
				return response.Error(ctx, fiber.StatusUnauthorized, "access token is invalid or expired", "unauthenticated", nil)
			}
			c.Logger.Error("authenticate request failed", zap.Error(err))
			return response.Error(ctx, fiber.StatusInternalServerError, "internal server error", "internal_error", nil)
		}

		middleware.SetIdentity(ctx, user.ID, user.Email)
		return ctx.Next()
	}
}

// requireTenant resolves the active workspace, verifies the caller's membership,
// and stores the tenant scope the handlers query with.
//
// Like requireUser, a scope that is already resolved is left alone: the stream
// middleware resolves it from the query string, and re-resolving it here would
// reject the request.
//
// The check is on the stored scope, never on the workspace header: the header is
// a request value, so treating it as proof of an already-verified tenant would
// skip the membership check entirely.
func (c *Container) requireTenant() fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		if c.Services.Workspace == nil {
			return response.Unavailable(ctx, "workspaces are not configured", "workspace_not_configured")
		}
		if !middleware.Scope(ctx).IsZero() {
			return ctx.Next()
		}

		workspaceID, err := requestedWorkspaceID(ctx)
		if err != nil {
			return response.Error(ctx, fiber.StatusBadRequest, err.Error(), "invalid_workspace", nil)
		}

		userID := middleware.CurrentUserID(ctx)
		role, err := c.Services.Workspace.Resolve(ctx.UserContext(), workspaceID, userID)
		if err != nil {
			if errors.Is(err, workspacedomain.ErrNotMember) {
				return response.Error(ctx, fiber.StatusForbidden, "you are not a member of this workspace", "not_a_member", nil)
			}
			c.Logger.Error("resolve membership failed", zap.Error(err))
			return response.Error(ctx, fiber.StatusInternalServerError, "internal server error", "internal_error", nil)
		}

		middleware.SetScope(ctx, database.Scope{UserID: userID, WorkspaceID: workspaceID})
		middleware.SetRole(ctx, role)
		return ctx.Next()
	}
}

// requireStreamAuth authenticates the live-stream endpoints.
//
// It is requireUser and requireTenant with one difference: the token and the
// workspace may come from the query string. A browser's WebSocket and
// EventSource clients cannot set a header, so without this the live stream would
// be unreachable from a page — and the alternative, an unauthenticated stream,
// would leak another tenant's events.
func (c *Container) requireStreamAuth(notConfiguredCode string) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		if c.Services.Auth == nil || c.Services.Workspace == nil {
			return response.Unavailable(ctx, "the live stream is not configured", notConfiguredCode)
		}

		token := bearerToken(ctx)
		if token == "" {
			token = strings.TrimSpace(ctx.Query("token"))
		}
		if token == "" {
			return response.Error(ctx, fiber.StatusUnauthorized, "access token is required", "unauthenticated", nil)
		}

		user, err := c.Services.Auth.Authenticate(ctx.UserContext(), token)
		if err != nil {
			if errors.Is(err, authdomain.ErrInvalidToken) {
				return response.Error(ctx, fiber.StatusUnauthorized, "access token is invalid or expired", "unauthenticated", nil)
			}
			c.Logger.Error("authenticate stream failed", zap.Error(err))
			return response.Error(ctx, fiber.StatusInternalServerError, "internal server error", "internal_error", nil)
		}
		middleware.SetIdentity(ctx, user.ID, user.Email)

		workspaceID, err := requestedWorkspaceID(ctx)
		if err != nil {
			return response.Error(ctx, fiber.StatusBadRequest, err.Error(), "invalid_workspace", nil)
		}

		role, err := c.Services.Workspace.Resolve(ctx.UserContext(), workspaceID, user.ID)
		if err != nil {
			if errors.Is(err, workspacedomain.ErrNotMember) {
				return response.Error(ctx, fiber.StatusForbidden, "you are not a member of this workspace", "not_a_member", nil)
			}
			c.Logger.Error("resolve membership for stream failed", zap.Error(err))
			return response.Error(ctx, fiber.StatusInternalServerError, "internal server error", "internal_error", nil)
		}

		middleware.SetScope(ctx, database.Scope{UserID: user.ID, WorkspaceID: workspaceID})
		middleware.SetRole(ctx, role)

		return ctx.Next()
	}
}

// requireRole rejects the request unless the caller's role in the active
// workspace is one of roles.
func requireRole(roles ...string) fiber.Handler {
	allowed := make(map[string]bool, len(roles))
	for _, role := range roles {
		allowed[role] = true
	}

	return func(ctx *fiber.Ctx) error {
		if !allowed[middleware.Role(ctx)] {
			return response.Error(ctx, fiber.StatusForbidden, "your role does not allow this action", "forbidden", nil)
		}
		return ctx.Next()
	}
}

// bearerToken reads the access token from the Authorization header.
func bearerToken(ctx *fiber.Ctx) string {
	header := strings.TrimSpace(ctx.Get(fiber.HeaderAuthorization))
	if header == "" {
		return ""
	}

	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// requestedWorkspaceID reads the active workspace from the request header, then
// from the query string. Anything that is not a UUID is rejected rather than
// passed to the database.
func requestedWorkspaceID(ctx *fiber.Ctx) (uuid.UUID, error) {
	raw := strings.TrimSpace(ctx.Get(HeaderWorkspaceID))
	if raw == "" {
		raw = strings.TrimSpace(ctx.Query("workspace_id"))
	}
	if raw == "" {
		return uuid.Nil, fmt.Errorf("workspace id is required")
	}

	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("workspace id is not a uuid")
	}
	return id, nil
}

// durationMinutes converts a configured minute count to a duration, falling back
// to a default when the value is missing.
func durationMinutes(minutes int, fallback time.Duration) time.Duration {
	if minutes <= 0 {
		return fallback
	}
	return time.Duration(minutes) * time.Minute
}

// durationHours converts a configured hour count to a duration.
func durationHours(hours int, fallback time.Duration) time.Duration {
	if hours <= 0 {
		return fallback
	}
	return time.Duration(hours) * time.Hour
}

// requireVerified is the middleware that enforces a verified email. It is built
// on top of requireUser so the identity is always present.
func (c *Container) requireVerified() fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		if c.Services.Auth == nil {
			return response.Unavailable(ctx, "authentication is not configured", "auth_not_configured")
		}

		if err := c.Services.Auth.AssertVerified(ctx.UserContext(), middleware.CurrentUserID(ctx)); err != nil {
			if errors.Is(err, authdomain.ErrEmailNotVerified) {
				return response.Error(ctx, fiber.StatusForbidden, "verify your email first", "email_not_verified", nil)
			}
			c.Logger.Error("verify account failed", zap.Error(err))
			return response.Error(ctx, fiber.StatusInternalServerError, "internal server error", "internal_error", nil)
		}
		return ctx.Next()
	}
}
