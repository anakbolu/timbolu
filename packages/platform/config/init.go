package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
	"go.yaml.in/yaml/v3"
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
	// A viper instance of this call's own rather than the package-level one, because `Load` is called
	// by tests and by several binaries in one process: the global instance accumulates every merge,
	// so a second call would inherit the first's values and a test would depend on the one before it.
	loader := viper.New()
	loader.SetConfigType("yaml")
	loader.AddConfigPath("./internal/platform/config/")
	loader.AddConfigPath("./config/")
	loader.AddConfigPath(".")
	loader.AutomaticEnv()
	loader.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	// The base file supplies every default and `config-<environment>.yaml` overlays what differs, in
	// that order so the overlay wins. Local is the one exception: it reads its own file alone, which
	// is what a developer runs and what keeps a key added to the base from silently changing it.
	merged := map[string]any{}
	for _, name := range configNames(env) {
		layer, err := readConfigLayer(name)
		if err != nil {
			return nil, err
		}
		if layer == nil {
			continue
		}
		mergeLayer(merged, layer)
	}

	// A deployment with no config file at all is still valid: the environment can supply every
	// required value, which is what the container images rely on.
	if len(merged) > 0 {
		if err := loader.MergeConfigMap(merged); err != nil {
			return nil, err
		}
	}

	var cfg Config
	// UnmarshalExact rather than Unmarshal: a key that matches no field is a typo, and a config file
	// with a typo in it is a setting that silently does nothing. That is how a staging deployment
	// comes to run production's retention window while its YAML says otherwise.
	if err := loader.UnmarshalExact(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// configNames is the files an environment reads, base first.
//
// The overlay exists so staging and production carry only the values that differ — a log level, a
// retention window, a quota — instead of two hundred-line copies of the base that drift apart the
// first time somebody edits one of them.
func configNames(env string) []string {
	trimmed := strings.TrimSpace(env)
	if trimmed == "" || trimmed == localEnv {
		return []string{"config-local"}
	}

	return []string{"config", "config-" + trimmed}
}

// readConfigLayer reads one file into a map, or nil when it is absent.
//
// The file is parsed here rather than through a second viper instance, and that is not fussiness:
// `AllSettings` re-reads every value through viper's own casting, so a number arrives as whatever
// type viper inferred — an `int32` field filled from an overlay came back as `int32`, which then had
// to be compared differently from the same field filled from the base. Parsing the YAML directly
// gives the shapes the decoder expects, from both layers.
func readConfigLayer(name string) (map[string]any, error) {
	for _, dir := range configPaths {
		path := filepath.Join(dir, name+".yaml")
		content, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}

			return nil, fmt.Errorf("read %s: %w", path, err)
		}

		layer := map[string]any{}
		if err := yaml.Unmarshal(content, &layer); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}

		return layer, nil
	}

	return nil, nil
}

// configPaths is where a config file may live, in the order they are tried: beside the package for a
// test, then the layouts a binary runs in. The shared package carries the files, so a process run
// from the repository root finds them under `packages/platform/config/`, one run from an app
// directory two levels up, and one in a container image where the Dockerfile placed them at the same
// relative path.
var configPaths = []string{
	"./packages/platform/config/",
	"../../packages/platform/config/",
	"./internal/platform/config/",
	"./config/",
	".",
}

