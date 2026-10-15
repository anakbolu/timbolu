package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	healthrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/health/repository"
	"github.com/robzlabz/employeebot/packages/platform/redis"
)

// TestRedisClientAgainstARealServer covers the happy path of the shared Redis
// client, which carries the realtime channels, rate limit buckets and the live
// quota counter.
func TestRedisClientAgainstARealServer(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "redis:7-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	require.NoError(t, err, "start redis container")

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		require.NoError(t, container.Terminate(cleanupCtx))
	})

	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "6379")
	require.NoError(t, err)

	client, err := redis.New(ctx, "redis://"+host+":"+port.Port()+"/0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	require.NoError(t, client.Ping(ctx))
	require.NotNil(t, client.Raw())
}

// TestHealthRepositoryPingAgainstPostgres runs the sqlc-generated query through
// the real repository, so the generated code and the pool wiring are covered.
func TestHealthRepositoryPingAgainstPostgres(t *testing.T) {
	dsn := migratedDatabase(t)
	ctx := t.Context()

	pool, err := newPool(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	require.NoError(t, healthrepo.New(pool).Ping(ctx))
}
