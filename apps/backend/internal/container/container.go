// Package container is the single assembly point of the application: it opens
// the connections, builds every repository, service and handler, and registers
// the routes. Nothing else calls a constructor, and no global variable exists.
//
// Each cmd/ binary calls container.New and then uses only the part it needs:
//
//	cmd/api                 → App() (Fiber) + GracefulShutdown
//	cmd/agent-worker        → Temporal() + Logger()
//	cmd/integration-worker  → Temporal() + Logger()
package container

import (
	"context"
	"fmt"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	taskactivity "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/activity"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/authn"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/config"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/logger"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/mail"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/oauth/google"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/ratelimit"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/redis"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/temporal"
)

// Container owns every long-lived dependency of one process.
type Container struct {
	Config   *config.Config
	Logger   *zap.Logger
	DB       *database.Pool
	Redis    *redis.Client
	Temporal *temporal.Client

	// Authentication primitives. They are nil when the database is not
	// configured, and the auth routes then answer 503.
	Issuer  *authn.Issuer
	States  *authn.StateSigner
	Mailer  mail.Sender
	Limiter *ratelimit.Limiter
	Google  *google.Client

	// Costs prices a model call. It is built with the gateway and shared with
	// the task runtime, so a task's own total and the ledger cannot disagree.
	Costs llmdomain.CostTable
	// Tasks is the activity set the agent worker registers. It is built in every
	// process so the API and the worker share one assembly point, and only the
	// worker runs it.
	Tasks *taskactivity.Activities

	Repositories *Repositories
	Services     *Services
	Handlers     *Handlers

	app *fiber.App
}

// options holds the dependency overrides. A dependency that was explicitly
// provided (even as nil) is never opened by New, which is what lets tests and
// the worker binaries skip connections they do not need.
type options struct {
	logger      *zap.Logger
	loggerSet   bool
	db          *database.Pool
	dbSet       bool
	redis       *redis.Client
	redisSet    bool
	temporal    *temporal.Client
	temporalSet bool
	mailer      mail.Sender
	mailerSet   bool
	google      *google.Client
	googleSet   bool
	limiter     *ratelimit.Limiter
	limiterSet  bool
	buildApp    bool
}

// Option customises container assembly.
type Option func(*options)

// WithLogger injects an existing logger.
func WithLogger(log *zap.Logger) Option {
	return func(o *options) {
		o.logger = log
		o.loggerSet = true
	}
}

// WithDB injects an existing pool. Pass nil to run without a database.
func WithDB(pool *database.Pool) Option {
	return func(o *options) {
		o.db = pool
		o.dbSet = true
	}
}

// WithRedis injects an existing Redis client. Pass nil to run without Redis.
func WithRedis(client *redis.Client) Option {
	return func(o *options) {
		o.redis = client
		o.redisSet = true
	}
}

// WithTemporal injects an existing Temporal client. Pass nil to run without it.
func WithTemporal(client *temporal.Client) Option {
	return func(o *options) {
		o.temporal = client
		o.temporalSet = true
	}
}

// WithMailer injects a mail sender. Tests use it to capture the verification
// and invitation links instead of printing them.
func WithMailer(sender mail.Sender) Option {
	return func(o *options) {
		o.mailer = sender
		o.mailerSet = true
	}
}

// WithGoogle injects a Google client, which lets tests point the endpoints at a
// stub server.
func WithGoogle(client *google.Client) Option {
	return func(o *options) {
		o.google = client
		o.googleSet = true
	}
}

// WithLimiter injects a rate limiter.
func WithLimiter(limiter *ratelimit.Limiter) Option {
	return func(o *options) {
		o.limiter = limiter
		o.limiterSet = true
	}
}

// WithoutHTTP skips building the Fiber app. The worker binaries use it so they
// never allocate an HTTP server they will not listen on.
func WithoutHTTP() Option {
	return func(o *options) { o.buildApp = false }
}

