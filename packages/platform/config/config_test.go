package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// clearOverrides removes every environment variable the loader understands, so
// a developer's shell cannot change the expectations.
func clearOverrides(t *testing.T) {
	t.Helper()

	for _, key := range []string{
		"ENVIRONMENT", "HTTP_ADDRESS", "DATABASE_URL", "REDIS_URL",
		"TEMPORAL_HOST_PORT", "TEMPORAL_NAMESPACE", "TEMPORAL_TASK_QUEUE_AGENT",
		"TEMPORAL_TASK_QUEUE_INTEGRATION",
		"TEMPORAL_API_KEY", "JWT_SECRET", "LOG_LEVEL", "LOG_FORMAT",
		"SECRET_ENCRYPTION_KEY", "LLM_DEFAULT_ADAPTER", "LLM_DEFAULT_BASE_URL",
		"LLM_DEFAULT_MODEL", "LLM_DEFAULT_API_KEY", "LLM_DEFAULT_MAX_TOKENS",
		"LLM_DEFAULT_CONTEXT_TOKENS", "LLM_QUOTA_ENABLED", "LLM_TOKENS_PER_PERIOD",
		"LLM_CHAIN_LIMIT",
		"BILLING_PROVIDER", "DUITKU_MERCHANT_CODE", "DUITKU_API_KEY", "DUITKU_SANDBOX",
		"DUITKU_PAYMENT_METHOD", "DUITKU_EXPIRY_MINUTES", "DUITKU_CALLBACK_URL",
		"DUITKU_RETURN_URL", "DUITKU_BASE_URL",
		"BILLING_GRACE_DAYS", "BILLING_SYNC_INTERVAL", "BILLING_SWEEP_INTERVAL",
		"AUTH_REQUIRE_EMAIL_VERIFICATION",
		"MAIL_DRIVER", "MAIL_TIMEOUT_SECONDS",
		"VCOMPUTER_CONTROL_URL", "VCOMPUTER_CONTROL_TOKEN", "VCOMPUTER_CONTROL_TIMEOUT",
		"CONTENT_FRAME_ANCESTORS", "ATTACHMENT_ALLOWED_EXTENSIONS",
	} {
		t.Setenv(key, "")
	}
}

func TestLoadLocalDefaults(t *testing.T) {
	clearOverrides(t)
	t.Setenv("ENVIRONMENT", "local")

	cfg, err := Load()
	require.NoError(t, err)

	require.Equal(t, "local", cfg.Application.Environment)
	require.Equal(t, ":8080", cfg.Http.Address)
	require.Equal(t, "/api", cfg.Http.ApiPrefix)
	require.Equal(t, "redis://localhost:6379/0", cfg.Redis.URL)
	require.Equal(t, "localhost:7233", cfg.Temporal.HostPort)
	require.Equal(t, "default", cfg.Temporal.Namespace)
	require.Equal(t, "bolu-agent", cfg.Temporal.TaskQueueAgent)
	require.Equal(t, "console", cfg.Logging.Format)
	require.Equal(t, "debug", cfg.Logging.Level)
}

func TestLoadAppliesEnvironmentOverrides(t *testing.T) {
	clearOverrides(t)
	t.Setenv("ENVIRONMENT", "local")
	t.Setenv("HTTP_ADDRESS", ":9999")
	t.Setenv("DATABASE_URL", "postgres://user:pass@db:5432/bolu")
	t.Setenv("REDIS_URL", "redis://cache:6379/3")
	t.Setenv("TEMPORAL_HOST_PORT", "ap-southeast-1.aws.api.temporal.io:7233")
	t.Setenv("TEMPORAL_NAMESPACE", "bolu.prod")
	t.Setenv("TEMPORAL_TASK_QUEUE_AGENT", "bolu-agent-prod")
	t.Setenv("TEMPORAL_TASK_QUEUE_INTEGRATION", "bolu-integration-prod")
	t.Setenv("TEMPORAL_API_KEY", "cloud-key")
	t.Setenv("LOG_LEVEL", "warn")
	t.Setenv("LOG_FORMAT", "json")

	cfg, err := Load()
	require.NoError(t, err)

	require.Equal(t, ":9999", cfg.Http.Address)
	require.Equal(t, "postgres://user:pass@db:5432/bolu", cfg.Database.URL)
	require.Equal(t, "redis://cache:6379/3", cfg.Redis.URL)
	require.Equal(t, "ap-southeast-1.aws.api.temporal.io:7233", cfg.Temporal.HostPort)
	require.Equal(t, "bolu.prod", cfg.Temporal.Namespace)
	require.Equal(t, "bolu-agent-prod", cfg.Temporal.TaskQueueAgent)
	require.Equal(t, "bolu-integration-prod", cfg.Temporal.TaskQueueIntegration)
	require.Equal(t, "cloud-key", cfg.Temporal.APIKey)
	require.Equal(t, "warn", cfg.Logging.Level)
	require.Equal(t, "json", cfg.Logging.Format)
}

