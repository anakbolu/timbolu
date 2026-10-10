// Package repository implements the workspace data access with pgx and the
// sqlc-generated queries. Every method runs inside the caller's scope, so Row
// Level Security filters the statements even if a query forgets a filter.
package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/repository/sqlcgen"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
)

const uniqueViolation = "23505"

// Default teams created with every workspace.
const (
	teamBolu = "Tim Bolu"
	teamHore = "Tim Hore"
)

// TemplateProvisioner copies the Bolu templates into a new workspace, inside the
// onboarding transaction. It is declared here rather than in the domain because
// its signature carries a transaction handle, which is an infrastructure type;
// the container hands over the agent module's repository.
type TemplateProvisioner interface {
	Provision(ctx context.Context, tx pgx.Tx, workspaceID, teamID uuid.UUID) error
}

// PolicyProvisioner gives a new workspace the conservative approval policies it
// starts with, inside the onboarding transaction.
//
// It is declared here rather than in the domain for the same reason the template
// provisioner is: its signature carries a transaction handle, which is an
// infrastructure type, and the container hands over the approval module's
// repository so neither module imports the other.
type PolicyProvisioner interface {
	SeedPolicies(ctx context.Context, tx pgx.Tx, workspaceID uuid.UUID) error
}

// OnboardedMarker records that an account finished onboarding, inside the same
// transaction that creates its workspace.
//
// It is declared here for the same reason as the two above: the signature carries
// a transaction handle, so the container hands over the auth module's repository
// and neither module imports the other. Marking the account here rather than in a
// second call is what keeps a person from owning a workspace that their session
// does not believe in — which is the shape that sends them back to the onboarding
// screen after every sign-in.
type OnboardedMarker interface {
	MarkOnboardedInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error
}

// WorkingHours is the window a workspace lets actions leave in.
type WorkingHours struct {
	Start    string
	End      string
	Timezone string
}

// Repository is the Postgres-backed implementation of domain.Repository.
type Repository struct {
	pool *database.Pool
	// provisioner copies the Bolu templates inside the onboarding transaction.
	// It is the agent module's repository, handed over by the container so this
	// module never imports it.
	provisioner TemplateProvisioner
	// policies seeds the approval policies inside the same transaction, so a
	// workspace never exists without one for every action that reaches outside it.
	policies PolicyProvisioner
	// onboarded marks the account finished, inside the same transaction, so owning
	// a workspace and the session believing it are one fact rather than two.
	onboarded OnboardedMarker
}

// SetPolicyProvisioner installs the approval repository as the second
// provisioner. The container calls it after both are built.
func (r *Repository) SetPolicyProvisioner(provisioner PolicyProvisioner) {
	if r != nil {
		r.policies = provisioner
	}
}

// SetOnboardedMarker installs the auth repository as the onboarding marker. The
// container calls it after both are built.
func (r *Repository) SetOnboardedMarker(marker OnboardedMarker) {
	if r != nil {
		r.onboarded = marker
	}
}

// New builds the repository. A nil provisioner leaves a new workspace without
// its Bolu, which is what a deployment without the agent module looks like.
func New(pool *database.Pool, provisioner TemplateProvisioner) *Repository {
	return &Repository{pool: pool, provisioner: provisioner}
}

