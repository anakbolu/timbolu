package container

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/packages/platform/config"
)

// TestNewReportsUnusableDependencies keeps every misconfigured dependency a
// startup error with a clear cause, instead of a service that boots and fails
// later.
func TestNewReportsUnusableDependencies(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*config.Config)
		wantErr string
	}{
		{
			name:    "unsupported log format",
			mutate:  func(c *config.Config) { c.Logging.Format = "xml" },
			wantErr: "build logger",
		},
		{
			name:    "unsupported log level",
			mutate:  func(c *config.Config) { c.Logging.Level = "loud" },
			wantErr: "build logger",
		},
		{
			name:    "unreachable redis",
			mutate:  func(c *config.Config) { c.Redis.URL = "redis://127.0.0.1:1" },
			wantErr: "open redis",
		},
		{
			name: "unreachable temporal",
			mutate: func(c *config.Config) {
				c.Temporal.HostPort = "127.0.0.1:1"
				c.Temporal.TLS = true
				c.Temporal.APIKey = "cloud-key"
			},
			wantErr: "open temporal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Logging = config.LoggingConfig{Level: "info", Format: "json"}
			tt.mutate(cfg)

			c, err := New(context.Background(), cfg, WithLogger(nil), WithDB(nil))
			if c != nil {
				c.Close()
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// TestLoggerIsBuiltFromConfig covers the default path where no logger is
// injected: the container builds one from the logging configuration.
func TestLoggerIsBuiltFromConfig(t *testing.T) {
	cfg := testConfig()
	cfg.Logging = config.LoggingConfig{Level: "debug", Format: "console"}

	c, err := New(context.Background(), cfg, WithDB(nil), WithRedis(nil), WithTemporal(nil))
	require.NoError(t, err)
	t.Cleanup(c.Close)

	require.NotNil(t, c.Logger)
}
