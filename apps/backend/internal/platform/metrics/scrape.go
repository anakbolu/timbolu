package metrics

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The scrape endpoint.
//
// It is written here rather than taken from the OpenTelemetry Prometheus exporter, and the reason is
// what that exporter costs: it pulls in `prometheus/client_golang`, which needs a C toolchain to
// build on some platforms — so a deployment that builds with `CGO_ENABLED=0` everywhere else would
// suddenly need one, and a laptop without the platform's developer tools could not run the linter
// over the package that serves metrics. What the exporter does for us is turn one `metricdata`
// snapshot into text, which is the whole of what is below.
//
// The format is Prometheus's own text exposition: one `# HELP` and one `# TYPE` line per metric
// family, then a line per data point with its labels. Two conventions matter and both are followed:
// a monotonic sum is a counter and its name takes the `_total` suffix (which is what every dashboard
// written against an OpenTelemetry exporter expects), and a histogram is three families — cumulative
// buckets with an `le` label, a sum, and a count.

// Scraper renders one snapshot of a reader's measurements as the exposition format.
//
// It holds no state between scrapes: every request reads the reader, so what a scraper sees is what
// the process holds at that moment rather than what it held at the last scrape.
type Scraper struct {
	reader sdkmetric.Reader
	// rendering serialises concurrent scrapes. Two collectors reading one manual reader at once
	// would race inside the SDK, and a scrape is a few kilobytes of text — the lock costs nothing
	// and the alternative is a data race in a code path nobody would think to test.
	rendering sync.Mutex
}

// NewScraper builds the endpoint over a reader.
func NewScraper(reader sdkmetric.Reader) *Scraper {
	return &Scraper{reader: reader}
}

// ServeHTTP renders the current measurements for a scraper.
func (s *Scraper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := s.render(r.Context())
	if err != nil {
		http.Error(w, "metrics: the reader could not be collected", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(body))
}

// render collects the reader and writes every measurement out.
func (s *Scraper) render(ctx context.Context) (string, error) {
	s.rendering.Lock()
	defer s.rendering.Unlock()

	var collected metricdata.ResourceMetrics
	if err := s.reader.Collect(ctx, &collected); err != nil {
		return "", fmt.Errorf("metrics: collect: %w", err)
	}

	var out strings.Builder

	for _, scope := range collected.ScopeMetrics {
		// The scope is the instrumentation library and is not a metric label: a deployment has one
		// scope, and rendering it per series would triple the output for nothing.
		for _, item := range scope.Metrics {
			writeMetric(&out, item)
		}
	}

	return out.String(), nil
}

// writeMetric renders one metric family.
func writeMetric(out *strings.Builder, item metricdata.Metrics) {
	name := prometheusName(item.Name, item.Unit)
	description := item.Description

	switch data := item.Data.(type) {
	case metricdata.Sum[int64]:
		// A monotonic sum is a counter, and Prometheus's convention is the `_total` suffix. A
		// non-monotonic one is a gauge, which is what an up-down counter renders as.
		if data.IsMonotonic {
			name += "_total"
		}
		writeHeader(out, name, description, kindOf(data.IsMonotonic))

		for _, point := range data.DataPoints {
			writePoint(out, name, labelsOf(point.Attributes), formatInt(point.Value))
		}

	case metricdata.Sum[float64]:
		if data.IsMonotonic {
			name += "_total"
		}
		writeHeader(out, name, description, kindOf(data.IsMonotonic))

		for _, point := range data.DataPoints {
			writePoint(out, name, labelsOf(point.Attributes), formatFloat(point.Value))
		}

	case metricdata.Gauge[int64]:
		writeHeader(out, name, description, "gauge")
		for _, point := range data.DataPoints {
			writePoint(out, name, labelsOf(point.Attributes), formatInt(point.Value))
		}

	case metricdata.Gauge[float64]:
		writeHeader(out, name, description, "gauge")
		for _, point := range data.DataPoints {
			writePoint(out, name, labelsOf(point.Attributes), formatFloat(point.Value))
		}

	case metricdata.Histogram[int64]:
		writeHistogram(out, name, description, data.DataPoints, func(value int64) string {
			return formatInt(value)
		})

	case metricdata.Histogram[float64]:
		writeHistogram(out, name, description, data.DataPoints, formatFloat)

	default:
		// A summary or an exponential histogram is a shape this deployment does not record, and
		// saying nothing is better than inventing a rendering for it. The instrument set is declared
		// in one place, so an unreachable branch here is a fact rather than a risk.
	}
}

// kindOf names the metric type a sum renders as.
func kindOf(monotonic bool) string {
	if monotonic {
		return "counter"
	}

	return "gauge"
}

// histogramValue is what the two histogram shapes have in common once a value printer is applied.
func writeHistogram[T int64 | float64](
	out *strings.Builder,
	name, description string,
	points []metricdata.HistogramDataPoint[T],
	render func(T) string,
) {
	writeHeader(out, name, description, "histogram")

	for _, point := range points {
		labels := labelsOf(point.Attributes)

		// The buckets are cumulative and every one of them is rendered, so a scrape can compute a
		// rate over any of them. Bounds is already sorted by the SDK.
		for index, bound := range point.Bounds {
			// The buckets are cumulative, and the SDK's are too, so the count of each is the count
			// of everything at or below its bound — which is what the format's `le` means.
			writePoint(out, name+"_bucket",
				append(labels, label{key: "le", value: formatFloat(bound)}),
				formatUint(point.BucketCounts[index]))
		}
		writePoint(out, name+"_bucket", append(labels, label{key: "le", value: "+Inf"}),
			formatUint(point.Count))

		writePoint(out, name+"_sum", labels, render(point.Sum))
		writePoint(out, name+"_count", labels, formatUint(point.Count))
	}
}

// prometheusName turns an instrument name into a Prometheus metric name.
//
// Two rules, and both are the format's rather than a choice: a metric name may only contain letters,
// digits, underscores, and colons (so the dots this package names its instruments with become
// underscores), and the unit belongs in the name — `bolu_task_duration` in seconds is
// `bolu_task_duration_seconds`, which is what a dashboard written against any OpenTelemetry exporter
// expects to find.
func prometheusName(name, unit string) string {
	sanitized := sanitizeMetricName(name)

	if suffix := prometheusUnitSuffix(unit); suffix != "" && !strings.HasSuffix(sanitized, suffix) {
		sanitized += suffix
	}

	return sanitized
}

// prometheusUnitSuffix is the Prometheus spelling of a unit, for the units this deployment declares.
//
// A unit this map does not know leaves the name alone, which is the safe direction: an invented
// suffix is a series no dashboard matches, while a missing one is a series that is simply named what
// the instrument is called.
func prometheusUnitSuffix(unit string) string {
	switch unit {
	case "s":
		return "_seconds"
	case "By":
		return "_bytes"
	default:
		// An annotated unit (`{task}`), an empty one, and a count the format has no suffix for all
		// mean the same thing here: nothing to add.
		return ""
	}
}

// sanitizeMetricName replaces every character the format does not allow, and makes sure the first
// one is usable as well.
func sanitizeMetricName(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == ':':
			return r
		default:
			return '_'
		}
	}, name)

	if cleaned == "" {
		return "_"
	}
	if first := cleaned[0]; first >= '0' && first <= '9' {
		return "_" + cleaned
	}

	return cleaned
}