// TestLoadReadsTheModelGatewaySettings covers the environment the gateway needs:
// the key that seals provider secrets, the fallback provider, and the quota.
func TestLoadReadsTheModelGatewaySettings(t *testing.T) {
	clearOverrides(t)
	t.Setenv("ENVIRONMENT", "local")
	t.Setenv("SECRET_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("LLM_DEFAULT_ADAPTER", "openai")
	t.Setenv("LLM_DEFAULT_BASE_URL", "http://ollama:11434")
	t.Setenv("LLM_DEFAULT_MODEL", "llama3.1")
	t.Setenv("LLM_DEFAULT_API_KEY", "local-key")
	t.Setenv("LLM_DEFAULT_MAX_TOKENS", "2048")
	t.Setenv("LLM_DEFAULT_CONTEXT_TOKENS", "32000")
	t.Setenv("LLM_QUOTA_ENABLED", "true")
	t.Setenv("LLM_TOKENS_PER_PERIOD", "1500000")
	t.Setenv("LLM_CHAIN_LIMIT", "2")

	cfg, err := Load()
	require.NoError(t, err)

	require.Equal(t, "0123456789abcdef0123456789abcdef", cfg.Llm.SecretEncryptionKey)
	require.Equal(t, "openai", cfg.Llm.Default.Adapter)
	require.Equal(t, "http://ollama:11434", cfg.Llm.Default.BaseURL)
	require.Equal(t, "llama3.1", cfg.Llm.Default.Model)
	require.Equal(t, "local-key", cfg.Llm.Default.APIKey)
	require.Equal(t, 2048, cfg.Llm.Default.MaxTokens)
	require.Equal(t, 32000, cfg.Llm.Default.ContextTokens)
	require.True(t, cfg.Llm.QuotaEnabled)
	require.Equal(t, int64(1500000), cfg.Llm.TokensPerPeriod)
	require.Equal(t, 2, cfg.Llm.ChainLimit)
}

// TestLoadRejectsAMalformedQuota: a mistyped allowance must fail the startup
// rather than silently becoming "unlimited".
func TestLoadRejectsAMalformedQuota(t *testing.T) {
	clearOverrides(t)
	t.Setenv("ENVIRONMENT", "local")
	t.Setenv("LLM_TOKENS_PER_PERIOD", "banyak")

	_, err := Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "LLM_TOKENS_PER_PERIOD")

	t.Setenv("LLM_TOKENS_PER_PERIOD", "1000")
	t.Setenv("LLM_QUOTA_ENABLED", "mungkin")

	_, err = Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "LLM_QUOTA_ENABLED")
}

