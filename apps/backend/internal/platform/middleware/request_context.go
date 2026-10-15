// Package middleware holds the Fiber middleware shared by every process: the
// request context (request_id, workspace_id, trace_id) and its Zap logger.
package middleware

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/robzlabz/employeebot/packages/platform/logger"
)

// Header names understood by the request context middleware.
const (
	HeaderRequestID   = "X-Request-Id"
	HeaderWorkspaceID = "X-Workspace-Id"
	HeaderTraceID     = "X-Trace-Id"
	HeaderTraceparent = "traceparent"
)

// Locals keys used to carry the context values through the request.
const (
	localRequestID   = "request_id"
	localWorkspaceID = "workspace_id"
	localTraceID     = "trace_id"
	localLogger      = "logger"
)

var (
	uuidPattern       = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)
	traceparentFormat = regexp.MustCompile(`^[0-9a-f]{2}-([0-9a-f]{32})-([0-9a-f]{16})-[0-9a-f]{2}$`)
)

// RequestContext attaches request_id, workspace_id and trace_id to a request
// logger and stores both on the context. Downstream handlers read them with
// RequestID/WorkspaceID/TraceID/Logger, so no handler builds its own logger.
func RequestContext(base *zap.Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		requestID := sanitizeID(c.Get(HeaderRequestID))
		if requestID == "" {
			requestID = uuid.NewString()
		}

		traceID := traceIDFrom(c)
		workspaceID := sanitizeID(c.Get(HeaderWorkspaceID))

		reqLog := base.With(zap.String("request_id", requestID))
		reqLog = reqLog.With(zap.String("trace_id", traceID))
		if workspaceID != "" {
			reqLog = reqLog.With(zap.String("workspace_id", workspaceID))
		}

		c.Locals(localRequestID, requestID)
		c.Locals(localTraceID, traceID)
		c.Locals(localWorkspaceID, workspaceID)
		c.Locals(localLogger, reqLog)

		// The response echoes the identifiers so a user can quote them when
		// reporting a problem.
		c.Set(HeaderRequestID, requestID)
		c.Set(HeaderTraceID, traceID)

		start := time.Now()
		handlerErr := c.Next()

		fields := []zap.Field{
			zap.String("method", c.Method()),
			zap.String("path", c.Path()),
			zap.String("query", logger.RedactQuery(string(c.Request().URI().QueryString()))),
			zap.Int("status", c.Response().StatusCode()),
			zap.Duration("latency", time.Since(start)),
			zap.String("ip", c.IP()),
			zap.String("user_agent", c.Get(fiber.HeaderUserAgent)),
		}
		if base.Core().Enabled(zapcore.DebugLevel) {
			fields = append(fields, zap.Any("headers", logger.RedactHeaders(requestHeaders(c))))
		}
		if handlerErr != nil {
			// The error handler runs after this middleware returns, so the
			// response status is still the default: take the status from the
			// error itself.
			fields = append(fields,
				zap.Int("status", statusFromError(handlerErr)),
				zap.Error(handlerErr),
			)
			reqLog.Error("request failed", fields...)
			return handlerErr
		}

		reqLog.Info("request", fields...)
		return nil
	}
}

// RequestID returns the request identifier, generating one when the middleware
// was not installed (unit tests that call a handler directly).
func RequestID(c *fiber.Ctx) string {
	if id, ok := c.Locals(localRequestID).(string); ok && id != "" {
		return id
	}
	return uuid.NewString()
}

// WorkspaceID returns the active workspace carried by the request, or an empty
// string when the request has no tenant context yet.
func WorkspaceID(c *fiber.Ctx) string {
	id, _ := c.Locals(localWorkspaceID).(string)
	return id
}

// TraceID returns the correlation id shared with tracing (EPIC 13, #105). It
// generates one when the middleware was not installed, so a handler can always
// attach a trace id to its own log lines.
func TraceID(c *fiber.Ctx) string {
	if id, ok := c.Locals(localTraceID).(string); ok && id != "" {
		return id
	}
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

// Logger returns the request-scoped logger. Handlers must log through it so
// every line carries request_id, trace_id and workspace_id.
func Logger(c *fiber.Ctx) *zap.Logger {
	if l, ok := c.Locals(localLogger).(*zap.Logger); ok && l != nil {
		return l
	}
	return zap.NewNop()
}

// statusFromError reports the status code an error will produce: Fiber errors
// carry their own code, anything else becomes a 500.
func statusFromError(err error) int {
	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		return fiberErr.Code
	}
	return fiber.StatusInternalServerError
}

// traceIDFrom reads the request's own span first, which is the identifier the trace in the
// collector is filed under — the two must be the same string or the log line and the trace it
// names are different requests. It falls back to a W3C traceparent header, then X-Trace-Id, and
// generates an id when none is present, so a trace id is always available.
func traceIDFrom(c *fiber.Ctx) string {
	if span := SpanFrom(c); span != nil {
		if id := span.SpanContext().TraceID(); id.IsValid() {
			return id.String()
		}
	}
	if matches := traceparentFormat.FindStringSubmatch(strings.ToLower(c.Get(HeaderTraceparent))); matches != nil {
		return matches[1]
	}
	if id := sanitizeID(c.Get(HeaderTraceID)); id != "" {
		return strings.ReplaceAll(id, "-", "")
	}
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

// sanitizeID accepts only UUID-shaped values so a caller cannot push arbitrary
// content into the logs or the response headers.
func sanitizeID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !uuidPattern.MatchString(value) {
		return ""
	}
	return value
}

func requestHeaders(c *fiber.Ctx) http.Header {
	headers := http.Header{}
	c.Request().Header.VisitAll(func(key, value []byte) {
		headers.Add(string(key), string(value))
	})
	return headers
}
