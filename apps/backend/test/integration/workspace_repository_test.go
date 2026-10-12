package integration

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	authrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/repository"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	workspacerepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/repository"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
)

// workspaceFixture is one onboarded user with their workspace.
type workspaceFixture struct {
	repo      *workspacerepo.Repository
	dsn       string
	userID    uuid.UUID
	workspace workspacedomain.Workspace
	teams     []workspacedomain.Team
	scope     workspacedomain.Scope
}

// newWorkspaceFixture creates a user and onboards them.
func newWorkspaceFixture(t *testing.T, dsn string) *workspaceFixture {
	t.Helper()

	pool, err := database.New(t.Context(), database.Config{URL: dsn, MaxOpenConns: 5, MaxIdleConns: 1})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	repo := workspacerepo.New(pool)

	user, err := authrepo.New(pool.PgxPool()).CreateUser(t.Context(), uuid.NewString()+"@example.com", "$argon2id$hash")
	require.NoError(t, err)

	result, err := repo.Onboard(t.Context(), user.ID, workspacedomain.OnboardRequest{
		Name:          "Toko Sinar",
		BusinessField: "Retail",
		Timezone:      "Asia/Jakarta",
	})
	require.NoError(t, err)
	require.True(t, result.Created)

	return &workspaceFixture{
		repo:      repo,
		dsn:       dsn,
		userID:    user.ID,
		workspace: result.Workspace,
		teams:     result.Teams,
		scope:     workspacedomain.Scope{UserID: user.ID, WorkspaceID: result.Workspace.ID},
	}
}

// createUser registers an account directly, so a test can set up an invitee.
func createUser(t *testing.T, dsn, email string) uuid.UUID {
	t.Helper()

	user, err := authrepo.New(poolFor(t, dsn).PgxPool()).CreateUser(t.Context(), email, "$argon2id$hash")
	require.NoError(t, err)
	return user.ID
}

// poolFor opens a pool for a test and closes it afterwards.
func poolFor(t *testing.T, dsn string) *database.Pool {
	t.Helper()

	pool, err := database.New(t.Context(), database.Config{URL: dsn, MaxOpenConns: 5, MaxIdleConns: 1})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	return pool
}

