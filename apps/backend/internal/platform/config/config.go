package config

import (
	"strings"
	"time"
)

// Config is the root application configuration. Values come from the YAML file
// matching ENVIRONMENT and are then overridden by environment variables.
type Config struct {
	Application  AppConfig
	Http         HttpConfig
	Database     DatabaseConfig
	Redis        RedisConfig
	Temporal     TemporalConfig
	JWT          JWTConfig
	Auth         AuthConfig
	Google       GoogleConfig
	Mail         MailConfig
	Plan         PlanConfig
	Llm          LlmConfig
	Kms          KmsConfig
	Integrations IntegrationConfig
	Tasks        TasksConfig
	Memory       MemoryConfig
	Storage      StorageConfig
	WhatsApp     WhatsAppConfig
	Billing      BillingConfig
	Retention    RetentionConfig
	Telemetry    TelemetryConfig
	Logging      LoggingConfig
}

// DefaultServiceName names this binary in the traces, unless the deployment named it.
func (t *TelemetryConfig) DefaultServiceName(name string) {
	if strings.TrimSpace(t.ServiceName) == "" {
		t.ServiceName = name
	}
}

// TelemetryConfig is where a deployment's traces go.
//
// The default is nowhere: a deployment that has not said where telemetry should be sent should not
// be sending it. The provider is installed either way, so a request still has a trace id and its
// log lines can be correlated — only the spans are dropped.
type TelemetryConfig struct {
	// ServiceName is what the spans are filed under. Each binary names itself, so a collector
	// groups the API and the workers separately rather than under one name — which is what an
	// incident reads as: one process or several.
	ServiceName string `mapstructure:"ServiceName" validate:"optional"`
	// Exporter is "none", "console" (a development run) or "otlp" (a collector).
	Exporter string `mapstructure:"Exporter" validate:"optional"`
	// Endpoint is the collector's host:port, required by the otlp exporter.
	Endpoint string `mapstructure:"Endpoint" validate:"optional"`
	// SampleRatio is the share of root traces kept, between 0 and 1. A trace a caller already
	// sampled is always kept: dropping the work of a sampled request would leave a hole exactly
	// where the work happened.
	SampleRatio float64 `mapstructure:"SampleRatio" validate:"optional"`
	// Insecure drops TLS to the collector, which is what a collector on the compose network
	// wants.
	Insecure bool `mapstructure:"Insecure" validate:"optional"`

	// MetricsExporter is where measurements go: "none", "prometheus" (a scrape endpoint),
	// "otlp" (a collector), or "console". It is a separate switch from the traces because the
	// two are asked for separately in practice.
	MetricsExporter string `mapstructure:"MetricsExporter" validate:"optional"`
	// MetricsEndpoint is the collector's host:port, required by the otlp metrics exporter.
	MetricsEndpoint string `mapstructure:"MetricsEndpoint" validate:"optional"`
	// MetricsInterval is how often measurements are pushed. Zero takes the SDK's default.
	// It does not apply to the Prometheus exporter, which is scraped.
	MetricsInterval time.Duration `mapstructure:"MetricsInterval" validate:"optional"`
}

// RetentionConfig is how long the deployment keeps what a customer said and what a model
// answered.
//
// The split is the design rather than a pair of numbers: a task's *shape* — which step, which
// tool, how many tokens — is kept for good, and its raw payload is not. A retention rule that
// removed whole rows would take the task history and the cost report with it, which is what
// makes "keep less" and "the history stays readable" one decision instead of two that
// contradict each other.
type RetentionConfig struct {
	// StepDays is how long a task step's raw input and output are kept. Zero takes the
	// product default of thirty days.
	StepDays int `mapstructure:"StepDays" validate:"optional"`
	// AttachmentDays is how long an attachment's bytes are kept. Zero means attachments are
	// kept as long as the message that carries them, which is what a deployment that does
	// not want an attachment policy runs with.
	AttachmentDays int `mapstructure:"AttachmentDays" validate:"optional"`
	// Interval is how often the job runs. Zero disables it, which local development may
	// want.
	Interval time.Duration `mapstructure:"Interval" validate:"optional"`
	// Batch bounds how many steps one pass prunes, so a deployment with years of history
	// makes progress in slices rather than in one unbounded statement.
	Batch int `mapstructure:"Batch" validate:"optional"`
	// SweepLimit bounds how many workspaces one pass looks at.
	SweepLimit int `mapstructure:"SweepLimit" validate:"optional"`
}