// label is one rendered label, kept as a pair so the order is explicit.
type label struct {
	key   string
	value string
}

// labelsOf renders a data point's attributes, sorted by key.
//
// The sort is not tidiness: the output is compared between scrapes by whatever reads it, and a map
// iteration order would make two identical snapshots differ.
func labelsOf(attributes attribute.Set) []label {
	out := make([]label, 0, attributes.Len())
	for _, item := range attributes.ToSlice() {
		// The key is sanitized for the same reason the metric name is: a label name has the same
		// allowed characters, and a key that reached the scraper as `net.host` would be rejected by
		// the reader rather than by us.
		out = append(out, label{key: sanitizeMetricName(string(item.Key)), value: item.Value.String()})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })

	return out
}

// writeHeader writes the two comment lines a metric family is introduced with.
func writeHeader(out *strings.Builder, name, description, kind string) {
	if description != "" {
		fmt.Fprintf(out, "# HELP %s %s\n", name, escapeHelp(description))
	}
	fmt.Fprintf(out, "# TYPE %s %s\n", name, kind)
}

// writePoint writes one series.
func writePoint(out *strings.Builder, name string, labels []label, value string) {
	if len(labels) == 0 {
		fmt.Fprintf(out, "%s %s\n", name, value)

		return
	}

	out.WriteString(name)
	out.WriteByte('{')
	for index, item := range labels {
		if index > 0 {
			out.WriteByte(',')
		}
		fmt.Fprintf(out, `%s="%s"`, item.key, escapeLabel(item.value))
	}
	fmt.Fprintf(out, "} %s\n", value)
}

// escapeLabel escapes a label value, where a backslash, a quote, and a newline are the three
// characters the format gives meaning to.
func escapeLabel(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

	return replacer.Replace(value)
}

// escapeHelp escapes a help line, where only a backslash and a newline are special.
func escapeHelp(value string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(value)
}

// formatInt renders an integer value.
func formatInt(value int64) string { return strconv.FormatInt(value, 10) }

// formatUint renders an unsigned value.
func formatUint(value uint64) string { return strconv.FormatUint(value, 10) }

// formatFloat renders a float the way the format expects, including the three values that are not
// numbers: Prometheus writes them by name rather than as a decimal.
func formatFloat(value float64) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "+Inf"
	case math.IsInf(value, -1):
		return "-Inf"
	default:
		return strconv.FormatFloat(value, 'g', -1, 64)
	}
}
