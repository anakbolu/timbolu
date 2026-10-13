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
