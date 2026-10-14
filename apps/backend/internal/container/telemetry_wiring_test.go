package container

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/packages/platform/config"
)

// The wiring tests for telemetry cover what only the container can get wrong: that a deployment
// which named no exporter still has a tracer for its logs, that a container without one asks Temporal
// for no interceptor rather than panicking, and that the service name is the binary's.

// TestTelemetryIsInstalledEvenWithNoExporter: spans are dropped and a trace id exists, which is what
// keeps a log line and a request correlated in development.
func TestTelemetryIsInstalledEvenWithNoExporter(t *testing.T) {
	c := &Container{
		Config: testConfig(),
		Logger: zap.NewNop(),
	}

	require.NoError(t, c.openTelemetry(t.Context(), &config.Config{
		Telemetry: config.TelemetryConfig{ServiceName: "bolu-api", Exporter: "none"},
	}))
	require.NotNil(t, c.Telemetry)
	require.False(t, c.Telemetry.Enabled())

	// And the tracer the middleware and the modules are handed is real: a span started from it
	// has a valid trace id even though nothing exports it.
	_, span := c.telemetryTracer().Start(t.Context(), "an-operation")
	defer span.End()
	require.True(t, span.SpanContext().IsValid())

	t.Cleanup(func() { require.NoError(t, c.Telemetry.Shutdown(t.Context())) })
}

// TestAnUnknownExporterStopsTheProcess: where telemetry goes is configuration, and a typo in it is a
// startup failure rather than a deployment that silently reports nothing.
func TestAnUnknownExporterStopsTheProcess(t *testing.T) {
	c := &Container{Config: testConfig(), Logger: zap.NewNop()}

	err := c.openTelemetry(t.Context(), &config.Config{
		Telemetry: config.TelemetryConfig{ServiceName: "bolu-api", Exporter: "honeycomb"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown exporter")
	require.Nil(t, c.Telemetry)
}

// TestAContainerWithoutTelemetryAsksForNoInterceptor: a deployment with telemetry off must dial
// Temporal exactly as it did before, and a test container must be able to ask for the interceptors
// without a provider to build them from.
func TestAContainerWithoutTelemetryAsksForNoInterceptor(t *testing.T) {
	c := &Container{Logger: zap.NewNop()}

	tracing, err := c.temporalInterceptors()
	require.NoError(t, err)
	require.Nil(t, tracing)
	require.Nil(t, temporalClientInterceptors(tracing))
	require.Nil(t, temporalWorkerInterceptors(tracing))

	// And the middleware's tracer is a no-op one rather than a nil dereference.
	require.NotNil(t, c.telemetryTracer())
	require.Equal(t, noop.NewTracerProvider().Tracer("bolu"), c.telemetryTracer())
}

// TestTheInterceptorIsBuiltFromTheProvidersTracer: with telemetry installed, both the client and the
// worker are given the same interceptor — the workflow's span hangs under the request that started
// it, and each activity's under the workflow's.
func TestTheInterceptorIsBuiltFromTheProvidersTracer(t *testing.T) {
	c := &Container{Config: testConfig(), Logger: zap.NewNop()}
	require.NoError(t, c.openTelemetry(t.Context(), &config.Config{
		Telemetry: config.TelemetryConfig{ServiceName: "bolu-agent-worker", Exporter: "none"},
	}))
	t.Cleanup(func() { require.NoError(t, c.Telemetry.Shutdown(t.Context())) })

	tracing, err := c.temporalInterceptors()
	require.NoError(t, err)
	require.NotNil(t, tracing)

	require.Len(t, temporalClientInterceptors(tracing), 1)
	require.Len(t, temporalWorkerInterceptors(tracing), 1)
}

// TestABinaryNamesItselfUnlessTheDeploymentDid: the API and the workers are separate services in a
// collector, and a deployment that named the service itself keeps that name.
func TestABinaryNamesItselfUnlessTheDeploymentDid(t *testing.T) {
	unnamed := config.TelemetryConfig{}
	unnamed.DefaultServiceName("bolu-agent-worker")
	require.Equal(t, "bolu-agent-worker", unnamed.ServiceName)

	named := config.TelemetryConfig{ServiceName: "bolu-prod"}
	named.DefaultServiceName("bolu-agent-worker")
	require.Equal(t, "bolu-prod", named.ServiceName)
}
