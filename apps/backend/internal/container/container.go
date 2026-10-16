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
	"time"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	billingdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/billing/domain"
	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	memoryactivity "github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/activity"
	memorydomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/domain"
	orderdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/order/domain"
	retentiondomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/retention/domain"
	routineactivity "github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/activity"
	routinedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/domain"
	taskactivity "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/activity"
	vcomputertemporal "github.com/robzlabz/employeebot/apps/backend/internal/modules/vcomputer/temporal"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/authn"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/mail"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/oauth/google"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/ratelimit"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/temporal"
	"github.com/robzlabz/employeebot/packages/platform/config"
	"github.com/robzlabz/employeebot/packages/platform/database"
	"github.com/robzlabz/employeebot/packages/platform/kms"
	"github.com/robzlabz/employeebot/packages/platform/logger"
	"github.com/robzlabz/employeebot/packages/platform/metrics"
	"github.com/robzlabz/employeebot/packages/platform/redis"
	"github.com/robzlabz/employeebot/packages/platform/telemetry"
	whatsappdomain "github.com/robzlabz/employeebot/packages/whatsapp/domain"
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
	Issuer *authn.Issuer
	States *authn.StateSigner
	Mailer mail.Sender
	// AsyncMailer is the worker pool behind Mailer when the SMTP driver is
	// configured. It is nil for the log driver, and Close drains it so a message
	// queued by the last request still leaves.
	AsyncMailer *mail.AsyncSender
	Limiter     *ratelimit.Limiter
	Google      *google.Client

	// Costs prices a model call. It is built with the gateway and shared with
	// the task runtime, so a task's own total and the ledger cannot disagree.
	Costs llmdomain.CostTable
	// Tasks is the activity set the agent worker registers. It is built in every
	// process so the API and the worker share one assembly point, and only the
	// worker runs it.
	Tasks *taskactivity.Activities
	// Memory is the memory service, kept on the container because the chat
	// service and the extraction schedule both reach it before the handler set is
	// built.
	Memory memorydomain.Service
	// MemoryTasks is the extraction activity set, registered by the agent worker
	// alongside the task one.
	MemoryTasks *memoryactivity.Activities
	// Routine is the routine service, kept on the container because the task runtime
	// reports a finished task's outcome back through it and the workspace update
	// reschedules through it.
	Routine routinedomain.Service
	// RoutineTasks is the routine activity set, registered by the agent worker.
	RoutineTasks *routineactivity.Activities
	// VComputerTasks is the virtual computer's activity set, registered by the agent worker
	// alongside the task one. It is built in every process, even when no control plane is
	// configured: the activities then answer domain.ErrNotConfigured, which is a deployment
	// without a virtual computer rather than a worker that has never heard of one.
	VComputerTasks *vcomputertemporal.Activities
	// WhatsApp is the module: the numbers a workspace linked and who answers them.
	// It is kept on the container because the gateway process hands it its own
	// runtime, and because the integration adapter reaches it.
	WhatsApp whatsappdomain.Service
	// Order is the workspace's own commerce data, kept here because the integration
	// adapter resolves it when a tool is called: the adapter is registered with the
	// hub before this module exists, the way the WhatsApp adapter is, and it looks
	// the module up rather than holding it.
	Order orderdomain.Service
	// Billing is the module: the package a workspace is on, the allowance it grants,
	// what it owes, and the sweep that reconciles the three. It is kept here because
	// three other modules ask it a question — the model gateway for an allowance, the
	// Bolu registry for a cap, and the tenant middleware for whether a write is
	// allowed at all.
	Billing billingdomain.Service
	// Retention is the job that keeps the deployment's content windows: it removes
	// what a task step recorded and what an attachment held once they are past their
	// window, and leaves the rows and their shape alone. It is kept on the container
	// because it runs on a timer in the API process rather than on a request.
	Retention retentiondomain.Service
	// Metrics is the meter the whole process reports to: what the task runtime accepted and
	// spent, what people did with its drafts, and what the model chain cost. It is nil when the
	// deployment named no exporter, which is what makes every recording call a no-op rather than
	// a branch at each call site.
	Metrics *metrics.Provider
	// Telemetry is the tracer provider the whole process reports to: the HTTP middleware, the
	// Temporal client and workers, and the model and tool calls inside them. It is kept on the
	// container because it must be shut down last, after the spans of the last request it
	// started have been flushed.
	Telemetry *telemetry.Provider
	// Keys is the key ring that wraps a workspace's data key. It is kept here
	// because two modules seal credentials with it: the hub seals tokens, and this
	// one seals a WhatsApp session.
	Keys *kms.Local

	Repositories *Repositories
	Services     *Services
	Handlers     *Handlers

	app *fiber.App
}

