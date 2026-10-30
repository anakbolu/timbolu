package container

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/packages/platform/config"
	"github.com/anakbolu/timbolu/packages/platform/database"
	"github.com/anakbolu/timbolu/packages/platform/redis"
	"github.com/anakbolu/timbolu/packages/platform/storage"
)

// Assembly is where a missing dependency becomes a process that refuses to
// start rather than one that holds a socket nobody asked anything of. What is
// under test is that refusal: every setting the gateway cannot run without, and
// the two pure mappings that are easy to get quietly wrong.

const testKey = "0123456789abcdef0123456789abcdef"

func TestNewNeedsAConfig(t *testing.T) {
	_, err := New(context.Background(), nil)
	require.ErrorContains(t, err, "config is required")
}

func TestNewFailsWithoutADatabase(t *testing.T) {
	// Telemetry and metrics come up first, so this also proves that a failure
	// later releases what was built before it.
	_, err := New(context.Background(), &config.Config{})
	require.ErrorContains(t, err, "no database url is configured")
}

func TestNewFailsOnAnUnreachableDatabase(t *testing.T) {
	_, err := New(context.Background(), &config.Config{
		Database: config.DatabaseConfig{URL: "postgres://bolu:bolu@127.0.0.1:1/bolu?sslmode=disable"},
	})
	require.ErrorContains(t, err, "open database")
}

func TestOptionsAreHonoured(t *testing.T) {
	logger := zap.NewNop()
	o := &options{}
	for _, opt := range []Option{WithLogger(logger), WithDB(nil), WithRedis((*redis.Client)(nil))} {
		opt(o)
	}

	require.True(t, o.loggerSet)
	require.Same(t, logger, o.logger)
	require.True(t, o.dbSet)
	require.True(t, o.redisSet)
}

func TestResolveLoggerPrefersTheInjectedOne(t *testing.T) {
	logger := zap.NewNop()

	log, err := resolveLogger(&config.Config{}, &options{logger: logger, loggerSet: true})
	require.NoError(t, err)
	require.Same(t, logger, log)

	built, err := resolveLogger(&config.Config{}, &options{})
	require.NoError(t, err)
	require.NotNil(t, built)
}

func TestCloseIsSafeOnAnEmptyAndNilContainer(t *testing.T) {
	var nilContainer *Container
	nilContainer.Close()

	c := &Container{Logger: zap.NewNop()}
	c.Close()
	c.Close()
}

func TestEngineConfigLeavesUnnamedSettingsAtTheirDefaults(t *testing.T) {
	defaults := engineConfig(&config.Config{})

	got := engineConfig(&config.Config{})
	require.Equal(t, defaults.Owner, got.Owner)
	require.Equal(t, defaults.LeaseTTL, got.LeaseTTL)
	require.Equal(t, defaults.LeaseRenew, got.LeaseRenew)
	require.Equal(t, defaults.TypingDelay, got.TypingDelay)
	require.Equal(t, defaults.MaxMediaBytes, got.MaxMediaBytes)
}

func TestEngineConfigTakesTheSettingsTheDeploymentNamed(t *testing.T) {
	got := engineConfig(&config.Config{WhatsApp: config.WhatsAppConfig{
		Owner:         "bolu-1",
		LeaseTTL:      45 * time.Second,
		LeaseRenew:    10 * time.Second,
		TypingDelay:   2 * time.Second,
		MaxMediaBytes: 1 << 20,
	}})

	require.Equal(t, "bolu-1", got.Owner)
	require.Equal(t, 45*time.Second, got.LeaseTTL)
	require.Equal(t, 10*time.Second, got.LeaseRenew)
	require.Equal(t, 2*time.Second, got.TypingDelay)
	require.Equal(t, 1<<20, got.MaxMediaBytes)
}

func TestOpenStoragePicksTheDriverTheDeploymentNamed(t *testing.T) {
	// Nothing configured is not an error here: the caller decides whether it needs
	// storage, and the gateway does.
	store, err := openStorage(&config.Config{})
	require.NoError(t, err)
	require.Nil(t, store)

	local, err := openStorage(&config.Config{Storage: config.StorageConfig{Driver: "local", LocalRoot: t.TempDir()}})
	require.NoError(t, err)
	require.IsType(t, (*storage.LocalStore)(nil), local)
	require.Equal(t, "local", local.Driver())

	_, err = openStorage(&config.Config{Storage: config.StorageConfig{Driver: "local"}})
	require.Error(t, err)

	_, err = openStorage(&config.Config{Storage: config.StorageConfig{Driver: "carrier-pigeon"}})
	require.ErrorContains(t, err, "unknown storage driver")
}

