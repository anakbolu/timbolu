// Package container is the gateway's single assembly point.
//
// It is a smaller container than the API's on purpose: the gateway owns the
// sockets, the sealed sessions, the fleet directory, and the media bytes, and it
// owns nothing else. There is no Fiber app, no Temporal client, and no product
// module here — the product is reached over the internal seam, through the same
// shared secret the other direction uses.
//
// What it does build is the four things the engine cannot run without: a
// database pool, a Redis client for the session lease, the key ring that seals a
// device blob, and object storage for what a customer sent. Missing any of them
// is a process that cannot do its job, so assembly fails rather than starting a
// gateway that holds a socket nobody asked anything of.
package container

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/whatsapp-gw/internal/engine"
	"github.com/anakbolu/timbolu/apps/whatsapp-gw/internal/media"
	"github.com/anakbolu/timbolu/apps/whatsapp-gw/internal/repository"
	"github.com/anakbolu/timbolu/apps/whatsapp-gw/internal/sealer"
	"github.com/anakbolu/timbolu/packages/platform/config"
	"github.com/anakbolu/timbolu/packages/platform/database"
	"github.com/anakbolu/timbolu/packages/platform/kms"
	"github.com/anakbolu/timbolu/packages/platform/logger"
	"github.com/anakbolu/timbolu/packages/platform/metrics"
	"github.com/anakbolu/timbolu/packages/platform/redis"
	"github.com/anakbolu/timbolu/packages/platform/storage"
	"github.com/anakbolu/timbolu/packages/platform/telemetry"
	"github.com/anakbolu/timbolu/packages/whatsapp/seam"
)

// Container holds everything the gateway process built.
type Container struct {
	Config *config.Config
	Logger *zap.Logger

	DB    *database.Pool
	Redis *redis.Client

	// Telemetry and Metrics are the providers the process reports through. Either
	// may be nil, which is what a deployment that named no exporter looks like.
	Telemetry *telemetry.Provider
	Metrics   *metrics.Provider

	// Keys is the key ring that seals a device blob. It is the same ring the hub
	// uses, so a session sealed by the API opens here.
	Keys *kms.Local

	// Sessions and Directory are the gateway's own narrow view of the schema: the
	// device blobs it reads and writes, and the fleet it follows.
	Sessions  *repository.Repository
	Directory *repository.Repository
	// Sealer seals and opens a session with the workspace's own data key.
	Sealer *sealer.Sealer
	// Media stores and reads the bytes of what a customer sent.
	Media *media.Store
	// Product is the client that reaches the API's internal seam: the effects the
	// engine has on the product travel this way.
	Product *seam.ProductClient

	// Engine is the whatsmeow driver, and Gateway is the surface the API reaches
	// it through. Both are built here because the engine is both halves at once:
	// it records through Product and it is served through Gateway.
	Engine  *engine.Engine
	Gateway *seam.Gateway
}

// options holds the dependency overrides a test supplies.
type options struct {
	logger    *zap.Logger
	loggerSet bool
	db        *database.Pool
	dbSet     bool
	redis     *redis.Client
	redisSet  bool
}

// Option customises assembly.
type Option func(*options)

// WithLogger injects an existing logger.
func WithLogger(log *zap.Logger) Option {
	return func(o *options) { o.logger = log; o.loggerSet = true }
}

// WithDB injects an existing pool.
func WithDB(pool *database.Pool) Option {
	return func(o *options) { o.db = pool; o.dbSet = true }
}

// WithRedis injects an existing Redis client.
func WithRedis(client *redis.Client) Option {
	return func(o *options) { o.redis = client; o.redisSet = true }
}

// New assembles the gateway container. Every connection it opened is released by
// Close.
func New(ctx context.Context, cfg *config.Config, opts ...Option) (*Container, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is required")
	}

	o := &options{}
	for _, opt := range opts {
		opt(o)
	}

	log, err := resolveLogger(cfg, o)
	if err != nil {
		return nil, err
	}

	// Whatever the loader could not use from the environment is said here, which is the first
	// point in this process with a logger: the gateway shares the loader with the backend, so a
	// setting it ignores has to be named here too rather than only where the setting is used.
	for _, warning := range cfg.Warnings() {
		log.Warn(warning)
	}

	c := &Container{Config: cfg, Logger: log}

	// Telemetry and metrics come first: everything assembled after them may start
	// a span or record a measurement, and a provider installed later would miss
	// the first of both.
	if err := c.openTelemetry(ctx, cfg); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.openMetrics(ctx, cfg); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.openDatabase(ctx, cfg, o); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.openRedis(ctx, cfg, o); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.openWhatsApp(ctx, cfg); err != nil {
		c.Close()
		return nil, err
	}

	return c, nil
}