// BillingConfig holds what taking money online needs.
//
// Empty Duitku credentials leave the module usable and its payment path closed:
// invoices are still written, the package and its quota still decide what a
// workspace may spend, and the screen says to transfer and have it confirmed. That
// is a real deployment — the product's first customers are paid by hand — and it is
// the same shape as the WhatsApp gateway's missing URL.
type BillingConfig struct {
	// Provider names the payment gateway this deployment speaks. Empty means none,
	// and no webhook route is registered at all.
	Provider string `mapstructure:"Provider" validate:"optional"`
	// GraceDays is how long a workspace keeps working after its period ends. Zero
	// takes the product default of a week.
	GraceDays int `mapstructure:"GraceDays" validate:"optional"`
	// SyncInterval is how often one workspace is reconciled into the durable
	// mirror the screens read.
	SyncInterval time.Duration `mapstructure:"SyncInterval" validate:"optional"`
	// SweepInterval is how often the reconciliation pass itself runs. Zero disables
	// it, which local development may want.
	SweepInterval time.Duration `mapstructure:"SweepInterval" validate:"optional"`
	// SweepLimit bounds one pass.
	SweepLimit int `mapstructure:"SweepLimit" validate:"optional"`
	// Duitku holds the provider's own settings, which only mean anything when
	// Provider is "duitku".
	Duitku DuitkuConfig
}

// DuitkuConfig is the Duitku adapter's settings.
//
// Duitku authenticates both directions with one key and one primitive — HMAC-SHA256 over
// a documented string — so the key is the whole security of the callback as well as the
// authentication of the inquiry.
type DuitkuConfig struct {
	// MerchantCode identifies the project at Duitku.
	MerchantCode string `mapstructure:"MerchantCode" validate:"optional"`
	// APIKey signs the inquiry and verifies the callback.
	APIKey string `mapstructure:"APIKey" validate:"optional"`
	// Sandbox selects Duitku's sandbox host. It is explicit because a sandbox key
	// against the production host is a charge on somebody's card.
	Sandbox bool `mapstructure:"Sandbox" validate:"optional"`
	// PaymentMethod is which Duitku payment channel a person is offered, as Duitku's
	// own two-letter code. Empty takes a bank virtual account.
	PaymentMethod string `mapstructure:"PaymentMethod" validate:"optional"`
	// ExpiryMinutes is how long a payment stays payable. Zero takes a day.
	ExpiryMinutes int `mapstructure:"ExpiryMinutes" validate:"optional"`
	// CallbackURL is where Duitku posts the payment notification. It must be an
	// absolute address this deployment serves: Duitku reaches it from outside, and
	// without it a payment could be made that nothing ever hears about.
	CallbackURL string `mapstructure:"CallbackURL" validate:"optional"`
	// ReturnURL is where a person lands after the payment page.
	ReturnURL string `mapstructure:"ReturnURL" validate:"optional"`
	// BaseURL overrides Duitku's host. It is what points a sandbox run — or a
	// deployment verifying its own wiring — at a stand-in.
	BaseURL string `mapstructure:"BaseURL" validate:"optional"`
}

