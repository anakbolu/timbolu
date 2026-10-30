// Package handler exposes the workspace module over HTTP. It talks to the
// domain service interface only.
package handler

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/workspace/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/middleware"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/response"
)

// Handler serves the workspace endpoints.
type Handler struct {
	service domain.Service
	log     *zap.Logger
	// afterClockChange runs when an update moved the workspace's timezone or working
	// window. It is a plain callback rather than a port because the workspace module
	// has no business knowing what depends on its clock: the container is the only
	// place that knows, and it is the one that wires this.
	afterClockChange func(ctx context.Context, workspaceID uuid.UUID)
}

// New builds the handler.
func New(service domain.Service, log *zap.Logger) *Handler {
	return &Handler{service: service, log: log}
}

// WithClockChange installs the callback that runs after the workspace's clock moves.
func (h *Handler) WithClockChange(fn func(ctx context.Context, workspaceID uuid.UUID)) *Handler {
	h.afterClockChange = fn
	return h
}

type onboardRequest struct {
	Name          string `json:"name"`
	BusinessField string `json:"business_field"`
	Timezone      string `json:"timezone"`
}

type updateWorkspaceRequest struct {
	Name          string `json:"name"`
	BusinessField string `json:"business_field"`
	Timezone      string `json:"timezone"`
	Language      string `json:"language"`
	// The window outbound actions may leave in, as "HH:MM" and ISO weekdays.
	WorkHoursStart string `json:"work_hours_start"`
	WorkHoursEnd   string `json:"work_hours_end"`
	WorkDays       []int  `json:"work_days"`
}

type setRoleRequest struct {
	Role string `json:"role"`
}

type inviteRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

type acceptInvitationRequest struct {
	Token string `json:"token"`
}

type workspacePayload struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	BusinessField string `json:"business_field"`
	Timezone      string `json:"timezone"`
	Language      string `json:"language"`
	Role          string `json:"role,omitempty"`
	// The window outbound actions may leave in. The Pengaturan screen edits it and
	// the approval gate reads it, so the screen shows the value the gate uses.
	WorkHoursStart string `json:"work_hours_start"`
	WorkHoursEnd   string `json:"work_hours_end"`
	WorkDays       []int  `json:"work_days"`
	CreatedAt      string `json:"created_at"`
}

type teamPayload struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type memberPayload struct {
	UserID        string `json:"user_id"`
	Email         string `json:"email"`
	Role          string `json:"role"`
	EmailVerified bool   `json:"email_verified"`
	JoinedAt      string `json:"joined_at"`
}

type invitationPayload struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	Role        string `json:"role"`
	ExpiresAt   string `json:"expires_at"`
	InvitedAt   string `json:"invited_at"`
	WorkspaceID string `json:"workspace_id,omitempty"`
}

type onboardPayload struct {
	Workspace workspacePayload `json:"workspace"`
	Teams     []teamPayload    `json:"teams"`
	Created   bool             `json:"created"`
}

// Onboard creates the caller's workspace with its owner membership and the two
// default teams.
func (h *Handler) Onboard(c *fiber.Ctx) error {
	var req onboardRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	result, err := h.service.Onboard(c.UserContext(), middleware.CurrentUserID(c), domain.OnboardRequest{
		Name:          req.Name,
		BusinessField: req.BusinessField,
		Timezone:      req.Timezone,
	})
	if err != nil {
		return h.fail(c, "onboard", err)
	}

	return response.OK(c, "workspace ready", toOnboardPayload(result))
}

// List returns every workspace the caller belongs to.
func (h *Handler) List(c *fiber.Ctx) error {
	workspaces, err := h.service.Workspaces(c.UserContext(), middleware.CurrentUserID(c))
	if err != nil {
		return h.fail(c, "list workspaces", err)
	}

	payload := make([]workspacePayload, 0, len(workspaces))
	for _, workspace := range workspaces {
		payload = append(payload, toWorkspacePayload(workspace))
	}

	return response.OK(c, "ok", payload)
}