// Onboard creates the workspace, its owner membership and the two default teams
// in one transaction. It is idempotent: a caller who already owns a workspace
// gets that workspace back instead of a second one.
func (r *Repository) Onboard(ctx context.Context, userID uuid.UUID, req domain.OnboardRequest) (domain.OnboardResult, error) {
	if r == nil || r.pool == nil {
		return domain.OnboardResult{}, fmt.Errorf("workspace: database is not configured")
	}

	scope := database.Scope{UserID: userID}
	var result domain.OnboardResult

	err := r.pool.InScope(ctx, scope, func(tx pgx.Tx) error {
		queries := sqlcgen.New(tx)

		existing, err := queries.GetOwnedWorkspaceForUser(ctx, &userID)
		switch {
		case err == nil:
			// Already onboarded: return the workspace and its teams unchanged.
			if err := database.SetWorkspace(ctx, tx, existing.ID); err != nil {
				return err
			}
			// And record it if it never was. An account that onboarded before the
			// marker was wired owns a workspace its session does not believe in,
			// so it is healed on the next attempt rather than staying stuck.
			if err := r.markOnboarded(ctx, tx, userID); err != nil {
				return err
			}
			teams, err := queries.ListTeams(ctx, existing.ID)
			if err != nil {
				return fmt.Errorf("workspace: list teams: %w", err)
			}
			result = domain.OnboardResult{
				Workspace: toWorkspace(existing, domain.RoleOwner),
				Teams:     toTeams(teams),
				Created:   false,
			}
			return nil
		case errors.Is(err, pgx.ErrNoRows):
			// No workspace yet: fall through and create one.
		default:
			return fmt.Errorf("workspace: look up owned workspace: %w", err)
		}

		created, err := queries.CreateWorkspace(ctx, sqlcgen.CreateWorkspaceParams{
			Name:          req.Name,
			BusinessField: req.BusinessField,
			Timezone:      req.Timezone,
			OwnerUserID:   &userID,
		})
		if err != nil {
			return fmt.Errorf("workspace: create workspace: %w", err)
		}

		// The rows that follow belong to the new workspace, so the tenant
		// context is activated before they are written.
		if err := database.SetWorkspace(ctx, tx, created.ID); err != nil {
			return err
		}

		if _, err := queries.CreateMember(ctx, sqlcgen.CreateMemberParams{
			WorkspaceID: created.ID,
			UserID:      userID,
			Role:        domain.RoleOwner,
		}); err != nil {
			return fmt.Errorf("workspace: create owner membership: %w", err)
		}

		teams := make([]sqlcgen.Team, 0, 2)
		var boluTeamID uuid.UUID
		for _, team := range []struct {
			name string
			kind string
		}{
			{teamBolu, "bolu"},
			{teamHore, "hore"},
		} {
			createdTeam, err := queries.CreateTeam(ctx, sqlcgen.CreateTeamParams{
				WorkspaceID: created.ID,
				Name:        team.name,
				Kind:        team.kind,
			})
			if err != nil {
				return fmt.Errorf("workspace: create team %s: %w", team.name, err)
			}
			if team.kind == "bolu" {
				boluTeamID = createdTeam.ID
			}
			teams = append(teams, createdTeam)
		}

		// The Bolu templates are copied inside this transaction, so a workspace
		// never exists without its team.
		if r.provisioner != nil {
			if err := r.provisioner.Provision(ctx, tx, created.ID, boluTeamID); err != nil {
				return err
			}
		}

		// And the conservative approval policies, so no action kind reaches the
		// outside world without a row the workspace can see and change.
		if r.policies != nil {
			if err := r.policies.SeedPolicies(ctx, tx, created.ID); err != nil {
				return err
			}
		}

		if err := r.markOnboarded(ctx, tx, userID); err != nil {
			return err
		}

		result = domain.OnboardResult{
			Workspace: toWorkspace(created, domain.RoleOwner),
			Teams:     toTeams(teams),
			Created:   true,
		}
		return nil
	})
	if err != nil {
		return domain.OnboardResult{}, err
	}

	return result, nil
}

// markOnboarded records the account inside the onboarding transaction. A
// deployment without the auth module simply has nothing to mark.
func (r *Repository) markOnboarded(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	if r.onboarded == nil {
		return nil
	}
	if err := r.onboarded.MarkOnboardedInTx(ctx, tx, userID); err != nil {
		return fmt.Errorf("workspace: mark onboarded: %w", err)
	}
	return nil
}

// WorkingHours reads the window a workspace lets actions leave in.
//
// It runs unscoped by workspace id rather than through the tenant helpers: the
// caller is the approval runtime, which already knows the workspace and needs
// three columns of it, and the row is the workspace's own settings rather than
// tenant data belonging to somebody else.
func (r *Repository) WorkingHours(ctx context.Context, workspaceID uuid.UUID) (WorkingHours, error) {
	if r == nil || r.pool == nil {
		return WorkingHours{}, fmt.Errorf("workspace: database is not configured")
	}

	var window WorkingHours
	err := r.pool.InScopeRead(ctx, database.Scope{UserID: uuid.New(), WorkspaceID: workspaceID}, func(tx pgx.Tx) error {
		row, err := sqlcgen.New(tx).GetWorkspaceHours(ctx, workspaceID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrWorkspaceNotFound
			}
			return fmt.Errorf("workspace: read hours: %w", err)
		}

		window = WorkingHours{
			Start:    formatClock(row.WorkHoursStart),
			End:      formatClock(row.WorkHoursEnd),
			Timezone: row.Timezone,
		}
		return nil
	})
	if err != nil {
		return WorkingHours{}, err
	}

	return window, nil
}

