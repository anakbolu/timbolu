package repository

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/health/domain"
)

// TestPingWithoutAPool keeps the "not configured" state an error rather than a
// panic; the service turns it into a not_configured component.
func TestPingWithoutAPool(t *testing.T) {
	require.ErrorContains(t, New(nil).Ping(context.Background()), "not configured")
}

// TestPingReportsAnUnreachableDatabase proves a broken connection is reported as
// a failure, which is what makes the readiness probe useful.
func TestPingReportsAnUnreachableDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, "postgres://user:pass@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	require.Error(t, New(pool).Ping(ctx))
}

// TestRepositorySatisfiesTheDomainContract is a compile-time guarantee that the
// container can wire the repository wherever the interface is expected.
func TestRepositorySatisfiesTheDomainContract(t *testing.T) {
	var _ domain.Repository = New(nil)
}