// WhatsAppConfig holds what the WhatsApp gateway needs.
//
// It is two configurations in one namespace because the two ends are one
// deployment: the API process reaches the gateway, and the gateway process owns
// the connections. Which end a value belongs to is said on the field.
type WhatsAppConfig struct {
	// GatewayURL is the wa-gateway's internal address, as the API reaches it.
	// Empty means numbers can be listed and read but not linked or sent as.
	GatewayURL string `mapstructure:"GatewayURL" validate:"optional"`
	// GatewayToken authenticates the API to the gateway. It is a shared secret
	// because both ends are ours and the gateway has no user of its own. Empty
	// refuses every call in both directions.
	GatewayToken string `mapstructure:"GatewayToken" validate:"optional"`
	// Address is where the gateway process listens. It is not a public address:
	// the gateway speaks for every tenant it serves.
	Address string `mapstructure:"Address" validate:"optional"`
	// Owner names this gateway instance in the lease. Empty generates one per
	// process, which is right for a single instance and wrong for a fleet that
	// restarts: a fresh name each time looks like a new claimant.
	Owner string `mapstructure:"Owner" validate:"optional"`
	// HourlyLimit is the default send ceiling of a number, in messages per hour.
	// Zero means the product default.
	HourlyLimit int `mapstructure:"HourlyLimit" validate:"optional"`
	// LeaseTTL is how long a lease survives without a renewal, which is the upper
	// bound on a takeover.
	LeaseTTL time.Duration `mapstructure:"LeaseTTL" validate:"optional"`
	// LeaseRenew is how often a held lease is extended.
	LeaseRenew time.Duration `mapstructure:"LeaseRenew" validate:"optional"`
	// TypingDelay is how long the typing indicator shows before a message leaves.
	TypingDelay time.Duration `mapstructure:"TypingDelay" validate:"optional"`
	// MaxMediaBytes refuses an inbound attachment larger than this.
	MaxMediaBytes int `mapstructure:"MaxMediaBytes" validate:"optional"`
	// LogWhatsmeow turns on whatsmeow's own protocol logging. It writes a line
	// per decrypted message, so it is a debugging switch rather than a default.
	LogWhatsmeow bool `mapstructure:"LogWhatsmeow" validate:"optional"`
}

// IntegrationConfig holds what the integration hub needs beyond its adapters.
type IntegrationConfig struct {
	// OAuthStateKey signs the value an authorization flow carries. It must be at
	// least 32 characters: the value travels through a browser, and a guessable
	// signature is a forged callback. Empty disables connection flows.
	OAuthStateKey string `mapstructure:"OAuthStateKey" validate:"optional"`
	// OAuthRedirectBase is the public origin providers redirect back to. It has
	// to be the address registered with the provider, so it is configured rather
	// than derived from the request.
	OAuthRedirectBase string `mapstructure:"OAuthRedirectBase" validate:"optional"`
	// DefaultCallsPerMinute paces every application that has no ceiling of its
	// own. Zero means unlimited, which is what a deployment without Redis runs
	// with.
	DefaultCallsPerMinute int `mapstructure:"DefaultCallsPerMinute" validate:"optional"`
	// RateLimits overrides the ceiling per application, as `app:perMinute` pairs
	// separated by commas. A provider publishes its own quota, and a routine that
	// runs every hour must not spend a day's worth of it.
	RateLimits string `mapstructure:"RateLimits" validate:"optional"`
	// MCPTimeout bounds one request to an MCP server. A task is blocked while it
	// runs, so a server that does not answer within it is treated as unavailable
	// rather than waited on. Zero takes the default.
	MCPTimeout time.Duration `mapstructure:"MCPTimeout" validate:"optional"`
	// GmailClientID and GmailClientSecret are the deployment's own OAuth
	// application. They are not shared with the sign-in flow's: a person signing
	// in and a workspace connecting a mailbox are different consents, and Google
	// requires them to be different clients.
	//
	// Both empty disables the Gmail adapter, which is what a deployment that has
	// not registered an application looks like.
	GmailClientID     string `mapstructure:"GmailClientID" validate:"optional"`
	GmailClientSecret string `mapstructure:"GmailClientSecret" validate:"optional"`
	// GmailAPIBase, GmailAuthEndpoint, and GmailTokenEndpoint point the adapter
	// somewhere other than Google. They exist for the same reason the model
	// gateway has a base URL: a staging deployment or a smoke test needs to run
	// the whole path without a real account. Empty means Google.
	GmailAPIBase       string `mapstructure:"GmailAPIBase" validate:"optional"`
	GmailAuthEndpoint  string `mapstructure:"GmailAuthEndpoint" validate:"optional"`
	GmailTokenEndpoint string `mapstructure:"GmailTokenEndpoint" validate:"optional"`
}

