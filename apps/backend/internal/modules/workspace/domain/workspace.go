// Package domain defines the workspace and membership contracts: entities,
// roles, and the repository and service interfaces. It imports only the
// standard library and github.com/google/uuid.
package domain

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Roles inside a workspace.
const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// Errors returned by the service.
var (
	ErrNotMember            = errors.New("workspace: you are not a member of this workspace")
	ErrForbidden            = errors.New("workspace: your role does not allow this action")
	ErrLastOwner            = errors.New("workspace: a workspace must keep at least one owner")
	ErrAlreadyMember        = errors.New("workspace: this user is already a member")
	ErrInvitationInvalid    = errors.New("workspace: invitation is invalid, expired, or already accepted")
	ErrInvitationEmailMatch = errors.New("workspace: sign in with the invited email address to accept")
	ErrWorkspaceNotFound    = errors.New("workspace: workspace not found")
	// ErrInvalidInput means the request itself is wrong (a blank name, an
	// unknown timezone, a malformed address). Handlers answer 400 for it.
	ErrInvalidInput   = errors.New("workspace: invalid input")
	ErrInvalidRole    = errors.New("workspace: unknown role")
	ErrMemberNotFound = errors.New("workspace: member not found")
	// ErrPlanLimit means the workspace's package does not allow another member. It
	// is a refusal to buy out of rather than one to retry.
	ErrPlanLimit             = errors.New("workspace: paket ini tidak mengizinkan anggota baru")
	ErrCannotRemoveYourself  = errors.New("workspace: you cannot remove your own membership")
	ErrInvitationAlreadySent = errors.New("workspace: an invitation for this address is already pending")
)

// Workspace is a tenant. Role is the caller's role in it.
type Workspace struct {
	ID            uuid.UUID
	Name          string
	BusinessField string
	Timezone      string
	Language      string
	// WorkHoursStart and WorkHoursEnd are "HH:MM" in the workspace's own
	// timezone. The approval gate holds an action outside them.
	WorkHoursStart string
	WorkHoursEnd   string
	// WorkDays is the ISO weekdays the window applies to (1 = Monday). A window
	// that opens on a day the business is closed is not a window.
	WorkDays  []int
	Role      string
	CreatedAt time.Time
}

// Team groups the Bolu of one workspace. A new workspace always gets Tim Bolu.
type Team struct {
	ID   uuid.UUID
	Name string
	Kind string
}

// Member is a workspace member with the account's address.
type Member struct {
	UserID        uuid.UUID
	Email         string
	Role          string
	EmailVerified bool
	CreatedAt     time.Time
}

// Invitation is a pending invitation to join a workspace.
type Invitation struct {
	ID            uuid.UUID
	WorkspaceID   uuid.UUID
	WorkspaceName string
	Email         string
	Role          string
	ExpiresAt     time.Time
	CreatedAt     time.Time
}

// OnboardRequest is the onboarding form.
type OnboardRequest struct {
	Name          string
	BusinessField string
	Timezone      string
}

// OnboardResult is the outcome of onboarding. Created is false when the caller
// already owned a workspace, which makes a retried submit harmless.
type OnboardResult struct {
	Workspace Workspace
	Teams     []Team
	Created   bool
}

// UpdateRequest changes the workspace profile.
//
// It carries every field the Pengaturan screen edits, not only the ones a form
// happened to submit: the screen saves a field at a time, and a partial update
// that dropped the working hours would change when the approval gate lets an
// action leave.
type UpdateRequest struct {
	Name           string
	BusinessField  string
	Timezone       string
	Language       string
	WorkHoursStart string
	WorkHoursEnd   string
	WorkDays       []int
}

