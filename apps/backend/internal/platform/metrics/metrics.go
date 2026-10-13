// Package metrics owns the deployment's OpenTelemetry meter: the instruments the product records
// into, and the exporters that carry them to a scraper or a collector.
//
// Two decisions shape everything here, and both are about cardinality — which is the only thing that
// makes metrics expensive.
//
//   - **No identifier is ever a label.** No workspace, no Bolu, no task, no workflow. A metric label
//     with unbounded values is a time series per value, and a label naming a workspace would be one
//     set of series per customer, growing without limit and paid for forever. What is recorded is
//     the *shape* of the work — which trigger, which outcome, which reason class — and the
//     per-workspace question is answered by a query over `usage_ledger`, `tasks`, and `drafts`,
//     which is where the rows already are and where the answer can be as precise as the question.
//   - **The reason is classified, not copied.** A failure reason is free text in the database, and
//     the same text as a label is a new time series per message the product learns to produce. The
//     recorder maps it to a small set of classes, and a reason it does not recognise becomes
//     `other` rather than a new series.
//
// The instruments are declared in one place so a name and a unit cannot drift between the module
// that records a value and the dashboard that reads it.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
	"go.uber.org/zap"
)

// Exporters a deployment may name.
const (
	// ExporterNone records nothing, which is the default: a deployment that has not said where its
	// metrics should go should not be building them.
	ExporterNone = "none"
	// ExporterPrometheus serves them to a scraper over HTTP. It is the deployment's own endpoint,
	// which is what a self-hosted Prometheus scrapes.
	ExporterPrometheus = "prometheus"
	// ExporterOTLP pushes them to a collector.
	ExporterOTLP = "otlp"
	// ExporterConsole prints them, which is what a development run wants.
	ExporterConsole = "console"
)

// ScopeName is the instrumentation scope the instruments are filed under.
const ScopeName = "github.com/robzlabz/employeebot"

// Config is how a deployment reports metrics.
type Config struct {
	// ServiceName files the measurements under one service, so the API and the workers are
	// separate in a collector.
	ServiceName string
	// Environment is the deployment, e.g. "local" or "production".
	Environment string
	// Exporter is ExporterNone, ExporterPrometheus, ExporterOTLP, or ExporterConsole.
	Exporter string
	// Endpoint is the collector's host:port, required by the otlp exporter.
	Endpoint string
	// Interval is how often measurements are exported. Zero takes the SDK's default.
	Interval time.Duration
	// Insecure drops TLS to the collector.
	Insecure bool
}

// Provider is the installed meter provider and the instruments built from it.
type Provider struct {
	provider *sdkmetric.MeterProvider
	enabled  bool
	// Handler is the Prometheus scrape handler, present only when that exporter is configured.
	// The container mounts it; a process without an HTTP surface leaves it alone.
	Handler http.Handler
	// Instruments are the product's own measures.
	Instruments *Instruments
}

// Setup installs the meter provider the deployment configured and builds the instruments.
//
// It is the only entry point a binary uses. `NewWithReader` is the same thing over a reader the
// caller supplies, which is what a test reads measurements back through — a test that exported them
// would be proving the exporter rather than the instrument, and one that reached into the provider's
// internals would be proving the test's own wiring.
func Setup(ctx context.Context, config Config, logger *zap.Logger) (*Provider, error) {
	if logger == nil {
		logger = zap.NewNop()
	}

	reader, handler, enabled, err := buildReader(ctx, config)
	if err != nil {
		return nil, err
	}

	provider, err := newProvider(ctx, config, reader, logger)
	if err != nil {
		return nil, err
	}

	provider.enabled = enabled
	provider.Handler = handler

	return provider, nil
}

// NewWithReader builds a provider over the caller's reader, bypassing the configuration.
//
// It exists so a test can read the measurements the instruments recorded, and it is exported rather
// than kept in a test file because that is where the *other* packages' metrics tests need it too:
// the task activity's own test asserts what it recorded, and it should not have to reimplement this.
func NewWithReader(ctx context.Context, config Config, reader sdkmetric.Reader) (*Provider, error) {
	return newProvider(ctx, config, reader, zap.NewNop())
}