// KmsConfig holds the key-management key that protects the data keys of every
// workspace's integration tokens.
//
// The data key is what actually encrypts a token, so the tokens survive a
// rotation of this key: rotating it re-wraps the data keys and nothing else. The
// retired keys are still configured after a rotation, which is what lets an
// existing connection be opened and re-wrapped instead of being broken.
type KmsConfig struct {
	// MasterKey wraps the per-workspace data keys. Same accepted forms as
	// SECRET_ENCRYPTION_KEY: base64, hex, or 32 raw characters. Empty leaves
	// integrations read-only, because no token could be sealed.
	MasterKey string `mapstructure:"MasterKey" validate:"optional"`
	// MasterKeyID names the active key, so a wrap records which key made it.
	MasterKeyID string `mapstructure:"MasterKeyID" validate:"optional"`
	// RetiredKeys are the older keys, as `id:key` pairs separated by commas.
	// They are read-only: a wrap always uses the active key.
	RetiredKeys string `mapstructure:"RetiredKeys" validate:"optional"`
}

// MemoryConfig bounds what memory costs and how long a conversation must be
// quiet before it is read into it.
//
// Every field is optional: a zero falls back to the product default, so a
// deployment only states what it wants to differ from. The one that matters most
// is MainTokens, because that text travels in every single call.
type MemoryConfig struct {
	// MainTokens caps the workspace memory that is sent on every call. The cap is
	// in tokens rather than rows because that is what the bill is in.
	MainTokens int `mapstructure:"MainTokens" validate:"optional"`
	// RecallMemories and RecallKnowledge bound what one round retrieves.
	RecallMemories  int `mapstructure:"RecallMemories" validate:"optional"`
	RecallKnowledge int `mapstructure:"RecallKnowledge" validate:"optional"`
	// Similarity is how close a candidate has to be to count as the same fact.
	// Above it, a newer memory replaces the older one instead of joining it.
	Similarity float64 `mapstructure:"Similarity" validate:"optional"`
	// QuietSeconds is how long a conversation must not move before extraction
	// reads it. Shorter reads more and costs more; longer reads a settled thread.
	QuietSeconds int `mapstructure:"QuietSeconds" validate:"optional"`
	// ExtractionMessages bounds how much of a thread the extractor reads.
	ExtractionMessages int `mapstructure:"ExtractionMessages" validate:"optional"`
	// ChunkRunes is the size of one knowledge chunk, and ChunkOverlapRunes is how
	// much of the end of one chunk repeats at the start of the next so a sentence
	// cut in half is still findable.
	ChunkRunes        int `mapstructure:"ChunkRunes" validate:"optional"`
	ChunkOverlapRunes int `mapstructure:"ChunkOverlapRunes" validate:"optional"`
	// MaxContentRunes bounds one memory and one chunk.
	MaxContentRunes int `mapstructure:"MaxContentRunes" validate:"optional"`
}

// TasksConfig bounds what one task may spend. Every field is optional: a zero
// falls back to the product default, so a deployment only states what it wants
// to differ from.
type TasksConfig struct {
	// MaxSteps is how many rounds of the model-and-tools loop a task may take.
	MaxSteps int `mapstructure:"MaxSteps" validate:"optional"`
	// MaxTokens is how many tokens one task may spend, input and output
	// together.
	MaxTokens int64 `mapstructure:"MaxTokens" validate:"optional"`
	// MaxHandoffDepth is how deep a chain of handoffs may nest.
	MaxHandoffDepth int `mapstructure:"MaxHandoffDepth" validate:"optional"`
	// MaxToolResultBytes truncates a tool's answer, which is what keeps one
	// large read from being re-sent to the model on every later round.
	MaxToolResultBytes int `mapstructure:"MaxToolResultBytes" validate:"optional"`
}

