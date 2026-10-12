package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain/mocks"
)

type harness struct {
	service domain.Service
	repo    *mocks.Repository
	mailer  *mocks.Mailer
	tokens  *mocks.Tokens
	now     time.Time
}

func newHarness(t *testing.T, opts ...func(*Deps)) *harness {
	t.Helper()

	h := &harness{
		repo:   mocks.NewRepository(t),
		mailer: mocks.NewMailer(t),
		tokens: mocks.NewTokens(t),
		now:    time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
	}

	deps := Deps{
		Repository: h.repo,
		Mailer:     h.mailer,
		Tokens:     h.tokens,
		Config: Config{
			InvitationTTL: 7 * 24 * time.Hour,
			FrontendURL:   "https://app.example.com",
			Clock:         func() time.Time { return h.now },
		},
	}
	for _, opt := range opts {
		opt(&deps)
	}

	h.service = New(deps)
	return h
}

func scope() domain.Scope {
	return domain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}
}

func TestOnboard(t *testing.T) {
	t.Run("creates the workspace with its two default teams", func(t *testing.T) {
		h := newHarness(t)
		userID := uuid.New()
		workspace := domain.Workspace{ID: uuid.New(), Name: "Toko Sinar", Timezone: "Asia/Jakarta"}
		boluTeam := domain.Team{ID: uuid.New(), Name: "Tim Bolu", Kind: "bolu"}
		horeTeam := domain.Team{ID: uuid.New(), Name: "Tim Hore", Kind: "hore"}

		h.repo.EXPECT().Onboard(mock.Anything, userID, mock.Anything).
			RunAndReturn(func(_ context.Context, _ uuid.UUID, req domain.OnboardRequest) (domain.OnboardResult, error) {
				require.Equal(t, "Toko Sinar", req.Name)
				require.Equal(t, "Retail", req.BusinessField)
				require.Equal(t, "Asia/Makassar", req.Timezone)
				return domain.OnboardResult{
					Workspace: workspace,
					Teams:     []domain.Team{boluTeam, horeTeam},
					Created:   true,
				}, nil
			}).Once()

		result, err := h.service.Onboard(context.Background(), userID, domain.OnboardRequest{
			Name:          "  Toko Sinar ",
			BusinessField: "Retail",
			Timezone:      "Asia/Makassar",
		})
		require.NoError(t, err)
		require.True(t, result.Created)
		require.Len(t, result.Teams, 2)
	})

	t.Run("defaults the timezone and does not re-provision on a retry", func(t *testing.T) {
		h := newHarness(t)
		userID := uuid.New()
		workspace := domain.Workspace{ID: uuid.New(), Name: "Toko Sinar", Timezone: "Asia/Jakarta"}

		h.repo.EXPECT().Onboard(mock.Anything, userID, mock.Anything).
			RunAndReturn(func(_ context.Context, _ uuid.UUID, req domain.OnboardRequest) (domain.OnboardResult, error) {
				require.Equal(t, "Asia/Jakarta", req.Timezone, "an empty timezone must fall back to the default")
				// Already onboarded: the repository returns the existing workspace.
				return domain.OnboardResult{
					Workspace: workspace,
					Teams:     []domain.Team{{ID: uuid.New(), Name: "Tim Bolu", Kind: "bolu"}},
					Created:   false,
				}, nil
			}).Once()

		result, err := h.service.Onboard(context.Background(), userID, domain.OnboardRequest{Name: "Toko Sinar"})
		require.NoError(t, err)
		require.False(t, result.Created, "a repeated submit must not create a second workspace")
	})

	t.Run("rejects an empty name", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.Onboard(context.Background(), uuid.New(), domain.OnboardRequest{Name: "   "})
		require.Error(t, err)
	})

	t.Run("rejects an unknown timezone", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.Onboard(context.Background(), uuid.New(), domain.OnboardRequest{
			Name:     "Toko Sinar",
			Timezone: "Mars/Olympus",
		})
		require.ErrorContains(t, err, "unknown timezone")
	})

	t.Run("rejects an anonymous caller", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.Onboard(context.Background(), uuid.Nil, domain.OnboardRequest{Name: "Toko Sinar"})
		require.ErrorIs(t, err, domain.ErrNotMember)
	})
}

