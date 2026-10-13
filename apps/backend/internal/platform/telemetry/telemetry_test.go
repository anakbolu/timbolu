package telemetry

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// TestAnUnknownExporterIsRefused: a deployment that misspells where its telemetry should go must
// hear about it at startup rather than run untraced and discover it during an incident.
func TestAnUnknownExporterIsRefused(t *testing.T) {
	_, err := Setup(t.Context(), Config{ServiceName: "bolu-api", Exporter: "datadog"}, zap.NewNop())
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown exporter")

	// The otlp exporter is the one that has a required setting of its own.
	_, err = Setup(t.Context(), Config{ServiceName: "bolu-api", Exporter: ExporterOTLP}, zap.NewNop())
	require.Error(t, err)
	require.Contains(t, err.Error(), "endpoint")
}

// TestADeploymentWithoutAnExporterStillHasTraceIDs is the decision that matters for development
// and for an outage: spans are dropped, and a request still has a trace id its log lines carry.
// Without it, "no collector" would mean "no correlation between a log line and the request".
func TestADeploymentWithoutAnExporterStillHasTraceIDs(t *testing.T) {
	provider, err := Setup(t.Context(), Config{ServiceName: "bolu-api", Exporter: ExporterNone}, zap.NewNop())
	require.NoError(t, err)
	require.False(t, provider.Enabled(), "nothing leaves the process")
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	// The provider is installed globally, which is what a package that never received it relies
	// on for its spans to be part of the same trace.
	require.Equal(t, provider.Tracer(), otel.GetTracerProvider().Tracer(ScopeName))

	_, span := provider.Tracer().Start(t.Context(), "an-operation")
	defer span.End()

	require.True(t, span.SpanContext().IsValid(), "a trace id exists even when the span is dropped")
	require.False(t, span.IsRecording(), "and nothing is collected")
}

// TestSamplingIsParentBased: a trace a caller already sampled stays sampled in this deployment, and
// the ratio decides the roots. The first half is the important one — dropping a sampled caller's
// work would leave a trace with a hole exactly where the work happened.
func TestSamplingIsParentBased(t *testing.T) {
	t.Run("a sampled parent wins over a ratio of zero", func(t *testing.T) {
		provider, err := Setup(t.Context(), Config{
			ServiceName: "bolu-api", Exporter: ExporterNone, SampleRatio: 0,
		}, zap.NewNop())
		require.NoError(t, err)
		t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

		// A root of this deployment is dropped at a ratio of zero.
		_, root := provider.Tracer().Start(t.Context(), "root")
		defer root.End()
		require.False(t, root.SpanContext().IsSampled())

		sampled := trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    trace.TraceID{1, 2, 3},
			SpanID:     trace.SpanID{4, 5, 6},
			TraceFlags: trace.FlagsSampled,
			Remote:     true,
		})
		parent := trace.ContextWithSpanContext(t.Context(), sampled)

		_, child := provider.Tracer().Start(parent, "child")
		defer child.End()

		require.True(t, child.SpanContext().IsSampled(), "the caller's decision holds")
		require.Equal(t, sampled.TraceID(), child.SpanContext().TraceID(), "and it is the same trace")
	})

	t.Run("a ratio of one keeps the roots", func(t *testing.T) {
		provider, err := Setup(t.Context(), Config{
			ServiceName: "bolu-api", Exporter: ExporterNone, SampleRatio: 1,
		}, zap.NewNop())
		require.NoError(t, err)
		t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

		_, root := provider.Tracer().Start(t.Context(), "root")
		defer root.End()

		require.True(t, root.SpanContext().IsSampled())
	})
}

// TestTheTraceCrossesAProcessBoundaryOverW3C: injecting into headers and extracting on the other
// side is what joins the API's span to a worker's. It is asserted through the global propagator
// rather than the package's own, because that is the one the middleware uses.
func TestTheTraceCrossesAProcessBoundaryOverW3C(t *testing.T) {
	provider, err := Setup(t.Context(), Config{ServiceName: "bolu-api", Exporter: ExporterNone}, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	ctx, span := provider.Tracer().Start(t.Context(), "api-request")
	defer span.End()

	headers := propagation.HeaderCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, headers)
	require.NotEmpty(t, headers.Get("traceparent"), "the W3C header is what carries it")

	// The other side of the boundary: a fresh context, the same header.
	extracted := otel.GetTextMapPropagator().Extract(context.Background(), headers)
	remote := trace.SpanContextFromContext(extracted)

	require.Equal(t, span.SpanContext().TraceID(), remote.TraceID())
	require.Equal(t, span.SpanContext().SpanID(), remote.SpanID(), "the caller is the parent")
	require.True(t, remote.IsRemote())
}

// TestASampleRatioOutsideItsDomainIsClamped: a deployment that writes 2 or -1 in a settings file
// means "everything" or "nothing" rather than a number the sampler has to interpret.
func TestASampleRatioOutsideItsDomainIsClamped(t *testing.T) {
	require.Equal(t, 0.0, sampleRatio(-1))
	require.Equal(t, 1.0, sampleRatio(2))
	require.Equal(t, 0.25, sampleRatio(0.25))
}
