// Package repository implements the health module data access with pgx and the
// sqlc-generated queries.
package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/health/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/health/repository/sqlcgen"
)

// Repository is the Postgres-backed implementation of domain.Repository.
type Repository struct {
	pool *pgxpool.Pool
}

// New builds the repository. A nil pool is allowed: Ping then reports that the
// database is not configured instead of panicking, which is what lets the API
// boot without Postgres during local development.
func New(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// Ping verifies the database answers a real query.
func (r *Repository) Ping(ctx context.Context) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("database is not configured")
	}

	if err := r.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	if _, err := sqlcgen.New(r.pool).Ping(ctx); err != nil {
		return fmt.Errorf("query database: %w", err)
	}
	return nil
}

// Compile-time check that the repository satisfies the domain contract.
var _ domain.Repository = (*Repository)(nil)