// TestRoleMatrix is the permission table from the task: a member may not manage
// members, an admin may, but only an owner may grant ownership.
func TestRoleMatrix(t *testing.T) {
	target := uuid.New()
	actor := uuid.New()

	tests := []struct {
		name      string
		actorRole string
		action    string
		wantErr   error
	}{
		{name: "member cannot invite", actorRole: domain.RoleMember, action: "invite", wantErr: domain.ErrForbidden},
		{name: "member cannot change a role", actorRole: domain.RoleMember, action: "set-role", wantErr: domain.ErrForbidden},
		{name: "member cannot remove a member", actorRole: domain.RoleMember, action: "remove", wantErr: domain.ErrForbidden},
		{name: "member cannot revoke an invitation", actorRole: domain.RoleMember, action: "revoke", wantErr: domain.ErrForbidden},
		{name: "admin cannot grant ownership", actorRole: domain.RoleAdmin, action: "grant-owner", wantErr: domain.ErrForbidden},
		{name: "admin cannot update workspace settings", actorRole: domain.RoleAdmin, action: "update", wantErr: domain.ErrForbidden},
		{name: "unknown role is refused", actorRole: "superuser", action: "invite", wantErr: domain.ErrForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			s := scope()

			var err error
			switch tt.action {
			case "invite":
				_, err = h.service.Invite(ctx, s, tt.actorRole, actor, "new@example.com", domain.RoleMember)
			case "set-role":
				_, err = h.service.SetRole(ctx, s, tt.actorRole, target, domain.RoleMember)
			case "remove":
				err = h.service.RemoveMember(ctx, s, tt.actorRole, actor, target)
			case "revoke":
				err = h.service.RevokeInvitation(ctx, s, tt.actorRole, uuid.New())
			case "grant-owner":
				_, err = h.service.SetRole(ctx, s, tt.actorRole, target, domain.RoleOwner)
			case "update":
				_, err = h.service.Update(ctx, s, tt.actorRole, domain.UpdateRequest{Name: "Toko"})
			}

			require.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestSetRoleGuards(t *testing.T) {
	t.Run("an unknown role is refused", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.SetRole(context.Background(), scope(), domain.RoleOwner, uuid.New(), "wizard")
		require.ErrorIs(t, err, domain.ErrInvalidRole)
	})

	// An admin must not be able to demote or remove an owner, which would let
	// them take over the workspace.
	t.Run("an admin cannot touch an owner", func(t *testing.T) {
		h := newHarness(t)
		target := uuid.New()

		h.repo.EXPECT().Members(mock.Anything, mock.Anything).
			Return([]domain.Member{{UserID: target, Role: domain.RoleOwner}}, nil).Once()

		_, err := h.service.SetRole(context.Background(), scope(), domain.RoleAdmin, target, domain.RoleMember)
		require.ErrorIs(t, err, domain.ErrForbidden)
	})

	t.Run("an owner may promote another member", func(t *testing.T) {
		h := newHarness(t)
		target := uuid.New()

		h.repo.EXPECT().SetRole(mock.Anything, mock.Anything, target, domain.RoleAdmin).
			Return(domain.Member{UserID: target, Role: domain.RoleAdmin}, nil).Once()

		member, err := h.service.SetRole(context.Background(), scope(), domain.RoleOwner, target, domain.RoleAdmin)
		require.NoError(t, err)
		require.Equal(t, domain.RoleAdmin, member.Role)
	})

	t.Run("the last owner cannot be demoted", func(t *testing.T) {
		h := newHarness(t)
		target := uuid.New()

		h.repo.EXPECT().SetRole(mock.Anything, mock.Anything, target, domain.RoleMember).
			Return(domain.Member{}, domain.ErrLastOwner).Once()

		_, err := h.service.SetRole(context.Background(), scope(), domain.RoleOwner, target, domain.RoleMember)
		require.ErrorIs(t, err, domain.ErrLastOwner)
	})
}

func TestRemoveMember(t *testing.T) {
	t.Run("nobody may remove themselves", func(t *testing.T) {
		h := newHarness(t)
		actor := uuid.New()

		err := h.service.RemoveMember(context.Background(), scope(), domain.RoleOwner, actor, actor)
		require.ErrorIs(t, err, domain.ErrCannotRemoveYourself)
	})

	t.Run("the last owner cannot be removed", func(t *testing.T) {
		h := newHarness(t)
		target := uuid.New()

		h.repo.EXPECT().RemoveMember(mock.Anything, mock.Anything, target).
			Return(domain.ErrLastOwner).Once()

		err := h.service.RemoveMember(context.Background(), scope(), domain.RoleOwner, uuid.New(), target)
		require.ErrorIs(t, err, domain.ErrLastOwner)
	})
}

func TestInvite(t *testing.T) {
	t.Run("stores the hash and emails the link", func(t *testing.T) {
		h := newHarness(t)
		actor := uuid.New()
		s := scope()

		h.tokens.EXPECT().Generate().Return("raw-token", "token-hash", nil).Once()
		h.repo.EXPECT().CreateInvitation(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, received domain.Scope, invitation domain.NewInvitation) (domain.Invitation, error) {
				require.Equal(t, s.WorkspaceID, received.WorkspaceID)
				require.Equal(t, "new@example.com", invitation.Email, "the address must be normalised")
				require.Equal(t, domain.RoleAdmin, invitation.Role)
				require.Equal(t, "token-hash", invitation.TokenHash, "only the hash is stored")
				require.Equal(t, actor, invitation.InvitedBy)
				require.WithinDuration(t, h.now.Add(7*24*time.Hour), invitation.ExpiresAt, time.Second)
				return domain.Invitation{ID: uuid.New(), WorkspaceID: received.WorkspaceID, Email: invitation.Email, Role: invitation.Role}, nil
			}).Once()
		h.mailer.EXPECT().Send(mock.Anything, "new@example.com", mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, _, _, body string) error {
				require.Contains(t, body, "https://app.example.com/join?token=raw-token")
				require.Contains(t, body, "admin")
				return nil
			}).Once()

		invitation, err := h.service.Invite(context.Background(), s, domain.RoleOwner, actor, " New@Example.com ", domain.RoleAdmin)
		require.NoError(t, err)
		require.Equal(t, "new@example.com", invitation.Email)
	})

	t.Run("rejects an invalid address", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.Invite(context.Background(), scope(), domain.RoleOwner, uuid.New(), "not-an-address", domain.RoleMember)
		require.Error(t, err)
	})

	t.Run("an admin cannot invite an owner", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.Invite(context.Background(), scope(), domain.RoleAdmin, uuid.New(), "new@example.com", domain.RoleOwner)
		require.ErrorIs(t, err, domain.ErrForbidden)
	})

	t.Run("a pending invitation for the same address is reported", func(t *testing.T) {
		h := newHarness(t)
		h.tokens.EXPECT().Generate().Return("raw", "hash", nil).Once()
		h.repo.EXPECT().CreateInvitation(mock.Anything, mock.Anything, mock.Anything).
			Return(domain.Invitation{}, domain.ErrInvitationAlreadySent).Once()

		_, err := h.service.Invite(context.Background(), scope(), domain.RoleOwner, uuid.New(), "new@example.com", domain.RoleMember)
		require.ErrorIs(t, err, domain.ErrInvitationAlreadySent)
	})
}