// TestLoadReadsTheMemorySettings is the E10 configuration gate: every bound the
// memory module reads can be set from the environment, and a mistyped one is an
// error rather than a silent default.
//
// A silently-defaulted bound is the failure mode worth catching: a quota that
// reads as "unlimited" because a typo fell back to zero is only discovered on the
// invoice, and a quiet period that falls back to five minutes is only discovered
// when an extraction does not happen.
func TestLoadReadsTheMemorySettings(t *testing.T) {
	clearOverrides(t)
	t.Setenv("ENVIRONMENT", "local")
	t.Setenv("MEMORY_MAIN_TOKENS", "900")
	t.Setenv("MEMORY_RECALL_MEMORIES", "3")
	t.Setenv("MEMORY_RECALL_KNOWLEDGE", "2")
	t.Setenv("MEMORY_SIMILARITY", "0.75")
	t.Setenv("MEMORY_QUIET_SECONDS", "20")
	t.Setenv("MEMORY_EXTRACTION_MESSAGES", "12")
	t.Setenv("MEMORY_CHUNK_RUNES", "500")
	t.Setenv("MEMORY_CHUNK_OVERLAP_RUNES", "50")
	t.Setenv("MEMORY_MAX_CONTENT_RUNES", "800")

	cfg, err := Load()
	require.NoError(t, err)

	require.Equal(t, 900, cfg.Memory.MainTokens)
	require.Equal(t, 3, cfg.Memory.RecallMemories)
	require.Equal(t, 2, cfg.Memory.RecallKnowledge)
	require.Equal(t, 0.75, cfg.Memory.Similarity)
	require.Equal(t, 20, cfg.Memory.QuietSeconds)
	require.Equal(t, 12, cfg.Memory.ExtractionMessages)
	require.Equal(t, 500, cfg.Memory.ChunkRunes)
	require.Equal(t, 50, cfg.Memory.ChunkOverlapRunes)
	require.Equal(t, 800, cfg.Memory.MaxContentRunes)
}

// TestLoadRejectsAMalformedMemorySetting: a value that will not parse is an error,
// for the same reason the quota is.
func TestLoadRejectsAMalformedMemorySetting(t *testing.T) {
	clearOverrides(t)
	t.Setenv("ENVIRONMENT", "local")
	t.Setenv("MEMORY_QUIET_SECONDS", "sebentar")

	_, err := Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "MEMORY_QUIET_SECONDS")

	t.Setenv("MEMORY_QUIET_SECONDS", "20")
	t.Setenv("MEMORY_SIMILARITY", "mirip")

	_, err = Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "MEMORY_SIMILARITY")
}

// TestLoadReadsTheAttachmentContextSettings covers the bounds on what a
// message's files contribute to a model call, and the defaults a deployment that
// names none gets.
func TestLoadReadsTheAttachmentContextSettings(t *testing.T) {
	clearOverrides(t)
	t.Setenv("ENVIRONMENT", "local")

	cfg, err := Load()
	require.NoError(t, err)

	require.Equal(t, int64(10485760), cfg.AttachmentContext.MaxFileBytes)
	require.Equal(t, 20000, cfg.AttachmentContext.MaxCharsPerFile)
	require.Equal(t, 60000, cfg.AttachmentContext.MaxCharsTotal)
	require.Equal(t, 4, cfg.AttachmentContext.MaxImages)
	require.Equal(t, int64(5242880), cfg.AttachmentContext.MaxImageBytes)

	t.Setenv("ATTACHMENT_CONTEXT_MAX_FILE_BYTES", "1024")
	t.Setenv("ATTACHMENT_CONTEXT_MAX_CHARS_PER_FILE", "100")
	t.Setenv("ATTACHMENT_CONTEXT_MAX_CHARS_TOTAL", "250")
	t.Setenv("ATTACHMENT_CONTEXT_MAX_IMAGES", "1")
	t.Setenv("ATTACHMENT_CONTEXT_MAX_IMAGE_BYTES", "2048")

	cfg, err = Load()
	require.NoError(t, err)

	require.Equal(t, int64(1024), cfg.AttachmentContext.MaxFileBytes)
	require.Equal(t, 100, cfg.AttachmentContext.MaxCharsPerFile)
	require.Equal(t, 250, cfg.AttachmentContext.MaxCharsTotal)
	require.Equal(t, 1, cfg.AttachmentContext.MaxImages)
	require.Equal(t, int64(2048), cfg.AttachmentContext.MaxImageBytes)
}

// TestLoadRejectsAMalformedAttachmentContextSetting: a bound that will not parse
// is an error rather than a silently unlimited one.
func TestLoadRejectsAMalformedAttachmentContextSetting(t *testing.T) {
	clearOverrides(t)
	t.Setenv("ENVIRONMENT", "local")
	t.Setenv("ATTACHMENT_CONTEXT_MAX_CHARS_TOTAL", "banyak")

	_, err := Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "ATTACHMENT_CONTEXT_MAX_CHARS_TOTAL")
}

