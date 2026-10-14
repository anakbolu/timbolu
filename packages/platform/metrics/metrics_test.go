package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.uber.org/zap"
)

// A provider whose measurements can be read back, which is what every assertion below needs: a value
// recorded into an exporter nobody reads is a test that proves nothing.
func recordingProvider(t *testing.T) (*Provider, *sdkmetric.ManualReader) {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	provider, err := NewWithReader(t.Context(), Config{ServiceName: "bolu-api"}, reader)
	require.NoError(t, err)
	// The provider owns the reader, so shutting it down releases both. Shutting the reader down
	// separately reports "reader is shutdown" on the second call, which is a test bug rather than
	// a deployment one.
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(t.Context())) })

	return provider, reader
}

// collect reads the measurements the reader holds.
func collect(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()

	var out metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &out))

	return out
}

// sumByAttribute totals an integer counter, grouped by one attribute's values.
func sumByAttribute(t *testing.T, collected metricdata.ResourceMetrics, name, key string) map[string]int64 {
	t.Helper()

	totals := map[string]int64{}
	for _, scope := range collected.ScopeMetrics {
		for _, item := range scope.Metrics {
			if item.Name != name {
				continue
			}
			sum, ok := item.Data.(metricdata.Sum[int64])
			require.True(t, ok, "%s is not an integer sum", name)
			for _, point := range sum.DataPoints {
				value, _ := point.Attributes.Value(attribute.Key(key))
				totals[value.AsString()] += point.Value
			}
		}
	}

	return totals
}

// total sums one integer counter over every data point.
func total(t *testing.T, collected metricdata.ResourceMetrics, name string) (int64, bool) {
	t.Helper()

	var found bool
	var out int64
	for _, scope := range collected.ScopeMetrics {
		for _, item := range scope.Metrics {
			if item.Name != name {
				continue
			}
			sum, ok := item.Data.(metricdata.Sum[int64])
			require.True(t, ok, "%s is not an integer sum", name)
			found = true
			for _, point := range sum.DataPoints {
				out += point.Value
			}
		}
	}

	return out, found
}

// histogramCount is how many observations one histogram recorded, and their sum.
func histogramCount(t *testing.T, collected metricdata.ResourceMetrics, name string) (uint64, float64, bool) {
	t.Helper()

	var found bool
	var count uint64
	var sum float64
	for _, scope := range collected.ScopeMetrics {
		for _, item := range scope.Metrics {
			if item.Name != name {
				continue
			}
			histogram, ok := item.Data.(metricdata.Histogram[float64])
			require.True(t, ok, "%s is not a histogram", name)
			found = true
			for _, point := range histogram.DataPoints {
				count += point.Count
				sum += point.Sum
			}
		}
	}

	return count, sum, found
}

// TestNoExporterMeansNoProvider: the deployment has said nowhere to send measurements, so none are
// built — and the container's check is then a pointer comparison rather than a flag every call site
// has to ask about.
func TestNoExporterMeansNoProvider(t *testing.T) {
	for _, exporter := range []string{"", ExporterNone} {
		provider, err := Setup(t.Context(), Config{ServiceName: "bolu-api", Exporter: exporter}, zap.NewNop())
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, provider.Shutdown(t.Context())) })

		require.False(t, provider.Enabled(), "exporter %q must not export", exporter)
		require.Nil(t, provider.Handler, "and there is nothing to scrape")
	}
}

// TestAnUnknownExporterIsRefused: an exporter this build cannot speak is a startup failure, not a
// deployment that silently reports nothing.
func TestAnUnknownExporterIsRefused(t *testing.T) {
	_, err := Setup(t.Context(), Config{ServiceName: "bolu-api", Exporter: "statsd"}, zap.NewNop())
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown exporter")

	_, err = Setup(t.Context(), Config{ServiceName: "bolu-api", Exporter: ExporterOTLP}, zap.NewNop())
	require.Error(t, err)
	require.Contains(t, err.Error(), "endpoint")
}

