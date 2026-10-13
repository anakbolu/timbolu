package container

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
)

// tracingMiddleware starts the request's span.
//
// It is a method rather than a free function because the tracer comes from the deployment's
// provider, and a container built in a test traces into the test's provider rather than into
// whatever is installed globally.
func (c *Container) tracingMiddleware() fiber.Handler {
	return middleware.Tracing(c.telemetryTracer())
}

// telemetryTracer is the process's tracer, or a no-op one when telemetry was never installed.
func (c *Container) telemetryTracer() trace.Tracer {
	if c == nil || c.Telemetry == nil {
		return noop.NewTracerProvider().Tracer("bolu")
	}

	return c.Telemetry.Tracer()
}

// requestContextMiddleware attaches request_id, workspace_id and trace_id to
// every request and to the request-scoped logger.
func requestContextMiddleware(log *zap.Logger) fiber.Handler {
	return middleware.RequestContext(log)
}

// corsMiddleware lets the web app call the API from the browser.
//
// The client sends the refresh cookie, so the response must name the caller's
// origin exactly: browsers reject `Access-Control-Allow-Origin: *` on a
// credentialed request. The configured origins are allowed; with none
// configured (local development) the request origin is reflected, which is what
// makes `localhost:3000` and the docker frontend both work.
func corsMiddleware(origins []string) fiber.Handler {
	allowed := make(map[string]bool, len(origins))
	for _, origin := range origins {
		allowed[origin] = true
	}

	return cors.New(cors.Config{
		AllowOrigins: strings.Join(origins, ","),
		AllowMethods: "GET,POST,HEAD,PUT,DELETE,PATCH,OPTIONS",
		// An explicit list, not "*": on a credentialed request the browser reads
		// the wildcard literally instead of as a wildcard, so the preflight
		// would fail for every real header.
		AllowHeaders:  "Content-Type,Authorization,X-Workspace-Id,X-Request-Id,X-Trace-Id,Accept,Origin",
		ExposeHeaders: "X-Request-Id,X-Trace-Id",
		// Required for the refresh cookie to be sent and stored.
		AllowCredentials: true,
		AllowOriginsFunc: func(origin string) bool {
			if origin == "" {
				return false
			}
			// No configured list means local development: reflect the caller.
			return len(allowed) == 0 || allowed[origin]
		},
	})
}

// errorHandler turns an unhandled error into the standard JSON envelope and
// logs it with the request context. Fiber's default handler would answer with
// a plain-text body, which the frontend cannot parse.
func errorHandler(log *zap.Logger) fiber.ErrorHandler {
	return func(c *fiber.Ctx, err error) error {
		code := fiber.StatusInternalServerError
		message := "internal server error"
		errorCode := "internal_error"

		var fiberErr *fiber.Error
		if errors.As(err, &fiberErr) {
			code = fiberErr.Code
			message = fiberErr.Message
			errorCode = "http_error"
		}

		middleware.Logger(c).Error("unhandled error",
			zap.Int("status", code),
			zap.String("path", c.Path()),
			zap.Error(err),
		)

		return response.Error(c, code, message, errorCode, nil)
	}
}