// addMember invites an account and accepts the invitation, which is the real
// path a second member takes.
func addMember(t *testing.T, fixture *workspaceFixture, role string) uuid.UUID {
	t.Helper()

	ctx := t.Context()
	email := uuid.NewString() + "@example.com"
	userID := createUser(t, fixture.dsn, email)
	hash := "invite-hash-" + uuid.NewString()

	_, err := fixture.repo.CreateInvitation(ctx, fixture.scope, workspacedomain.NewInvitation{
		Email:     email,
		Role:      role,
		TokenHash: hash,
		InvitedBy: fixture.userID,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)

	_, err = fixture.repo.AcceptInvitation(ctx, hash, userID, email)
	require.NoError(t, err)

	return userID
}

func TestWorkspaceRepositoryOnboarding(t *testing.T) {
	ctx := t.Context()
	fixture := newWorkspaceFixture(t, appDatabase(t))

	t.Run("creates the workspace with an owner and two teams", func(t *testing.T) {
		require.Equal(t, "Toko Sinar", fixture.workspace.Name)
		require.Equal(t, workspacedomain.RoleOwner, fixture.workspace.Role)
		require.Len(t, fixture.teams, 2)

		names := map[string]string{}
		for _, team := range fixture.teams {
			names[team.Kind] = team.Name
		}
		require.Equal(t, "Tim Bolu", names["bolu"])
		require.Equal(t, "Tim Hore", names["hore"])
	})

	t.Run("is idempotent", func(t *testing.T) {
		again, err := fixture.repo.Onboard(ctx, fixture.userID, workspacedomain.OnboardRequest{
			Name:     "Toko Lain",
			Timezone: "Asia/Jakarta",
		})
		require.NoError(t, err)
		require.False(t, again.Created, "a repeated onboarding must not create a second workspace")
		require.Equal(t, fixture.workspace.ID, again.Workspace.ID)
		require.Equal(t, "Toko Sinar", again.Workspace.Name, "the existing workspace must be returned unchanged")
		require.Len(t, again.Teams, 2)
	})

	t.Run("lists the workspace with the caller's role", func(t *testing.T) {
		workspaces, err := fixture.repo.Workspaces(ctx, fixture.userID)
		require.NoError(t, err)
		require.Len(t, workspaces, 1)
		require.Equal(t, workspacedomain.RoleOwner, workspaces[0].Role)
	})

	t.Run("a stranger owns nothing", func(t *testing.T) {
		workspaces, err := fixture.repo.Workspaces(ctx, uuid.New())
		require.NoError(t, err)
		require.Empty(t, workspaces)
	})

	t.Run("membership resolves inside the tenant scope", func(t *testing.T) {
		role, err := fixture.repo.Membership(ctx, fixture.scope, fixture.userID)
		require.NoError(t, err)
		require.Equal(t, workspacedomain.RoleOwner, role)
	})

	// The membership check is the authorisation gate: it is what the tenant
	// middleware calls before it lets a request name a workspace.
	t.Run("a stranger has no membership", func(t *testing.T) {
		stranger := workspacedomain.Scope{UserID: uuid.New(), WorkspaceID: fixture.workspace.ID}

		role, err := fixture.repo.Membership(ctx, stranger, stranger.UserID)
		require.ErrorIs(t, err, workspacedomain.ErrNotMember)
		require.Empty(t, role)
	})

	t.Run("reads the workspace and its teams", func(t *testing.T) {
		workspace, err := fixture.repo.Workspace(ctx, fixture.scope)
		require.NoError(t, err)
		require.Equal(t, fixture.workspace.ID, workspace.ID)

		teams, err := fixture.repo.Teams(ctx, fixture.scope)
		require.NoError(t, err)
		require.Len(t, teams, 2)
	})

	t.Run("updates the profile", func(t *testing.T) {
		updated, err := fixture.repo.UpdateWorkspace(ctx, fixture.scope, workspacedomain.UpdateRequest{
			Name:          "Toko Sinar Jaya",
			BusinessField: "Retail dan Grosir",
			Timezone:      "Asia/Makassar",
		})
		require.NoError(t, err)
		require.Equal(t, "Toko Sinar Jaya", updated.Name)
		require.Equal(t, "Asia/Makassar", updated.Timezone)
	})
}

func TestWorkspaceRepositoryMembers(t *testing.T) {
	ctx := t.Context()
	dsn := appDatabase(t)
	fixture := newWorkspaceFixture(t, dsn)

	member := addMember(t, fixture, workspacedomain.RoleMember)
	admin := addMember(t, fixture, workspacedomain.RoleAdmin)

	t.Run("lists every member with their address", func(t *testing.T) {
		members, err := fixture.repo.Members(ctx, fixture.scope)
		require.NoError(t, err)
		require.Len(t, members, 3)
		for _, entry := range members {
			require.NotEmpty(t, entry.Email)
			require.False(t, entry.EmailVerified, "these accounts were created without verifying")
		}
	})

	t.Run("changes a role", func(t *testing.T) {
		updated, err := fixture.repo.SetRole(ctx, fixture.scope, member, workspacedomain.RoleAdmin)
		require.NoError(t, err)
		require.Equal(t, workspacedomain.RoleAdmin, updated.Role)
		require.NotEmpty(t, updated.Email)

		// Put it back for the remaining assertions.
		_, err = fixture.repo.SetRole(ctx, fixture.scope, member, workspacedomain.RoleMember)
		require.NoError(t, err)
	})

	t.Run("an unknown member is reported", func(t *testing.T) {
		_, err := fixture.repo.SetRole(ctx, fixture.scope, uuid.New(), workspacedomain.RoleMember)
		require.ErrorIs(t, err, workspacedomain.ErrMemberNotFound)

		err = fixture.repo.RemoveMember(ctx, fixture.scope, uuid.New())
		require.ErrorIs(t, err, workspacedomain.ErrMemberNotFound)
	})

	// The invariant lives in the repository because it must hold inside the same
	// transaction as the write.
	t.Run("the last owner cannot be demoted or removed", func(t *testing.T) {
		_, err := fixture.repo.SetRole(ctx, fixture.scope, fixture.userID, workspacedomain.RoleMember)
		require.ErrorIs(t, err, workspacedomain.ErrLastOwner)

		err = fixture.repo.RemoveMember(ctx, fixture.scope, fixture.userID)
		require.ErrorIs(t, err, workspacedomain.ErrLastOwner)
	})

	t.Run("a second owner can be demoted", func(t *testing.T) {
		_, err := fixture.repo.SetRole(ctx, fixture.scope, admin, workspacedomain.RoleOwner)
		require.NoError(t, err)

		_, err = fixture.repo.SetRole(ctx, fixture.scope, admin, workspacedomain.RoleMember)
		require.NoError(t, err, "with two owners, one may be demoted")
	})

	t.Run("removes a member", func(t *testing.T) {
		require.NoError(t, fixture.repo.RemoveMember(ctx, fixture.scope, member))

		members, err := fixture.repo.Members(ctx, fixture.scope)
		require.NoError(t, err)
		require.Len(t, members, 2)
	})
}

func TestWorkspaceRepositoryInvitations(t *testing.T) {
	ctx := t.Context()
	dsn := appDatabase(t)
	fixture := newWorkspaceFixture(t, dsn)

	invitee := createUser(t, dsn, "invitee@example.com")

	t.Run("stores and lists a pending invitation", func(t *testing.T) {
		created, err := fixture.repo.CreateInvitation(ctx, fixture.scope, workspacedomain.NewInvitation{
			Email:     "invitee@example.com",
			Role:      workspacedomain.RoleMember,
			TokenHash: "invite-hash",
			InvitedBy: fixture.userID,
			ExpiresAt: time.Now().Add(time.Hour),
		})
		require.NoError(t, err)
		require.Equal(t, "invitee@example.com", created.Email)

		pending, err := fixture.repo.Invitations(ctx, fixture.scope)
		require.NoError(t, err)
		require.Len(t, pending, 1)
	})

	t.Run("refuses a second pending invitation for the same address", func(t *testing.T) {
		_, err := fixture.repo.CreateInvitation(ctx, fixture.scope, workspacedomain.NewInvitation{
			Email:     "INVITEE@example.com",
			Role:      workspacedomain.RoleMember,
			TokenHash: "invite-hash-2",
			InvitedBy: fixture.userID,
			ExpiresAt: time.Now().Add(time.Hour),
		})
		require.ErrorIs(t, err, workspacedomain.ErrInvitationAlreadySent)
	})

	t.Run("the invitee accepts and becomes a member", func(t *testing.T) {
		workspace, err := fixture.repo.AcceptInvitation(ctx, "invite-hash", invitee, "invitee@example.com")
		require.NoError(t, err)
		require.Equal(t, fixture.workspace.ID, workspace.ID)
		require.Equal(t, workspacedomain.RoleMember, workspace.Role)

		role, err := fixture.repo.Membership(ctx, fixture.scope, invitee)
		require.NoError(t, err)
		require.Equal(t, workspacedomain.RoleMember, role)
	})

	t.Run("the invitation cannot be accepted twice", func(t *testing.T) {
		_, err := fixture.repo.AcceptInvitation(ctx, "invite-hash", invitee, "invitee@example.com")
		require.ErrorIs(t, err, workspacedomain.ErrInvitationInvalid)
	})

	// A signed-in user who is not the invitee must not even learn that the
	// token exists, so the answer is the same as for an unknown token.
	t.Run("a different address cannot accept, and cannot tell the token exists", func(t *testing.T) {
		_, err := fixture.repo.CreateInvitation(ctx, fixture.scope, workspacedomain.NewInvitation{
			Email:     "someone-else@example.com",
			Role:      workspacedomain.RoleMember,
			TokenHash: "invite-hash-3",
			InvitedBy: fixture.userID,
			ExpiresAt: time.Now().Add(time.Hour),
		})
		require.NoError(t, err)

		other := createUser(t, dsn, "other@example.com")
		_, err = fixture.repo.AcceptInvitation(ctx, "invite-hash-3", other, "other@example.com")
		require.ErrorIs(t, err, workspacedomain.ErrInvitationInvalid)
	})

	t.Run("an expired invitation is refused", func(t *testing.T) {
		_, err := fixture.repo.CreateInvitation(ctx, fixture.scope, workspacedomain.NewInvitation{
			Email:     "expired@example.com",
			Role:      workspacedomain.RoleMember,
			TokenHash: "invite-hash-4",
			InvitedBy: fixture.userID,
			ExpiresAt: time.Now().Add(-time.Minute),
		})
		require.NoError(t, err)

		expired := createUser(t, dsn, "expired@example.com")
		_, err = fixture.repo.AcceptInvitation(ctx, "invite-hash-4", expired, "expired@example.com")
		require.ErrorIs(t, err, workspacedomain.ErrInvitationInvalid)
	})

	t.Run("an unknown token is refused", func(t *testing.T) {
		_, err := fixture.repo.AcceptInvitation(ctx, "no-such-hash", invitee, "invitee@example.com")
		require.ErrorIs(t, err, workspacedomain.ErrInvitationInvalid)
	})

	t.Run("revokes a pending invitation", func(t *testing.T) {
		pending, err := fixture.repo.Invitations(ctx, fixture.scope)
		require.NoError(t, err)
		require.NotEmpty(t, pending)

		require.NoError(t, fixture.repo.RevokeInvitation(ctx, fixture.scope, pending[0].ID))

		remaining, err := fixture.repo.Invitations(ctx, fixture.scope)
		require.NoError(t, err)
		require.Len(t, remaining, len(pending)-1)

		// Revoking again reports that there is nothing left to revoke.
		err = fixture.repo.RevokeInvitation(ctx, fixture.scope, pending[0].ID)
		require.ErrorIs(t, err, workspacedomain.ErrInvitationInvalid)
	})
}

// TestWorkspaceRepositoryTenantIsolation is the RLS gate at the repository
// level.
//
// What Row Level Security guarantees here: inside a scope, a statement that
// forgets a filter still only sees that workspace's rows, and a write that names
// another workspace is refused. What it does not do is decide which workspace
// the scope may name — that is the tenant middleware's job, and
// TestInvitationFlow proves it answers 403 for a non-member.
func TestWorkspaceRepositoryTenantIsolation(t *testing.T) {
	ctx := t.Context()
	dsn := appDatabase(t)

	first := newWorkspaceFixture(t, dsn)
	second := newWorkspaceFixture(t, dsn)

	firstMembers, err := first.repo.Members(ctx, first.scope)
	require.NoError(t, err)
	require.Len(t, firstMembers, 1)

	secondMembers, err := second.repo.Members(ctx, second.scope)
	require.NoError(t, err)
	require.Len(t, secondMembers, 1)

	require.NotEqual(t, firstMembers[0].UserID, secondMembers[0].UserID)

	t.Run("a filterless query inside a scope only sees that workspace", func(t *testing.T) {
		pool := poolFor(t, dsn)

		count := func(scope workspacedomain.Scope) int {
			var total int
			require.NoError(t, pool.InScopeRead(ctx, database.Scope{
				UserID:      scope.UserID,
				WorkspaceID: scope.WorkspaceID,
			}, func(tx pgx.Tx) error {
				// Deliberately no WHERE clause: RLS is the only filter here.
				return tx.QueryRow(ctx, "SELECT count(*) FROM members").Scan(&total)
			}))
			return total
		}

		require.Equal(t, 1, count(first.scope), "the first workspace must only see its own member")
		require.Equal(t, 1, count(second.scope), "the second workspace must only see its own member")
	})

	t.Run("an empty tenant setting matches nothing", func(t *testing.T) {
		pool := poolFor(t, dsn)

		require.NoError(t, pool.InScopeRead(ctx, database.Scope{UserID: first.userID}, func(tx pgx.Tx) error {
			var total int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM teams").Scan(&total); err != nil {
				return err
			}
			require.Zero(t, total, "with no active workspace, tenant tables are empty")
			return nil
		}))
	})

	t.Run("a write naming another workspace is refused", func(t *testing.T) {
		// The insert carries the other workspace's id while the scope is the
		// first one: the WITH CHECK clause rejects it.
		pool := poolFor(t, dsn)

		err := pool.InScope(ctx, database.Scope{UserID: first.userID, WorkspaceID: first.scope.WorkspaceID},
			func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx,
					"INSERT INTO teams (workspace_id, name, kind) VALUES ($1, 'Penyusup', 'bolu')",
					second.scope.WorkspaceID)
				return err
			})
		require.Error(t, err, "writing into another workspace must fail")

		// And the other workspace is unchanged.
		teams, err := second.repo.Teams(ctx, second.scope)
		require.NoError(t, err)
		require.Len(t, teams, 2)
	})

	t.Run("removing a member of another workspace is refused", func(t *testing.T) {
		err := first.repo.RemoveMember(ctx, first.scope, second.userID)
		require.Error(t, err, "a foreign member is not visible, so there is nothing to remove")
	})
}

