package middleware

import (
	"bytes"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/anakbolu/timbolu/packages/platform/logger"
)

func newTestLogger(buf *bytes.Buffer, level zapcore.Level) *zap.Logger {
	cfg := zap.NewProductionEncoderConfig()
	cfg.TimeKey = "ts"
	return zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(cfg), zapcore.AddSync(buf), level))
}

func newApp(buf *bytes.Buffer, level zapcore.Level, handler fiber.Handler) *fiber.App {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(RequestContext(newTestLogger(buf, level)))
	app.Get("/things", handler)
	return app
}

func TestRequestContextGeneratesIdentifiers(t *testing.T) {
	var buf bytes.Buffer
	app := newApp(&buf, zapcore.InfoLevel, func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"request_id":   RequestID(c),
			"trace_id":     TraceID(c),
			"workspace_id": WorkspaceID(c),
			"logger":       Logger(c) != nil,
		})
	})

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/things", nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get(HeaderRequestID); uuid.Validate(got) != nil {
		t.Fatalf("response must echo a generated request id, got %q", got)
	}
	if got := resp.Header.Get(HeaderTraceID); !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(got) {
		t.Fatalf("response must echo a 32-hex trace id, got %q", got)
	}

	body := bodyString(t, resp.Body)
	if !strings.Contains(body, `"logger":true`) {
		t.Fatalf("handler must receive a logger, got %s", body)
	}
	if !strings.Contains(buf.String(), `"request_id"`) || !strings.Contains(buf.String(), `"trace_id"`) {
		t.Fatalf("log line is missing the request context: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"status":200`) || !strings.Contains(buf.String(), `"latency"`) {
		t.Fatalf("log line is missing the response fields: %s", buf.String())
	}
}

func TestRequestContextUsesCallerIdentifiers(t *testing.T) {
	var buf bytes.Buffer
	requestID := uuid.NewString()
	workspaceID := uuid.NewString()
	traceparent := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	app := newApp(&buf, zapcore.InfoLevel, func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"request_id":   RequestID(c),
			"workspace_id": WorkspaceID(c),
			"trace_id":     TraceID(c),
		})
	})

	req := httptest.NewRequest(fiber.MethodGet, "/things", nil)
	req.Header.Set(HeaderRequestID, requestID)
	req.Header.Set(HeaderWorkspaceID, workspaceID)
	req.Header.Set(HeaderTraceparent, traceparent)

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body := bodyString(t, resp.Body)
	for _, want := range []string{requestID, workspaceID, "4bf92f3577b34da6a3ce929d0e0e4736"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q in the handler context, got %s", want, body)
		}
	}
	if !strings.Contains(buf.String(), `"workspace_id":"`+workspaceID+`"`) {
		t.Fatalf("log line must carry the workspace id: %s", buf.String())
	}
}

// TestRequestContextRejectsMalformedIdentifiers keeps arbitrary caller content
// out of the logs and the response headers.
func TestRequestContextRejectsMalformedIdentifiers(t *testing.T) {
	var buf bytes.Buffer
	app := newApp(&buf, zapcore.InfoLevel, func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"workspace_id": WorkspaceID(c)})
	})

	req := httptest.NewRequest(fiber.MethodGet, "/things", nil)
	req.Header.Set(HeaderWorkspaceID, "not-a-uuid; DROP TABLE workspaces")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if body := bodyString(t, resp.Body); !strings.Contains(body, `"workspace_id":""`) {
		t.Fatalf("expected the malformed workspace id to be dropped, got %s", body)
	}
	if strings.Contains(buf.String(), "DROP TABLE") {
		t.Fatalf("malformed header leaked into the log: %s", buf.String())
	}
}

// TestRequestContextRedactsSecrets is the redaction gate: tokens in headers and
// query parameters must never reach the log.
func TestRequestContextRedactsSecrets(t *testing.T) {
	var buf bytes.Buffer
	app := newApp(&buf, zapcore.DebugLevel, func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	req := httptest.NewRequest(fiber.MethodGet, "/things?access_token=super-secret-query&page=2", nil)
	req.Header.Set("Authorization", "Bearer super-secret-header")
	req.Header.Set("X-Api-Key", "super-secret-key")
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	logged := buf.String()
	for _, secret := range []string{"super-secret-query", "super-secret-header", "super-secret-key"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("secret %q leaked into the log: %s", secret, logged)
		}
	}
	if !strings.Contains(logged, logger.Redacted) {
		t.Fatalf("expected redaction markers in the log: %s", logged)
	}
	if !strings.Contains(logged, "application/json") {
		t.Fatalf("harmless headers must survive: %s", logged)
	}
}

func TestRequestContextLogsHandlerErrors(t *testing.T) {
	var buf bytes.Buffer

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ErrorHandler: func(c *fiber.Ctx, _ error) error {
			return c.SendStatus(fiber.StatusTeapot)
		},
	})
	app.Use(RequestContext(newTestLogger(&buf, zapcore.InfoLevel)))
	app.Get("/things", func(c *fiber.Ctx) error {
		return fiber.NewError(fiber.StatusTeapot, "boom")
	})

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/things", nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != fiber.StatusTeapot {
		t.Fatalf("expected 418, got %d", resp.StatusCode)
	}
	if !strings.Contains(buf.String(), "request failed") {
		t.Fatalf("expected the failure to be logged: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"status":418`) {
		t.Fatalf("expected the status in the failure log: %s", buf.String())
	}
}

func TestLoggerFallsBackWhenMiddlewareIsAbsent(t *testing.T) {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	// The accessors must be usable without the middleware; a handler failure is
	// reported through the response instead of t.Fatal, which is illegal from
	// the goroutine Fiber runs the handler in.
	app.Get("/bare", func(c *fiber.Ctx) error {
		if RequestID(c) == "" || Logger(c) == nil || TraceID(c) == "" {
			return c.SendStatus(fiber.StatusInternalServerError)
		}
		return c.SendStatus(fiber.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/bare", nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("accessors must work without the middleware, got status %d", resp.StatusCode)
	}
}
