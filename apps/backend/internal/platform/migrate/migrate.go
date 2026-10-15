// Package migrate holds the backend's own migration runner: the golang-migrate
// step that applies the SQL embedded in this module.
//
// It lives in the backend rather than in `packages` because the migrations are
// the backend's own assets — the gateway shares the schema, not the files that
// create it — so the only binary that embeds them is the one that runs them.
package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib" // init() registers the database/sql driver golang-migrate uses

	"github.com/robzlabz/employeebot/apps/backend/migrations"
)

// ErrNoChange is returned when the database is already at the target version.
var ErrNoChange = migrate.ErrNoChange

// MigrateUp applies every pending migration against the configured database.
func MigrateUp(ctx context.Context, url string) error {
	m, closeFn, err := newMigrator(url)
	if err != nil {
		return err
	}
	defer closeFn()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// MigrateDown rolls every migration back. It exists so CI can prove both
// directions work (see .github/workflows/ci.yml).
func MigrateDown(ctx context.Context, url string) error {
	m, closeFn, err := newMigrator(url)
	if err != nil {
		return err
	}
	defer closeFn()

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}

// MigrateVersion reports the applied version, or 0 when the schema is empty.
func MigrateVersion(ctx context.Context, url string) (uint, bool, error) {
	m, closeFn, err := newMigrator(url)
	if err != nil {
		return 0, false, err
	}
	defer closeFn()

	version, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("migrate version: %w", err)
	}
	return version, dirty, nil
}

func newMigrator(url string) (*migrate.Migrate, func(), error) {
	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, nil, fmt.Errorf("open embedded migrations: %w", err)
	}

	connConfig, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, nil, fmt.Errorf("parse database url: %w", err)
	}

	db := stdlib.OpenDB(*connConfig)
	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{})
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("create migrate driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("create migrator: %w", err)
	}

	return m, func() { _ = db.Close() }, nil
}
