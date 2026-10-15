package container

import (
	"context"
	"strings"

	"github.com/gofiber/fiber/v2/middleware/adaptor"

	"github.com/robzlabz/employeebot/packages/platform/config"
	"github.com/robzlabz/employeebot/packages/platform/metrics"
)

// openMetrics installs the meter and the instruments the product records into.
//
// It is a separate switch from the tracer, because the two are asked for separately in practice: a
// deployment may want a collector's traces and a scraper's numbers, either one alone, or neither.
//
// A deployment that named no exporter gets a nil provider rather than a provider that records
// nothing. That is deliberate: nil is what makes the guard cheap (a nil pointer comparison rather
// than a provider that has to ask its own flag), and it makes "this deployment reports nothing" a
// fact a test can assert on the container rather than a behaviour it has to observe.
func (c *Container) openMetrics(ctx context.Context, cfg *config.Config) error {
	if metricsNone(cfg) {
		c.Logger.Info("metrics are disabled: no exporter configured")
		return nil
	}

	provider, err := metrics.Setup(ctx, metrics.Config{
		ServiceName: cfg.Telemetry.ServiceName,
		Environment: cfg.Application.Environment,
		Exporter:    cfg.Telemetry.MetricsExporter,
		Endpoint:    cfg.Telemetry.MetricsEndpoint,
		Interval:    cfg.Telemetry.MetricsInterval,
		Insecure:    cfg.Telemetry.Insecure,
	}, c.Logger)
	if err != nil {
		return err
	}

	c.Metrics = provider

	return nil
}

// metricsNone reports whether this deployment exports no measurements.
//
// An empty value means none, and so does the explicit "none": a deployment that has not said where
// its metrics should go should not be building them, and a settings file that says "none" out loud
// says the same thing.
func metricsNone(cfg *config.Config) bool {
	if cfg == nil {
		return true
	}

	exporter := strings.ToLower(strings.TrimSpace(cfg.Telemetry.MetricsExporter))

	return exporter == "" || exporter == metrics.ExporterNone
}

// registerMetricsRoutes mounts the scrape endpoint when the Prometheus exporter is configured.
//
// It is mounted outside the API prefix and before every guard, because a scraper is infrastructure
// rather than a tenant: it has no session, no workspace, and no user, and the endpoint must not be
// behind a middleware that would ask it for one. What it serves is what the deployment chose to
// record, which carries classes and counts and no tenant's rows — see `platform/metrics`.
//
// When the deployment pushes to a collector instead, no endpoint exists at all: an unpublished route
// that answered 503 would look like a broken scrape rather than a deployment that does not scrape.
func (c *Container) registerMetricsRoutes() {
	if c.Metrics == nil || c.Metrics.Handler == nil {
		return
	}

	c.app.Get("/metrics", adaptor.HTTPHandler(c.Metrics.Handler))
}
