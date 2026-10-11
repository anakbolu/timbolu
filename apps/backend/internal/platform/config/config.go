package config

import "time"

// Config is the root application configuration. Values come from the YAML file
// matching ENVIRONMENT and are then overridden by environment variables.
type Config struct {
	Application AppConfig
	Http        HttpConfig
	Database    DatabaseConfig
	Redis       RedisConfig
	Temporal    TemporalConfig
	JWT         JWTConfig
	Auth        AuthConfig
	Google      GoogleConfig
	Mail        MailConfig
	Plan        PlanConfig
	Llm         LlmConfig
	Tasks       TasksConfig
	Storage     StorageConfig
	Logging     LoggingConfig
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
	// package.
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
	// TokensPerPeriod is the monthly token allowance of a workspace. Zero means
	// unlimited. EPIC 12 (#97) replaces this with the subscription's package.
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