// newProvider installs the meter provider and builds the instruments.
func newProvider(ctx context.Context, config Config, reader sdkmetric.Reader, logger *zap.Logger) (*Provider, error) {
	options := []sdkmetric.Option{sdkmetric.WithResource(buildResource(ctx, config))}
	if reader != nil {
		options = append(options, sdkmetric.WithReader(reader))
	}

	provider := sdkmetric.NewMeterProvider(options...)
	otel.SetMeterProvider(provider)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		if err != nil {
			logger.Warn("metrics reported an error", zap.Error(err))
		}
	}))

	instruments, err := newInstruments(provider.Meter(ScopeName))
	if err != nil {
		return nil, err
	}

	logger.Info("metrics installed",
		zap.String("service", config.ServiceName),
		zap.String("exporter", strings.ToLower(strings.TrimSpace(config.Exporter))),
		zap.Bool("exporting", reader != nil),
	)

	return &Provider{provider: provider, enabled: reader != nil, Instruments: instruments}, nil
}

// Shutdown flushes what is buffered and releases the reader. Safe on a nil provider.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.provider == nil {
		return nil
	}

	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	return p.provider.Shutdown(flushCtx)
}

// Enabled reports whether measurements leave the process.
func (p *Provider) Enabled() bool { return p != nil && p.enabled }

// Instruments is the product's measures, declared in one place.
//
// Each is a counter, a histogram, or an up-down counter, and the unit says what it counts. None of
// them takes an identifier as an attribute — see the package comment for why that is the rule rather
// than a preference.
type Instruments struct {
	meter metric.Meter

	// DraftReadySeconds is how long a draft took from its task opening to being ready for a
	// person, which is the product's first promise: the bot did the work.
	draftReady metric.Float64Histogram
	// DraftDecisions counts what a person did with a draft, by outcome. `outcome` is one of
	// approved, revised, rejected.
	draftDecisions metric.Int64Counter

	// TaskFinished counts finished tasks by trigger and outcome, so a failure rate is a query
	// rather than an estimate.
	taskFinished metric.Int64Counter
	// TaskTokens counts tokens a task spent, by direction.
	taskTokens metric.Int64Counter
	// TaskCost counts what a task cost, in micro-rupiah.
	taskCost metric.Int64Counter
	// TaskDuration is how long a task ran, which is what a slow fleet is read from.
	taskDuration metric.Float64Histogram

	// ToolCalls counts tool calls by tool class and outcome. The tool *name* is bounded by the
	// registry rather than by anything a caller sends, which is what makes it a usable label.
	toolCalls metric.Int64Counter
	// ToolFailures counts tools that failed, by class.
	toolFailures metric.Int64Counter
	// ToolDuration is how long a tool took, which is what a slow integration reads as.
	toolDuration metric.Float64Histogram

	// ModelCalls counts calls by provider and purpose.
	modelCalls metric.Int64Counter
	// ModelFallbacks counts calls that had to try another provider, which is the number that
	// explains an unexpected bill.
	modelFallbacks metric.Int64Counter
	// ModelLatency is how long a provider took, by provider.
	modelLatency metric.Float64Histogram

	// RateLimited counts refusals by surface, which is what says whether a limit is too tight.
	rateLimited metric.Int64Counter
	// QueueDepth is how much work is waiting, per queue. It is set from a poll rather than
	// observed here, because only Temporal knows.
	queueDepth metric.Int64UpDownCounter
	// ActivityRetries counts activity attempts past the first, by activity.
	activityRetries metric.Int64Counter
	// TaskDispatched is work this deployment accepted. It is a counter rather than half of an
	// up-down pair, because the two halves happen in different processes: the API dispatches a
	// task and a worker finishes it, so one process's depth would read `+1` forever. Two
	// counters scraped from the same fleet answer the question with a subtraction, which is
	// also what makes the depth survive a worker restart — the counters do not go backwards.
	taskDispatched metric.Int64Counter
	// TaskSettled is work that reached a terminal state: the other half of the subtraction.
	taskSettled metric.Int64Counter

	// RetentionPruned counts what the retention pass removed, by kind.
	retentionPruned metric.Int64Counter
}

