package observability

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
)

// PrometheusExporter serves metrics in Prometheus text exposition format.
type PrometheusExporter struct {
	registry *Registry
}

// NewPrometheusExporter creates a new exporter backed by the given registry.
func NewPrometheusExporter(registry *Registry) *PrometheusExporter {
	return &PrometheusExporter{registry: registry}
}

// ServeHTTP implements http.Handler for the /metrics endpoint.
func (e *PrometheusExporter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(e.Format()))
}

// Format returns all metrics in Prometheus text exposition format.
//
// Output follows https://prometheus.io/docs/instrumenting/exposition_formats/
//
//	# HELP <name> <help>
//	# TYPE <name> <type>
//	<name>{labels} <value>
func (e *PrometheusExporter) Format() string {
	var b strings.Builder

	e.registry.mu.RLock()
	defer e.registry.mu.RUnlock()

	// --- Counters ---
	counterNames := sortedKeys(e.registry.counters)
	for _, name := range counterNames {
		c := e.registry.counters[name]
		b.WriteString(fmt.Sprintf("# HELP %s %s\n", c.name, c.help))
		b.WriteString(fmt.Sprintf("# TYPE %s counter\n", c.name))
		b.WriteString(fmt.Sprintf("%s%s %s\n", c.name, formatLabels(c.labels), formatFloat(c.Value())))
		b.WriteByte('\n')
	}

	// --- Gauges ---
	gaugeNames := sortedKeys(e.registry.gauges)
	for _, name := range gaugeNames {
		g := e.registry.gauges[name]
		b.WriteString(fmt.Sprintf("# HELP %s %s\n", g.name, g.help))
		b.WriteString(fmt.Sprintf("# TYPE %s gauge\n", g.name))
		b.WriteString(fmt.Sprintf("%s%s %s\n", g.name, formatLabels(g.labels), formatFloat(g.Value())))
		b.WriteByte('\n')
	}

	// --- Histograms ---
	histNames := sortedKeys(e.registry.histograms)
	for _, name := range histNames {
		h := e.registry.histograms[name]
		buckets, counts, sum, count := h.BucketCounts()

		b.WriteString(fmt.Sprintf("# HELP %s %s\n", h.name, h.help))
		b.WriteString(fmt.Sprintf("# TYPE %s histogram\n", h.name))

		lblStr := formatLabels(h.labels)

		// Per-bucket lines: <name>_bucket{le="<bound>",..} <cumulative_count>
		for i, bound := range buckets {
			leLabel := addLabel(h.labels, "le", formatFloat(bound))
			b.WriteString(fmt.Sprintf("%s_bucket%s %d\n", h.name, leLabel, counts[i]))
		}
		// +Inf bucket
		infLabel := addLabel(h.labels, "le", "+Inf")
		b.WriteString(fmt.Sprintf("%s_bucket%s %d\n", h.name, infLabel, count))

		// _sum and _count
		b.WriteString(fmt.Sprintf("%s_sum%s %s\n", h.name, lblStr, formatFloat(sum)))
		b.WriteString(fmt.Sprintf("%s_count%s %d\n", h.name, lblStr, count))
		b.WriteByte('\n')
	}

	return b.String()
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

// formatLabels returns a Prometheus label string like {k1="v1",k2="v2"}.
// Returns an empty string if there are no labels.
func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}

	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// addLabel returns a label string with an extra key=value pair merged in.
func addLabel(base map[string]string, key, value string) string {
	merged := make(map[string]string, len(base)+1)
	for k, v := range base {
		merged[k] = v
	}
	merged[key] = value
	return formatLabels(merged)
}

// formatFloat formats a float64 for Prometheus output.
// Integers are printed without decimal points; others get up to 6 significant digits.
func formatFloat(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	if math.IsInf(v, -1) {
		return "-Inf"
	}
	if math.IsNaN(v) {
		return "NaN"
	}
	// If it's a whole number, render without decimals.
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return fmt.Sprintf("%g", v)
	}
	return fmt.Sprintf("%g", v)
}
