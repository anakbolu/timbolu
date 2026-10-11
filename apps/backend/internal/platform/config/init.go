package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/viper"
)

const (
	localEnv = "local"
	envKey   = "ENVIRONMENT"
)

// Load reads the YAML config for the current ENVIRONMENT (local uses
// config-local.yaml, anything else config.yaml) and applies environment
// variable overrides on top of it.
func Load() (*Config, error) {
	cfg, err := loadFromFile(os.Getenv(envKey))
	if err != nil {
		return nil, err
	}

	if err := loadFromEnv(cfg); err != nil {
		return nil, err
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func loadFromFile(env string) (*Config, error) {
	if env == localEnv {
		viper.SetConfigName("config-local")
	} else {
		viper.SetConfigName("config")
	}

	viper.SetConfigType("yaml")
	viper.AddConfigPath("./internal/platform/config/")
	viper.AddConfigPath("./config/")
	viper.AddConfigPath(".")
	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	if err := viper.ReadInConfig(); err != nil {
		// A missing config file is fine: environment variables can supply
		// every required value. errors.As rather than a type assertion, so a
		// wrapped ConfigFileNotFoundError is still recognised.
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return nil, err
		}
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// loadFromEnv applies the supported environment variable overrides. Only
// non-empty variables win, so a YAML default is never wiped by an unset env.
//
// A value that cannot be parsed is an error rather than a silent default: a
// mistyped quota silently becomes "unlimited", which is the kind of mistake that
// is only discovered on the invoice.
func loadFromEnv(cfg *Config) error {
	overrides := map[string]*string{
		"ENVIRONMENT":                     &cfg.Application.Environment,
		"HTTP_ADDRESS":                    &cfg.Http.Address,
		"DATABASE_URL":                    &cfg.Database.URL,
		"REDIS_URL":                       &cfg.Redis.URL,
		"TEMPORAL_HOST_PORT":              &cfg.Temporal.HostPort,
		"TEMPORAL_NAMESPACE":              &cfg.Temporal.Namespace,
		"TEMPORAL_TASK_QUEUE_AGENT":       &cfg.Temporal.TaskQueueAgent,
		"TEMPORAL_TASK_QUEUE_INTEGRATION": &cfg.Temporal.TaskQueueIntegration,
		"TEMPORAL_API_KEY":                &cfg.Temporal.APIKey,
		"JWT_SECRET":                      &cfg.JWT.Secret,
		"LOG_LEVEL":                       &cfg.Logging.Level,
		"LOG_FORMAT":                      &cfg.Logging.Format,
		"FRONTEND_URL":                    &cfg.Application.FrontendURL,
		"GOOGLE_CLIENT_ID":                &cfg.Google.ClientID,
		"GOOGLE_CLIENT_SECRET":            &cfg.Google.ClientSecret,
		"GOOGLE_REDIRECT_URL":             &cfg.Google.RedirectURL,
		"MAIL_DRIVER":                     &cfg.Mail.Driver,
		"MAIL_FROM":                       &cfg.Mail.From,
		"MAIL_HOST":                       &cfg.Mail.Host,
		"MAIL_USERNAME":                   &cfg.Mail.Username,
		"MAIL_PASSWORD":                   &cfg.Mail.Password,
		"SECRET_ENCRYPTION_KEY":           &cfg.Llm.SecretEncryptionKey,
		"LLM_DEFAULT_ADAPTER":             &cfg.Llm.Default.Adapter,
		"LLM_DEFAULT_BASE_URL":            &cfg.Llm.Default.BaseURL,
		"LLM_DEFAULT_MODEL":               &cfg.Llm.Default.Model,
		"LLM_DEFAULT_API_KEY":             &cfg.Llm.Default.APIKey,
		"STORAGE_DRIVER":                  &cfg.Storage.Driver,
		"STORAGE_LOCAL_ROOT":              &cfg.Storage.LocalRoot,
		"S3_ENDPOINT":                     &cfg.Storage.Endpoint,
		"S3_REGION":                       &cfg.Storage.Region,
		"S3_BUCKET":                       &cfg.Storage.Bucket,
		"S3_ACCESS_KEY":                   &cfg.Storage.AccessKey,
		"S3_SECRET_KEY":                   &cfg.Storage.SecretKey,
		"CONTENT_ORIGIN":                  &cfg.Storage.ContentOrigin,
	}
	for key, target := range overrides {
		if v := os.Getenv(key); v != "" {
			*target = v
		}
	}

	if v := os.Getenv("LLM_QUOTA_ENABLED"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("parse LLM_QUOTA_ENABLED: %w", err)
		}
		cfg.Llm.QuotaEnabled = enabled
	}

	if v := os.Getenv("LLM_TOKENS_PER_PERIOD"); v != "" {
		tokens, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("parse LLM_TOKENS_PER_PERIOD: %w", err)
		}
		cfg.Llm.TokensPerPeriod = tokens
	}

	if v := os.Getenv("LLM_CHAIN_LIMIT"); v != "" {
		limit, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse LLM_CHAIN_LIMIT: %w", err)
		}
		cfg.Llm.ChainLimit = limit
	}

	if v := os.Getenv("LLM_DEFAULT_MAX_TOKENS"); v != "" {
		tokens, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse LLM_DEFAULT_MAX_TOKENS: %w", err)
		}
		cfg.Llm.Default.MaxTokens = tokens
	}

	if v := os.Getenv("CONTENT_FRAME_ANCESTORS"); v != "" {
		ancestors := make([]string, 0, 2)
		for _, origin := range strings.Split(v, ",") {
			if trimmed := strings.TrimSpace(origin); trimmed != "" {
				ancestors = append(ancestors, trimmed)
			}
		}
		cfg.Storage.ContentFrameAncestors = ancestors
	}

	if v := os.Getenv("S3_USE_SSL"); v != "" {
		useSSL, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("parse S3_USE_SSL: %w", err)
		}
		cfg.Storage.UseSSL = useSSL
	}

	if v := os.Getenv("S3_PATH_STYLE"); v != "" {
		pathStyle, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("parse S3_PATH_STYLE: %w", err)
		}
		cfg.Storage.PathStyle = pathStyle
	}

	if v := os.Getenv("LLM_DEFAULT_CONTEXT_TOKENS"); v != "" {
		tokens, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse LLM_DEFAULT_CONTEXT_TOKENS: %w", err)
		}
		cfg.Llm.Default.ContextTokens = tokens
	}

	if v := os.Getenv("PLAN_DAILY_COST_MICROS"); v != "" {
		micros, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("parse PLAN_DAILY_COST_MICROS: %w", err)
		}
		cfg.Plan.DailyCostMicros = micros
	}

	if v := os.Getenv("TASK_MAX_STEPS"); v != "" {
		steps, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse TASK_MAX_STEPS: %w", err)
		}
		cfg.Tasks.MaxSteps = steps
	}

	if v := os.Getenv("TASK_MAX_TOKENS"); v != "" {
		tokens, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("parse TASK_MAX_TOKENS: %w", err)
		}
		cfg.Tasks.MaxTokens = tokens
	}

	if v := os.Getenv("TASK_MAX_HANDOFF_DEPTH"); v != "" {
		depth, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse TASK_MAX_HANDOFF_DEPTH: %w", err)
		}
		cfg.Tasks.MaxHandoffDepth = depth
	}

	if v := os.Getenv("TASK_MAX_TOOL_RESULT_BYTES"); v != "" {
		size, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse TASK_MAX_TOOL_RESULT_BYTES: %w", err)
		}
		cfg.Tasks.MaxToolResultBytes = size
	}

	return nil
}
