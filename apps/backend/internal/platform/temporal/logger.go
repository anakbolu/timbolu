package temporal

import (
	"fmt"

	sdklog "go.temporal.io/sdk/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// ZapLogger writes the SDK's own lines — and a workflow's `workflow.GetLogger` lines — through the
// deployment's logger.
//
// It matters more than it looks. A workflow's log is where "which task, which workflow, which
// attempt" lives, and the SDK fills those in itself: passing them through this adapter is what puts
// `workflow_id` on the same line as the deployment's other fields, in the same format, at the same
// level as everything else. The default SDK logger prints to standard output in its own shape, so a
// workflow's error would be a line no log search finds and no level filter keeps.
type ZapLogger struct {
	logger *zap.Logger
	// keyvals are what an enclosing logger adds to every line — the workflow's own identifiers,
	// which the SDK passes in.
	keyvals []any
}

// NewZapLogger adapts a Zap logger for the Temporal SDK.
func NewZapLogger(logger *zap.Logger) *ZapLogger {
	if logger == nil {
		logger = zap.NewNop()
	}

	return &ZapLogger{logger: logger}
}

// Compile-time check that the adapter is what the SDK calls, in both directions: a caller may pass
// it to `Options.Logger`, and the SDK may ask it to derive a child.
var (
	_ sdklog.Logger          = (*ZapLogger)(nil)
	_ sdklog.WithLogger      = (*ZapLogger)(nil)
	_ sdklog.WithSkipCallers = (*ZapLogger)(nil)
)

// Debug implements log.Logger.
func (l *ZapLogger) Debug(msg string, keyvals ...any) { l.write(zapcore.DebugLevel, msg, keyvals) }

// Info implements log.Logger.
func (l *ZapLogger) Info(msg string, keyvals ...any) { l.write(zapcore.InfoLevel, msg, keyvals) }

// Warn implements log.Logger.
func (l *ZapLogger) Warn(msg string, keyvals ...any) { l.write(zapcore.WarnLevel, msg, keyvals) }

// Error implements log.Logger.
func (l *ZapLogger) Error(msg string, keyvals ...any) { l.write(zapcore.ErrorLevel, msg, keyvals) }

// With returns a logger that adds these pairs to every line. The SDK calls it with the workflow's
// own identifiers, which is how a workflow's lines end up carrying its workflow id.
func (l *ZapLogger) With(keyvals ...any) sdklog.Logger {
	child := &ZapLogger{
		logger:  l.logger,
		keyvals: append(append([]any{}, l.keyvals...), keyvals...),
	}

	return child
}

// WithCallerSkip returns a logger that skips more stack frames, which is what makes the reported
// call site a workflow's own line rather than the SDK's.
func (l *ZapLogger) WithCallerSkip(skip int) sdklog.Logger {
	return &ZapLogger{logger: l.logger.WithOptions(zap.AddCallerSkip(skip)), keyvals: l.keyvals}
}

// write emits one line with the pairs flattened into fields.
//
// An odd number of values would otherwise silently drop the last one, which is how a log line ends
// up missing exactly the field somebody was looking for — so the dangling value is kept under a name
// that says what happened.
func (l *ZapLogger) write(level zapcore.Level, msg string, keyvals []any) {
	if !l.logger.Core().Enabled(level) {
		// Building the fields of a line nobody will see is the cost this avoids: a workflow's
		// debug lines are frequent and the level is usually above them.
		return
	}

	fields := make([]zap.Field, 0, (len(l.keyvals)+len(keyvals))/2+1)
	fields = appendKeyvals(fields, l.keyvals...)
	fields = appendKeyvals(fields, keyvals...)

	l.logger.Check(level, msg).Write(fields...)
}

// appendKeyvals turns the SDK's alternating pairs into Zap fields.
func appendKeyvals(fields []zap.Field, keyvals ...any) []zap.Field {
	for index := 0; index < len(keyvals); index += 2 {
		key := fmt.Sprint(keyvals[index])
		if index+1 == len(keyvals) {
			fields = append(fields, zap.Any(key, "no value"))

			continue
		}

		fields = append(fields, zap.Any(key, keyvals[index+1]))
	}

	return fields
}
