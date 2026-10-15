// Package logger builds the structured Zap logger used by every process. The
// logger is always injected through the container; no package-level logger
// exists.
package logger

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Format selects the log encoding.
const (
	FormatJSON    = "json"
	FormatConsole = "console"
)

// New builds a logger for the given level (debug|info|warn|error) and format
// (json|console). Production uses JSON so logs are searchable; local
// development uses the console encoder.
func New(level, format string) (*zap.Logger, error) {
	zapLevel, err := parseLevel(level)
	if err != nil {
		return nil, err
	}

	encoder, err := encoderFor(format)
	if err != nil {
		return nil, err
	}

	core := zapcore.NewCore(encoder, zapcore.Lock(zapcore.AddSync(stdout{})), zapLevel)

	return zap.New(core, zap.AddCaller(), zap.ErrorOutput(zapcore.Lock(zapcore.AddSync(stderr{})))), nil
}

func parseLevel(level string) (zapcore.Level, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return zapcore.DebugLevel, nil
	case "info", "":
		return zapcore.InfoLevel, nil
	case "warn", "warning":
		return zapcore.WarnLevel, nil
	case "error":
		return zapcore.ErrorLevel, nil
	default:
		return zapcore.InfoLevel, fmt.Errorf("unsupported log level %q", level)
	}
}

func encoderFor(format string) (zapcore.Encoder, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case FormatJSON, "":
		return zapcore.NewJSONEncoder(jsonEncoderConfig()), nil
	case FormatConsole:
		cfg := zap.NewDevelopmentEncoderConfig()
		cfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
		return zapcore.NewConsoleEncoder(cfg), nil
	default:
		return nil, fmt.Errorf("unsupported log format %q", format)
	}
}

func jsonEncoderConfig() zapcore.EncoderConfig {
	cfg := zap.NewProductionEncoderConfig()
	cfg.TimeKey = "ts"
	cfg.EncodeTime = zapcore.ISO8601TimeEncoder
	return cfg
}