// formatClock renders a time-of-day as the "HH:MM" the approval rule parses.
//
// A `time` column arrives as pgtype.Time, which carries microseconds since
// midnight rather than a clock, so it is read as the number it is.
func formatClock(value pgtype.Time) string {
	if !value.Valid {
		return ""
	}

	micros := value.Microseconds
	return fmt.Sprintf("%02d:%02d", micros/3_600_000_000, (micros/60_000_000)%60)
}

// Workspaces lists the workspaces the user belongs to, with their role.
func (r *Repository) Workspaces(ctx context.Context, userID uuid.UUID) ([]domain.Workspace, error) {
	if r == nil || r.pool == nil {
		return nil, fmt.Errorf("workspace: database is not configured")
	}

	var workspaces []domain.Workspace
	err := r.pool.InScopeRead(ctx, database.Scope{UserID: userID}, func(tx pgx.Tx) error {
		rows, err := sqlcgen.New(tx).ListWorkspacesForUser(ctx, userID)
		if err != nil {
			return fmt.Errorf("workspace: list workspaces: %w", err)
		}
		workspaces = make([]domain.Workspace, 0, len(rows))
		for _, row := range rows {
			workspaces = append(workspaces, domain.Workspace{
				ID:            row.ID,
				Name:          row.Name,
				BusinessField: row.BusinessField,
				Timezone:      row.Timezone,
				Language:      row.Language,
				Role:          row.Role,
				CreatedAt:     row.CreatedAt,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return workspaces, nil
}

// Workspace reads one workspace inside the caller's scope.
func (r *Repository) Workspace(ctx context.Context, scope domain.Scope) (domain.Workspace, error) {
	var workspace domain.Workspace

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetWorkspace(ctx, scope.WorkspaceID)
		if err != nil {
			return mapNotFound(err, "get workspace")
		}
		workspace = toWorkspace(row, "")
		return nil
	})
	if err != nil {
		return domain.Workspace{}, err
	}

	return workspace, nil
}

// UpdateWorkspace changes the workspace profile.
func (r *Repository) UpdateWorkspace(ctx context.Context, scope domain.Scope, req domain.UpdateRequest) (domain.Workspace, error) {
	var workspace domain.Workspace

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.UpdateWorkspace(ctx, sqlcgen.UpdateWorkspaceParams{
			ID:            scope.WorkspaceID,
			Name:          req.Name,
			BusinessField: req.BusinessField,
			Timezone:      req.Timezone,
		})
		if err != nil {
			return mapNotFound(err, "update workspace")
		}
		workspace = toWorkspace(row, "")
		return nil
	})
	if err != nil {
		return domain.Workspace{}, err
	}

	return workspace, nil
}

// Teams lists the teams of the active workspace.
func (r *Repository) Teams(ctx context.Context, scope domain.Scope) ([]domain.Team, error) {
	var teams []domain.Team

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListTeams(ctx, scope.WorkspaceID)
		if err != nil {
			return fmt.Errorf("workspace: list teams: %w", err)
		}
		teams = toTeams(rows)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return teams, nil
}

// Membership returns the caller's role in the workspace, or ErrNotMember.
func (r *Repository) Membership(ctx context.Context, scope domain.Scope, userID uuid.UUID) (string, error) {
	var role string

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		member, err := queries.GetMembership(ctx, sqlcgen.GetMembershipParams{
			WorkspaceID: scope.WorkspaceID,
			UserID:      userID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrNotMember
			}
			return fmt.Errorf("workspace: get membership: %w", err)
		}
		role = member.Role
		return nil
	})
	if err != nil {
		return "", err
	}

	return role, nil
}

