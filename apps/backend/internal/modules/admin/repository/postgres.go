// Package repository implements the operator surface's storage: the account list
// and the numbers on the overview.
//
// Every statement here runs outside a tenant scope, and that is what these
// statements are: they read `users`, which is the one table in the schema that
// belongs to the deployment rather than to a workspace, and which carries no Row
// Level Security for exactly that reason. Nothing here reads a tenant table, so
// the missing scope cannot be a missing filter.
package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/admin/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/admin/repository/sqlcgen"
	"github.com/robzlabz/employeebot/packages/platform/database"
)

// Repository is the Postgres-backed implementation of domain.Repository.
type Repository struct {
	pool *database.Pool
}

// New builds the repository.
func New(pool *database.Pool) *Repository { return &Repository{pool: pool} }

// Compile-time check that the repository is the interface the module asked for.
var _ domain.Repository = (*Repository)(nil)

func (r *Repository) queries() (*sqlcgen.Queries, error) {
	if r == nil || r.pool == nil {
		return nil, domain.ErrNotConfigured
	}
	return sqlcgen.New(r.pool.PgxPool()), nil
}

// Users returns a page of accounts, newest first.
func (r *Repository) Users(ctx context.Context, filter domain.Filter) ([]domain.Account, error) {
	queries, err := r.queries()
	if err != nil {
		return nil, err
	}

	filter = filter.Normalised()
	rows, err := queries.ListUsers(ctx, sqlcgen.ListUsersParams{
		Search:      filter.Search,
		Role:        filter.Role,
		LimitCount:  int32(filter.Limit),
		OffsetCount: int32(filter.Offset),
	})
	if err != nil {
		return nil, fmt.Errorf("admin: list users: %w", err)
	}

	accounts := make([]domain.Account, 0, len(rows))
	for _, row := range rows {
		accounts = append(accounts, toAccount(row))
	}
	return accounts, nil
}

// CountUsers returns how many accounts the filter matches.
func (r *Repository) CountUsers(ctx context.Context, filter domain.Filter) (int64, error) {
	queries, err := r.queries()
	if err != nil {
		return 0, err
	}

	total, err := queries.CountUsers(ctx, sqlcgen.CountUsersParams{
		Search: filter.Search,
		Role:   filter.Role,
	})
	if err != nil {
		return 0, fmt.Errorf("admin: count users: %w", err)
	}
	return total, nil
}

// Stats returns what the accounts add up to.
func (r *Repository) Stats(ctx context.Context) (domain.Stats, error) {
	queries, err := r.queries()
	if err != nil {
		return domain.Stats{}, err
	}

	row, err := queries.UserStats(ctx)
	if err != nil {
		return domain.Stats{}, fmt.Errorf("admin: user stats: %w", err)
	}

	return domain.Stats{
		Total:          row.Total,
		Admins:         row.Admins,
		Users:          row.Users,
		Verified:       row.Verified,
		Onboarded:      row.Onboarded,
		GoogleAccounts: row.GoogleAccounts,
		NewLast7Days:   row.NewLast7Days,
	}, nil
}

// AccountByID reads one account.
func (r *Repository) AccountByID(ctx context.Context, id uuid.UUID) (domain.Account, error) {
	queries, err := r.queries()
	if err != nil {
		return domain.Account{}, err
	}

	row, err := queries.GetUser(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Account{}, fmt.Errorf("%w: %s", domain.ErrAccountNotFound, id)
		}
		return domain.Account{}, fmt.Errorf("admin: read account: %w", err)
	}

	return toAccount(row), nil
}

// Promote makes an account a platform admin.
func (r *Repository) Promote(ctx context.Context, id uuid.UUID) (domain.Account, error) {
	queries, err := r.queries()
	if err != nil {
		return domain.Account{}, err
	}

	row, err := queries.PromoteUser(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Account{}, fmt.Errorf("%w: %s", domain.ErrAccountNotFound, id)
		}
		return domain.Account{}, fmt.Errorf("admin: promote account: %w", err)
	}

	return toAccount(row), nil
}

// Demote takes the platform role away.
//
// The statement refuses the last admin, so no rows here is that refusal rather
// than a missing account: the service reads the account before it calls this, and
// the statement's own conditions are the two a concurrent demotion would slip
// between. It is reported as ErrLastAdmin for that reason.
func (r *Repository) Demote(ctx context.Context, id uuid.UUID) (domain.Account, error) {
	queries, err := r.queries()
	if err != nil {
		return domain.Account{}, err
	}

	row, err := queries.DemoteUser(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Account{}, fmt.Errorf("%w: %s", domain.ErrLastAdmin, id)
		}
		return domain.Account{}, fmt.Errorf("admin: demote account: %w", err)
	}

	return toAccount(row), nil
}

// WorkspaceCounts answers, per account id, how many workspaces it belongs to.
func (r *Repository) WorkspaceCounts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]int64, error) {
	counts := make(map[uuid.UUID]int64, len(ids))
	if len(ids) == 0 {
		return counts, nil
	}

	queries, err := r.queries()
	if err != nil {
		return nil, err
	}

	rows, err := queries.CountUserWorkspaces(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("admin: count user workspaces: %w", err)
	}
	for _, row := range rows {
		counts[row.UserID] = row.WorkspaceCount
	}
	return counts, nil
}

func toAccount(row sqlcgen.User) domain.Account {
	return domain.Account{
		ID:            row.ID,
		Email:         row.Email,
		Role:          row.Role,
		EmailVerified: row.EmailVerifiedAt != nil,
		Onboarded:     row.OnboardedAt != nil,
		HasGoogle:     row.GoogleSubject != nil && *row.GoogleSubject != "",
		HasPassword:   row.PasswordHash != "",
		CreatedAt:     row.CreatedAt,
	}
}
