// Package database owns the Postgres connection pool and the tenant context
// every query runs inside. Repositories receive a *Pool and never build their
// own connections.
package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool wraps the pgx connection pool.
type Pool struct {
	pool *pgxpool.Pool
}

// Config holds the connection settings.
type Config struct {
	URL             string
	MaxOpenConns    int32
	MaxIdleConns    int32
	ConnMaxLifetime int // seconds
}

// New opens the pool and verifies the connection with a ping, so a bad
// DATABASE_URL fails at startup instead of on the first request.
func New(ctx context.Context, cfg Config) (*Pool, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("database url is required")
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if cfg.MaxOpenConns > 0 {
		poolCfg.MaxConns = cfg.MaxOpenConns
	}
	if cfg.MaxIdleConns > 0 {
		poolCfg.MinConns = cfg.MaxIdleConns
	}
	if cfg.ConnMaxLifetime > 0 {
		poolCfg.MaxConnLifetime = time.Duration(cfg.ConnMaxLifetime) * time.Second
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return &Pool{pool: pool}, nil
}

// PgxPool exposes the underlying pool for repositories.
func (p *Pool) PgxPool() *pgxpool.Pool {
	return p.pool
}

// Ping reports whether Postgres is reachable; used by the readiness probe.
func (p *Pool) Ping(ctx context.Context) error {
	if p == nil || p.pool == nil {
		return fmt.Errorf("database pool is not configured")
	}
	return p.pool.Ping(ctx)
}

// Close releases every pooled connection. Safe on a nil pool.
func (p *Pool) Close() {
	if p == nil || p.pool == nil {
		return
	}
	p.pool.Close()
}