// Members lists the members of the active workspace.
func (r *Repository) Members(ctx context.Context, scope domain.Scope) ([]domain.Member, error) {
	var members []domain.Member

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListMembers(ctx, scope.WorkspaceID)
		if err != nil {
			return fmt.Errorf("workspace: list members: %w", err)
		}
		members = make([]domain.Member, 0, len(rows))
		for _, row := range rows {
			members = append(members, domain.Member{
				UserID:        row.UserID,
				Email:         row.Email,
				Role:          row.Role,
				EmailVerified: row.EmailVerifiedAt != nil,
				CreatedAt:     row.CreatedAt,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return members, nil
}

// SetRole changes a member's role, refusing to demote the last owner.
func (r *Repository) SetRole(ctx context.Context, scope domain.Scope, userID uuid.UUID, role string) (domain.Member, error) {
	var member domain.Member

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		current, err := queries.GetMembership(ctx, sqlcgen.GetMembershipParams{
			WorkspaceID: scope.WorkspaceID,
			UserID:      userID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrMemberNotFound
			}
			return fmt.Errorf("workspace: get membership: %w", err)
		}

		if current.Role == domain.RoleOwner && role != domain.RoleOwner {
			owners, err := queries.CountOwners(ctx, scope.WorkspaceID)
			if err != nil {
				return fmt.Errorf("workspace: count owners: %w", err)
			}
			if owners <= 1 {
				return domain.ErrLastOwner
			}
		}

		if _, err := queries.UpdateMemberRole(ctx, sqlcgen.UpdateMemberRoleParams{
			WorkspaceID: scope.WorkspaceID,
			UserID:      userID,
			Role:        role,
		}); err != nil {
			return mapNotFound(err, "update member role")
		}

		detail, err := queries.GetMemberDetail(ctx, sqlcgen.GetMemberDetailParams{
			WorkspaceID: scope.WorkspaceID,
			UserID:      userID,
		})
		if err != nil {
			return mapNotFound(err, "load member detail")
		}

		member = domain.Member{
			UserID:        detail.UserID,
			Email:         detail.Email,
			Role:          detail.Role,
			EmailVerified: detail.EmailVerifiedAt != nil,
			CreatedAt:     detail.CreatedAt,
		}
		return nil
	})
	if err != nil {
		return domain.Member{}, err
	}

	return member, nil
}

// RemoveMember deletes a membership, refusing to remove the last owner.
func (r *Repository) RemoveMember(ctx context.Context, scope domain.Scope, userID uuid.UUID) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		current, err := queries.GetMembership(ctx, sqlcgen.GetMembershipParams{
			WorkspaceID: scope.WorkspaceID,
			UserID:      userID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrMemberNotFound
			}
			return fmt.Errorf("workspace: get membership: %w", err)
		}

		if current.Role == domain.RoleOwner {
			owners, err := queries.CountOwners(ctx, scope.WorkspaceID)
			if err != nil {
				return fmt.Errorf("workspace: count owners: %w", err)
			}
			if owners <= 1 {
				return domain.ErrLastOwner
			}
		}

		removed, err := queries.DeleteMember(ctx, sqlcgen.DeleteMemberParams{
			WorkspaceID: scope.WorkspaceID,
			UserID:      userID,
		})
		if err != nil {
			return fmt.Errorf("workspace: remove member: %w", err)
		}
		if removed == 0 {
			return domain.ErrMemberNotFound
		}
		return nil
	})
}

// CreateInvitation stores a pending invitation.
func (r *Repository) CreateInvitation(ctx context.Context, scope domain.Scope, invitation domain.NewInvitation) (domain.Invitation, error) {
	var created domain.Invitation

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.CreateInvitation(ctx, sqlcgen.CreateInvitationParams{
			WorkspaceID: scope.WorkspaceID,
			Email:       invitation.Email,
			Role:        invitation.Role,
			TokenHash:   invitation.TokenHash,
			InvitedBy:   &invitation.InvitedBy,
			ExpiresAt:   invitation.ExpiresAt,
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
				return domain.ErrInvitationAlreadySent
			}
			return fmt.Errorf("workspace: create invitation: %w", err)
		}
		created = toInvitation(row, "")
		return nil
	})
	if err != nil {
		return domain.Invitation{}, err
	}

	return created, nil
}

// Invitations lists the pending invitations of the active workspace.
func (r *Repository) Invitations(ctx context.Context, scope domain.Scope) ([]domain.Invitation, error) {
	var invitations []domain.Invitation

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListInvitations(ctx, scope.WorkspaceID)
		if err != nil {
			return fmt.Errorf("workspace: list invitations: %w", err)
		}
		invitations = make([]domain.Invitation, 0, len(rows))
		for _, row := range rows {
			invitations = append(invitations, toInvitation(row, ""))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return invitations, nil
}

// RevokeInvitation deletes a pending invitation.
func (r *Repository) RevokeInvitation(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		removed, err := queries.DeleteInvitation(ctx, sqlcgen.DeleteInvitationParams{
			WorkspaceID: scope.WorkspaceID,
			ID:          id,
		})
		if err != nil {
			return fmt.Errorf("workspace: revoke invitation: %w", err)
		}
		if removed == 0 {
			return domain.ErrInvitationInvalid
		}
		return nil
	})
}