// TestLocalDefaultsSealProviderSecrets: the development key exists so a
// developer can store a provider key without configuring anything.
func TestLocalDefaultsSealProviderSecrets(t *testing.T) {
	clearOverrides(t)
	t.Setenv("ENVIRONMENT", "local")

	cfg, err := Load()
	require.NoError(t, err)

	require.Len(t, []byte(cfg.Llm.SecretEncryptionKey), 32)
	require.False(t, cfg.Llm.QuotaEnabled, "local development runs without the token check")
	require.Equal(t, 3, cfg.Llm.ChainLimit)
	require.Empty(t, cfg.Llm.Default.Model, "no provider is committed to the repository")
}

// TestLoadProductionRequiresSecrets keeps a missing secret a startup failure
// instead of a service that boots with an empty signing key.
func TestLoadProductionRequiresSecrets(t *testing.T) {
	clearOverrides(t)
	t.Setenv("ENVIRONMENT", "production")

	_, err := Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "jwt")

	t.Setenv("JWT_SECRET", "production-secret")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "json", cfg.Logging.Format, "production logs must be JSON")
	require.Empty(t, cfg.Temporal.HostPort, "no Temporal endpoint may be committed to the repository")
}

// TestAnEnvironmentOverlaysTheBaseFile is what "a separate configuration per environment" means in
// practice: the base supplies every default, the environment's own file wins where it names a key,
// and a key it does not mention keeps the base's value rather than being erased.
func TestAnEnvironmentOverlaysTheBaseFile(t *testing.T) {
	clearOverrides(t)
	t.Setenv("JWT_SECRET", "production-secret")
	t.Setenv("ENVIRONMENT", "staging")

	cfg, err := Load()
	require.NoError(t, err)

	// From the environment's own file.
	require.Equal(t, "staging", cfg.Application.Environment)
	require.Equal(t, "debug", cfg.Logging.Level, "staging is read by a person")
	require.Equal(t, "console", cfg.Logging.Format)
	require.Equal(t, int32(20), cfg.Database.MaxOpenConns)
	require.Equal(t, 24*time.Hour, cfg.Retention.Interval)
	require.Equal(t, 200, cfg.Retention.Batch)
	require.False(t, cfg.Llm.QuotaEnabled, "a staging run must not be refused by an allowance")

	// And from the base, which the overlay never mentions: a settings file that named only what
	// differs must not blank everything else.
	require.Equal(t, "/api", cfg.Http.ApiPrefix)
	require.Equal(t, "bolu-agent", cfg.Temporal.TaskQueueAgent)
	require.Equal(t, 24, cfg.JWT.ExpireHours)
	require.Equal(t, time.Hour, cfg.Billing.SyncInterval, "the overlay's own value")
	require.Equal(t, 3, cfg.Llm.ChainLimit)
}

// TestProductionNamesWhatItDependsOn: every key in the production overlay also exists in the base, and
// naming it again is what stops an edit to the shared base from changing production by accident. This
// test is the reason the file is not empty.
func TestProductionNamesWhatItDependsOn(t *testing.T) {
	clearOverrides(t)
	t.Setenv("JWT_SECRET", "production-secret")
	t.Setenv("ENVIRONMENT", "production")

	cfg, err := Load()
	require.NoError(t, err)

	require.Equal(t, "json", cfg.Logging.Format)
	require.Equal(t, "info", cfg.Logging.Level)
	require.True(t, cfg.Llm.QuotaEnabled, "production enforces the allowance")
	require.True(t, cfg.Auth.CookieSecure)
	require.Equal(t, 6*time.Hour, cfg.Retention.Interval)
	require.Equal(t, 500, cfg.Retention.Batch)
	require.Equal(t, 200, cfg.Retention.SweepLimit)
	require.Equal(t, 15*time.Minute, cfg.Billing.SweepInterval)

	// And the base still supplies everything the overlay does not name.
	require.Equal(t, "bolu-integration", cfg.Temporal.TaskQueueIntegration)
	require.Equal(t, 100, int(cfg.Database.MaxOpenConns))
}