// mergeLayer writes one file's values over another's, section by section.
//
// The merge is deep rather than whole-key: an overlay that names only `Http.ReadTimeout` must not
// erase the rest of the `Http` block, which is exactly what a shallow assignment would do.
func mergeLayer(into, layer map[string]any) {
	for key, value := range layer {
		nested, isMap := value.(map[string]any)
		if !isMap {
			into[key] = value

			continue
		}

		existing, ok := into[key].(map[string]any)
		if !ok {
			into[key] = nested

			continue
		}

		mergeLayer(existing, nested)
	}
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
		"OPS_ADDRESS":                     &cfg.Http.OpsAddress,
		"INTERNAL_ADDRESS":                &cfg.Http.InternalAddress,
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
		"KMS_MASTER_KEY":                  &cfg.Kms.MasterKey,
		"KMS_MASTER_KEY_ID":               &cfg.Kms.MasterKeyID,
		"KMS_RETIRED_KEYS":                &cfg.Kms.RetiredKeys,
		"OAUTH_STATE_KEY":                 &cfg.Integrations.OAuthStateKey,
		"OAUTH_REDIRECT_BASE":             &cfg.Integrations.OAuthRedirectBase,
		"INTEGRATION_RATE_LIMITS":         &cfg.Integrations.RateLimits,
		"GMAIL_CLIENT_ID":                 &cfg.Integrations.GmailClientID,
		"GMAIL_CLIENT_SECRET":             &cfg.Integrations.GmailClientSecret,
		"GMAIL_API_BASE":                  &cfg.Integrations.GmailAPIBase,
		"GMAIL_AUTH_ENDPOINT":             &cfg.Integrations.GmailAuthEndpoint,
		"GMAIL_TOKEN_ENDPOINT":            &cfg.Integrations.GmailTokenEndpoint,
		"SLACK_CLIENT_ID":                 &cfg.Integrations.SlackClientID,
		"SLACK_CLIENT_SECRET":             &cfg.Integrations.SlackClientSecret,
		"SLACK_API_BASE":                  &cfg.Integrations.SlackAPIBase,
		"SLACK_AUTH_ENDPOINT":             &cfg.Integrations.SlackAuthEndpoint,
		"SLACK_TOKEN_ENDPOINT":            &cfg.Integrations.SlackTokenEndpoint,
		"GITHUB_CLIENT_ID":                &cfg.Integrations.GitHubClientID,
		"GITHUB_CLIENT_SECRET":            &cfg.Integrations.GitHubClientSecret,
		"GITHUB_API_BASE":                 &cfg.Integrations.GitHubAPIBase,
		"GITHUB_AUTH_ENDPOINT":            &cfg.Integrations.GitHubAuthEndpoint,
		"GITHUB_TOKEN_ENDPOINT":           &cfg.Integrations.GitHubTokenEndpoint,
		// WhatsApp: the two processes reach each other over one shared secret, so
		// every one of these is a secret or an address and none of them belongs in a
		// YAML file that ships with the image.
		"WA_GATEWAY_URL":   &cfg.WhatsApp.GatewayURL,
		"WA_API_URL":       &cfg.WhatsApp.APIURL,
		"WA_GATEWAY_TOKEN": &cfg.WhatsApp.GatewayToken,
		"WA_ADDRESS":       &cfg.WhatsApp.Address,
		"WA_OWNER":         &cfg.WhatsApp.Owner,
		// Virtual computer: the control plane is reached over the internal network with a
		// shared secret, so both values come from the environment and the address stays out
		// of the YAML file that ships with the image. An empty address is a deployment
		// without a virtual computer, which is what every deployment runs until it has one.
		"VCOMPUTER_CONTROL_URL":   &cfg.VComputer.ControlURL,
		"VCOMPUTER_CONTROL_TOKEN": &cfg.VComputer.Token,
	}
	for key, target := range overrides {
		if v := os.Getenv(key); v != "" {
			*target = v
		}
	}

	if v := os.Getenv("MAIL_TIMEOUT_SECONDS"); v != "" {
		seconds, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse MAIL_TIMEOUT_SECONDS: %w", err)
		}
		cfg.Mail.TimeoutSeconds = seconds
	}

	if v := os.Getenv("INTEGRATION_CALLS_PER_MINUTE"); v != "" {
		perMinute, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse INTEGRATION_CALLS_PER_MINUTE: %w", err)
		}
		cfg.Integrations.DefaultCallsPerMinute = perMinute
	}

	if v := os.Getenv("MCP_TIMEOUT"); v != "" {
		timeout, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parse MCP_TIMEOUT: %w", err)
		}
		cfg.Integrations.MCPTimeout = timeout
	}

	if v := os.Getenv("LLM_QUOTA_ENABLED"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("parse LLM_QUOTA_ENABLED: %w", err)
		}
		cfg.Llm.QuotaEnabled = enabled
	}

	if v := os.Getenv("AUTH_REQUIRE_EMAIL_VERIFICATION"); v != "" {
		required, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("parse AUTH_REQUIRE_EMAIL_VERIFICATION: %w", err)
		}
		cfg.Auth.RequireEmailVerification = required
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

	if v := os.Getenv("BILLING_PROVIDER"); v != "" {
		cfg.Billing.Provider = strings.TrimSpace(v)
	}

	if v := os.Getenv("DUITKU_MERCHANT_CODE"); v != "" {
		cfg.Billing.Duitku.MerchantCode = strings.TrimSpace(v)
		// The merchant code is what decides whether the provider is spoken at all, so
		// naming it here is what keeps the two settings from disagreeing: a deployment
		// that supplies a code meant to take payment.
		if strings.TrimSpace(cfg.Billing.Provider) == "" {
			cfg.Billing.Provider = "duitku"
		}
	}

	if v := os.Getenv("DUITKU_API_KEY"); v != "" {
		cfg.Billing.Duitku.APIKey = strings.TrimSpace(v)
		if strings.TrimSpace(cfg.Billing.Provider) == "" {
			cfg.Billing.Provider = "duitku"
		}
	}

	if v := os.Getenv("DUITKU_SANDBOX"); v != "" {
		sandbox, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("parse DUITKU_SANDBOX: %w", err)
		}
		cfg.Billing.Duitku.Sandbox = sandbox
	}

	if v := os.Getenv("DUITKU_PAYMENT_METHOD"); v != "" {
		cfg.Billing.Duitku.PaymentMethod = strings.TrimSpace(v)
	}

	if v := os.Getenv("DUITKU_EXPIRY_MINUTES"); v != "" {
		minutes, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse DUITKU_EXPIRY_MINUTES: %w", err)
		}
		cfg.Billing.Duitku.ExpiryMinutes = minutes
	}

	if v := os.Getenv("DUITKU_CALLBACK_URL"); v != "" {
		cfg.Billing.Duitku.CallbackURL = strings.TrimSpace(v)
	}

	if v := os.Getenv("DUITKU_RETURN_URL"); v != "" {
		cfg.Billing.Duitku.ReturnURL = strings.TrimSpace(v)
	}

	if v := os.Getenv("DUITKU_BASE_URL"); v != "" {
		cfg.Billing.Duitku.BaseURL = strings.TrimSpace(v)
	}

	if v := os.Getenv("BILLING_GRACE_DAYS"); v != "" {
		days, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse BILLING_GRACE_DAYS: %w", err)
		}
		cfg.Billing.GraceDays = days
	}

	if v := os.Getenv("BILLING_SYNC_INTERVAL"); v != "" {
		interval, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parse BILLING_SYNC_INTERVAL: %w", err)
		}
		cfg.Billing.SyncInterval = interval
	}

	if v := os.Getenv("BILLING_SWEEP_INTERVAL"); v != "" {
		interval, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parse BILLING_SWEEP_INTERVAL: %w", err)
		}
		cfg.Billing.SweepInterval = interval
	}

	if v := os.Getenv("WA_HOURLY_LIMIT"); v != "" {
		limit, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse WA_HOURLY_LIMIT: %w", err)
		}
		cfg.WhatsApp.HourlyLimit = limit
	}

	if v := os.Getenv("WA_LEASE_TTL"); v != "" {
		ttl, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parse WA_LEASE_TTL: %w", err)
		}
		cfg.WhatsApp.LeaseTTL = ttl
	}

	if v := os.Getenv("WA_LEASE_RENEW"); v != "" {
		renew, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parse WA_LEASE_RENEW: %w", err)
		}
		cfg.WhatsApp.LeaseRenew = renew
	}

	if v := os.Getenv("WA_TYPING_DELAY"); v != "" {
		delay, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parse WA_TYPING_DELAY: %w", err)
		}
		cfg.WhatsApp.TypingDelay = delay
	}

	if v := os.Getenv("WA_MAX_MEDIA_BYTES"); v != "" {
		size, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse WA_MAX_MEDIA_BYTES: %w", err)
		}
		cfg.WhatsApp.MaxMediaBytes = size
	}

	if v := os.Getenv("WA_LOG_WHATSMEOW"); v != "" {
		log, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("parse WA_LOG_WHATSMEOW: %w", err)
		}
		cfg.WhatsApp.LogWhatsmeow = log
	}

	if v := os.Getenv("VCOMPUTER_CONTROL_TIMEOUT"); v != "" {
		timeout, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parse VCOMPUTER_CONTROL_TIMEOUT: %w", err)
		}
		cfg.VComputer.Timeout = timeout
	}

	if v := os.Getenv("PLAN_DAILY_COST_MICROS"); v != "" {
		micros, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("parse PLAN_DAILY_COST_MICROS: %w", err)
		}
		cfg.Plan.DailyCostMicros = micros
	}

	// The Temporal UI link the task screen offers. It is a base URL rather than a built one,
	// because the path below it is the UI's own and the deployment is the only thing that knows
	// which UI it is.
	if v := os.Getenv("TEMPORAL_UI_BASE_URL"); v != "" {
		cfg.Temporal.UIBaseURL = v
	}

	if v := os.Getenv("OTEL_EXPORTER"); v != "" {
		cfg.Telemetry.Exporter = v
	}

	if v := os.Getenv("OTEL_ENDPOINT"); v != "" {
		cfg.Telemetry.Endpoint = v
	}

	if v := os.Getenv("OTEL_SERVICE_NAME"); v != "" {
		cfg.Telemetry.ServiceName = v
	}

	if v := os.Getenv("OTEL_SAMPLE_RATIO"); v != "" {
		ratio, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("parse OTEL_SAMPLE_RATIO: %w", err)
		}
		cfg.Telemetry.SampleRatio = ratio
	}

	if v := os.Getenv("OTEL_INSECURE"); v != "" {
		cfg.Telemetry.Insecure = strings.EqualFold(v, "true") || v == "1"
	}

	if v := os.Getenv("OTEL_METRICS_EXPORTER"); v != "" {
		cfg.Telemetry.MetricsExporter = v
	}

	if v := os.Getenv("OTEL_METRICS_ENDPOINT"); v != "" {
		cfg.Telemetry.MetricsEndpoint = v
	}

	if v := os.Getenv("OTEL_METRICS_INTERVAL"); v != "" {
		interval, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parse OTEL_METRICS_INTERVAL: %w", err)
		}
		cfg.Telemetry.MetricsInterval = interval
	}

	if v := os.Getenv("RETENTION_STEP_DAYS"); v != "" {
		days, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse RETENTION_STEP_DAYS: %w", err)
		}
		cfg.Retention.StepDays = days
	}

	if v := os.Getenv("RETENTION_ATTACHMENT_DAYS"); v != "" {
		days, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse RETENTION_ATTACHMENT_DAYS: %w", err)
		}
		cfg.Retention.AttachmentDays = days
	}

	if v := os.Getenv("RETENTION_INTERVAL"); v != "" {
		interval, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parse RETENTION_INTERVAL: %w", err)
		}
		cfg.Retention.Interval = interval
	}

	if v := os.Getenv("RETENTION_BATCH"); v != "" {
		batch, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse RETENTION_BATCH: %w", err)
		}
		cfg.Retention.Batch = batch
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

	if v := os.Getenv("MEMORY_MAIN_TOKENS"); v != "" {
		tokens, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse MEMORY_MAIN_TOKENS: %w", err)
		}
		cfg.Memory.MainTokens = tokens
	}

	if v := os.Getenv("MEMORY_RECALL_MEMORIES"); v != "" {
		count, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse MEMORY_RECALL_MEMORIES: %w", err)
		}
		cfg.Memory.RecallMemories = count
	}

	if v := os.Getenv("MEMORY_RECALL_KNOWLEDGE"); v != "" {
		count, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse MEMORY_RECALL_KNOWLEDGE: %w", err)
		}
		cfg.Memory.RecallKnowledge = count
	}

	if v := os.Getenv("MEMORY_SIMILARITY"); v != "" {
		similarity, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("parse MEMORY_SIMILARITY: %w", err)
		}
		cfg.Memory.Similarity = similarity
	}

	if v := os.Getenv("MEMORY_QUIET_SECONDS"); v != "" {
		seconds, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse MEMORY_QUIET_SECONDS: %w", err)
		}
		cfg.Memory.QuietSeconds = seconds
	}

	if v := os.Getenv("MEMORY_EXTRACTION_MESSAGES"); v != "" {
		count, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse MEMORY_EXTRACTION_MESSAGES: %w", err)
		}
		cfg.Memory.ExtractionMessages = count
	}

	if v := os.Getenv("MEMORY_CHUNK_RUNES"); v != "" {
		runes, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse MEMORY_CHUNK_RUNES: %w", err)
		}
		cfg.Memory.ChunkRunes = runes
	}

	if v := os.Getenv("MEMORY_CHUNK_OVERLAP_RUNES"); v != "" {
		runes, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse MEMORY_CHUNK_OVERLAP_RUNES: %w", err)
		}
		cfg.Memory.ChunkOverlapRunes = runes
	}

	if v := os.Getenv("MEMORY_MAX_CONTENT_RUNES"); v != "" {
		runes, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse MEMORY_MAX_CONTENT_RUNES: %w", err)
		}
		cfg.Memory.MaxContentRunes = runes
	}

	return nil
}