// New assembles the container for cfg. Connections that were not overridden are
// opened here and closed by Close.
func New(ctx context.Context, cfg *config.Config, opts ...Option) (*Container, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is required")
	}

	o := &options{buildApp: true}
	for _, opt := range opts {
		opt(o)
	}

	log, err := resolveLogger(cfg, o)
	if err != nil {
		return nil, err
	}

	c := &Container{Config: cfg, Logger: log}

	if err := c.openDatabase(ctx, cfg, o); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.openRedis(ctx, cfg, o); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.openTemporal(ctx, cfg, o); err != nil {
		c.Close()
		return nil, err
	}

	if o.mailerSet {
		c.Mailer = o.mailer
	}
	if o.googleSet {
		c.Google = o.google
	}
	if o.limiterSet {
		c.Limiter = o.limiter
	}

	c.Repositories = newRepositories(c.DB)
	c.Services = newServices(c.Repositories, c.Redis)

	// The auth and workspace services are assembled after the repositories,
	// because they need them, and before the handlers, because they need the
	// services.
	if err := c.openAuth(ctx, cfg); err != nil {
		c.Close()
		return nil, err
	}

	// The model gateway is assembled after the repositories, because it needs
	// them, and after the auth services for the same reason the handlers are: it
	// is wired into the route table below.
	if err := c.openLLM(ctx, cfg); err != nil {
		c.Close()
		return nil, err
	}

	// The task runtime is assembled before the conversations: a chat message is
	// handed to it, so it must exist when the chat service is built.
	if err := c.openTask(ctx, cfg); err != nil {
		c.Close()
		return nil, err
	}

	// Conversations come after the runtime: they answer with the gateway, address
	// the Bolu the registry owns, and hand a message to the task runtime, so all
	// three must be assembled first.
	if err := c.openChat(ctx, cfg); err != nil {
		c.Close()
		return nil, err
	}

	c.Handlers = newHandlers(c.Services, c.Logger, cfg)

	if o.buildApp {
		c.app = c.newApp(cfg)
		// The stream routes go on first: their middleware authenticates from the
		// query string, and the tenant middleware that follows accepts the
		// identity it resolved.
		c.registerStreamRoutes()
		c.registerRoutes()
	}

	return c, nil
}

// App returns the HTTP application, or nil when it was skipped.
func (c *Container) App() *fiber.App {
	if c == nil {
		return nil
	}
	return c.app
}

// Close releases every dependency this container opened or received. It is
// safe to call more than once.
func (c *Container) Close() {
	if c == nil {
		return
	}
	if c.Temporal != nil {
		c.Temporal.Close()
		c.Temporal = nil
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

func (c *Container) openDatabase(ctx context.Context, cfg *config.Config, o *options) error {
	if o.dbSet {
		c.DB = o.db
		return nil
	}
	if cfg.Database.URL == "" {
		// Running without a database is supported: the API then serves only
		// dependency-free routes and readiness reports not_configured.
		c.Logger.Warn("database url is empty, starting without a database")
		return nil
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
		c.Logger.Warn("redis url is empty, starting without redis")
		return nil
	}

	client, err := redis.New(ctx, cfg.Redis.URL)
	if err != nil {
		return fmt.Errorf("open redis: %w", err)
	}
	c.Redis = client
	return nil
}

func (c *Container) openTemporal(ctx context.Context, cfg *config.Config, o *options) error {
	if o.temporalSet {
		c.Temporal = o.temporal
		return nil
	}
	if cfg.Temporal.HostPort == "" {
		c.Logger.Warn("temporal host port is empty, starting without temporal")
		return nil
	}

	client, err := temporal.New(ctx, temporal.Config{
		HostPort:             cfg.Temporal.HostPort,
		Namespace:            cfg.Temporal.Namespace,
		TaskQueueAgent:       cfg.Temporal.TaskQueueAgent,
		TaskQueueIntegration: cfg.Temporal.TaskQueueIntegration,
		TLS:                  cfg.Temporal.TLS,
		APIKey:               cfg.Temporal.APIKey,
	})
	if err != nil {
		return fmt.Errorf("open temporal: %w", err)
	}
	c.Temporal = client
	return nil
}

func (c *Container) newApp(cfg *config.Config) *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:      "bolu-api",
		ReadTimeout:  cfg.Http.ReadTimeout,
		WriteTimeout: cfg.Http.WriteTimeout,
		ErrorHandler: errorHandler(c.Logger),
	})
	app.Use(requestContextMiddleware(c.Logger))
	app.Use(corsMiddleware(cfg.Application.CORSOrigins))
	return app
}