// TestRequireEmailVerificationIsOnByDefaultAndCanBeTurnedOff: requiring a confirmed address is the
// product's default, and the environment is where a rollout that has not finished its mail path opts
// out of it. A value that is not a boolean is refused rather than read as "false", which is the
// mistake that would open the gate by accident.
func TestRequireEmailVerificationIsOnByDefaultAndCanBeTurnedOff(t *testing.T) {
	clearOverrides(t)
	t.Setenv("ENVIRONMENT", "local")

	cfg, err := Load()
	require.NoError(t, err)
	require.True(t, cfg.Auth.RequireEmailVerification, "verification is the default")

	t.Setenv("AUTH_REQUIRE_EMAIL_VERIFICATION", "false")
	cfg, err = Load()
	require.NoError(t, err)
	require.False(t, cfg.Auth.RequireEmailVerification)

	t.Setenv("AUTH_REQUIRE_EMAIL_VERIFICATION", "not-a-bool")
	_, err = Load()
	require.Error(t, err)
}

// TestMailTimeoutSecondsBoundsOneDelivery: the bound that keeps an unreachable mail server from
// holding a request. It is read from the environment so a deployment can tighten or loosen it, and a
// value that is not a number is refused rather than read as "no timeout".
func TestMailTimeoutSecondsBoundsOneDelivery(t *testing.T) {
	clearOverrides(t)

	// The base file every deployment reads supplies the default; local runs the log driver and
	// names no timeout, which leaves the mail package's own bound in place.
	base, err := loadFromFile("staging")
	require.NoError(t, err)
	require.Equal(t, 10, base.Mail.TimeoutSeconds, "the shipped default bounds the session")

	t.Setenv("ENVIRONMENT", "local")
	t.Setenv("MAIL_TIMEOUT_SECONDS", "3")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, 3, cfg.Mail.TimeoutSeconds)

	t.Setenv("MAIL_TIMEOUT_SECONDS", "soon")
	_, err = Load()
	require.Error(t, err)
}

// TestAttachmentAllowedExtensionsComeFromTheEnvironment: the chat module's allowlist is
// deployment policy, and a person writes an extension with its dot. Both spellings are
// read, blanks between commas are dropped, and the field is left empty — which the module
// reads as "the default list" — when nothing is set.
func TestAttachmentAllowedExtensionsComeFromTheEnvironment(t *testing.T) {
	clearOverrides(t)
	t.Setenv("ENVIRONMENT", "local")

	cfg, err := Load()
	require.NoError(t, err)
	require.Empty(t, cfg.Storage.AttachmentAllowedExtensions, "empty means the module's default list")

	t.Setenv("ATTACHMENT_ALLOWED_EXTENSIONS", " .PNG ,pdf, ,txt")
	cfg, err = Load()
	require.NoError(t, err)
	require.Equal(t, []string{"png", "pdf", "txt"}, cfg.Storage.AttachmentAllowedExtensions,
		"the dot is stripped, the case is lowered, and blanks are dropped")
}

// TestAnUnknownEnvironmentStillLoadsTheBase: a deployment that mistypes its environment name gets the
// base defaults and, if it named nothing else, a clear validation failure — rather than a process
// that refuses to start because a file it invented does not exist.
func TestAnUnknownEnvironmentStillLoadsTheBase(t *testing.T) {
	clearOverrides(t)
	t.Setenv("ENVIRONMENT", "qa")

	cfg, err := loadFromFile("qa")
	require.NoError(t, err)
	require.Equal(t, "bolu-agent", cfg.Temporal.TaskQueueAgent)
	require.Contains(t, configNames("qa"), "config-qa")
}

// TestAnOverlayTypoIsRefused is the gate that makes the overlay files trustworthy: a key that matches
// no field is a setting that silently does nothing, and that is how a staging deployment comes to run
// production's retention window while its own YAML says otherwise.
func TestAnOverlayTypoIsRefused(t *testing.T) {
	clearOverrides(t)

	// The loader reads from the working directory, so a temporary one holds the fixture.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("Logging:\n  Level: \"info\"\n"), 0o600))
	// The typo is deliberate, so the linter is told rather than the test being written around it.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config-staging.yaml"),
		[]byte("Logging:\n  Levle: \"debug\"\n"), 0o600)) //nolint:misspell // the typo is the fixture

	previous, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(previous) })

	_, err = loadFromFile("staging")
	require.Error(t, err, "a key that matches no field must fail rather than be ignored")
}

