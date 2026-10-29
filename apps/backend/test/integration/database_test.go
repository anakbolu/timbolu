// Package integration holds the cross-module tests that need a real Postgres.
// The container is started with testcontainers-go and the schema is applied by
// the same embedded migrations the binaries use, so these tests fail when a
// migration is broken.
package integration

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/robzlabz/employeebot/apps/backend/internal/platform/migrate"
	"github.com/robzlabz/employeebot/packages/platform/database"
)

// latestMigration is the version of the newest migration file.
// TestLatestMigrationMatchesMigrations fails when it drifts from migrations/.
const latestMigration = 27

const (
	pgUser     = "postgres"
	pgPassword = "postgres"
	pgDatabase = "bolu"

	// appRole is the non-superuser role the application connects as. A
	// superuser bypasses Row Level Security, so the isolation tests must run
	// as a plain role to prove the policies work.
	appRole     = "bolu_app"
	appPassword = "bolu_app_secret"
)

// sharedPostgres is one container for the whole package.
//
// Starting a container per test dominated the suite's runtime and pushed it past
// Go's default ten-minute limit. Isolation does not need a container: each test
// gets its own database inside this one, created and migrated on demand, which is
// what keeps a test's rows its own while paying for the container once.
var (
	sharedOnce sync.Once
	sharedHost string
	sharedPort string
	sharedErr  error
	// databaseSeq names each test's database, so two tests running together in one
	// package cannot collide on one.
	databaseSeq atomic.Int64
)

// postgresDSN returns a connection string to a database of its own inside the
// package's container. Docker being unavailable skips the test instead of failing
// it.
func postgresDSN(t *testing.T) string {
	t.Helper()

	testcontainers.SkipIfProviderIsNotHealthy(t)
	startSharedPostgres(t)

	name := fmt.Sprintf("bolu_%d", databaseSeq.Add(1))

	// The database is created through the container's own default database, which
	// is what the image's POSTGRES_DB created.
	admin := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		pgUser, pgPassword, sharedHost, sharedPort, pgDatabase)

	ctx := t.Context()
	pool, err := pgxpool.New(ctx, admin)
	require.NoError(t, err)
	defer pool.Close()

	if _, err := pool.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create test database %s: %v", name, err)
	}

	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		pgUser, pgPassword, sharedHost, sharedPort, name)
}

// startSharedPostgres starts the package's container once.
func startSharedPostgres(t *testing.T) {
	t.Helper()

	sharedOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image:        "pgvector/pgvector:pg16",
				ExposedPorts: []string{"5432/tcp"},
				Env: map[string]string{
					"POSTGRES_USER":     pgUser,
					"POSTGRES_PASSWORD": pgPassword,
					"POSTGRES_DB":       pgDatabase,
				},
				WaitingFor: wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(2 * time.Minute),
			},
			Started: true,
		})
		if err != nil {
			sharedErr = fmt.Errorf("start postgres container: %w", err)
			return
		}

		sharedHost, err = container.Host(ctx)
		if err != nil {
			sharedErr = fmt.Errorf("resolve postgres host: %w", err)
			return
		}
		port, err := container.MappedPort(ctx, "5432")
		if err != nil {
			sharedErr = fmt.Errorf("resolve postgres port: %w", err)
			return
		}
		sharedPort = port.Port()

		// The container outlives every test in the package, so it is stopped by the
		// process rather than by a test's cleanup.
		registerShutdown(func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer shutdownCancel()
			_ = container.Terminate(shutdownCtx)
		})
	})

	require.NoError(t, sharedErr)
}

// migratedDatabase returns a DSN whose schema is up to date.
func migratedDatabase(t *testing.T) string {
	t.Helper()

	dsn := postgresDSN(t)
	ctx := t.Context()

	require.NoError(t, migrate.MigrateUp(ctx, dsn))

	return dsn
}

// appDatabase migrates a fresh database and returns a DSN that connects as the
// non-superuser application role.
//
// Tests that assert Row Level Security must use this: a superuser bypasses every
// policy, so testing as one proves nothing.
func appDatabase(t *testing.T) string {
	t.Helper()

	dsn := migratedDatabase(t)
	require.NoError(t, createAppRole(t.Context(), dsn))

	return appDSN(t, dsn)
}

// TestMigrationsRunBothWays is the migration gate: CI runs the same cycle, so a
// down file that does not reverse its up file fails here first.
func TestMigrationsRunBothWays(t *testing.T) {
	dsn := postgresDSN(t)
	ctx := t.Context()

	version, dirty, err := migrate.MigrateVersion(ctx, dsn)
	require.NoError(t, err)
	require.Equal(t, uint(0), version, "a fresh database must have no version")
	require.False(t, dirty)

	require.NoError(t, migrate.MigrateUp(ctx, dsn))
	version, dirty, err = migrate.MigrateVersion(ctx, dsn)
	require.NoError(t, err)
	// latestMigration is the version of the newest file in migrations/. Bump it
	// when a migration is added: this assertion is what catches a migration that
	// silently fails to apply.
	require.Equal(t, uint(latestMigration), version)
	require.False(t, dirty)

	require.NoError(t, migrate.MigrateUp(ctx, dsn), "up must be idempotent")

	require.NoError(t, migrate.MigrateDown(ctx, dsn))
	version, _, err = migrate.MigrateVersion(ctx, dsn)
	require.NoError(t, err)
	require.Equal(t, uint(0), version, "down must remove the schema again")

	require.NoError(t, migrate.MigrateUp(ctx, dsn), "up after down must work")
}