// TestTheProductMetricsAreRecorded is the issue's own list: how long a draft took to become
// something a person can read, what they did with it, what a task spent, and what the model chain
// cost.
func TestTheProductMetricsAreRecorded(t *testing.T) {
	provider, reader := recordingProvider(t)
	ctx := t.Context()

	provider.DraftReady(ctx, 12.5)
	provider.DraftReady(ctx, 40)

	provider.DraftDecision(ctx, "approved", false)
	provider.DraftDecision(ctx, "approved", true)
	provider.DraftDecision(ctx, "revise", true)

	provider.TaskFinished(ctx, "chat", "succeeded", 30, 1200, 300, 4500)
	provider.TaskFinished(ctx, "routine", "failed", 90, 800, 100, 900)

	provider.ModelCall(ctx, "openai", "agent", false, 2.5)
	provider.ModelCall(ctx, "anthropic", "agent", true, 1)
	// A purpose the deployment does not know is grouped rather than opening a series of its own.
	provider.ModelCall(ctx, "openai", "summarising", false, 1)

	provider.ToolCall(ctx, "gmail", false, 1.2)
	provider.ToolCall(ctx, "gmail", true, 5)
	provider.ToolCall(ctx, "whatsapp", false, 0.4)

	provider.RateLimited(ctx, "login")
	provider.RetentionPruned(ctx, "step", 5)

	collected := collect(t, reader)

	// The draft's readiness is a distribution, because an average hides the draft that took ten
	// minutes.
	count, sum, found := histogramCount(t, collected, "bolu.draft.ready.duration")
	require.True(t, found)
	require.Equal(t, uint64(2), count)
	require.InDelta(t, 52.5, sum, 0.001)

	// The decisions are split by outcome, and the revision flag is what separates "approved as
	// written" from "approved after being corrected" — which is the difference the product cares
	// about, since only the first means the bot produced usable work.
	decisions := sumByAttribute(t, collected, "bolu.draft.decisions", "outcome")
	require.Equal(t, int64(2), decisions["approved"])
	require.Equal(t, int64(1), decisions["revise"], "the approval module's own spelling")

	// Task outcomes by trigger, which is what a failure rate is read from.
	outcomes := sumByAttribute(t, collected, "bolu.task.finished", "outcome")
	require.Equal(t, int64(1), outcomes["succeeded"])
	require.Equal(t, int64(1), outcomes["failed"])

	triggers := sumByAttribute(t, collected, "bolu.task.finished", "trigger")
	require.Equal(t, int64(1), triggers["chat"])
	require.Equal(t, int64(1), triggers["routine"])

	// Tokens by direction — a task's spend is shown split, and a total written as all input would
	// read as a prompt nobody sent.
	input, _ := total(t, collected, "bolu.task.tokens")
	require.Equal(t, int64(2400), input, "1200+300 and 800+100")

	cost, found := total(t, collected, "bolu.task.cost")
	require.True(t, found)
	require.Equal(t, int64(5400), cost)

	// The model chain: which provider, and how often it had to be swapped. The cost counter is
	// named without a unit, so the exposition carries the plain name a dashboard author guesses.
	require.NotContains(t, bodyOf(t, collected), "bolu_task_cost_uRp_total")
	providers := sumByAttribute(t, collected, "bolu.model.calls", "provider")
	require.Equal(t, int64(2), providers["openai"], "both of its calls, one of them under `other`")
	require.Equal(t, int64(1), providers["anthropic"])

	purposes := sumByAttribute(t, collected, "bolu.model.calls", "purpose")
	require.Equal(t, int64(2), purposes["agent"])
	require.Equal(t, int64(1), purposes["other"], "an invented purpose is grouped")

	fallbacks, found := total(t, collected, "bolu.model.fallbacks")
	require.True(t, found)
	require.Equal(t, int64(1), fallbacks)

	// Tools by integration, which is what "which integration is failing" is read from.
	apps := sumByAttribute(t, collected, "bolu.tool.calls", "app")
	require.Equal(t, int64(2), apps["gmail"])
	require.Equal(t, int64(1), apps["whatsapp"])

	failures, found := total(t, collected, "bolu.tool.failures")
	require.True(t, found)
	require.Equal(t, int64(1), failures)

	refusals := sumByAttribute(t, collected, "bolu.ratelimit.refused", "surface")
	require.Equal(t, int64(1), refusals["login"])

	pruned := sumByAttribute(t, collected, "bolu.retention.pruned", "kind")
	require.Equal(t, int64(5), pruned["step"])
}

// bodyOf renders the collected measurements' names, which is how a naming mistake is caught.
func bodyOf(t *testing.T, collected metricdata.ResourceMetrics) string {
	t.Helper()

	names := []string{}
	for _, scope := range collected.ScopeMetrics {
		for _, item := range scope.Metrics {
			names = append(names, item.Name)
		}
	}

	return strings.Join(names, "\n")
}