func TestValidate(t *testing.T) {
	valid := func() *Config {
		return &Config{
			Http: HttpConfig{
				Address:      ":8080",
				ReadTimeout:  30 * time.Second,
				WriteTimeout: 30 * time.Second,
				ApiPrefix:    "/api",
			},
			Database: DatabaseConfig{URL: "", MaxOpenConns: 0},
			JWT:      JWTConfig{Secret: "secret"},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "valid config passes", mutate: func(*Config) {}},
		{
			name:    "http address is required",
			mutate:  func(c *Config) { c.Http.Address = "" },
			wantErr: "http config: field Address is required",
		},
		{
			name:    "jwt secret is required",
			mutate:  func(c *Config) { c.JWT.Secret = "" },
			wantErr: "jwt config: field Secret is required",
		},
		{
			name: "an empty database url is allowed",
			mutate: func(c *Config) {
				c.Database.URL = ""
			},
		},
		{
			name:   "boolean fields are never required",
			mutate: func(c *Config) { c.Temporal.TLS = false },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid()
			tt.mutate(cfg)

			err := cfg.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestLoadDotEnv(t *testing.T) {
	t.Run("missing file is not an error", func(t *testing.T) {
		t.Chdir(t.TempDir())
		require.NoError(t, LoadDotEnv())
	})

	t.Run("a malformed file is reported", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte("THIS LINE HAS NO EQUALS SIGN\n"), 0o600))
		t.Chdir(dir)

		require.Error(t, LoadDotEnv(), "a broken .env must not be ignored")
	})

	t.Run("values are loaded and the environment wins", func(t *testing.T) {
		dir := t.TempDir()
		content := []byte("BOLU_DOTENV_TEST_FILE=from-file\nBOLU_DOTENV_TEST_BOTH=from-file\n")
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), content, 0o600))
		t.Chdir(dir)
		// godotenv must not overwrite a variable the environment already set.
		t.Setenv("BOLU_DOTENV_TEST_BOTH", "from-env")

		require.NoError(t, LoadDotEnv())
		require.Equal(t, "from-file", os.Getenv("BOLU_DOTENV_TEST_FILE"))
		require.Equal(t, "from-env", os.Getenv("BOLU_DOTENV_TEST_BOTH"))
	})
}

// TestLoadReadsTheBillingSettings is the E12 configuration gate: every value the billing
// module reads can be set from the environment, and a mistyped one is an error rather than
// a silent default.
//
// The provider is the interesting one. Naming a merchant code or an API key is what turns
// online payment on, so a deployment that supplies one and forgets the other must find out
// while it is starting rather than when the first customer tries to pay.
func TestLoadReadsTheBillingSettings(t *testing.T) {
	clearOverrides(t)
	t.Setenv("ENVIRONMENT", "local")

	t.Setenv("DUITKU_MERCHANT_CODE", "D1234")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "duitku", cfg.Billing.Provider, "naming a merchant code is what takes payment")

	t.Setenv("BILLING_PROVIDER", "duitku")
	t.Setenv("DUITKU_API_KEY", "an-api-key")
	t.Setenv("DUITKU_SANDBOX", "false")
	t.Setenv("DUITKU_PAYMENT_METHOD", "SP")
	t.Setenv("DUITKU_EXPIRY_MINUTES", "90")
	t.Setenv("DUITKU_CALLBACK_URL", "https://api.example.com/api/billing/webhooks/duitku")
	t.Setenv("DUITKU_RETURN_URL", "https://app.example.com/settings/subscription")
	t.Setenv("DUITKU_BASE_URL", "http://127.0.0.1:9098")
	t.Setenv("BILLING_GRACE_DAYS", "3")
	t.Setenv("BILLING_SYNC_INTERVAL", "5m")
	t.Setenv("BILLING_SWEEP_INTERVAL", "1h")

	cfg, err = Load()
	require.NoError(t, err)

	require.Equal(t, "duitku", cfg.Billing.Provider)
	require.Equal(t, "D1234", cfg.Billing.Duitku.MerchantCode)
	require.Equal(t, "an-api-key", cfg.Billing.Duitku.APIKey)
	require.False(t, cfg.Billing.Duitku.Sandbox, "the host is chosen by configuration, never by the key's shape")
	require.Equal(t, "SP", cfg.Billing.Duitku.PaymentMethod)
	require.Equal(t, 90, cfg.Billing.Duitku.ExpiryMinutes)
	require.Equal(t, "https://api.example.com/api/billing/webhooks/duitku", cfg.Billing.Duitku.CallbackURL)
	require.Equal(t, "https://app.example.com/settings/subscription", cfg.Billing.Duitku.ReturnURL)
	require.Equal(t, "http://127.0.0.1:9098", cfg.Billing.Duitku.BaseURL)
	require.Equal(t, 3, cfg.Billing.GraceDays)
	require.Equal(t, 5*time.Minute, cfg.Billing.SyncInterval)
	require.Equal(t, time.Hour, cfg.Billing.SweepInterval)
}