// TestSchemaObjectsExist proves the bootstrap migration created the pgvector
// extension, the six Bolu templates and the trial plan.
func TestSchemaObjectsExist(t *testing.T) {
	dsn := migratedDatabase(t)
	ctx := t.Context()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	var extension string
	require.NoError(t, pool.QueryRow(ctx, "SELECT extname FROM pg_extension WHERE extname = 'vector'").Scan(&extension))
	require.Equal(t, "vector", extension)

	var templates int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM agent_templates").Scan(&templates))
	require.Equal(t, 6, templates, "the six Bolu templates must be seeded")

	var plans int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM plans WHERE is_trial").Scan(&plans))
	require.Equal(t, 1, plans, "the trial plan must be seeded")

	// The packages a workspace can move to, and the packs it can buy on top. They are
	// product decisions, and a workspace that reaches its allowance has to be offered
	// something concrete — so a deployment whose seeds failed is a dead end rather than
	// a smaller product.
	var paid int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM plans
		WHERE NOT is_trial AND monthly_price_idr > 0 AND token_quota > 0`).Scan(&paid))
	require.GreaterOrEqual(t, paid, 2, "the paid packages must be seeded")

	var topups int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM topup_packages WHERE active AND token_amount > 0 AND price_idr > 0`).Scan(&topups))
	require.GreaterOrEqual(t, topups, 1, "at least one top-up pack must be seeded")

	// The audit trail is append-only, enforced by the database rather than by every
	// caller remembering. A row that can be rewritten answers "who changed this" with
	// whatever the last writer wanted it to say, which is worse than having no trail.
	//
	// It is asserted here rather than only in the module's own test because it is a
	// property of the schema: the trigger, not a service, is what refuses.
	requireAuditTrailIsAppendOnly(t, pool, ctx)

	// Every package bounds something, and the trial is short on purpose: a package
	// nobody ever leaves is not a trial.
	var days int
	require.NoError(t, pool.QueryRow(ctx, "SELECT period_days FROM plans WHERE is_trial").Scan(&days))
	require.Less(t, days, 30, "a trial is shorter than a month")
}

// TestRowLevelSecurityIsolatesWorkspaces is the tenant isolation gate: with the
// app.workspace_id setting applied, a query can neither read nor write another
// workspace's rows.
func TestRowLevelSecurityIsolatesWorkspaces(t *testing.T) {
	dsn := migratedDatabase(t)
	ctx := t.Context()

	require.NoError(t, createAppRole(ctx, dsn))

	appPool, err := database.New(ctx, database.Config{
		URL:             appDSN(t, dsn),
		MaxOpenConns:    5,
		MaxIdleConns:    1,
		ConnMaxLifetime: 60,
	})
	require.NoError(t, err)
	t.Cleanup(appPool.Close)

	workspaceA := insertWorkspace(t, ctx, dsn, "Toko A")
	workspaceB := insertWorkspace(t, ctx, dsn, "Toko B")
	userA := insertUser(t, ctx, dsn, "a@example.com")
	userB := insertUser(t, ctx, dsn, "b@example.com")

	// Writing into the active workspace is allowed.
	require.NoError(t, appPool.WithWorkspace(ctx, workspaceA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO members (workspace_id, user_id, role) VALUES ($1, $2, 'owner')", workspaceA, userA)
		return err
	}))

	// Writing into another workspace is rejected by the WITH CHECK clause.
	err = appPool.WithWorkspace(ctx, workspaceA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO members (workspace_id, user_id, role) VALUES ($1, $2, 'owner')", workspaceB, userB)
		return err
	})
	require.Error(t, err, "inserting a row of another workspace must fail")

	// A legitimate row for the second workspace still works.
	require.NoError(t, appPool.WithWorkspace(ctx, workspaceB, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO members (workspace_id, user_id, role) VALUES ($1, $2, 'owner')", workspaceB, userB)
		return err
	}))

	// Each tenant only sees its own row.
	require.NoError(t, appPool.WithWorkspaceRead(ctx, workspaceA, func(tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM members").Scan(&count); err != nil {
			return err
		}
		require.Equal(t, 1, count, "tenant A must only see its own member")
		return nil
	}))

	require.NoError(t, appPool.WithWorkspaceRead(ctx, workspaceB, func(tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM members").Scan(&count); err != nil {
			return err
		}
		require.Equal(t, 1, count, "tenant B must only see its own member")
		return nil
	}))

	// Without a tenant context nothing is visible: a forgotten middleware must
	// fail closed, not open.
	require.NoError(t, appPool.WithWorkspaceRead(ctx, workspaceA, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.workspace_id', '', true)"); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM members").Scan(&count); err != nil {
			return err
		}
		require.Zero(t, count, "an empty tenant setting must match no rows")
		return nil
	}))
}