// AppConfig holds application-level settings.
type AppConfig struct {
	Environment string `mapstructure:"Environment"`
	// FrontendURL is the base URL of the web app. It is where verification,
	// reset, and invitation links point, and where the Google callback sends
	// the browser back to.
	FrontendURL string `mapstructure:"FrontendURL" validate:"optional"`
	// CORSOrigins lists the browser origins allowed to call the API. An empty
	// list allows any origin, which is only acceptable in local development.
	CORSOrigins []string `mapstructure:"CORSOrigins" validate:"optional"`
}

// HttpConfig holds HTTP server settings.
type HttpConfig struct {
	// OpsAddress is where a process serves its operational surface — /health, /ready, and
	// /metrics — when it has no HTTP API of its own, e.g. ":9101". The API has one already, so it
	// answers on its own listener; a worker has none, and without this an orchestrator has nothing
	// to probe and a scraper nothing to read. Empty means the process serves none, which is what a
	// deployment that relies on pushes wants.
	OpsAddress   string        `mapstructure:"OpsAddress" validate:"optional"`
	Address      string        `mapstructure:"Address"`
	ReadTimeout  time.Duration `mapstructure:"ReadTimeout"`
	WriteTimeout time.Duration `mapstructure:"WriteTimeout"`
	ApiPrefix    string        `mapstructure:"ApiPrefix"`
}

// DatabaseConfig holds Postgres connection settings. URL is optional so a
// developer can run the API without a database; when it is empty the container
// wires no repositories and only dependency-free routes are served.
type DatabaseConfig struct {
	URL string `mapstructure:"URL" validate:"optional"`
	// Pool sizing is optional: an operator who only supplies DATABASE_URL gets
	// the pgx defaults instead of a validation error.
	MaxOpenConns    int32 `mapstructure:"MaxOpenConns" validate:"optional"`
	MaxIdleConns    int32 `mapstructure:"MaxIdleConns" validate:"optional"`
	ConnMaxLifetime int   `mapstructure:"ConnMaxLifetime" validate:"optional"` // seconds
}

// RedisConfig holds the Redis connection settings. Redis carries realtime
// pub/sub channels, per-integration rate limit buckets, and the live quota
// counter, so every process that needs it shares this configuration.
type RedisConfig struct {
	URL string `mapstructure:"URL" validate:"optional"`
}

// TemporalConfig holds the Temporal connection settings. HostPort is empty in
// the checked-in config files so no production endpoint is committed to the
// repository; staging and production supply it through the environment.
type TemporalConfig struct {
	HostPort             string `mapstructure:"HostPort" validate:"optional"`
	Namespace            string `mapstructure:"Namespace" validate:"optional"`
	TaskQueueAgent       string `mapstructure:"TaskQueueAgent" validate:"optional"`
	TaskQueueIntegration string `mapstructure:"TaskQueueIntegration" validate:"optional"`
	TLS                  bool   `mapstructure:"TLS"`
	APIKey               string `mapstructure:"APIKey" validate:"optional"`
	// UIBaseURL is where this deployment's Temporal UI is reachable, e.g.
	// "http://localhost:8233". A stuck task is diagnosed by opening its workflow, and that link
	// is a deployment fact: the self-hosted UI is a port on the compose network and Temporal
	// Cloud is a different host entirely. Empty means no link is offered, which is the honest
	// answer for a deployment whose UI is not reachable from where the person reading is.
	UIBaseURL string `mapstructure:"UIBaseURL" validate:"optional"`
}

// JWTConfig holds JWT signing settings.
type JWTConfig struct {
	Secret      string `mapstructure:"Secret"`
	ExpireHours int    `mapstructure:"ExpireHours" validate:"optional"`
}