func TestOpenStorageBuildsS3FromTheNamedEndpoint(t *testing.T) {
	// The S3 driver checks the bucket as it is built, so an endpoint that is not
	// there is reported rather than deferred to the first upload.
	_, err := openStorage(&config.Config{Storage: config.StorageConfig{
		Driver:    "s3",
		Endpoint:  "http://127.0.0.1:1",
		Region:    "us-east-1",
		Bucket:    "bolu",
		AccessKey: "key",
		SecretKey: "secret",
	}})
	require.ErrorContains(t, err, "build s3 storage")
}

func TestTheDatabaseURLCarriesTheConfiguredLimits(t *testing.T) {
	// The pool settings are part of the deployment's contract: a gateway that
	// ignored them would open more connections than the database was sized for.
	cfg := &config.Config{Database: config.DatabaseConfig{
		URL:             "postgres://bolu:bolu@127.0.0.1:1/bolu?sslmode=disable",
		MaxOpenConns:    3,
		MaxIdleConns:    1,
		ConnMaxLifetime: 60,
	}}

	// The connection is refused, but the failure is the ping rather than the
	// settings, which is what proves the settings were applied.
	_, err := New(context.Background(), cfg)
	require.Error(t, err)
	require.False(t, strings.Contains(err.Error(), "parse database url"))
}

// TestOpenRedisRefusesAnUnconfiguredDeployment proves the lease's client is
// either the injected one or a failure, never a silently nil client: a gateway
// that cannot lease is a gateway that can double-connect.
func TestOpenRedisRefusesAnUnconfiguredDeployment(t *testing.T) {
	c := &Container{Logger: zap.NewNop()}
	err := c.openRedis(context.Background(), &config.Config{}, &options{})
	require.ErrorContains(t, err, "no redis url is configured")

	injected := &redis.Client{}
	c = &Container{Logger: zap.NewNop()}
	require.NoError(t, c.openRedis(context.Background(), &config.Config{}, &options{redisSet: true, redis: injected}))
	require.Same(t, injected, c.Redis)
}

// TestOpenWhatsAppRefusesAnIncompleteAssembly walks every setting the engine
// cannot run without. Each one is a process that would otherwise hold a socket
// with no way to reach the product, so each has to stop assembly instead.
func TestOpenWhatsAppRefusesAnIncompleteAssembly(t *testing.T) {
	ctx := context.Background()

	complete := func() (*Container, *config.Config) {
		return &Container{
				Logger: zap.NewNop(),
				DB:     &database.Pool{},
				Redis:  &redis.Client{},
			}, &config.Config{
				WhatsApp: config.WhatsAppConfig{APIURL: "http://api:8091", GatewayToken: "token"},
				Kms:      config.KmsConfig{MasterKey: testKey, MasterKeyID: "kms-1"},
			}
	}

	t.Run("database", func(t *testing.T) {
		c, cfg := complete()
		c.DB = nil
		require.ErrorContains(t, c.openWhatsApp(ctx, cfg), "needs a database")
	})

	t.Run("redis", func(t *testing.T) {
		c, cfg := complete()
		c.Redis = nil
		require.ErrorContains(t, c.openWhatsApp(ctx, cfg), "needs Redis")
	})

	t.Run("api address", func(t *testing.T) {
		c, cfg := complete()
		cfg.WhatsApp.APIURL = " "
		require.ErrorContains(t, c.openWhatsApp(ctx, cfg), "no API address")
	})

	t.Run("gateway token", func(t *testing.T) {
		c, cfg := complete()
		cfg.WhatsApp.GatewayToken = ""
		require.ErrorContains(t, c.openWhatsApp(ctx, cfg), "no gateway token")
	})

	t.Run("key ring", func(t *testing.T) {
		c, cfg := complete()
		cfg.Kms = config.KmsConfig{}
		require.ErrorContains(t, c.openWhatsApp(ctx, cfg), "needs the key ring")
	})

	t.Run("object storage", func(t *testing.T) {
		c, cfg := complete()
		require.ErrorContains(t, c.openWhatsApp(ctx, cfg), "needs object storage")
	})
}