// Close releases every dependency this container opened or received. It is safe
// to call more than once.
func (c *Container) Close() {
	if c == nil {
		return
	}
	if c.Metrics != nil {
		_ = c.Metrics.Shutdown(context.Background())
		c.Metrics = nil
	}
	if c.Telemetry != nil {
		_ = c.Telemetry.Shutdown(context.Background())
		c.Telemetry = nil
	}
	if c.Redis != nil {
		_ = c.Redis.Close()
		c.Redis = nil
	}
	if c.DB != nil {
		c.DB.Close()
		c.DB = nil
	}
	if c.Logger != nil {
		_ = c.Logger.Sync()
	}
}

func resolveLogger(cfg *config.Config, o *options) (*zap.Logger, error) {
	if o.loggerSet && o.logger != nil {
		return o.logger, nil
	}

	log, err := logger.New(cfg.Logging.Level, cfg.Logging.Format)
	if err != nil {
		return nil, fmt.Errorf("build logger: %w", err)
	}

	return log, nil
}

// openTelemetry installs the tracer provider. A deployment with no exporter
// still gets one, so a log line and a request stay correlated.
func (c *Container) openTelemetry(ctx context.Context, cfg *config.Config) error {
	provider, err := telemetry.Setup(ctx, telemetry.Config{
		ServiceName: cfg.Telemetry.ServiceName,
		Environment: cfg.Application.Environment,
		Exporter:    cfg.Telemetry.Exporter,
		Endpoint:    cfg.Telemetry.Endpoint,
		SampleRatio: cfg.Telemetry.SampleRatio,
		Insecure:    cfg.Telemetry.Insecure,
	}, c.Logger)
	if err != nil {
		return err
	}

	c.Telemetry = provider

	return nil
}

// openMetrics installs the meter, or leaves it nil when the deployment named no
// exporter for it.
func (c *Container) openMetrics(ctx context.Context, cfg *config.Config) error {
	exporter := strings.ToLower(strings.TrimSpace(cfg.Telemetry.MetricsExporter))
	if exporter == "" || exporter == metrics.ExporterNone {
		c.Logger.Info("metrics are disabled: no exporter configured")
		return nil
	}

	provider, err := metrics.Setup(ctx, metrics.Config{
		ServiceName: cfg.Telemetry.ServiceName,
		Environment: cfg.Application.Environment,
		Exporter:    cfg.Telemetry.MetricsExporter,
		Endpoint:    cfg.Telemetry.MetricsEndpoint,
		Interval:    cfg.Telemetry.MetricsInterval,
		Insecure:    cfg.Telemetry.Insecure,
	}, c.Logger)
	if err != nil {
		return err
	}

	c.Metrics = provider

	return nil
}

func (c *Container) openDatabase(ctx context.Context, cfg *config.Config, o *options) error {
	if o.dbSet {
		c.DB = o.db
		return nil
	}
	if cfg.Database.URL == "" {
		return fmt.Errorf("whatsapp-gw: no database url is configured (set DATABASE_URL)")
	}

	pool, err := database.New(ctx, database.Config{
		URL:             cfg.Database.URL,
		MaxOpenConns:    cfg.Database.MaxOpenConns,
		MaxIdleConns:    cfg.Database.MaxIdleConns,
		ConnMaxLifetime: cfg.Database.ConnMaxLifetime,
	})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	c.DB = pool

	return nil
}

func (c *Container) openRedis(ctx context.Context, cfg *config.Config, o *options) error {
	if o.redisSet {
		c.Redis = o.redis
		return nil
	}
	if cfg.Redis.URL == "" {
		return fmt.Errorf("whatsapp-gw: no redis url is configured (set REDIS_URL)")
	}

	client, err := redis.New(ctx, cfg.Redis.URL)
	if err != nil {
		return fmt.Errorf("open redis: %w", err)
	}
	c.Redis = client

	return nil
}