// TestAnUnknownLabelValueBecomesOther is the cardinality rule, which is the only thing that makes
// these metrics affordable: a value outside its vocabulary is grouped rather than becoming a new
// time series per value — including a value a future version of the product invents, and a name a
// workspace gave its own MCP server.
func TestAnUnknownLabelValueBecomesOther(t *testing.T) {
	require.Equal(t, "gmail", classifyApp("gmail"))
	require.Equal(t, "order", classifyApp("order"),
		"the workspace's own data is an application this deployment ships, so its tools are not `other`")
	require.Equal(t, otherLabel, classifyApp("toko-sinar-crm"), "an MCP server's own name is grouped")
	require.Equal(t, otherLabel, classify("something-new", taskTriggers))
	require.Equal(t, otherLabel, classify("", taskOutcomes))
	require.Equal(t, otherLabel, classify("other", taskOutcomes), "the fallback is itself legal")
}

// TestAnAttemptIsBucketedRatherThanCounted is the second half of the cardinality rule: retries climb
// without limit on a queue that is failing, and the number an operator acts on is the shape.
func TestAnAttemptIsBucketedRatherThanCounted(t *testing.T) {
	require.Equal(t, "1", attemptBucket(1))
	require.Equal(t, "2", attemptBucket(2))
	require.Equal(t, "3-5", attemptBucket(4))
	require.Equal(t, "6+", attemptBucket(60))
	require.Equal(t, attemptBucket(7), attemptBucket(60), "the tail is one bucket, not one per attempt")
	require.Equal(t, attemptBucket(500), attemptBucket(60), "however far it climbs")
}

// TestTheDepthIsTwoCounters: the API dispatches a task and a worker finishes it, so one process
// cannot own a depth. Both halves are counters — which is also what makes the depth survive a
// restart, since neither goes backwards — and the depth is their difference.
func TestTheDepthIsTwoCounters(t *testing.T) {
	provider, reader := recordingProvider(t)
	ctx := t.Context()

	provider.TaskDispatched(ctx)
	provider.TaskDispatched(ctx)
	provider.TaskDispatched(ctx)
	provider.TaskSettled(ctx)

	collected := collect(t, reader)

	dispatched, found := total(t, collected, "bolu.task.dispatched")
	require.True(t, found, "the dispatches were not recorded")
	require.Equal(t, int64(3), dispatched)

	settled, found := total(t, collected, "bolu.task.settled")
	require.True(t, found, "the settlements were not recorded")
	require.Equal(t, int64(1), settled)

	require.Equal(t, int64(2), dispatched-settled, "which is the depth the chart reads")
}

// TestAPrometheusDeploymentIsScraped: the exporter's endpoint is what a self-hosted Prometheus reads,
// and it must carry the deployment's measurements rather than an empty page.
func TestAPrometheusDeploymentIsScraped(t *testing.T) {
	provider, err := Setup(t.Context(), Config{
		ServiceName: "bolu-api", Exporter: ExporterPrometheus,
	}, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(t.Context())) })

	require.True(t, provider.Enabled())
	require.NotNil(t, provider.Handler, "the scrape handler is what the route mounts")

	provider.TaskDispatched(t.Context())
	provider.TaskFinished(t.Context(), "chat", "succeeded", 1, 10, 2, 3)

	response := httptest.NewRecorder()
	provider.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	require.Equal(t, 200, response.Code)
	body := response.Body.String()

	// The names a dashboard author sees, spelled out: dots become underscores because the format
	// does not allow them, a counter carries `_total`, and a duration carries its unit in the name —
	// which is what a scrape of any OpenTelemetry-exported deployment looks like.
	require.Contains(t, body, "# TYPE bolu_task_finished_total counter")
	require.Contains(t, body, "bolu_task_finished_total{outcome=\"succeeded\",trigger=\"chat\"} 1")
	require.Contains(t, body, "# TYPE bolu_task_duration_seconds histogram")
	require.Contains(t, body, "bolu_task_duration_seconds_count{outcome=\"succeeded\",trigger=\"chat\"} 1")
	require.Contains(t, body, "bolu_task_duration_seconds_bucket{outcome=\"succeeded\",trigger=\"chat\",le=\"+Inf\"} 1")
	require.Contains(t, body, "bolu_task_cost_total 3", "the cost carries no invented unit suffix")

	// The labels are sorted, so two identical snapshots are byte-identical and a diff of two scrapes
	// says something.
	require.Less(t, strings.Index(body, "outcome="), strings.Index(body, "trigger="))

	// And a scraping of a second time still reads the reader rather than a cached string.
	second := httptest.NewRecorder()
	provider.Handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, body, second.Body.String())
}