// Current returns the active workspace.
func (h *Handler) Current(c *fiber.Ctx) error {
	workspace, err := h.service.Current(c.UserContext(), scope(c))
	if err != nil {
		return h.fail(c, "current workspace", err)
	}
	workspace.Role = middleware.Role(c)

	return response.OK(c, "ok", toWorkspacePayload(workspace))
}

// Update changes the workspace profile. Owner only.
func (h *Handler) Update(c *fiber.Ctx) error {
	var req updateWorkspaceRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	// The window that was in force before the write, so a rename does not reschedule
	// every routine of the workspace. Only a moved clock does, which is E11.3.
	before, beforeErr := h.service.Current(c.UserContext(), scope(c))

	workspace, err := h.service.Update(c.UserContext(), scope(c), middleware.Role(c), domain.UpdateRequest{
		Name:           req.Name,
		BusinessField:  req.BusinessField,
		Timezone:       req.Timezone,
		Language:       req.Language,
		WorkHoursStart: req.WorkHoursStart,
		WorkHoursEnd:   req.WorkHoursEnd,
		WorkDays:       req.WorkDays,
	})
	if err != nil {
		return h.fail(c, "update workspace", err)
	}
	workspace.Role = middleware.Role(c)

	if beforeErr == nil && h.afterClockChange != nil && clockMoved(before, workspace) {
		h.afterClockChange(c.UserContext(), workspace.ID)
	}

	return response.OK(c, "workspace updated", toWorkspacePayload(workspace))
}

// clockMoved reports whether an update changed what a scheduled time means.
//
// Only these fields move a routine: the zone the wall-clock times are read in, and
// the window itself. A rename must not touch the engine at all — re-registering every
// schedule of a workspace because somebody fixed a typo is work nobody asked for.
func clockMoved(before, after domain.Workspace) bool {
	if before.Timezone != after.Timezone {
		return true
	}
	if before.WorkHoursStart != after.WorkHoursStart || before.WorkHoursEnd != after.WorkHoursEnd {
		return true
	}
	if len(before.WorkDays) != len(after.WorkDays) {
		return true
	}
	for i := range before.WorkDays {
		if before.WorkDays[i] != after.WorkDays[i] {
			return true
		}
	}
	return false
}

// Teams lists the teams of the active workspace.
func (h *Handler) Teams(c *fiber.Ctx) error {
	teams, err := h.service.Teams(c.UserContext(), scope(c))
	if err != nil {
		return h.fail(c, "list teams", err)
	}

	payload := make([]teamPayload, 0, len(teams))
	for _, team := range teams {
		payload = append(payload, teamPayload{ID: team.ID.String(), Name: team.Name, Kind: team.Kind})
	}

	return response.OK(c, "ok", payload)
}

// Members lists the members of the active workspace.
func (h *Handler) Members(c *fiber.Ctx) error {
	members, err := h.service.Members(c.UserContext(), scope(c))
	if err != nil {
		return h.fail(c, "list members", err)
	}

	payload := make([]memberPayload, 0, len(members))
	for _, member := range members {
		payload = append(payload, memberPayload{
			UserID:        member.UserID.String(),
			Email:         member.Email,
			Role:          member.Role,
			EmailVerified: member.EmailVerified,
			JoinedAt:      member.CreatedAt.UTC().Format(time.RFC3339),
		})
	}

	return response.OK(c, "ok", payload)
}

// SetRole changes a member's role.
func (h *Handler) SetRole(c *fiber.Ctx) error {
	userID, err := uuid.Parse(c.Params("userID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid user id", "invalid_request", nil)
	}

	var req setRoleRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	member, err := h.service.SetRole(c.UserContext(), scope(c), middleware.Role(c), userID, req.Role)
	if err != nil {
		return h.fail(c, "set role", err)
	}

	return response.OK(c, "role updated", memberPayload{
		UserID:        member.UserID.String(),
		Email:         member.Email,
		Role:          member.Role,
		EmailVerified: member.EmailVerified,
		JoinedAt:      member.CreatedAt.UTC().Format(time.RFC3339),
	})
}

