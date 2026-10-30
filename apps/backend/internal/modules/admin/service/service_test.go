package service_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/admin/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/admin/domain/mocks"
	adminservice "github.com/anakbolu/timbolu/apps/backend/internal/modules/admin/service"
	authdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/auth/domain"
)

// The service tests are about the three refusals the module exists for: who may
// reach the surface, who may demote whom, and what happens to the last admin.
// Everything else here — the page bounds, the workspace counts — is what keeps
// the list readable.

func newService(t *testing.T) (*adminservice.Service, *mocks.Repository) {
	t.Helper()

	repository := mocks.NewRepository(t)
	return adminservice.New(adminservice.Deps{Repository: repository}), repository
}

// account is one row of the account table, with only the fields a test cares
// about set.
func account(id uuid.UUID, email, role string) domain.Account {
	return domain.Account{
		ID:        id,
		Email:     email,
		Role:      role,
		CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func TestChangeRoleRefusesACallerWhoIsNotAnAdmin(t *testing.T) {
	svc, repository := newService(t)
	actorID, targetID := uuid.New(), uuid.New()

	repository.EXPECT().AccountByID(mock.Anything, actorID).
		Return(account(actorID, "member@example.com", authdomain.RoleUser), nil).Once()

	_, err := svc.ChangeRole(t.Context(), domain.ChangeRoleRequest{
		ActorID:  actorID,
		TargetID: targetID,
		Role:     authdomain.RoleAdmin,
	})

	require.ErrorIs(t, err, domain.ErrNotAdmin)
	// The target is never read: there is nothing about the deployment a caller
	// who may not reach it is entitled to learn.
	repository.AssertNotCalled(t, "AccountByID", mock.Anything, targetID)
	repository.AssertNotCalled(t, "Promote", mock.Anything, mock.Anything)
}

func TestChangeRoleRejectsAnUnknownRole(t *testing.T) {
	svc, repository := newService(t)

	_, err := svc.ChangeRole(t.Context(), domain.ChangeRoleRequest{
		ActorID:  uuid.New(),
		TargetID: uuid.New(),
		Role:     "owner",
	})

	require.ErrorIs(t, err, domain.ErrInvalidInput)
	repository.AssertNotCalled(t, "AccountByID", mock.Anything, mock.Anything)
}

func TestChangeRolePromotesAnAccount(t *testing.T) {
	svc, repository := newService(t)
	actorID, targetID := uuid.New(), uuid.New()

	repository.EXPECT().AccountByID(mock.Anything, actorID).
		Return(account(actorID, "admin@example.com", authdomain.RoleAdmin), nil).Once()
	repository.EXPECT().AccountByID(mock.Anything, targetID).
		Return(account(targetID, "member@example.com", authdomain.RoleUser), nil).Once()
	repository.EXPECT().Promote(mock.Anything, targetID).
		Return(account(targetID, "member@example.com", authdomain.RoleAdmin), nil).Once()
	repository.EXPECT().WorkspaceCounts(mock.Anything, []uuid.UUID{targetID}).
		Return(map[uuid.UUID]int64{targetID: 2}, nil).Once()

	updated, err := svc.ChangeRole(t.Context(), domain.ChangeRoleRequest{
		ActorID:  actorID,
		TargetID: targetID,
		Role:     authdomain.RoleAdmin,
	})

	require.NoError(t, err)
	require.Equal(t, authdomain.RoleAdmin, updated.Role)
	require.Equal(t, int64(2), updated.Workspaces)
	require.True(t, updated.Admin())
}

func TestChangeRoleDemotesAnAccount(t *testing.T) {
	svc, repository := newService(t)
	actorID, targetID := uuid.New(), uuid.New()

	repository.EXPECT().AccountByID(mock.Anything, actorID).
		Return(account(actorID, "admin@example.com", authdomain.RoleAdmin), nil).Once()
	repository.EXPECT().AccountByID(mock.Anything, targetID).
		Return(account(targetID, "other@example.com", authdomain.RoleAdmin), nil).Once()
	repository.EXPECT().Demote(mock.Anything, targetID).
		Return(account(targetID, "other@example.com", authdomain.RoleUser), nil).Once()
	repository.EXPECT().WorkspaceCounts(mock.Anything, []uuid.UUID{targetID}).
		Return(map[uuid.UUID]int64{}, nil).Once()

	updated, err := svc.ChangeRole(t.Context(), domain.ChangeRoleRequest{
		ActorID:  actorID,
		TargetID: targetID,
		Role:     authdomain.RoleUser,
	})

	require.NoError(t, err)
	require.Equal(t, authdomain.RoleUser, updated.Role)
	require.False(t, updated.Admin())
	require.Zero(t, updated.Workspaces, "an account in no workspace counts zero rather than being absent")
}

// TestChangeRoleRefusesToDemoteTheCaller is the rule that keeps a mis-click from
// ending the session that made it.
func TestChangeRoleRefusesToDemoteTheCaller(t *testing.T) {
	svc, repository := newService(t)
	actorID := uuid.New()

	repository.EXPECT().AccountByID(mock.Anything, actorID).
		Return(account(actorID, "admin@example.com", authdomain.RoleAdmin), nil).Twice()

	_, err := svc.ChangeRole(t.Context(), domain.ChangeRoleRequest{
		ActorID:  actorID,
		TargetID: actorID,
		Role:     authdomain.RoleUser,
	})

	require.ErrorIs(t, err, domain.ErrSelfDemotion)
	repository.AssertNotCalled(t, "Demote", mock.Anything, mock.Anything)
}

// TestChangeRoleRefusesTheLastAdmin: the rule is enforced by the demotion
// statement, so the service reports the statement's refusal rather than reading a
// count of its own and racing another demotion against it.
func TestChangeRoleRefusesTheLastAdmin(t *testing.T) {
	svc, repository := newService(t)
	actorID, targetID := uuid.New(), uuid.New()

	repository.EXPECT().AccountByID(mock.Anything, actorID).
		Return(account(actorID, "admin@example.com", authdomain.RoleAdmin), nil).Once()
	repository.EXPECT().AccountByID(mock.Anything, targetID).
		Return(account(targetID, "other@example.com", authdomain.RoleAdmin), nil).Once()
	repository.EXPECT().Demote(mock.Anything, targetID).
		Return(domain.Account{}, domain.ErrLastAdmin).Once()

	_, err := svc.ChangeRole(t.Context(), domain.ChangeRoleRequest{
		ActorID:  actorID,
		TargetID: targetID,
		Role:     authdomain.RoleUser,
	})

	require.ErrorIs(t, err, domain.ErrLastAdmin)
}

func TestChangeRoleIsIdempotent(t *testing.T) {
	svc, repository := newService(t)
	actorID, targetID := uuid.New(), uuid.New()

	repository.EXPECT().AccountByID(mock.Anything, actorID).
		Return(account(actorID, "admin@example.com", authdomain.RoleAdmin), nil).Once()
	repository.EXPECT().AccountByID(mock.Anything, targetID).
		Return(account(targetID, "admin@example.com", authdomain.RoleAdmin), nil).Once()
	repository.EXPECT().WorkspaceCounts(mock.Anything, []uuid.UUID{targetID}).
		Return(map[uuid.UUID]int64{targetID: 1}, nil).Once()

	updated, err := svc.ChangeRole(t.Context(), domain.ChangeRoleRequest{
		ActorID:  actorID,
		TargetID: targetID,
		Role:     authdomain.RoleAdmin,
	})

	require.NoError(t, err)
	require.Equal(t, authdomain.RoleAdmin, updated.Role)
	repository.AssertNotCalled(t, "Promote", mock.Anything, mock.Anything)
}

func TestChangeRoleReportsAMissingAccount(t *testing.T) {
	svc, repository := newService(t)
	actorID, targetID := uuid.New(), uuid.New()

	repository.EXPECT().AccountByID(mock.Anything, actorID).
		Return(account(actorID, "admin@example.com", authdomain.RoleAdmin), nil).Once()
	repository.EXPECT().AccountByID(mock.Anything, targetID).
		Return(domain.Account{}, domain.ErrAccountNotFound).Once()

	_, err := svc.ChangeRole(t.Context(), domain.ChangeRoleRequest{
		ActorID:  actorID,
		TargetID: targetID,
		Role:     authdomain.RoleAdmin,
	})

	require.ErrorIs(t, err, domain.ErrAccountNotFound)
}

func TestChangeRoleReportsAnUnconfiguredDatabase(t *testing.T) {
	svc := adminservice.New(adminservice.Deps{})

	_, err := svc.ChangeRole(t.Context(), domain.ChangeRoleRequest{
		ActorID:  uuid.New(),
		TargetID: uuid.New(),
		Role:     authdomain.RoleAdmin,
	})

	require.ErrorIs(t, err, domain.ErrNotConfigured)
}

func TestListClampsThePageAndFillsWorkspaceCounts(t *testing.T) {
	svc, repository := newService(t)
	first, second := uuid.New(), uuid.New()

	// The request asks for 500 rows and a negative offset; what reaches the
	// repository is the bounded version of both.
	expected := domain.Filter{Search: "bolu", Limit: domain.MaxPageSize, Offset: 0}
	repository.EXPECT().Users(mock.Anything, expected).
		Return([]domain.Account{
			account(first, "a@example.com", authdomain.RoleUser),
			account(second, "b@example.com", authdomain.RoleAdmin),
		}, nil).Once()
	repository.EXPECT().CountUsers(mock.Anything, expected).Return(int64(41), nil).Once()
	repository.EXPECT().WorkspaceCounts(mock.Anything, []uuid.UUID{first, second}).
		Return(map[uuid.UUID]int64{second: 3}, nil).Once()

	accounts, total, err := svc.List(t.Context(), domain.Filter{Search: "bolu", Limit: 500, Offset: -7})

	require.NoError(t, err)
	require.Equal(t, int64(41), total)
	require.Len(t, accounts, 2)
	require.Zero(t, accounts[0].Workspaces, "an account in no workspace counts zero")
	require.Equal(t, int64(3), accounts[1].Workspaces)
}

func TestListRejectsAnUnknownRoleFilter(t *testing.T) {
	svc, repository := newService(t)

	_, _, err := svc.List(t.Context(), domain.Filter{Role: "owner"})

	require.ErrorIs(t, err, domain.ErrInvalidInput)
	repository.AssertNotCalled(t, "Users", mock.Anything, mock.Anything)
}

func TestOverviewAnswersTheCounts(t *testing.T) {
	svc, repository := newService(t)

	repository.EXPECT().Stats(mock.Anything).Return(domain.Stats{
		Total:          41,
		Admins:         2,
		Users:          39,
		Verified:       30,
		Onboarded:      25,
		GoogleAccounts: 11,
		NewLast7Days:   4,
	}, nil).Once()

	stats, err := svc.Overview(t.Context())

	require.NoError(t, err)
	require.Equal(t, int64(41), stats.Total)
	require.Equal(t, int64(2), stats.Admins)
	require.Equal(t, int64(4), stats.NewLast7Days)
}

// TestAReadFailureIsReportedRatherThanReadAsEmpty: an operator screen that shows
// "no accounts" while the database is down is worse than one that shows an error,
// because it looks like an answer.
func TestAReadFailureIsReportedRatherThanReadAsEmpty(t *testing.T) {
	svc, repository := newService(t)

	repository.EXPECT().Stats(mock.Anything).Return(domain.Stats{}, errors.New("the account store is down")).Once()

	_, err := svc.Overview(t.Context())
	require.Error(t, err)

	repository.EXPECT().Users(mock.Anything, mock.Anything).Return(nil, errors.New("the account store is down")).Once()
	repository.EXPECT().CountUsers(mock.Anything, mock.Anything).Return(int64(0), nil).Maybe()

	_, _, err = svc.List(t.Context(), domain.Filter{})
	require.Error(t, err)
}
