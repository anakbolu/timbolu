package container

import (
	"context"

	temporalotel "go.temporal.io/sdk/contrib/opentelemetry"
	"go.temporal.io/sdk/interceptor"

	"github.com/robzlabz/employeebot/packages/platform/config"
	"github.com/robzlabz/employeebot/packages/platform/telemetry"
)

// openTelemetry installs the tracer provider and the W3C propagator.
//
// It runs first, before the database and before Temporal, because everything after it may start a
// span: the HTTP middleware, the workflows, the activities, and the model calls inside them. A
// provider installed after the client was dialled would leave the first spans of a process
// unattached to anything.
//
// A deployment with no exporter is still installed. The spans are dropped and the trace ids are
// real, which is what keeps a log line and a request correlated in development — the case somebody
// is looking at when they need it.
func (c *Container) openTelemetry(ctx context.Context, cfg *config.Config) error {
	provider, err := telemetry.Setup(ctx, telemetry.Config{
		ServiceName: cfg.Telemetry.ServiceName,
		Environment: cfg.Application.Environment,
		Exporter:    cfg.Telemetry.Exporter,
		Endpoint:    cfg.Telemetry.Endpoint,
		SampleRatio: cfg.Telemetry.SampleRatio,
		Insecure:    cfg.Telemetry.Insecure,
	}, c.Logger)
	if err != nil {
		return err
	}

	c.Telemetry = provider

	return nil
}

// temporalInterceptors returns the tracing interceptor Temporal is dialled and run with.
//
// It is what carries a trace across the process boundary: the workflow's span is a child of the
// API request that started it, and each activity's span is a child of the workflow's — including
// when the worker is a different process, or the activity is retried an hour later. Without it, a
// trace would end at the API and the work it asked for would be untraceable, which is the half of
// the story an incident is actually about.
func (c *Container) temporalInterceptors() (interceptor.Interceptor, error) {
	if c.Telemetry == nil {
		return nil, nil
	}

	// The tracer is passed explicitly rather than taken from the global provider, so a container
	// built in a test traces into the test's provider.
	return temporalotel.NewTracingInterceptor(temporalotel.TracerOptions{
		Tracer: c.Telemetry.Tracer(),
	})
}

// temporalClientInterceptors is the interceptor the client is dialled with, as a list — and an
// empty one when telemetry was never installed, so a deployment without a provider dials Temporal
// exactly as it did before.
func temporalClientInterceptors(tracing interceptor.Interceptor) []interceptor.ClientInterceptor {
	if tracing == nil {
		return nil
	}

	return []interceptor.ClientInterceptor{tracing}
}

// temporalWorkerInterceptors is the same interceptor for a worker.
func temporalWorkerInterceptors(tracing interceptor.Interceptor) []interceptor.WorkerInterceptor {
	if tracing == nil {
		return nil
	}

	return []interceptor.WorkerInterceptor{tracing}
}