// AcceptInvitation consumes the invitation and creates the membership in one
// transaction.
func (r *Repository) AcceptInvitation(ctx context.Context, tokenHash string, userID uuid.UUID, email string) (domain.Workspace, error) {
	if r == nil || r.pool == nil {
		return domain.Workspace{}, fmt.Errorf("workspace: database is not configured")
	}

	var workspace domain.Workspace

	err := r.pool.InScope(ctx, database.Scope{UserID: userID}, func(tx pgx.Tx) error {
		queries := sqlcgen.New(tx)

		invitation, err := queries.GetInvitationByTokenHash(ctx, tokenHash)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrInvitationInvalid
			}
			return fmt.Errorf("workspace: get invitation: %w", err)
		}

		if invitation.AcceptedAt != nil || invitation.ExpiresAt.Before(time.Now()) {
			return domain.ErrInvitationInvalid
		}
		if !strings.EqualFold(invitation.Email, email) {
			return domain.ErrInvitationEmailMatch
		}

		if err := database.SetWorkspace(ctx, tx, invitation.WorkspaceID); err != nil {
			return err
		}

		// Already a member: accepting again is not an error, the invitation is
		// simply consumed.
		if _, err := queries.GetMembership(ctx, sqlcgen.GetMembershipParams{
			WorkspaceID: invitation.WorkspaceID,
			UserID:      userID,
		}); err == nil {
			if _, err := queries.AcceptInvitation(ctx, sqlcgen.AcceptInvitationParams{
				ID:         invitation.ID,
				AcceptedBy: &userID,
			}); err != nil {
				return fmt.Errorf("workspace: accept invitation: %w", err)
			}
			row, err := queries.GetWorkspace(ctx, invitation.WorkspaceID)
			if err != nil {
				return mapNotFound(err, "get workspace")
			}
			workspace = toWorkspace(row, invitation.Role)
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("workspace: get membership: %w", err)
		}

		if _, err := queries.CreateMember(ctx, sqlcgen.CreateMemberParams{
			WorkspaceID: invitation.WorkspaceID,
			UserID:      userID,
			Role:        invitation.Role,
		}); err != nil {
			return fmt.Errorf("workspace: create member: %w", err)
		}

		if _, err := queries.AcceptInvitation(ctx, sqlcgen.AcceptInvitationParams{
			ID:         invitation.ID,
			AcceptedBy: &userID,
		}); err != nil {
			return mapNotFound(err, "accept invitation")
		}

		row, err := queries.GetWorkspace(ctx, invitation.WorkspaceID)
		if err != nil {
			return mapNotFound(err, "get workspace")
		}
		workspace = toWorkspace(row, invitation.Role)
		return nil
	})
	if err != nil {
		return domain.Workspace{}, err
	}

	return workspace, nil
}

// read runs fn in a read-only transaction scoped to the caller.
func (r *Repository) read(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("workspace: database is not configured")
	}
	return r.pool.InScopeRead(ctx, toDatabaseScope(scope), func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

// write runs fn in a transaction scoped to the caller.
func (r *Repository) write(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("workspace: database is not configured")
	}
	return r.pool.InScope(ctx, toDatabaseScope(scope), func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

// toDatabaseScope translates the domain scope into the one the scoped helpers
// expect, so the service never imports the database package.
func toDatabaseScope(scope domain.Scope) database.Scope {
	return database.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}
}

func toWorkspace(row sqlcgen.Workspace, role string) domain.Workspace {
	return domain.Workspace{
		ID:            row.ID,
		Name:          row.Name,
		BusinessField: row.BusinessField,
		Timezone:      row.Timezone,
		Language:      row.Language,
		Role:          role,
		CreatedAt:     row.CreatedAt,
	}
}

func toTeams(rows []sqlcgen.Team) []domain.Team {
	teams := make([]domain.Team, 0, len(rows))
	for _, row := range rows {
		teams = append(teams, domain.Team{ID: row.ID, Name: row.Name, Kind: row.Kind})
	}
	return teams
}

func toInvitation(row sqlcgen.Invitation, workspaceName string) domain.Invitation {
	return domain.Invitation{
		ID:            row.ID,
		WorkspaceID:   row.WorkspaceID,
		WorkspaceName: workspaceName,
		Email:         row.Email,
		Role:          row.Role,
		ExpiresAt:     row.ExpiresAt,
		CreatedAt:     row.CreatedAt,
	}
}

func mapNotFound(err error, operation string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrWorkspaceNotFound
	}
	return fmt.Errorf("workspace: %s: %w", operation, err)
}

var _ domain.Repository = (*Repository)(nil)
