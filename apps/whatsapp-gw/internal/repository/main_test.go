// Package repository's tests run against a real Postgres.
//
// The repository is the gateway's only database access, and what it does is
// mapping rows to the domain's own types. A fake driver would prove the mapping
// and nothing else; a real one proves the SQL too — that `ServingWhatsAppNumbers`
// selects what it claims to, that the upsert returns the row it wrote, and that
// the key insert really does keep the first wrap. So the container is the point,
// not a convenience.
//
// One container serves the whole package and each test gets a database of its
// own inside it, which is the backend's own arrangement: a container per test
// dominated the suite's runtime, and isolation only needs a database.
package repository

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	pgUser     = "postgres"
	pgPassword = "postgres"
	pgDatabase = "postgres"
)

var (
	shutdownMu       sync.Mutex
	shutdownHandlers []func()

	sharedOnce      sync.Once
	sharedContainer testcontainers.Container
	sharedHost      string
	sharedPort      string
	sharedErr       error

	databaseSeq atomic.Int64
)

// pgxPool opens a raw pool, for the harness's own setup statements.
func pgxPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return pgxpool.New(ctx, dsn)
}

// registerShutdown records a handler to run after the package's tests finish.
func registerShutdown(handler func()) {
	shutdownMu.Lock()
	defer shutdownMu.Unlock()
	shutdownHandlers = append(shutdownHandlers, handler)
}

// TestMain stops what the package started.
func TestMain(m *testing.M) {
	code := m.Run()

	shutdownMu.Lock()
	handlers := append([]func(){}, shutdownHandlers...)
	shutdownMu.Unlock()

	for _, handler := range handlers {
		handler()
	}
	os.Exit(code)
}

// postgresDSN returns a DSN to a database of its own inside the package's
// container, with the gateway's schema applied. Docker being unavailable skips
// the test instead of failing it.
func postgresDSN(t *testing.T) string {
	t.Helper()

	testcontainers.SkipIfProviderIsNotHealthy(t)
	startSharedPostgres(t)

	name := fmt.Sprintf("gateway_%d", databaseSeq.Add(1))
	admin := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		pgUser, pgPassword, sharedHost, sharedPort, pgDatabase)

	ctx := t.Context()
	pool, err := pgxPool(ctx, admin)
	require.NoError(t, err)
	defer pool.Close()

	if _, err := pool.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create test database %s: %v", name, err)
	}

	dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		pgUser, pgPassword, sharedHost, sharedPort, name)

	applySchema(t, dsn)

	return dsn
}

// startSharedPostgres starts the package's container once.
func startSharedPostgres(t *testing.T) {
	t.Helper()

	sharedOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				// pgvector rather than plain Postgres, because it is the image the
				// rest of the stack uses and the one already pulled for the
				// backend's own integration tests.
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
		sharedContainer = container

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

		registerShutdown(func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer shutdownCancel()
			_ = sharedContainer.Terminate(shutdownCtx)
		})
	})

	require.NoError(t, sharedErr)
}

// gatewaySchema is the slice of the WhatsApp schema the gateway reads and writes.
//
// It is written out rather than taken from the migrations because the migrations
// are the backend's assets — the gateway shares the schema, not the files that
// create it — and because the gateway must not be able to reach the product's
// tables at all. Only the directory, the sessions, and the key ring exist here,
// which is exactly what the gateway's queries touch.
const gatewaySchema = `
CREATE TABLE workspaces (
    id UUID PRIMARY KEY
);

CREATE TABLE whatsapp_numbers (
    id UUID PRIMARY KEY,
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE
);

CREATE TABLE whatsapp_sessions (
    number_id UUID PRIMARY KEY REFERENCES whatsapp_numbers (id) ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    sealed_device BYTEA NOT NULL,
    jid TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE whatsapp_directory (
    number_id UUID PRIMARY KEY REFERENCES whatsapp_numbers (id) ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    enabled BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE workspace_keys (
    workspace_id UUID PRIMARY KEY REFERENCES workspaces (id) ON DELETE CASCADE,
    wrapped_data_key BYTEA NOT NULL,
    kms_key_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

// applySchema creates the gateway's tables in a fresh database.
func applySchema(t *testing.T, dsn string) {
	t.Helper()

	pool, err := pgxPool(t.Context(), dsn)
	require.NoError(t, err)
	defer pool.Close()

	_, err = pool.Exec(t.Context(), gatewaySchema)
	require.NoError(t, err)
}
