// The tracing handler: one HTTP span per request, and the trace id the request's logs carry.
//
// It is installed before the request-context middleware so the logger and the span agree: the
// request logger's `trace_id` is read off the span this handler started, which is what makes a log
// line and a trace the same request rather than two identifiers for it.
package middleware

import (
	"github.com/gofiber/fiber/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
	"go.opentelemetry.io/otel/trace"
)

// SpanContextKey is the Locals key the request's span is stored under, so a middleware that runs
// later — or a test — can reach it without going through the user context.
const SpanContextKey = "otel_span"

// Tracing starts one server span per request and carries it into the handler chain.
//
// What it records is deliberately thin: the method, the matched *route* rather than the path, the
// status, and the workspace once the tenant is known. **The URL and the query string are never
// recorded**, and that is not an oversight — the event stream authenticates from the query string,
// so a span carrying the URL would carry a token into a second system. The request log redacts its
// query for the same reason; a trace has no redactor, so it keeps none.
//
// A caller's `traceparent` is honoured, which is what stitches this deployment's spans to whatever
// is upstream of it. A deployment with no exporter still runs this: the span's trace id is what the
// log lines carry, and dropping that would mean a request with no way to correlate its own logs.
func Tracing(tracer trace.Tracer) fiber.Handler {
	propagator := otel.GetTextMapPropagator()

	return func(c *fiber.Ctx) error {
		ctx := propagator.Extract(c.UserContext(), propagation.HeaderCarrier(requestHeaders(c)))

		ctx, span := tracer.Start(ctx, spanName(c, ""),
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(semconv.HTTPRequestMethodKey.String(c.Method())),
		)
		defer span.End()

		// The span travels on the user context, which is how every handler passes a context to a
		// service: a service that starts a child span is then part of this request's trace without
		// knowing anything about HTTP.
		c.SetUserContext(trace.ContextWithSpan(ctx, span))
		c.Locals(SpanContextKey, span)

		handlerErr := c.Next()

		// The route is only known once the chain has matched, so the name is finished here rather
		// than at the start. A route that never matched keeps the path it was asked for, which is
		// exactly what a 404 wants to say.
		if route := c.Route(); route != nil && route.Path != "" {
			span.SetName(spanName(c, route.Path))
			span.SetAttributes(semconv.HTTPRouteKey.String(route.Path))
		}

		if workspace := WorkspaceID(c); workspace != "" {
			span.SetAttributes(attribute.String("workspace_id", workspace))
		}

		status := c.Response().StatusCode()
		if handlerErr != nil {
			status = statusFromError(handlerErr)
			// The reason is recorded as an error event with its message. It is the deployment's
			// own text — a refusal's explanation — and not customer content: what a model or a
			// tool said never travels through an HTTP error.
			span.RecordError(handlerErr)
		}

		span.SetAttributes(semconv.HTTPResponseStatusCodeKey.Int(status))
		if status >= fiber.StatusInternalServerError {
			span.SetStatus(codes.Error, "the request failed")
		}

		return handlerErr
	}
}

// SpanFrom returns the span a request is running under, or nil when the tracing middleware was not
// installed.
func SpanFrom(c *fiber.Ctx) trace.Span {
	span, _ := c.Locals(SpanContextKey).(trace.Span)

	return span
}

// spanName is the span's name: the method and the route, or the path until the route is known.
func spanName(c *fiber.Ctx, route string) string {
	if route == "" {
		route = c.Path()
	}

	return c.Method() + " " + route
}