// newInstruments builds every instrument, failing loudly on a name or a type a reader would reject.
//
// The failure is not hypothetical: an instrument whose name collides with another's and whose unit
// or kind differs makes the whole reader reject the batch, so a deployment would report nothing at
// all and say nothing about it.
func newInstruments(meter metric.Meter) (*Instruments, error) {
	draftReady, err := meter.Float64Histogram("bolu.draft.ready.duration",
		metric.WithUnit("s"),
		metric.WithDescription("how long a draft took from its task opening to being ready for a person"))
	if err != nil {
		return nil, fmt.Errorf("metrics: draft ready: %w", err)
	}

	draftDecisions, err := meter.Int64Counter("bolu.draft.decisions",
		metric.WithUnit("{decision}"),
		metric.WithDescription("what a person did with a draft, by outcome"))
	if err != nil {
		return nil, fmt.Errorf("metrics: draft decisions: %w", err)
	}

	taskFinished, err := meter.Int64Counter("bolu.task.finished",
		metric.WithUnit("{task}"),
		metric.WithDescription("tasks that finished, by trigger and outcome"))
	if err != nil {
		return nil, fmt.Errorf("metrics: task finished: %w", err)
	}

	taskTokens, err := meter.Int64Counter("bolu.task.tokens",
		metric.WithUnit("{token}"),
		metric.WithDescription("tokens a task spent, by direction"))
	if err != nil {
		return nil, fmt.Errorf("metrics: task tokens: %w", err)
	}

	// No unit is declared, and the description carries it instead: the Prometheus exporter appends
	// a declared unit to the metric name, so a unit of "uRp" would serve the series as
	// `bolu_task_cost_uRp_total` — a name no dashboard author would guess. The rate is still
	// documented where it is read.
	taskCost, err := meter.Int64Counter("bolu.task.cost",
		metric.WithDescription("what a task cost, in micro-rupiah"))
	if err != nil {
		return nil, fmt.Errorf("metrics: task cost: %w", err)
	}

	taskDuration, err := meter.Float64Histogram("bolu.task.duration",
		metric.WithUnit("s"),
		metric.WithDescription("how long a task ran"))
	if err != nil {
		return nil, fmt.Errorf("metrics: task duration: %w", err)
	}

	toolCalls, err := meter.Int64Counter("bolu.tool.calls",
		metric.WithUnit("{call}"),
		metric.WithDescription("tool calls, by tool and outcome"))
	if err != nil {
		return nil, fmt.Errorf("metrics: tool calls: %w", err)
	}

	toolDuration, err := meter.Float64Histogram("bolu.tool.duration",
		metric.WithUnit("s"),
		metric.WithDescription("how long a tool call took, by integration"))
	if err != nil {
		return nil, fmt.Errorf("metrics: tool duration: %w", err)
	}

	toolFailures, err := meter.Int64Counter("bolu.tool.failures",
		metric.WithUnit("{failure}"),
		metric.WithDescription("tool calls that failed, by tool"))
	if err != nil {
		return nil, fmt.Errorf("metrics: tool failures: %w", err)
	}

	modelCalls, err := meter.Int64Counter("bolu.model.calls",
		metric.WithUnit("{call}"),
		metric.WithDescription("model calls, by provider and purpose"))
	if err != nil {
		return nil, fmt.Errorf("metrics: model calls: %w", err)
	}

	modelFallbacks, err := meter.Int64Counter("bolu.model.fallbacks",
		metric.WithUnit("{call}"),
		metric.WithDescription("model calls that had to try another provider"))
	if err != nil {
		return nil, fmt.Errorf("metrics: model fallbacks: %w", err)
	}

	modelLatency, err := meter.Float64Histogram("bolu.model.duration",
		metric.WithUnit("s"),
		metric.WithDescription("how long a provider took, by provider"))
	if err != nil {
		return nil, fmt.Errorf("metrics: model duration: %w", err)
	}

	rateLimited, err := meter.Int64Counter("bolu.ratelimit.refused",
		metric.WithUnit("{request}"),
		metric.WithDescription("requests refused by a rate limit, by surface"))
	if err != nil {
		return nil, fmt.Errorf("metrics: rate limited: %w", err)
	}

	queueDepth, err := meter.Int64UpDownCounter("bolu.queue.depth",
		metric.WithUnit("{item}"),
		metric.WithDescription("work waiting on a queue"))
	if err != nil {
		return nil, fmt.Errorf("metrics: queue depth: %w", err)
	}

	activityRetries, err := meter.Int64Counter("bolu.activity.retries",
		metric.WithUnit("{attempt}"),
		metric.WithDescription("activity attempts past the first, by activity"))
	if err != nil {
		return nil, fmt.Errorf("metrics: activity retries: %w", err)
	}

	taskDispatched, err := meter.Int64Counter("bolu.task.dispatched",
		metric.WithUnit("{task}"),
		metric.WithDescription("tasks this deployment accepted; the depth is dispatched minus settled"))
	if err != nil {
		return nil, fmt.Errorf("metrics: task dispatched: %w", err)
	}

	taskSettled, err := meter.Int64Counter("bolu.task.settled",
		metric.WithUnit("{task}"),
		metric.WithDescription("tasks that reached a terminal state; the depth is dispatched minus settled"))
	if err != nil {
		return nil, fmt.Errorf("metrics: task settled: %w", err)
	}

	retentionPruned, err := meter.Int64Counter("bolu.retention.pruned",
		metric.WithUnit("{item}"),
		metric.WithDescription("what the retention pass removed, by kind"))
	if err != nil {
		return nil, fmt.Errorf("metrics: retention pruned: %w", err)
	}

	return &Instruments{
		meter:           meter,
		draftReady:      draftReady,
		draftDecisions:  draftDecisions,
		taskFinished:    taskFinished,
		taskTokens:      taskTokens,
		taskCost:        taskCost,
		taskDuration:    taskDuration,
		toolCalls:       toolCalls,
		toolFailures:    toolFailures,
		toolDuration:    toolDuration,
		modelCalls:      modelCalls,
		modelFallbacks:  modelFallbacks,
		modelLatency:    modelLatency,
		rateLimited:     rateLimited,
		queueDepth:      queueDepth,
		activityRetries: activityRetries,
		taskDispatched:  taskDispatched,
		taskSettled:     taskSettled,
		retentionPruned: retentionPruned,
	}, nil
}