func TestAcceptInvitation(t *testing.T) {
	t.Run("hashes the token before lookup", func(t *testing.T) {
		h := newHarness(t)
		userID := uuid.New()

		h.repo.EXPECT().AcceptInvitation(mock.Anything, domain.HashToken("raw-token"), userID, "new@example.com").
			Return(domain.Workspace{ID: uuid.New(), Name: "Toko Sinar", Role: domain.RoleMember}, nil).Once()

		workspace, err := h.service.AcceptInvitation(context.Background(), "raw-token", userID, " New@Example.com ")
		require.NoError(t, err)
		require.Equal(t, domain.RoleMember, workspace.Role)
	})

	t.Run("rejects an empty token", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.AcceptInvitation(context.Background(), "  ", uuid.New(), "new@example.com")
		require.ErrorIs(t, err, domain.ErrInvitationInvalid)
	})

	t.Run("rejects an anonymous caller", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.AcceptInvitation(context.Background(), "raw-token", uuid.Nil, "new@example.com")
		require.ErrorIs(t, err, domain.ErrInvitationInvalid)
	})

	t.Run("reports an address mismatch", func(t *testing.T) {
		h := newHarness(t)
		h.repo.EXPECT().AcceptInvitation(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(domain.Workspace{}, domain.ErrInvitationEmailMatch).Once()

		_, err := h.service.AcceptInvitation(context.Background(), "raw-token", uuid.New(), "other@example.com")
		require.ErrorIs(t, err, domain.ErrInvitationEmailMatch)
	})
}

