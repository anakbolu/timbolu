package integration

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/backend/migrations"
)

// TestLatestMigrationMatchesMigrations keeps latestMigration honest without a
// database. The migration gate compares the version Postgres reports against that
// constant, so a constant left behind by the newest migration fails there as an
// unexplained mismatch; this names the real cause and fails on plain go test.
func TestLatestMigrationMatchesMigrations(t *testing.T) {
	src, err := iofs.New(migrations.FS, ".")
	require.NoError(t, err, "the embedded migrations must be readable")
	defer func() { _ = src.Close() }()

	newest, err := newestMigrationVersion(src)
	require.NoError(t, err)

	require.Equal(t, newest, uint(latestMigration),
		"latestMigration must be %d, the newest version in apps/backend/migrations", newest)
}

// newestMigrationVersion walks the same source golang-migrate reads, so a file the
// runner would reject cannot be counted here.
func newestMigrationVersion(src source.Driver) (uint, error) {
	newest, err := src.First()
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	// Next reports fs.ErrNotExist past the newest version instead of returning a
	// sentinel, which is what makes the loop termination the end of the list.
	for {
		next, err := src.Next(newest)
		if errors.Is(err, fs.ErrNotExist) {
			return newest, nil
		}
		if err != nil {
			return 0, err
		}
		newest = next
	}
}