// TestWorkspaceRepositoryWithoutAPool keeps the unconfigured state an error.
func TestWorkspaceRepositoryWithoutAPool(t *testing.T) {
	ctx := t.Context()
	repo := workspacerepo.New(nil)
	scope := workspacedomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}

	_, err := repo.Onboard(ctx, uuid.New(), workspacedomain.OnboardRequest{Name: "Toko"})
	require.ErrorContains(t, err, "not configured")

	_, err = repo.Workspaces(ctx, uuid.New())
	require.ErrorContains(t, err, "not configured")

	_, err = repo.Workspace(ctx, scope)
	require.ErrorContains(t, err, "not configured")

	_, err = repo.UpdateWorkspace(ctx, scope, workspacedomain.UpdateRequest{Name: "Toko"})
	require.ErrorContains(t, err, "not configured")

	_, err = repo.Teams(ctx, scope)
	require.ErrorContains(t, err, "not configured")

	_, err = repo.Membership(ctx, scope, uuid.New())
	require.ErrorContains(t, err, "not configured")

	_, err = repo.Members(ctx, scope)
	require.ErrorContains(t, err, "not configured")

	_, err = repo.SetRole(ctx, scope, uuid.New(), workspacedomain.RoleMember)
	require.ErrorContains(t, err, "not configured")

	require.ErrorContains(t, repo.RemoveMember(ctx, scope, uuid.New()), "not configured")

	_, err = repo.CreateInvitation(ctx, scope, workspacedomain.NewInvitation{})
	require.ErrorContains(t, err, "not configured")

	_, err = repo.Invitations(ctx, scope)
	require.ErrorContains(t, err, "not configured")

	require.ErrorContains(t, repo.RevokeInvitation(ctx, scope, uuid.New()), "not configured")

	_, err = repo.AcceptInvitation(ctx, "hash", uuid.New(), "a@example.com")
	require.ErrorContains(t, err, "not configured")
}