func TestResolve(t *testing.T) {
	t.Run("returns the role", func(t *testing.T) {
		h := newHarness(t)
		workspaceID, userID := uuid.New(), uuid.New()

		h.repo.EXPECT().Membership(mock.Anything, mock.Anything, userID).
			Return(domain.RoleAdmin, nil).Once()

		role, err := h.service.Resolve(context.Background(), workspaceID, userID)
		require.NoError(t, err)
		require.Equal(t, domain.RoleAdmin, role)
	})

	t.Run("a non-member is reported as such", func(t *testing.T) {
		h := newHarness(t)
		h.repo.EXPECT().Membership(mock.Anything, mock.Anything, mock.Anything).
			Return("", domain.ErrNotMember).Once()

		_, err := h.service.Resolve(context.Background(), uuid.New(), uuid.New())
		require.ErrorIs(t, err, domain.ErrNotMember)
	})

	t.Run("a zero id is refused without a query", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.Resolve(context.Background(), uuid.Nil, uuid.New())
		require.ErrorIs(t, err, domain.ErrNotMember)
	})
}

func TestUpdateWorkspace(t *testing.T) {
	t.Run("an owner may update", func(t *testing.T) {
		h := newHarness(t)

		h.repo.EXPECT().UpdateWorkspace(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, _ domain.Scope, req domain.UpdateRequest) (domain.Workspace, error) {
				require.Equal(t, "Toko Baru", req.Name)
				require.Equal(t, "Asia/Jakarta", req.Timezone)
				require.Equal(t, "id", req.Language)
				require.Equal(t, "08:00", req.WorkHoursStart)
				require.Equal(t, "21:00", req.WorkHoursEnd)
				require.Equal(t, []int{1, 2, 3, 4, 5}, req.WorkDays)
				return domain.Workspace{ID: uuid.New(), Name: req.Name, Timezone: req.Timezone}, nil
			}).Once()

		workspace, err := h.service.Update(context.Background(), scope(), domain.RoleOwner, domain.UpdateRequest{
			Name:           "Toko Baru",
			WorkHoursStart: "08:00",
			WorkHoursEnd:   "21:00",
			WorkDays:       []int{1, 2, 3, 4, 5},
		})
		require.NoError(t, err)
		require.Equal(t, "Toko Baru", workspace.Name)
	})

	t.Run("rejects an empty name", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.Update(context.Background(), scope(), domain.RoleOwner, domain.UpdateRequest{Name: ""})
		require.Error(t, err)
	})

	// The working window is what the approval gate reads before it lets an action
	// leave, so a value it could not parse, or a week with no working day in it,
	// would park every outbound action instead of reaching the caller as a refusal.
	t.Run("rejects an unparseable working window", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.Update(context.Background(), scope(), domain.RoleOwner, domain.UpdateRequest{
			Name:           "Toko Baru",
			WorkHoursStart: "pagi",
			WorkHoursEnd:   "21:00",
			WorkDays:       []int{1},
		})
		require.ErrorIs(t, err, domain.ErrInvalidInput)
	})

	t.Run("rejects a week with no working day", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.Update(context.Background(), scope(), domain.RoleOwner, domain.UpdateRequest{
			Name:           "Toko Baru",
			WorkHoursStart: "08:00",
			WorkHoursEnd:   "21:00",
			WorkDays:       nil,
		})
		require.ErrorIs(t, err, domain.ErrInvalidInput)
	})

	t.Run("rejects a day that is not a weekday", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.Update(context.Background(), scope(), domain.RoleOwner, domain.UpdateRequest{
			Name:           "Toko Baru",
			WorkHoursStart: "08:00",
			WorkHoursEnd:   "21:00",
			WorkDays:       []int{8},
		})
		require.ErrorIs(t, err, domain.ErrInvalidInput)
	})

	t.Run("rejects a window that never opens", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.Update(context.Background(), scope(), domain.RoleOwner, domain.UpdateRequest{
			Name:           "Toko Baru",
			WorkHoursStart: "08:00",
			WorkHoursEnd:   "08:00",
			WorkDays:       []int{1},
		})
		require.ErrorIs(t, err, domain.ErrInvalidInput)
	})

	// A day list that repeats is stored once, sorted, so the rule that reads it
	// cannot see the same day twice.
	t.Run("normalises the working week", func(t *testing.T) {
		h := newHarness(t)

		h.repo.EXPECT().UpdateWorkspace(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, _ domain.Scope, req domain.UpdateRequest) (domain.Workspace, error) {
				require.Equal(t, []int{1, 2, 5}, req.WorkDays)
				return domain.Workspace{Name: req.Name}, nil
			}).Once()

		_, err := h.service.Update(context.Background(), scope(), domain.RoleOwner, domain.UpdateRequest{
			Name:           "Toko Baru",
			WorkHoursStart: "7:5",
			WorkHoursEnd:   "21:00",
			WorkDays:       []int{5, 1, 2, 1},
		})
		require.NoError(t, err)
	})
}