// mailDrainTimeout bounds the wait for queued mail at shutdown: enough for the
// attempts of one message, short enough that a stuck SMTP server cannot hold the
// process open.
const mailDrainTimeout = 15 * time.Second

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
	// adapters are the integration adapters this deployment speaks. A test
	// supplies its own, which is how a provider's flow is exercised without
	// reaching the provider.
	adapters    []integrationdomain.Adapter
	adaptersSet bool
	// clock pins the approval gate's sense of the time. A test supplies one because
	// the working-hours window is read from it: without pinning, whether an
	// approved action leaves or is held depends on when the suite runs.
	clock    func() time.Time
	buildApp bool
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

// WithIntegrationAdapters replaces the applications this deployment speaks.
//
// The list is what the integration hub registers, so a test can exercise a
// provider's flow against a stub instead of the provider.
func WithIntegrationAdapters(adapters ...integrationdomain.Adapter) Option {
	return func(o *options) {
		o.adapters = adapters
		o.adaptersSet = true
	}
}

// WithApprovalClock pins the time the approval gate reads.
//
// The working-hours rule asks the clock whether the workspace is open, so a test
// that drives an approved action through the gate would otherwise pass during
// Jakarta's working day and fail outside it. Pinning it in the test is what makes
// the assertion about the executor rather than about the hour the suite ran.
func WithApprovalClock(clock func() time.Time) Option {
	return func(o *options) { o.clock = clock }
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

	// What the loader could not use from the environment is said here, which is the first point
	// in the process with a logger. The process starts either way — that is the whole reason the
	// loader collected these instead of refusing — so the one thing left is that a person reading
	// the log learns which setting was ignored before a call behaves unlike its configuration.
	for _, warning := range cfg.Warnings() {
		log.Warn(warning)
	}

	c := &Container{Config: cfg, Logger: log}

	// Telemetry comes first: everything assembled after it may start a span, and a provider
	// installed halfway through the assembly would leave the first spans of the process
	// unattached to anything — including the trace that the API request started.
	if err := c.openTelemetry(ctx, cfg); err != nil {
		c.Close()
		return nil, err
	}

	// Metrics are installed beside the tracer and for the same reason: everything assembled
	// after this point may report something, and an instrument built after the fact would miss
	// the first measurements of the process.
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

	// The operator surface comes after the auth service, whose account read it
	// guards its routes with, and before the handlers, which is when the route
	// table is built.
	if err := c.openAdmin(ctx, cfg); err != nil {
		c.Close()
		return nil, err
	}

	// Billing comes before the model gateway, because the gateway's quota policy is
	// this module's answer: a gateway built first would hold the configured fixed
	// allowance for the life of the process, and every workspace would be served on
	// it. It also comes before the integration hub, which asks it for a cap.
	if err := c.openBilling(ctx, cfg); err != nil {
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

	// The integration hub is assembled before the approval gate, because the gate
	// asks it for a token when an approved action is finally carried out.
	if err := c.openIntegration(ctx, cfg, o); err != nil {
		c.Close()
		return nil, err
	}

	// The approval gate comes before the task runtime: the runtime asks it whether
	// an action may leave, and a task that could not ask would have nothing
	// stopping it from sending.
	if err := c.openApproval(ctx, cfg, o); err != nil {
		c.Close()
		return nil, err
	}

	// The approval module owns the audit trail, so the three modules that record a
	// sensitive action learn their writer now. Everything between their construction and
	// this line is a process starting up, before any request is served.
	c.UseWorkspaceAuditor()
	c.UseIntegrationAuditor()
	c.UseModelAuditor()

	// Memory comes after the gateway, because a write is embedded before it is
	// stored, and after the approval repository, because the trail it writes to
	// lives there. It comes before the task runtime and the conversations, which
	// both ask it for something.
	if err := c.openMemory(ctx, cfg); err != nil {
		c.Close()
		return nil, err
	}

	// The task runtime is assembled before the conversations: a chat message is
	// handed to it, so it must exist when the chat service is built.
	if err := c.openTask(ctx, cfg); err != nil {
		c.Close()
		return nil, err
	}

	// Routines come after the task runtime, because a run's work becomes a task, and
	// before the conversations, because a chat message may start one.
	if err := c.openRoutine(ctx, cfg); err != nil {
		c.Close()
		return nil, err
	}

	// The virtual computer is assembled here because it depends on nothing above it: it is
	// an HTTP client to a control plane, so it needs no database, no Redis and no Temporal
	// client, and it is built even in a process that has none of them. It comes after the
	// routines only so that the order the assembly reads in matches the order the modules are
	// described in — the wiring itself is independent.
	c.openVComputer(cfg)

	// WhatsApp comes after the integrations, the task runtime, and the routines:
	// a linked number is a connection the hub shows, an inbound message opens a
	// task, and a routine may be watching the message event.
	if err := c.openWhatsApp(ctx, cfg); err != nil {
		c.Close()
		return nil, err
	}

	// The order module comes after the integration hub, which registers its adapter as
	// one of the applications this deployment speaks, and before the task runtime's
	// calls reach it: a tool call is what reaches the module, and the runtime is
	// assembled with the executor that makes those calls.
	if err := c.openOrder(ctx, cfg); err != nil {
		c.Close()
		return nil, err
	}

	// The checklist module comes after the integration hub for the same reason the order module
	// does — its adapter is registered while the hub is assembled — and before the task runtime's
	// calls reach it: a plan is written, ticked, and thrown away through the tools, and the runtime is
	// assembled with the executor that makes those calls.
	if err := c.openChecklist(ctx, cfg); err != nil {
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

	// Retention is assembled last of the modules, because it prunes none of the tables
	// itself: it removes task step contents through the task repository and attachments
	// through the chat service, so both must exist. That order is the design rather than
	// an accident — the module that owns a table is the module that writes it.
	if err := c.openRetention(ctx, cfg); err != nil {
		c.Close()
		return nil, err
	}

	c.Handlers = newHandlers(c.Services, c.Logger, cfg)

	// A changed timezone or working window moves every future routine run, so the
	// engine is told rather than left to fire at the old wall-clock time. Nothing is
	// cached to invalidate: this re-registers the schedules, it does not recompute a
	// stored time.
	if c.Handlers.Workspace != nil && c.Routine != nil {
		c.Handlers.Workspace = c.Handlers.Workspace.WithClockChange(c.RescheduleRoutines)
	}

	// A decision is handed to the task waiting on it through Temporal. It is
	// installed here because the handler is built before the client is known to it,
	// and because a decision that cannot be delivered is not an error: the workflow
	// reads its own status at its next checkpoint.
	if c.Handlers.Approval != nil && c.Temporal != nil {
		c.Handlers.Approval = c.Handlers.Approval.WithSignaler(approvalSignaler{client: c.Temporal})
	}

	if o.buildApp {
		c.app = c.newApp(cfg)
		// The stream routes go on first: their middleware authenticates from the
		// query string, and the tenant middleware that follows accepts the
		// identity it resolved.
		c.registerStreamRoutes()
		// The scrape endpoint goes on before the module routes and outside their prefix: a
		// scraper has no session and no workspace, so it must not be behind a guard that would
		// ask it for one.
		c.registerMetricsRoutes()
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
	// Queued mail first: a verification link the last request produced is worth a
	// bounded wait, and it needs the logger below to report a failure.
	if c.AsyncMailer != nil {
		drainCtx, cancel := context.WithTimeout(context.Background(), mailDrainTimeout)
		if err := c.AsyncMailer.Close(drainCtx); err != nil {
			c.Logger.Warn("mail queue did not drain before shutdown", zap.Error(err))
		}
		cancel()
		c.AsyncMailer = nil
	}
	// Metrics first: a measurement's last push is cheap, and a span that cannot be flushed is a
	// lost trace rather than a lost count.
	if c.Metrics != nil {
		_ = c.Metrics.Shutdown(context.Background())
		c.Metrics = nil
	}
	if c.Telemetry != nil {
		// The flush gets a bounded moment of its own: a process shutting down on a signal has
		// already lost the caller's deadline, and the buffered spans of the last seconds are
		// exactly what somebody is looking at.
		_ = c.Telemetry.Shutdown(context.Background())
		c.Telemetry = nil
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

	// The tracing interceptor carries a trace across the process boundary: a workflow's span
	// is a child of the request that started it, and an activity's span is a child of the
	// workflow's even when another process runs it.
	tracing, err := c.temporalInterceptors()
	if err != nil {
		return fmt.Errorf("open temporal: %w", err)
	}

	client, err := temporal.New(ctx, temporal.Config{
		HostPort:             cfg.Temporal.HostPort,
		Namespace:            cfg.Temporal.Namespace,
		TaskQueueAgent:       cfg.Temporal.TaskQueueAgent,
		TaskQueueIntegration: cfg.Temporal.TaskQueueIntegration,
		TLS:                  cfg.Temporal.TLS,
		APIKey:               cfg.Temporal.APIKey,
		Interceptors:         temporalClientInterceptors(tracing),
		Logger:               c.Logger,
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
	// Tracing goes on first so the request's trace id comes from its own span, and the logger
	// the next middleware builds is the same request as the trace.
	app.Use(c.tracingMiddleware())
	app.Use(requestContextMiddleware(c.Logger))
	app.Use(corsMiddleware(cfg.Application.CORSOrigins))
	return app
}