// AuthConfig holds the authentication lifetimes, cookie settings, and the login
// throttle. Lifetimes are expressed in the unit named by the field, which keeps
// the YAML readable.
type AuthConfig struct {
	// AccessTokenMinutes is the lifetime of an access token.
	AccessTokenMinutes int `mapstructure:"AccessTokenMinutes" validate:"optional"`
	// RefreshHours is the lifetime of a refresh session.
	RefreshHours int `mapstructure:"RefreshHours" validate:"optional"`
	// VerificationHours is the lifetime of an email verification link.
	VerificationHours int `mapstructure:"VerificationHours" validate:"optional"`
	// ResetMinutes is the lifetime of a password reset link.
	ResetMinutes int `mapstructure:"ResetMinutes" validate:"optional"`
	// StateMinutes is the lifetime of the signed OAuth state.
	StateMinutes int `mapstructure:"StateMinutes" validate:"optional"`
	// CookieDomain is optional; empty means the host that served the request.
	CookieDomain string `mapstructure:"CookieDomain" validate:"optional"`
	// CookieSecure must be true outside local development: the refresh cookie
	// must not travel over plain HTTP.
	CookieSecure bool `mapstructure:"CookieSecure"`
	// LoginThrottleCapacity is the burst size of the login bucket, and
	// LoginThrottleRefill the steady rate in attempts per second.
	LoginThrottleCapacity int     `mapstructure:"LoginThrottleCapacity" validate:"optional"`
	LoginThrottleRefill   float64 `mapstructure:"LoginThrottleRefill" validate:"optional"`
	// RequireEmailVerification makes a new account confirm its address before it
	// may use the product. It is on by default; a deployment that turns it off
	// lets a fresh account in immediately, which is what a private rollout wants
	// before the mail path is dependable.
	RequireEmailVerification bool `mapstructure:"RequireEmailVerification" validate:"optional"`
}

// GoogleConfig holds the Google OAuth client registration. An empty ClientID
// disables Google sign-in and the endpoints report it as not configured.
type GoogleConfig struct {
	ClientID     string `mapstructure:"ClientID" validate:"optional"`
	ClientSecret string `mapstructure:"ClientSecret" validate:"optional"`
	RedirectURL  string `mapstructure:"RedirectURL" validate:"optional"`
}

// MailConfig selects how transactional email is delivered. Driver is "log"
// (development) or "smtp" (staging and production).
type MailConfig struct {
	Driver   string `mapstructure:"Driver" validate:"optional"`
	From     string `mapstructure:"From" validate:"optional"`
	Host     string `mapstructure:"Host" validate:"optional"`
	Port     int    `mapstructure:"Port" validate:"optional"`
	Username string `mapstructure:"Username" validate:"optional"`
	Password string `mapstructure:"Password" validate:"optional"`
}

// PlanConfig holds the plan defaults the product has not decided yet. The
// subscription itself arrives in EPIC 12 (#97); until then these values bound
// what a workspace may create.
type PlanConfig struct {
	// MaxAgentsPerWorkspace caps the Bolu registry. Zero means unlimited.
	MaxAgentsPerWorkspace int `mapstructure:"MaxAgentsPerWorkspace" validate:"optional"`
	// DailyCostMicros is the cost ceiling of one day per workspace, in
	// micro-rupiah. Zero means unlimited.
	//
	// It is the second bound the quota asks, alongside the period's token
	// allowance: the period stops a workspace from spending a month's tokens,
	// and this stops one runaway task from spending a month in an afternoon.
	// EPIC 12 (#97) replaces the configured value with the subscription's
	// package, and this is what a deployment without billing runs with.
	DailyCostMicros int64 `mapstructure:"DailyCostMicros" validate:"optional"`
}

