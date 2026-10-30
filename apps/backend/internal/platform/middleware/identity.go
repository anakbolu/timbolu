package middleware

import (
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/anakbolu/timbolu/packages/platform/database"
)

// Locals keys for the authenticated identity and the active tenant. They are
// written only by the container's auth and tenant middleware, and read by
// module handlers through the accessors below.
const (
	localUserID    = "user_id"
	localUserEmail = "user_email"
	localRole      = "role"
	localScope     = "scope"
)

// SetIdentity stores the authenticated user on the request. The email is kept
// as well because accepting an invitation must match it against the invited
// address, and no module may read another module's repository.
func SetIdentity(c *fiber.Ctx, userID uuid.UUID, email string) {
	c.Locals(localUserID, userID)
	c.Locals(localUserEmail, email)
}

// CurrentEmail returns the authenticated user's address, or an empty string.
func CurrentEmail(c *fiber.Ctx) string {
	email, _ := c.Locals(localUserEmail).(string)
	return email
}

// SetScope stores the tenant scope the request may query with. Only the tenant
// middleware sets it, and only after the caller's membership was verified.
func SetScope(c *fiber.Ctx, scope database.Scope) {
	c.Locals(localScope, scope)
}

// SetRole stores the caller's role inside the active workspace.
func SetRole(c *fiber.Ctx, role string) {
	c.Locals(localRole, role)
}

// CurrentUserID returns the authenticated user, or the zero uuid when the
// request is anonymous. Handlers on authenticated routes can rely on it being
// set, because the auth middleware rejects the request otherwise.
func CurrentUserID(c *fiber.Ctx) uuid.UUID {
	id, ok := c.Locals(localUserID).(uuid.UUID)
	if !ok {
		return uuid.Nil
	}
	return id
}

// Role returns the caller's role in the active workspace, or an empty string.
func Role(c *fiber.Ctx) string {
	role, _ := c.Locals(localRole).(string)
	return role
}

// Scope returns the tenant scope set by the tenant middleware. A zero scope
// means the route is not tenant scoped.
func Scope(c *fiber.Ctx) database.Scope {
	scope, ok := c.Locals(localScope).(database.Scope)
	if !ok {
		return database.Scope{}
	}
	return scope
}

// WorkspaceUUID returns the active workspace id, or the zero uuid when the
// request is not tenant scoped. The tenant scope is authoritative because the
// tenant middleware only sets it after verifying the caller's membership; the
// header value is the fallback for a request that has not been through it.
func WorkspaceUUID(c *fiber.Ctx) uuid.UUID {
	if scope, ok := c.Locals(localScope).(database.Scope); ok && scope.WorkspaceID != uuid.Nil {
		return scope.WorkspaceID
	}

	id, err := uuid.Parse(WorkspaceID(c))
	if err != nil {
		return uuid.Nil
	}
	return id
}