// TestLoadRejectsAMalformedBillingSetting: a bound that reads as its default because a typo
// fell back to zero is only discovered on an invoice.
func TestLoadRejectsAMalformedBillingSetting(t *testing.T) {
	cases := map[string]string{
		"DUITKU_SANDBOX":         "mungkin",
		"DUITKU_EXPIRY_MINUTES":  "sejam",
		"BILLING_GRACE_DAYS":     "seminggu",
		"BILLING_SYNC_INTERVAL":  "sering",
		"BILLING_SWEEP_INTERVAL": "jarang",
	}

	for key, value := range cases {
		t.Run(key, func(t *testing.T) {
			clearOverrides(t)
			t.Setenv("ENVIRONMENT", "local")
			t.Setenv(key, value)

			_, err := Load()
			require.Error(t, err)
			require.Contains(t, err.Error(), key)
		})
	}
}

// TestLoadReadsTheVComputerSettings is the virtual computer's configuration gate: the control
// plane's address and its shared secret come from the environment, and the timeout that bounds
// one call is parsed rather than passed through.
//
// The empty default is the interesting half. A deployment without a control plane must be a
// valid deployment — the activities then answer ErrNotConfigured rather than calling an address
// nobody meant — so the YAML ships with the address empty and this test keeps it that way.
func TestLoadReadsTheVComputerSettings(t *testing.T) {
	clearOverrides(t)
	t.Setenv("ENVIRONMENT", "local")

	cfg, err := Load()
	require.NoError(t, err)

	require.Empty(t, cfg.VComputer.ControlURL, "no control plane is committed to the repository")
	require.Empty(t, cfg.VComputer.Token)
	require.Equal(t, 30*time.Second, cfg.VComputer.Timeout)

	t.Setenv("VCOMPUTER_CONTROL_URL", "http://vcomputer-control:8090")
	t.Setenv("VCOMPUTER_CONTROL_TOKEN", "shared-secret")
	t.Setenv("VCOMPUTER_CONTROL_TIMEOUT", "90s")

	cfg, err = Load()
	require.NoError(t, err)

	require.Equal(t, "http://vcomputer-control:8090", cfg.VComputer.ControlURL)
	require.Equal(t, "shared-secret", cfg.VComputer.Token)
	require.Equal(t, 90*time.Second, cfg.VComputer.Timeout)
}

// TestLoadRejectsAMalformedVComputerSetting: a timeout that will not parse is an error rather
// than a silent fall back to the default, for the same reason a mistyped quota is — a bound that
// quietly becomes something else is only discovered when a call behaves unlike its configuration.
func TestLoadRejectsAMalformedVComputerSetting(t *testing.T) {
	clearOverrides(t)
	t.Setenv("ENVIRONMENT", "local")
	t.Setenv("VCOMPUTER_CONTROL_TIMEOUT", "sebentar")

	_, err := Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "VCOMPUTER_CONTROL_TIMEOUT")
}