// TestWithWorkspaceRequiresATenantID documents the fail-closed contract of the
// helper itself.
func TestWithWorkspaceRequiresATenantID(t *testing.T) {
	dsn := migratedDatabase(t)
	ctx := t.Context()

	pool, err := database.New(ctx, database.Config{URL: dsn})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	require.Error(t, pool.WithWorkspace(ctx, uuidNil(), func(pgx.Tx) error { return nil }))
}

func uuidNil() database.WorkspaceID { return database.WorkspaceID{} }

// newPool opens a superuser pool against dsn; the RLS tests use appDSN instead.
func newPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return pgxpool.New(ctx, dsn)
}

// appRoleOnce creates the application role once per cluster.
//
// The role is cluster-wide while each test has a database of its own, so creating
// it per test would collide on the second one. What is per database is the grant,
// which is what this returns to do.
var appRoleOnce sync.Once

func createAppRole(ctx context.Context, dsn string) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	var roleErr error
	appRoleOnce.Do(func() {
		_, roleErr = pool.Exec(ctx,
			fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOBYPASSRLS`, appRole, appPassword))
	})
	if roleErr != nil {
		return fmt.Errorf("create the application role: %w", roleErr)
	}

	// The grants belong to this database, so they are made for every one of them.
	statements := []string{
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, appRole),
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %s`, appRole),
		fmt.Sprintf(`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %s`, appRole),
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			return fmt.Errorf("run %q: %w", statement, err)
		}
	}
	return nil
}

// appDSN rewrites the DSN so the application connects as the plain role, which
// is what makes the Row Level Security policies apply. The userinfo is replaced
// on the URL itself: pgx.ConnConfig.ConnString() returns the original string, so
// mutating the parsed config would silently keep the superuser credentials.
func appDSN(t *testing.T, dsn string) string {
	t.Helper()

	parsed, err := url.Parse(dsn)
	require.NoError(t, err)

	parsed.User = url.UserPassword(appRole, appPassword)
	return parsed.String()
}

func insertWorkspace(t *testing.T, ctx context.Context, dsn, name string) database.WorkspaceID {
	t.Helper()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()

	var id database.WorkspaceID
	require.NoError(t, pool.QueryRow(ctx,
		"INSERT INTO workspaces (name) VALUES ($1) RETURNING id", name).Scan(&id))
	return id
}

func insertUser(t *testing.T, ctx context.Context, dsn, email string) string {
	t.Helper()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()

	var id string
	require.NoError(t, pool.QueryRow(ctx,
		"INSERT INTO users (email) VALUES ($1) RETURNING id", email).Scan(&id))
	return id
}

// requireAuditTrailIsAppendOnly is the schema's own claim about the trail: a recorded action
// cannot be rewritten or removed, and the one way to remove it — a workspace erasure — has
// to say so explicitly.
func requireAuditTrailIsAppendOnly(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()

	var workspace uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO workspaces (name, business_field, timezone)
		VALUES ('Toko Jejak', 'Retail', 'Asia/Jakarta')
		RETURNING id`).Scan(&workspace))

	var entry int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO audit_logs (workspace_id, action, object_type, object_id, metadata)
		VALUES ($1, 'draft.sent', 'draft', 'd-1', '{"to":"a@b.c"}'::jsonb)
		RETURNING id`, workspace).Scan(&entry))

	// The row is written, which is the precondition for the two refusals below: a trigger
	// that refused everything would pass the rest of this for the wrong reason.
	var recorded int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_logs WHERE id = $1`, entry).Scan(&recorded))
	require.Equal(t, 1, recorded)

	// Rewriting it is refused by the database.
	_, err := pool.Exec(ctx,
		`UPDATE audit_logs SET action = 'draft.approved' WHERE id = $1`, entry)
	require.Error(t, err, "a recorded action cannot be rewritten")
	require.Contains(t, err.Error(), "append-only")

	// And so is removing it.
	_, err = pool.Exec(ctx, `DELETE FROM audit_logs WHERE id = $1`, entry)
	require.Error(t, err, "a recorded action cannot be removed")
	require.Contains(t, err.Error(), "append-only")

	// The one lawful way: a workspace erasure says so for the length of its transaction.
	// Without that flag the statement above fails, which is what keeps "users cannot change
	// the audit log" and "a person can have their data deleted" from being two rules that
	// contradict each other.
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT set_config('app.audit_erase', 'on', true)`); err != nil {
		require.NoError(t, err)
	}
	_, err = tx.Exec(ctx, `DELETE FROM audit_logs WHERE id = $1`, entry)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	var remaining int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_logs WHERE id = $1`, entry).Scan(&remaining))
	require.Zero(t, remaining, "a workspace erasure is the one path that removes a line")
}
