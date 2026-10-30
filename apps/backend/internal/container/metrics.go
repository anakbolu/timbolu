package container

import (
	"context"

	approvaldomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/approval/domain"
	llmdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
	taskdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/task/domain"
	"github.com/anakbolu/timbolu/packages/platform/metrics"
)

// The metrics adapters: three modules that report what they did, and one place that knows how a
// measurement is named.
//
// The modules own no telemetry — their domain packages declare a narrow port and the container
// implements it — which is the shape the audit trail and the retention pruners already use. The
// reason is not ceremony: it keeps every instrument name and every label vocabulary in
// `platform/metrics`, so a chart cannot be built against a name one call site spelled differently,
// and a module never grows a dependency on an SDK the rest of the deployment may not have installed.
//
// One adapter implements all three ports, because the port is the same object. A deployment with
// metrics off has a nil provider, and every method then returns without doing anything — which is
// what lets the modules report unconditionally rather than each guarding its own call site.

// fleetMetrics adapts the deployment's meter to every module that reports into it.
type fleetMetrics struct {
	provider *metrics.Provider
}

// TaskDispatched implements taskdomain.Metrics.
func (m fleetMetrics) TaskDispatched(ctx context.Context) { m.provider.TaskDispatched(ctx) }

// TaskSettled implements taskdomain.Metrics.
func (m fleetMetrics) TaskSettled(ctx context.Context) { m.provider.TaskSettled(ctx) }

// TaskFinished implements taskdomain.Metrics.
func (m fleetMetrics) TaskFinished(ctx context.Context, trigger, status string, seconds float64, inputTokens, outputTokens, costMicros int64) {
	m.provider.TaskFinished(ctx, trigger, status, seconds, inputTokens, outputTokens, costMicros)
}

// ToolCall implements taskdomain.Metrics.
func (m fleetMetrics) ToolCall(ctx context.Context, app string, failed bool, seconds float64) {
	m.provider.ToolCall(ctx, app, failed, seconds)
}

// DraftReady implements taskdomain.Metrics.
func (m fleetMetrics) DraftReady(ctx context.Context, seconds float64) {
	m.provider.DraftReady(ctx, seconds)
}

// DraftDecided implements taskdomain.Metrics and approvaldomain.Metrics: the task runtime reports
// the decision it observed and the approval gate reports the one it recorded, and they are the same
// measurement because they are the same event.
func (m fleetMetrics) DraftDecided(ctx context.Context, outcome string, revision bool) {
	m.provider.DraftDecision(ctx, outcome, revision)
}

// ActivityRetry implements taskdomain.Metrics.
func (m fleetMetrics) ActivityRetry(ctx context.Context, activity string, attempt int) {
	m.provider.ActivityRetry(ctx, activity, attempt)
}

// ModelCall implements llmdomain.Metrics.
func (m fleetMetrics) ModelCall(ctx context.Context, provider, purpose string, fellBack bool, seconds float64) {
	m.provider.ModelCall(ctx, provider, purpose, fellBack, seconds)
}

// Compile-time checks that the one adapter is every port the modules declare. A method added to one
// of them without being implemented here fails the build rather than silently reporting nothing.
var (
	_ taskdomain.Metrics     = fleetMetrics{}
	_ approvaldomain.Metrics = fleetMetrics{}
	_ llmdomain.Metrics      = fleetMetrics{}
)

// metricsFor returns the adapter set for this container.
func (c *Container) metricsFor() fleetMetrics {
	if c == nil {
		return fleetMetrics{}
	}

	return fleetMetrics{provider: c.Metrics}
}