// openWhatsApp builds the engine and the surface it is served through.
//
// It runs last because it depends on everything above: the database holds the
// sessions, Redis holds the lease, the key ring seals a blob, object storage
// holds the bytes, and the seam client is how the engine reaches the product.
func (c *Container) openWhatsApp(ctx context.Context, cfg *config.Config) error {
	if c.DB == nil {
		return fmt.Errorf("whatsapp-gw: the gateway needs a database")
	}
	if c.Redis == nil {
		// Two clients on one session is the failure that gets a number banned, and
		// the lease is what keeps them apart. A gateway without Redis cannot tell
		// whether it is alone, so it does not start.
		return fmt.Errorf("whatsapp-gw: the gateway needs Redis for the session lease")
	}
	if strings.TrimSpace(cfg.WhatsApp.APIURL) == "" {
		return fmt.Errorf("whatsapp-gw: no API address is configured (set WA_API_URL)")
	}
	if cfg.WhatsApp.GatewayToken == "" {
		return fmt.Errorf("whatsapp-gw: no gateway token is configured (set WA_GATEWAY_TOKEN)")
	}

	keys, err := kms.NewLocalFromConfig(cfg.Kms.MasterKey, cfg.Kms.MasterKeyID, cfg.Kms.RetiredKeys)
	if err != nil {
		return fmt.Errorf("whatsapp-gw: key management: %w", err)
	}
	if keys == nil {
		return fmt.Errorf("whatsapp-gw: the gateway needs the key ring to seal sessions (set KMS_MASTER_KEY)")
	}
	c.Keys = keys
	if retired := keys.RetiredIDs(); len(retired) > 0 {
		c.Logger.Info("key management is reading retired keys",
			zap.String("active", keys.ActiveKeyID()),
			zap.Strings("retired", retired),
		)
	}

	store, err := openStorage(cfg)
	if err != nil {
		return err
	}
	if store == nil {
		return fmt.Errorf("whatsapp-gw: the gateway needs object storage for media (set STORAGE_DRIVER)")
	}

	// The gateway's own repositories: the sessions it owns and the directory it
	// follows. They share the schema with the backend and nothing else.
	c.Sessions = repository.New(c.DB)
	c.Directory = repository.New(c.DB)
	c.Sealer = sealer.New(keys, repository.NewKeyStore(c.DB))
	c.Media = media.New(store)
	c.Product = seam.NewProductClient(cfg.WhatsApp.APIURL, cfg.WhatsApp.GatewayToken, 30*time.Second)

	c.Engine = engine.NewEngine(engineConfig(cfg), engine.EngineDeps{
		Sessions:  c.Sessions,
		Sealer:    c.Sealer,
		Directory: c.Directory,
		Lease:     engine.NewRedisLease(c.Redis.Raw()),
		Inbox:     c.Product,
		Drain:     c.Product,
		Status:    c.Product,
		Links:     c.Product,
		Media:     c.Media,
	}, c.Logger)

	if cfg.WhatsApp.LogWhatsmeow {
		c.Engine = c.Engine.WithLogger(engine.WALogger(c.Logger))
	}

	c.Gateway = seam.NewGateway(c.Engine, seam.GatewayConfig{
		Token: cfg.WhatsApp.GatewayToken,
	}, c.Logger)

	return nil
}

// engineConfig maps the deployment's settings onto the engine's, leaving every
// value the deployment did not name at its default.
func engineConfig(cfg *config.Config) engine.EngineConfig {
	out := engine.DefaultEngineConfig()
	out.Owner = cfg.WhatsApp.Owner
	if cfg.WhatsApp.LeaseTTL > 0 {
		out.LeaseTTL = cfg.WhatsApp.LeaseTTL
	}
	if cfg.WhatsApp.LeaseRenew > 0 {
		out.LeaseRenew = cfg.WhatsApp.LeaseRenew
	}
	if cfg.WhatsApp.TypingDelay > 0 {
		out.TypingDelay = cfg.WhatsApp.TypingDelay
	}
	if cfg.WhatsApp.MaxMediaBytes > 0 {
		out.MaxMediaBytes = cfg.WhatsApp.MaxMediaBytes
	}

	return out
}

// openStorage builds the configured object storage driver.
//
// The bytes stay in the same bucket the backend reads them from, under the same
// key, so the process that downloads an attachment and the process that serves
// it to a screen agree on where it is without either asking the other.
func openStorage(cfg *config.Config) (storage.Store, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Storage.Driver)) {
	case "":
		return nil, nil

	case "local":
		store, err := storage.NewLocal(cfg.Storage.LocalRoot)
		if err != nil {
			return nil, fmt.Errorf("build local storage: %w", err)
		}
		return store, nil

	case "s3":
		store, err := storage.NewS3(context.Background(), storage.S3Config{
			Endpoint:  cfg.Storage.Endpoint,
			Region:    cfg.Storage.Region,
			Bucket:    cfg.Storage.Bucket,
			AccessKey: cfg.Storage.AccessKey,
			SecretKey: cfg.Storage.SecretKey,
			UseSSL:    cfg.Storage.UseSSL,
			PathStyle: cfg.Storage.PathStyle,
		})
		if err != nil {
			return nil, fmt.Errorf("build s3 storage: %w", err)
		}
		return store, nil

	default:
		return nil, fmt.Errorf("unknown storage driver %q", cfg.Storage.Driver)
	}
}
