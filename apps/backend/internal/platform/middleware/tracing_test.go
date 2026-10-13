package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap/zapcore"
)

// newRecordingTracer installs a provider whose spans can be read back, and returns the exporter and
// the tracer the middleware is installed with.
//
// The provider is also set globally, because that is where the W3C propagator is read from — and a
// test that set its own propagator would be proving its own copy rather than the deployment's
// configuration.
func newRecordingTracer(t *testing.T) (*tracetest.InMemoryExporter, trace.Tracer) {
	t.Helper()

	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { _ = provider.Shutdown(t.Context()) })

	return exporter, provider.Tracer("bolu")
}

// tracedApp builds an app with tracing and the request-context middleware, in the order the
// deployment installs them.
func tracedApp(tracer trace.Tracer, buf *bytes.Buffer, handler fiber.Handler) *fiber.App {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(Tracing(tracer))
	app.Use(RequestContext(newTestLogger(buf, zapcore.InfoLevel)))
	app.Get("/api/things", handler)
	return app
}

// TestTheRequestLogCarriesTheSpansTraceID is the reason both middleware exist in this order: the
// trace id in a log line must be the trace id in the collector, or a line and the trace it names are
// two different requests.
func TestTheRequestLogCarriesTheSpansTraceID(t *testing.T) {
	exporter, tracer := newRecordingTracer(t)

	// A caller upstream of this deployment, whose trace must be joined rather than replaced.
	upstream := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{9, 8, 7},
		SpanID:     trace.SpanID{6, 5, 4},
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})

	var buf bytes.Buffer
	app := tracedApp(tracer, &buf, func(c *fiber.Ctx) error {
		Logger(c).Info("handled")
		return c.SendString("ok")
	})

	request := httptest.NewRequest(fiber.MethodGet, "/api/things", nil)
	// The W3C header as a caller sends it: version, trace id, span id, flags. Written out rather
	// than injected, so the extraction being asserted is the deployment's rather than this
	// test's own round trip.
	request.Header.Set("traceparent",
		fmt.Sprintf("00-%s-%s-01", upstream.TraceID(), upstream.SpanID()))

	response, err := app.Test(request, -1)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusOK, response.StatusCode)
	defer func() { require.NoError(t, response.Body.Close()) }()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	span := spans[0]

	require.Equal(t, upstream.TraceID(), span.SpanContext.TraceID(), "the caller's trace is joined")
	require.Equal(t, upstream.SpanID(), span.Parent.SpanID(), "and the caller is the parent")
	require.Equal(t, fiber.MethodGet+" /api/things", span.Name, "the name is the route, not the path")

	// The response echo and the log line both carry that same identifier.
	require.Equal(t, upstream.TraceID().String(), response.Header.Get(HeaderTraceID))

	// The handler's own line, out of the two the request writes: the middleware's summary and
	// the line the handler logged through the logger it was handed.
	var handled map[string]any
	for _, written := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		line := map[string]any{}
		require.NoError(t, json.Unmarshal(written, &line))
		if line["msg"] == "handled" {
			handled = line
		}
	}

	require.NotNil(t, handled, "the handler's line was written")
	require.Equal(t, upstream.TraceID().String(), handled["trace_id"], "the log line names the trace")
}

// TestTheQueryStringNeverReachesTheSpan is a security property rather than a nicety: the event stream
// authenticates from the query string, so a span carrying the URL would carry a token into a second
// store with its own access — one this product's retention does not reach.
func TestTheQueryStringNeverReachesTheSpan(t *testing.T) {
	exporter, tracer := newRecordingTracer(t)

	app := tracedApp(tracer, &bytes.Buffer{}, func(c *fiber.Ctx) error {
		return c.SendString("ok")
	})

	response, err := app.Test(httptest.NewRequest(
		fiber.MethodGet, "/api/things?access_token=super-secret-token&limit=10", nil), -1)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusOK, response.StatusCode)
	defer func() { require.NoError(t, response.Body.Close()) }()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)

	require.NotContains(t, spans[0].Name, "token", "the name is the route")
	for _, attribute := range spans[0].Attributes {
		require.NotContains(t, attribute.Value.String(), "super-secret-token",
			"attribute %s carries the query string", attribute.Key)
		require.NotContains(t, attribute.Value.String(), "limit=10")
	}
}

// TestAFailedRequestIsMarkedOnTheSpan: the status a screen sees and the status a trace shows are the
// same fact, including when the failure never produced a response body.
func TestAFailedRequestIsMarkedOnTheSpan(t *testing.T) {
	exporter, tracer := newRecordingTracer(t)

	app := tracedApp(tracer, &bytes.Buffer{}, func(*fiber.Ctx) error {
		return fiber.NewError(fiber.StatusServiceUnavailable, "the hub is unreachable")
	})

	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/api/things", nil), -1)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, fiber.StatusServiceUnavailable, response.StatusCode,
		"the error becomes the answer, which is the case the status is taken from the error for")

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)

	var status int64
	for _, attribute := range spans[0].Attributes {
		if attribute.Key == "http.response.status_code" {
			status = attribute.Value.AsInt64()
		}
	}
	require.Equal(t, int64(fiber.StatusServiceUnavailable), status)
	require.NotEmpty(t, spans[0].Events, "the error is recorded on the span")
}

// TestTheSpanTravelsOnTheUserContext: a service starts its own span from the context it is handed,
// and it must be a child of the request's. That is what makes a model call part of the same trace as
// the API request that asked for it.
func TestTheSpanTravelsOnTheUserContext(t *testing.T) {
	exporter, tracer := newRecordingTracer(t)

	var child trace.SpanContext
	app := tracedApp(tracer, &bytes.Buffer{}, func(c *fiber.Ctx) error {
		_, span := tracer.Start(c.UserContext(), "a-service-call")
		child = span.SpanContext()
		span.End()

		return c.SendString("ok")
	})

	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/api/things", nil), -1)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, fiber.StatusOK, response.StatusCode)

	spans := exporter.GetSpans()
	require.Len(t, spans, 2)

	require.Equal(t, spans[0].SpanContext.TraceID(), child.TraceID())
	require.Equal(t, spans[0].SpanContext.SpanID(), child.SpanID(), "the request is the parent")
}