// RemoveMember removes a member from the active workspace.
func (h *Handler) RemoveMember(c *fiber.Ctx) error {
	userID, err := uuid.Parse(c.Params("userID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid user id", "invalid_request", nil)
	}

	err = h.service.RemoveMember(c.UserContext(), scope(c), middleware.Role(c), middleware.CurrentUserID(c), userID)
	if err != nil {
		return h.fail(c, "remove member", err)
	}

	return response.OK(c, "member removed", nil)
}

// Invitations lists the pending invitations.
func (h *Handler) Invitations(c *fiber.Ctx) error {
	invitations, err := h.service.Invitations(c.UserContext(), scope(c))
	if err != nil {
		return h.fail(c, "list invitations", err)
	}

	payload := make([]invitationPayload, 0, len(invitations))
	for _, invitation := range invitations {
		payload = append(payload, invitationPayload{
			ID:        invitation.ID.String(),
			Email:     invitation.Email,
			Role:      invitation.Role,
			ExpiresAt: invitation.ExpiresAt.UTC().Format(time.RFC3339),
			InvitedAt: invitation.CreatedAt.UTC().Format(time.RFC3339),
		})
	}

	return response.OK(c, "ok", payload)
}

// Invite sends an invitation to join the active workspace.
func (h *Handler) Invite(c *fiber.Ctx) error {
	var req inviteRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	invitation, err := h.service.Invite(c.UserContext(), scope(c), middleware.Role(c),
		middleware.CurrentUserID(c), req.Email, req.Role)
	if err != nil {
		return h.fail(c, "invite", err)
	}

	return response.Created(c, "invitation sent", invitationPayload{
		ID:          invitation.ID.String(),
		Email:       invitation.Email,
		Role:        invitation.Role,
		ExpiresAt:   invitation.ExpiresAt.UTC().Format(time.RFC3339),
		InvitedAt:   invitation.CreatedAt.UTC().Format(time.RFC3339),
		WorkspaceID: invitation.WorkspaceID.String(),
	})
}

// RevokeInvitation deletes a pending invitation.
func (h *Handler) RevokeInvitation(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("invitationID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid invitation id", "invalid_request", nil)
	}

	if err := h.service.RevokeInvitation(c.UserContext(), scope(c), middleware.Role(c), id); err != nil {
		return h.fail(c, "revoke invitation", err)
	}

	return response.OK(c, "invitation revoked", nil)
}

// AcceptInvitation joins the caller to the workspace they were invited to.
func (h *Handler) AcceptInvitation(c *fiber.Ctx) error {
	var req acceptInvitationRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body", "invalid_request", nil)
	}

	workspace, err := h.service.AcceptInvitation(c.UserContext(), req.Token,
		middleware.CurrentUserID(c), middleware.CurrentEmail(c))
	if err != nil {
		return h.fail(c, "accept invitation", err)
	}

	return response.OK(c, "invitation accepted", toWorkspacePayload(workspace))
}

// UserRoutes registers the authenticated endpoints that do not need an active
// workspace and do not create tenancy.
func UserRoutes(group fiber.Router, h *Handler) {
	group.Get("/workspaces", h.List)
}

// VerifiedRoutes registers the endpoints that create or join tenancy. They sit
// behind the verified-email middleware: a workspace must not be created for an
// address nobody has confirmed.
func VerifiedRoutes(group fiber.Router, h *Handler) {
	group.Post("/workspaces/onboard", h.Onboard)
	group.Post("/invitations/accept", h.AcceptInvitation)
}

// ManagerRoutes registers the member and invitation management endpoints. The
// container applies the tenant middleware and a managing-role check.
func ManagerRoutes(group fiber.Router, h *Handler) {
	group.Patch("/workspaces/current/members/:userID", h.SetRole)
	group.Delete("/workspaces/current/members/:userID", h.RemoveMember)
	group.Post("/workspaces/current/invitations", h.Invite)
	group.Delete("/workspaces/current/invitations/:invitationID", h.RevokeInvitation)
}

// OwnerRoutes registers the workspace settings endpoints, which only an owner
// may change.
func OwnerRoutes(group fiber.Router, h *Handler) {
	group.Patch("/workspaces/current", h.Update)
}