// Repository is the persistence contract. Methods take the caller's scope so
// Row Level Security applies to every statement.
//
// Multi-step operations (onboarding, accepting an invitation) are single
// repository methods on purpose: they must run in one transaction, and the
// service layer is not allowed to depend on a driver.
type Repository interface {
	// Onboard creates the workspace, the owner membership and the default team
	// in one transaction. It returns the existing workspace instead of
	// creating a second one when the user already owns one.
	Onboard(ctx context.Context, userID uuid.UUID, req OnboardRequest) (OnboardResult, error)
	// Workspaces lists the workspaces the user belongs to, with their role.
	Workspaces(ctx context.Context, userID uuid.UUID) ([]Workspace, error)
	// Workspace reads one workspace inside the caller's scope.
	Workspace(ctx context.Context, scope Scope) (Workspace, error)
	// UpdateWorkspace changes the workspace profile.
	UpdateWorkspace(ctx context.Context, scope Scope, req UpdateRequest) (Workspace, error)
	// Teams lists the teams of the active workspace.
	Teams(ctx context.Context, scope Scope) ([]Team, error)
	// Membership returns the caller's role, or ErrNotMember.
	Membership(ctx context.Context, scope Scope, userID uuid.UUID) (string, error)
	// Members lists the members of the active workspace.
	Members(ctx context.Context, scope Scope) ([]Member, error)
	// SetRole changes a member's role. It refuses to demote the last owner.
	SetRole(ctx context.Context, scope Scope, userID uuid.UUID, role string) (Member, error)
	// RemoveMember deletes a membership. It refuses to remove the last owner.
	RemoveMember(ctx context.Context, scope Scope, userID uuid.UUID) error

	// CreateInvitation stores a pending invitation.
	CreateInvitation(ctx context.Context, scope Scope, invitation NewInvitation) (Invitation, error)
	// Invitations lists the pending invitations of the active workspace.
	Invitations(ctx context.Context, scope Scope) ([]Invitation, error)
	// RevokeInvitation deletes a pending invitation.
	RevokeInvitation(ctx context.Context, scope Scope, id uuid.UUID) error
	// AcceptInvitation consumes the invitation and creates the membership in
	// one transaction.
	AcceptInvitation(ctx context.Context, tokenHash string, userID uuid.UUID, email string) (Workspace, error)
}

// NewInvitation is an invitation to store.
type NewInvitation struct {
	Email     string
	Role      string
	TokenHash string
	InvitedBy uuid.UUID
	ExpiresAt time.Time
}

// Service is the workspace use case contract.
type Service interface {
	// Onboard is idempotent: submitting the form twice returns the same
	// workspace instead of creating a second one.
	Onboard(ctx context.Context, userID uuid.UUID, req OnboardRequest) (OnboardResult, error)
	Workspaces(ctx context.Context, userID uuid.UUID) ([]Workspace, error)
	Current(ctx context.Context, scope Scope) (Workspace, error)
	Update(ctx context.Context, scope Scope, actorRole string, req UpdateRequest) (Workspace, error)
	Teams(ctx context.Context, scope Scope) ([]Team, error)
	Members(ctx context.Context, scope Scope) ([]Member, error)
	SetRole(ctx context.Context, scope Scope, actorRole string, userID uuid.UUID, role string) (Member, error)
	RemoveMember(ctx context.Context, scope Scope, actorRole string, actorID, userID uuid.UUID) error
	Invite(ctx context.Context, scope Scope, actorRole string, actorID uuid.UUID, email, role string) (Invitation, error)
	Invitations(ctx context.Context, scope Scope) ([]Invitation, error)
	RevokeInvitation(ctx context.Context, scope Scope, actorRole string, id uuid.UUID) error
	AcceptInvitation(ctx context.Context, token string, userID uuid.UUID, email string) (Workspace, error)
	// Resolve returns the caller's role in a workspace, which is what the
	// tenant middleware uses to authorise a request.
	Resolve(ctx context.Context, workspaceID, userID uuid.UUID) (string, error)
}

// IsValidRole reports whether role is one of the three workspace roles.
func IsValidRole(role string) bool {
	switch role {
	case RoleOwner, RoleAdmin, RoleMember:
		return true
	default:
		return false
	}
}

// CanManageMembers reports whether a role may invite, re-role, or remove
// members. A plain member cannot.
func CanManageMembers(role string) bool {
	return role == RoleOwner || role == RoleAdmin
}

// CanAdminister reports whether a role may change workspace settings and
// manage owners.
func CanAdminister(role string) bool {
	return role == RoleOwner
}