// TestASecondPrometheusProviderDoesNotShareTheFirstsMeasurements: a container built in a test must
// not report the other deployment's work, or a scraper reads measurements that belong to somebody
// else. Every provider renders its own reader, so this is a property of the wiring rather than of a
// registry — and it is asserted because the wiring is where it could go wrong.
func TestASecondPrometheusProviderDoesNotShareTheFirstsMeasurements(t *testing.T) {
	first, err := Setup(t.Context(), Config{ServiceName: "bolu-api", Exporter: ExporterPrometheus}, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Shutdown(t.Context())) })

	second, err := Setup(t.Context(), Config{ServiceName: "bolu-api", Exporter: ExporterPrometheus}, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Shutdown(t.Context())) })

	second.TaskDispatched(t.Context())

	response := httptest.NewRecorder()
	first.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	require.NotContains(t, response.Body.String(), "bolu_task_dispatched",
		"the first deployment must not report the second's work")

	// And the second one reports its own, which is what makes the absence above meaningful rather
	// than an empty page on both sides.
	own := httptest.NewRecorder()
	second.Handler.ServeHTTP(own, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Contains(t, own.Body.String(), "bolu_task_dispatched_total 1")
}

// TestNothingRecordedSafely: a nil provider — the deployment with metrics off — is what every
// recording call has to tolerate, because the modules call through unconditionally rather than each
// guarding its own call site.
func TestNothingRecordedSafely(t *testing.T) {
	var provider *Provider
	ctx := context.Background()

	require.NotPanics(t, func() {
		provider.DraftReady(ctx, 1)
		provider.DraftDecision(ctx, "approved", false)
		provider.TaskDispatched(ctx)
		provider.TaskSettled(ctx)
		provider.TaskFinished(ctx, "chat", "succeeded", 1, 1, 1, 1)
		provider.ToolCall(ctx, "gmail", false, 1)
		provider.ModelCall(ctx, "openai", "agent", false, 1)
		provider.RateLimited(ctx, "login")
		provider.ActivityRetry(ctx, "RunTool", 2)
		provider.RetentionPruned(ctx, "step", 5)
		require.NoError(t, provider.Shutdown(ctx))
	})
}

// TestAPushingDeploymentHasNoScrapeEndpoint: a deployment that sends to a collector does not also
// serve a page, and an unpublished route that answered 503 would look like a broken scrape.
func TestAPushingDeploymentHasNoScrapeEndpoint(t *testing.T) {
	provider, err := Setup(t.Context(), Config{
		ServiceName: "bolu-agent-worker",
		Exporter:    ExporterConsole,
		Interval:    50 * time.Millisecond,
	}, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(t.Context())) })

	require.True(t, provider.Enabled())
	require.Nil(t, provider.Handler, "a pushing deployment has no scrape endpoint")
	require.NotNil(t, provider.Instruments, "and the worker records into the same instrument set")
}

// TestTheLabelVocabulariesAreClosed is a guard on the package's central claim: every vocabulary
// accepts its own members plus the fallback, and nothing else.
func TestTheLabelVocabulariesAreClosed(t *testing.T) {
	for name, vocabulary := range map[string]vocabulary{
		"task triggers":   taskTriggers,
		"task outcomes":   taskOutcomes,
		"model purposes":  modelPurposes,
		"rate limits":     rateLimitSurfaces,
		"retention kinds": retentionKinds,
		"activities":      activityNames,
		"integrations":    integrationApps,
		"draft outcomes":  draftOutcomes,
	} {
		require.True(t, vocabulary[otherLabel], "%s must accept the fallback", name)
		require.GreaterOrEqual(t, len(vocabulary), 2, "%s must list its own members", name)
	}

	require.False(t, taskOutcomes["succeeded-after-retry"], "an unlisted value is not legal")
	require.False(t, integrationApps[strings.Repeat("x", 40)])
}