// LlmConfig holds the model gateway settings: how the provider keys are sealed
// at rest, which provider the platform offers when a workspace configures none,
// and whether the token allowance is enforced.
type LlmConfig struct {
	// SecretEncryptionKey seals the provider API keys at rest. Base64, hex, or 32
	// raw characters. Empty leaves the feature read-only: no key can be stored,
	// and the settings screen says so instead of writing a secret in clear text.
	SecretEncryptionKey string `mapstructure:"SecretEncryptionKey" validate:"optional"`
	// Default is the platform-level provider, used by a workspace that has not
	// configured one of its own. It is how a self-hosted deployment works out of
	// the box. The environment variables LLM_DEFAULT_* override its fields.
	Default DefaultProviderConfig `mapstructure:"Default"`
	// QuotaEnabled turns the pre-flight token check on. Local development runs
	// with it off, so a fresh workspace can call a model before any package
	// exists.
	QuotaEnabled bool `mapstructure:"QuotaEnabled"`
	// TokensPerPeriod is the monthly token allowance of a workspace when no
	// subscription is wired — a deployment without billing at all. Zero means
	// unlimited. A wired subscription replaces it with its package's allowance.
	TokensPerPeriod int64 `mapstructure:"TokensPerPeriod"`
	// ChainLimit is how many providers one request tries before giving up.
	ChainLimit int `mapstructure:"ChainLimit"`
}

// DefaultProviderConfig is the platform-level provider. Every field is optional:
// an empty Model means this deployment offers no fallback provider, and a
// workspace without one cannot call a model until it configures one.
type DefaultProviderConfig struct {
	Adapter       string `mapstructure:"Adapter" validate:"optional"`
	BaseURL       string `mapstructure:"BaseURL" validate:"optional"`
	Model         string `mapstructure:"Model" validate:"optional"`
	APIKey        string `mapstructure:"APIKey" validate:"optional"`
	MaxTokens     int    `mapstructure:"MaxTokens" validate:"optional"`
	ContextTokens int    `mapstructure:"ContextTokens" validate:"optional"`
}

// StorageConfig holds the object storage settings: attachments, scan results,
// and the sandboxed HTML documents the chat renders.
type StorageConfig struct {
	// Driver is "local" (a directory, for development) or "s3" (anything that
	// speaks the S3 API: MinIO, R2, B2, AWS).
	Driver string `mapstructure:"Driver" validate:"optional"`
	// LocalRoot is where the local driver writes. It is resolved relative to the
	// working directory.
	LocalRoot string `mapstructure:"LocalRoot" validate:"optional"`
	// Endpoint is the S3 host without a scheme, e.g. "minio:9000".
	Endpoint string `mapstructure:"Endpoint" validate:"optional"`
	Region   string `mapstructure:"Region" validate:"optional"`
	Bucket   string `mapstructure:"Bucket" validate:"optional"`
	// AccessKey and SecretKey come from the environment in every deployment.
	AccessKey string `mapstructure:"AccessKey" validate:"optional"`
	SecretKey string `mapstructure:"SecretKey" validate:"optional"`
	UseSSL    bool   `mapstructure:"UseSSL" validate:"optional"`
	PathStyle bool   `mapstructure:"PathStyle" validate:"optional"`
	// ContentOrigin is where sandboxed HTML documents are served from. It is a
	// separate host in every deployment, because the sandbox is what makes
	// agent-written script safe to run.
	ContentOrigin string `mapstructure:"ContentOrigin" validate:"optional"`
	// ContentFrameAncestors lists the origins allowed to frame a content
	// document. It is the application's own origin, and it must be named
	// explicitly: the document is served from a different host than the app, so
	// `frame-ancestors 'self'` would refuse every frame and break the block.
	ContentFrameAncestors []string `mapstructure:"ContentFrameAncestors" validate:"optional"`
}

// LoggingConfig holds structured logging settings.
type LoggingConfig struct {
	Level  string `mapstructure:"Level" validate:"optional"`  // debug|info|warn|error
	Format string `mapstructure:"Format" validate:"optional"` // json|console
}