// TenantRoutes registers the endpoints that operate on the active workspace.
// The container applies the tenant middleware to this group.
func TenantRoutes(group fiber.Router, h *Handler) {
	group.Get("/workspaces/current", h.Current)
	group.Get("/workspaces/current/teams", h.Teams)
	group.Get("/workspaces/current/members", h.Members)
	group.Get("/workspaces/current/invitations", h.Invitations)
}

// scope builds the module's scope from the identity the tenant middleware
// stored, so the handler never touches the database package.
func scope(c *fiber.Ctx) domain.Scope {
	return domain.Scope{
		UserID:      middleware.CurrentUserID(c),
		WorkspaceID: middleware.WorkspaceUUID(c),
	}
}

// fail maps a domain error to a status code.
func (h *Handler) fail(c *fiber.Ctx, operation string, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotMember):
		return response.Error(c, fiber.StatusForbidden, "you are not a member of this workspace", "not_a_member", nil)
	case errors.Is(err, domain.ErrForbidden):
		return response.Error(c, fiber.StatusForbidden, "your role does not allow this action", "forbidden", nil)
	case errors.Is(err, domain.ErrLastOwner):
		return response.Error(c, fiber.StatusConflict, "a workspace must keep at least one owner", "last_owner", nil)
	case errors.Is(err, domain.ErrCannotRemoveYourself):
		return response.Error(c, fiber.StatusConflict, "you cannot remove your own membership", "cannot_remove_yourself", nil)
	case errors.Is(err, domain.ErrMemberNotFound):
		return response.Error(c, fiber.StatusNotFound, "member not found", "member_not_found", nil)
	case errors.Is(err, domain.ErrInvitationAlreadySent):
		return response.Error(c, fiber.StatusConflict, "an invitation for this address is already pending", "invitation_pending", nil)
	case errors.Is(err, domain.ErrInvitationInvalid):
		return response.Error(c, fiber.StatusBadRequest, "invitation is invalid, expired, or already accepted", "invalid_invitation", nil)
	case errors.Is(err, domain.ErrInvitationEmailMatch):
		return response.Error(c, fiber.StatusForbidden, "sign in with the invited email address to accept", "invitation_email_mismatch", nil)
	case errors.Is(err, domain.ErrInvalidInput):
		return response.Error(c, fiber.StatusBadRequest, err.Error(), "invalid_input", nil)
	case errors.Is(err, domain.ErrInvalidRole):
		return response.Error(c, fiber.StatusBadRequest, "unknown role", "invalid_role", nil)
	case errors.Is(err, domain.ErrPlanLimit):
		// 409 rather than 403: nothing is forbidden about who is asking, and the answer is
		// to buy a bigger package rather than to ask somebody else. The message names the
		// package's own number, which the service put in it.
		return response.Error(c, fiber.StatusConflict,
			strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(err.Error(), domain.ErrPlanLimit.Error()), ":")),
			"plan_limit_reached", nil)
	case errors.Is(err, domain.ErrWorkspaceNotFound):
		return response.Error(c, fiber.StatusNotFound, "workspace not found", "workspace_not_found", nil)
	default:
		h.log.Error("workspace request failed", zap.String("operation", operation), zap.Error(err))
		return response.Error(c, fiber.StatusInternalServerError, "internal server error", "internal_error", nil)
	}
}

func toWorkspacePayload(workspace domain.Workspace) workspacePayload {
	return workspacePayload{
		ID:             workspace.ID.String(),
		Name:           workspace.Name,
		BusinessField:  workspace.BusinessField,
		Timezone:       workspace.Timezone,
		Language:       workspace.Language,
		Role:           workspace.Role,
		WorkHoursStart: workspace.WorkHoursStart,
		WorkHoursEnd:   workspace.WorkHoursEnd,
		WorkDays:       workspace.WorkDays,
		CreatedAt:      workspace.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func toOnboardPayload(result domain.OnboardResult) onboardPayload {
	teams := make([]teamPayload, 0, len(result.Teams))
	for _, team := range result.Teams {
		teams = append(teams, teamPayload{ID: team.ID.String(), Name: team.Name, Kind: team.Kind})
	}

	return onboardPayload{
		Workspace: toWorkspacePayload(result.Workspace),
		Teams:     teams,
		Created:   result.Created,
	}
}