// buildReader picks the reader and, for Prometheus, the handler a scraper reaches.
func buildReader(ctx context.Context, config Config) (sdkmetric.Reader, http.Handler, bool, error) {
	switch strings.ToLower(strings.TrimSpace(config.Exporter)) {
	case "", ExporterNone:
		return nil, nil, false, nil

	case ExporterConsole:
		exporter, err := stdoutmetric.New()
		if err != nil {
			return nil, nil, false, fmt.Errorf("metrics: console exporter: %w", err)
		}

		return periodicReader(exporter, config), nil, true, nil

	case ExporterPrometheus:
		// The registry is this deployment's own rather than the process-wide default: a
		// container built in a test would otherwise add its instruments to the same registry as
		// the real one, and the endpoint would report measurements a scraper has no business
		// seeing.
		// A manual reader is what a scrape reads, and the handler is this package's own renderer:
		// the alternative — the OpenTelemetry Prometheus exporter — pulls in a dependency that
		// needs a C toolchain on some platforms, which would make a deployment that builds
		// `CGO_ENABLED=0` everywhere else need one for its metrics endpoint.
		reader := sdkmetric.NewManualReader()
		scraper := NewScraper(reader)

		return reader, scraper, true, nil

	case ExporterOTLP:
		if strings.TrimSpace(config.Endpoint) == "" {
			return nil, nil, false, errors.New("metrics: the otlp exporter needs an endpoint")
		}

		options := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(config.Endpoint)}
		if config.Insecure {
			options = append(options, otlpmetricgrpc.WithInsecure())
		}

		exporter, err := otlpmetricgrpc.New(ctx, options...)
		if err != nil {
			return nil, nil, false, fmt.Errorf("metrics: otlp exporter: %w", err)
		}

		return periodicReader(exporter, config), nil, true, nil

	default:
		return nil, nil, false, fmt.Errorf("metrics: unknown exporter %q", config.Exporter)
	}
}

// periodicReader wraps a pushing exporter, honouring the configured interval.
func periodicReader(exporter sdkmetric.Exporter, config Config) sdkmetric.Reader {
	if config.Interval > 0 {
		return sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(config.Interval))
	}

	return sdkmetric.NewPeriodicReader(exporter)
}

// buildResource describes the process the measurements came from.
func buildResource(ctx context.Context, config Config) *resource.Resource {
	attributes := []attribute.KeyValue{semconv.ServiceName(config.ServiceName)}
	if config.Environment != "" {
		attributes = append(attributes, semconv.DeploymentEnvironmentName(config.Environment))
	}

	built, err := resource.New(ctx,
		resource.WithAttributes(attributes...),
		resource.WithProcessPID(),
		resource.WithHost(),
	)
	if err != nil {
		return resource.NewSchemaless(attributes...)
	}

	return built
}
