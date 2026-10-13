// Package telemetry owns the deployment's OpenTelemetry setup: the tracer provider, the
// propagator, and the exporters that turn a trace into something an operator can look at.
//
// One trace is meant to hold the whole of a piece of work: the API request that asked for it, the
// workflow step that ran it, the tools it called, and the model calls it made. Nothing here knows
// what a task is — the spans are started where the work is, and what this package provides is the
// one provider they all report to and the one propagation format that carries a trace across the
// process boundary between the API and a worker.
//
// Three decisions are worth stating rather than discovering later:
//
//   - **A deployment with no exporter is still traced.** The provider is always installed, so a
//     request has a trace id and every log line can carry it; only the spans are dropped. A
//     collector that is down, or a development run with no collector at all, must not mean "no
//     correlation between a log line and the request that produced it" — that is the case
//     somebody is debugging when they need it most.
//   - **Sampling is parent-based, with a ratio for roots.** A trace that a caller already decided
//     to sample stays sampled in every service it touches, and a ratio decides the rest. Without
//     the parent half, a sampled caller's trace would arrive at the worker and be dropped, which
//     is a trace with a hole exactly where the work happened.
//   - **No customer content is ever an attribute.** Spans carry identifiers, counts, model names,
//     and outcomes. A prompt, a message, or a tool's result is the customer's words, and a trace
//     that carries them is a copy of the conversation in a second system nobody pruned.
package telemetry

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
)

// Exporters a deployment may name.
const (
	// ExporterNone installs the provider and drops the spans: a trace id exists for the logs and
	// nothing leaves the process. It is the default, because a deployment that has not said where
	// telemetry should go should not be sending it anywhere.
	ExporterNone = "none"
	// ExporterConsole prints spans to standard output, which is what a development run wants.
	ExporterConsole = "console"
	// ExporterOTLP sends spans over gRPC to a collector.
	ExporterOTLP = "otlp"
)

// ShutdownTimeout bounds the flush on the way out, so a process that is stopping still exits.
const ShutdownTimeout = 5 * time.Second

// Config is how a process is traced.
type Config struct {
	// ServiceName is what the spans are filed under: "bolu-api", "bolu-agent-worker".
	ServiceName string
	// Environment is the deployment, e.g. "local" or "production".
	Environment string
	// Exporter is ExporterNone, ExporterConsole, or ExporterOTLP. An empty value is
	// ExporterNone.
	Exporter string
	// Endpoint is the collector's host:port. It is required by ExporterOTLP and ignored by the
	// others.
	Endpoint string
	// SampleRatio is the share of root traces to keep, between 0 and 1. Zero means none, which
	// is a deployment that only follows traces its callers sampled.
	SampleRatio float64
	// Insecure drops TLS on the collector connection, which is what a compose collector wants.
	Insecure bool
}

// Provider is the installed tracer provider.
type Provider struct {
	provider *sdktrace.TracerProvider
	enabled  bool
	logger   *zap.Logger
}

// Setup installs the provider globally and sets the W3C propagator.
//
// The propagator is set even when no tracer is exported: the `traceparent` header is what connects
// this deployment's spans to a caller's, and a deployment that dropped the header would break
// every trace that passes through it.
func Setup(ctx context.Context, config Config, logger *zap.Logger) (*Provider, error) {
	if logger == nil {
		logger = zap.NewNop()
	}

	exporter, enabled, err := buildExporter(ctx, config)
	if err != nil {
		return nil, err
	}

	options := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio(config.SampleRatio)))),
		sdktrace.WithResource(buildResource(ctx, config)),
	}
	if exporter != nil {
		options = append(options, sdktrace.WithBatcher(exporter))
	}

	provider := sdktrace.NewTracerProvider(options...)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		if err != nil {
			logger.Warn("telemetry reported an error", zap.Error(err))
		}
	}))

	logger.Info("telemetry installed",
		zap.String("service", config.ServiceName),
		zap.String("exporter", strings.ToLower(strings.TrimSpace(config.Exporter))),
		zap.Bool("exporting", enabled),
		zap.Float64("sample_ratio", sampleRatio(config.SampleRatio)),
	)

	return &Provider{provider: provider, enabled: enabled, logger: logger}, nil
}

// Shutdown flushes what is buffered and releases the exporter. Safe on a nil provider, and safe to
// call more than once.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.provider == nil {
		return nil
	}

	// The caller's deadline may already be gone — a process shutting down on a signal has one —
	// so the flush gets its own.
	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ShutdownTimeout)
	defer cancel()

	return p.provider.Shutdown(flushCtx)
}

// Enabled reports whether spans leave the process.
func (p *Provider) Enabled() bool { return p != nil && p.enabled }

// Tracer returns the deployment's tracer.
func (p *Provider) Tracer() trace.Tracer {
	if p == nil || p.provider == nil {
		return noop.NewTracerProvider().Tracer(ScopeName)
	}

	return p.provider.Tracer(ScopeName)
}

// ScopeName is the instrumentation scope every span this codebase starts is filed under, so a
// collector's view groups this application's spans together.
const ScopeName = "github.com/robzlabz/employeebot"

// buildExporter picks the exporter from the configuration.
func buildExporter(ctx context.Context, config Config) (sdktrace.SpanExporter, bool, error) {
	switch strings.ToLower(strings.TrimSpace(config.Exporter)) {
	case "", ExporterNone:
		return nil, false, nil

	case ExporterConsole:
		exporter, err := stdouttrace.New()
		if err != nil {
			return nil, false, fmt.Errorf("telemetry: console exporter: %w", err)
		}

		return exporter, true, nil

	case ExporterOTLP:
		if strings.TrimSpace(config.Endpoint) == "" {
			return nil, false, fmt.Errorf("telemetry: the otlp exporter needs an endpoint")
		}

		options := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(config.Endpoint)}
		if config.Insecure {
			options = append(options, otlptracegrpc.WithInsecure())
		}

		exporter, err := otlptracegrpc.New(ctx, options...)
		if err != nil {
			return nil, false, fmt.Errorf("telemetry: otlp exporter: %w", err)
		}

		return exporter, true, nil

	default:
		return nil, false, fmt.Errorf("telemetry: unknown exporter %q", config.Exporter)
	}
}

// buildResource describes the process the spans came from.
//
// The service name and the environment are the two an operator filters by, and they are set from
// the configuration rather than from the environment variable conventions: a deployment that names
// its service in its own settings should not have to set a second variable for the SDK to agree.
func buildResource(ctx context.Context, config Config) *resource.Resource {
	attributes := []attribute.KeyValue{semconv.ServiceName(config.ServiceName)}
	if config.Environment != "" {
		attributes = append(attributes, semconv.DeploymentEnvironmentName(config.Environment))
	}

	built, err := resource.New(ctx,
		resource.WithAttributes(attributes...),
		// The process and host detectors add what a collector needs to group spans, and they are
		// cheap: a handful of attributes read once at startup.
		resource.WithProcessPID(),
		resource.WithHost(),
	)
	if err != nil {
		// A resource that could not be fully detected is still a usable one — the schema URL and
		// the attributes set above survive — so the fallback is the plain resource rather than an
		// error that would leave the process untraced.
		return resource.NewSchemaless(attributes...)
	}

	return built
}

// sampleRatio keeps the ratio inside its own domain, where zero means "no roots" rather than
// "unset", so a deployment that wants none can say so.
func sampleRatio(ratio float64) float64 {
	switch {
	case ratio < 0:
		return 0
	case ratio > 1:
		return 1
	default:
		return ratio
	}
}
