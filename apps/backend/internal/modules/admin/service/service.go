// Package service is the operator surface's business rules: who may read the
// account list, and which changes to a role are refused.
//
// The three refusals are the module's whole reason for having a service rather
// than a repository behind a handler:
//
//   - the caller must be an admin (the middleware checks it too, and this is the
//     second lock, because a service called from anywhere else must not be a way
//     around the first);
//   - the last admin stays, so a demotion can never leave the deployment with no
//     way in;
//   - nobody demotes themselves, so a mis-click cannot end the session that made
//     it.
//
// What it deliberately does not do is decide anything about a workspace. A
// platform admin administers accounts; what those accounts may do inside a
// workspace is the workspace's own business and is checked by the workspace's
// own roles.
package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/admin/domain"
	authdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/domain"
)

// Deps is what the service is built from.
type Deps struct {
	Repository domain.Repository
}

// Service implements domain.Service.
type Service struct {
	repository domain.Repository
}

// New builds the service.
func New(deps Deps) *Service { return &Service{repository: deps.Repository} }

// Compile-time check that the service is the interface the module asked for.
var _ domain.Service = (*Service)(nil)

// Overview answers what the deployment's accounts add up to.
func (s *Service) Overview(ctx context.Context) (domain.Stats, error) {
	if err := s.ready(); err != nil {
		return domain.Stats{}, err
	}

	stats, err := s.repository.Stats(ctx)
	if err != nil {
		return domain.Stats{}, err
	}
	return stats, nil
}

// List answers a page of accounts, with the total the filter matches.
//
// The workspace counts are fetched for the page's ids in one statement rather
// than per row, because a list of twenty-five accounts should not be
// twenty-six queries.
func (s *Service) List(ctx context.Context, filter domain.Filter) ([]domain.Account, int64, error) {
	if err := s.ready(); err != nil {
		return nil, 0, err
	}
	if !filter.Valid() {
		return nil, 0, fmt.Errorf("%w: role must be %q or %q", domain.ErrInvalidInput, authdomain.RoleUser, authdomain.RoleAdmin)
	}

	filter = filter.Normalised()

	accounts, err := s.repository.Users(ctx, filter)
	if err != nil {
		return nil, 0, err
	}

	total, err := s.repository.CountUsers(ctx, filter)
	if err != nil {
		return nil, 0, err
	}

	if len(accounts) > 0 {
		ids := make([]uuid.UUID, 0, len(accounts))
		for _, account := range accounts {
			ids = append(ids, account.ID)
		}

		counts, err := s.repository.WorkspaceCounts(ctx, ids)
		if err != nil {
			return nil, 0, err
		}
		for i := range accounts {
			accounts[i].Workspaces = counts[accounts[i].ID]
		}
	}

	return accounts, total, nil
}

// ChangeRole promotes or demotes an account.
//
// The order of the checks is the order the reasons are worth explaining in: the
// caller, then the request, then the account, then the two rules about a
// demotion.
//
// The caller is checked here as well as in the middleware, and this one is the
// authoritative check: it reads the account's role from the database rather than
// trusting a value the request carries, so the service is not a way around the
// middleware for any caller that reaches it.
func (s *Service) ChangeRole(ctx context.Context, req domain.ChangeRoleRequest) (domain.Account, error) {
	if err := s.ready(); err != nil {
		return domain.Account{}, err
	}
	if req.Role != authdomain.RoleUser && req.Role != authdomain.RoleAdmin {
		return domain.Account{}, fmt.Errorf("%w: role must be %q or %q", domain.ErrInvalidInput, authdomain.RoleUser, authdomain.RoleAdmin)
	}

	actor, err := s.repository.AccountByID(ctx, req.ActorID)
	if err != nil {
		return domain.Account{}, err
	}
	if !actor.Admin() {
		return domain.Account{}, domain.ErrNotAdmin
	}

	target, err := s.repository.AccountByID(ctx, req.TargetID)
	if err != nil {
		return domain.Account{}, err
	}

	// The role is already what was asked for, so there is nothing to write and
	// nothing to refuse: a repeated promotion is the same request twice, and
	// answering it with the account is what makes a retry safe.
	if target.Role == req.Role {
		return s.withWorkspaceCount(ctx, target), nil
	}

	// An admin who wants out asks another admin. It is what keeps a mis-click
	// from ending the session that made it, and it is checked here rather than in
	// the statement because it is about the caller, which the statement does not
	// see.
	if req.Role == authdomain.RoleUser && req.TargetID == req.ActorID {
		return domain.Account{}, domain.ErrSelfDemotion
	}

	// The last admin is refused by the demotion statement itself, so the rule
	// holds under two demotions running at once; see the query for why.
	var updated domain.Account
	if req.Role == authdomain.RoleAdmin {
		updated, err = s.repository.Promote(ctx, req.TargetID)
	} else {
		updated, err = s.repository.Demote(ctx, req.TargetID)
	}
	if err != nil {
		return domain.Account{}, err
	}

	return s.withWorkspaceCount(ctx, updated), nil
}

// withWorkspaceCount fills in how many workspaces an account belongs to, so a
// promotion answers with the row the list would show.
//
// A count that cannot be read is left at zero rather than failing the change: the
// role is already written at this point, and reporting a failure for a number
// the screen can reload would be a lie about what happened.
func (s *Service) withWorkspaceCount(ctx context.Context, account domain.Account) domain.Account {
	if counts, err := s.repository.WorkspaceCounts(ctx, []uuid.UUID{account.ID}); err == nil {
		account.Workspaces = counts[account.ID]
	}
	return account
}

// ready reports whether the service has a repository to work with. It is a
// deployment fact rather than a request one: an API without a database answers
// 503 for this surface instead of failing the query.
func (s *Service) ready() error {
	if s == nil || s.repository == nil {
		return domain.ErrNotConfigured
	}
	return nil
}
