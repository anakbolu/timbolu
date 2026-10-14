package database

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func TestNewRejectsBadConfiguration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "empty url", url: "", want: "database url is required"},
		{name: "unparsable url", url: "://nope", want: "parse database url"},
		{name: "unreachable server", url: "postgres://user:pass@127.0.0.1:1/none?sslmode=disable&connect_timeout=1", want: "ping database"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool, err := New(ctx, Config{URL: tt.url})
			require.ErrorContains(t, err, tt.want)
			require.Nil(t, pool)
		})
	}
}

// TestNilPoolIsSafe covers the "API without Postgres" path: every helper must
// report a clear error instead of panicking.
func TestNilPoolIsSafe(t *testing.T) {
	var pool *Pool

	require.Error(t, pool.Ping(context.Background()))
	require.NotPanics(t, pool.Close)
	require.Error(t, pool.WithWorkspace(context.Background(), uuid.New(), func(pgx.Tx) error { return nil }))
	require.Error(t, pool.WithWorkspaceRead(context.Background(), uuid.New(), func(pgx.Tx) error { return nil }))
}

// TestWithWorkspaceRequiresATenant proves the fail-closed contract: without a
// tenant id the transaction is never opened, so RLS can never be left unset.
func TestWithWorkspaceRequiresATenant(t *testing.T) {
	pool := &Pool{}

	require.ErrorContains(t,
		pool.WithWorkspace(context.Background(), uuid.Nil, func(pgx.Tx) error { return nil }),
		"workspace id is required")
	require.ErrorContains(t,
		pool.WithWorkspaceRead(context.Background(), uuid.Nil, func(pgx.Tx) error { return nil }),
		"workspace id is required")
}
