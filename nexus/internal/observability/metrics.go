package observability

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// MetricType identifies the kind of metric.
type MetricType string

const (
	MetricCounter   MetricType = "counter"
	MetricGauge     MetricType = "gauge"
	MetricHistogram MetricType = "histogram"
)

// MetricEntry represents a single metric value.
type MetricEntry struct {
	Name      string            `json:"name"`
	Type      MetricType        `json:"type"`
	Help      string            `json:"help"`
	Value     float64           `json:"value"`
	Labels    map[string]string `json:"labels,omitempty"`
	Timestamp time.Time         `json:"ts"`
}

// -----------------------------------------------------------------------
// Counter
// -----------------------------------------------------------------------

// Counter is a monotonically increasing counter.
// It stores the value as int64 * 1000 to provide 3 decimal places of precision
// while remaining lock-free via atomic operations.
type Counter struct {
	name   string
	help   string
	labels map[string]string
	value  atomic.Int64 // stored as int64 * 1000 for 3-decimal precision
}

// Inc increments the counter by 1.
func (c *Counter) Inc() {
	c.value.Add(1000)
}

// Add increments the counter by delta. Delta must be >= 0.
func (c *Counter) Add(delta float64) {
	if delta < 0 {
		return
	}
	c.value.Add(int64(math.Round(delta * 1000)))
}

// Value returns the current counter value.
func (c *Counter) Value() float64 {
	return float64(c.value.Load()) / 1000.0
}

// Entry returns a MetricEntry snapshot.
func (c *Counter) Entry() MetricEntry {
	return MetricEntry{
		Name:      c.name,
		Type:      MetricCounter,
		Help:      c.help,
		Value:     c.Value(),
		Labels:    copyLabels(c.labels),
		Timestamp: time.Now(),
	}
}

// -----------------------------------------------------------------------
// Gauge
// -----------------------------------------------------------------------

// Gauge can go up and down.
type Gauge struct {
	name   string
	help   string
	labels map[string]string
	mu     sync.Mutex
	value  float64
}

// Set sets the gauge to the given value.
func (g *Gauge) Set(v float64) {
	g.mu.Lock()
	g.value = v
	g.mu.Unlock()
}

// Inc increments the gauge by 1.
func (g *Gauge) Inc() {
	g.mu.Lock()
	g.value++
	g.mu.Unlock()
}

// Dec decrements the gauge by 1.
func (g *Gauge) Dec() {
	g.mu.Lock()
	g.value--
	g.mu.Unlock()
}

// Add adds delta to the gauge (may be negative).
func (g *Gauge) Add(delta float64) {
	g.mu.Lock()
	g.value += delta
	g.mu.Unlock()
}

// Value returns the current gauge value.
func (g *Gauge) Value() float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.value
}

// Entry returns a MetricEntry snapshot.
func (g *Gauge) Entry() MetricEntry {
	return MetricEntry{
		Name:      g.name,
		Type:      MetricGauge,
		Help:      g.help,
		Value:     g.Value(),
		Labels:    copyLabels(g.labels),
		Timestamp: time.Now(),
	}
}

// -----------------------------------------------------------------------
// Histogram
// -----------------------------------------------------------------------

// Histogram tracks value distributions in buckets.
// Buckets are upper-bound inclusive: a value <= bucket[i] increments counts[i].
type Histogram struct {
	name    string
	help    string
	labels  map[string]string
	mu      sync.Mutex
	buckets []float64 // sorted upper bounds
	counts  []int64   // count per bucket (cumulative style stored per-bucket)
	sum     float64
	count   int64
}

// Observe records a value into the histogram.
func (h *Histogram) Observe(v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sum += v
	h.count++
	for i, b := range h.buckets {
		if v <= b {
			h.counts[i]++
		}
	}
}

// Count returns the total number of observations.
func (h *Histogram) Count() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count
}

// Sum returns the sum of all observed values.
func (h *Histogram) Sum() float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sum
}

// Quantile returns an approximate percentile (0..1) from the histogram buckets.
// It uses linear interpolation between bucket boundaries.
func (h *Histogram) Quantile(q float64) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.count == 0 || q < 0 || q > 1 {
		return 0
	}

	// Target rank in the total count.
	target := q * float64(h.count)

	// h.counts[i] represents cumulative count of values <= buckets[i].
	// Walk the buckets to find where the target rank falls.
	for i, b := range h.buckets {
		cumCount := float64(h.counts[i])
		if cumCount >= target {
			// The target falls within this bucket.
			// Lower bound is either 0 or the previous bucket boundary.
			var lower float64
			var prevCount float64
			if i > 0 {
				lower = h.buckets[i-1]
				prevCount = float64(h.counts[i-1])
			}
			// Linear interpolation within the bucket.
			bucketCount := cumCount - prevCount
			if bucketCount == 0 {
				return b
			}
			fraction := (target - prevCount) / bucketCount
			return lower + fraction*(b-lower)
		}
	}

	// Target is beyond the last bucket, return the last bucket boundary.
	if len(h.buckets) > 0 {
		return h.buckets[len(h.buckets)-1]
	}
	return 0
}

// Entry returns a MetricEntry snapshot (value = count).
func (h *Histogram) Entry() MetricEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return MetricEntry{
		Name:      h.name,
		Type:      MetricHistogram,
		Help:      h.help,
		Value:     float64(h.count),
		Labels:    copyLabels(h.labels),
		Timestamp: time.Now(),
	}
}

// BucketCounts returns a snapshot of (upper-bound, cumulative-count) pairs.
// This is used by the Prometheus exporter.
func (h *Histogram) BucketCounts() (buckets []float64, counts []int64, sum float64, count int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := make([]float64, len(h.buckets))
	c := make([]int64, len(h.counts))
	copy(b, h.buckets)
	copy(c, h.counts)
	return b, c, h.sum, h.count
}

// -----------------------------------------------------------------------
// Registry
// -----------------------------------------------------------------------

// Registry manages all metrics. It is safe for concurrent use.
type Registry struct {
	mu         sync.RWMutex
	counters   map[string]*Counter
	gauges     map[string]*Gauge
	histograms map[string]*Histogram
}

// NewRegistry creates an empty metric registry.
func NewRegistry() *Registry {
	return &Registry{
		counters:   make(map[string]*Counter),
		gauges:     make(map[string]*Gauge),
		histograms: make(map[string]*Histogram),
	}
}

// NewCounter registers and returns a new counter metric.
// If a counter with the same name already exists, the existing one is returned.
func (r *Registry) NewCounter(name, help string, labels map[string]string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.counters[name]; ok {
		return existing
	}
	c := &Counter{
		name:   name,
		help:   help,
		labels: copyLabels(labels),
	}
	r.counters[name] = c
	return c
}

// NewGauge registers and returns a new gauge metric.
// If a gauge with the same name already exists, the existing one is returned.
func (r *Registry) NewGauge(name, help string, labels map[string]string) *Gauge {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.gauges[name]; ok {
		return existing
	}
	g := &Gauge{
		name:   name,
		help:   help,
		labels: copyLabels(labels),
	}
	r.gauges[name] = g
	return g
}

// NewHistogram registers and returns a new histogram metric.
// If a histogram with the same name already exists, the existing one is returned.
func (r *Registry) NewHistogram(name, help string, labels map[string]string, buckets []float64) *Histogram {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.histograms[name]; ok {
		return existing
	}
	// Sort and deduplicate buckets.
	sorted := make([]float64, len(buckets))
	copy(sorted, buckets)
	sort.Float64s(sorted)

	h := &Histogram{
		name:    name,
		help:    help,
		labels:  copyLabels(labels),
		buckets: sorted,
		counts:  make([]int64, len(sorted)),
	}
	r.histograms[name] = h
	return h
}

// GetCounter returns a registered counter or nil.
func (r *Registry) GetCounter(name string) *Counter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.counters[name]
}

// GetGauge returns a registered gauge or nil.
func (r *Registry) GetGauge(name string) *Gauge {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.gauges[name]
}

// GetHistogram returns a registered histogram or nil.
func (r *Registry) GetHistogram(name string) *Histogram {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.histograms[name]
}

// AllMetrics returns a snapshot of all registered metric entries.
func (r *Registry) AllMetrics() []MetricEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entries := make([]MetricEntry, 0, len(r.counters)+len(r.gauges)+len(r.histograms))

	// Collect counters in sorted order for deterministic output.
	counterNames := sortedKeys(r.counters)
	for _, name := range counterNames {
		entries = append(entries, r.counters[name].Entry())
	}

	gaugeNames := sortedKeys(r.gauges)
	for _, name := range gaugeNames {
		entries = append(entries, r.gauges[name].Entry())
	}

	histNames := sortedKeys(r.histograms)
	for _, name := range histNames {
		entries = append(entries, r.histograms[name].Entry())
	}

	return entries
}

// -----------------------------------------------------------------------
// Default Buckets
// -----------------------------------------------------------------------

// DefaultLatencyBuckets for latency histograms (in milliseconds).
var DefaultLatencyBuckets = []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

// -----------------------------------------------------------------------
// NexusMetrics creates a pre-configured registry with standard NEXUS metrics.
// -----------------------------------------------------------------------

func NexusMetrics() *Registry {
	r := NewRegistry()

	// --- Counters ---
	r.NewCounter("nexus_trades_total",
		"Total trades processed",
		map[string]string{"exchange": "", "symbol": ""})

	r.NewCounter("nexus_ticks_total",
		"Total ticks received",
		map[string]string{"exchange": "", "symbol": ""})

	r.NewCounter("nexus_orders_submitted_total",
		"Total orders submitted",
		map[string]string{"exchange": "", "strategy": ""})

	r.NewCounter("nexus_orders_filled_total",
		"Total orders filled",
		map[string]string{"exchange": "", "strategy": ""})

	r.NewCounter("nexus_orders_rejected_total",
		"Total orders rejected",
		map[string]string{"exchange": "", "strategy": ""})

	r.NewCounter("nexus_risk_checks_total",
		"Total risk checks performed",
		map[string]string{"decision": ""})

	r.NewCounter("nexus_strategy_signals_total",
		"Total strategy signals generated",
		map[string]string{"strategy": ""})

	r.NewCounter("nexus_intel_events_total",
		"Total intel events received",
		map[string]string{"topic": ""})

	// --- Gauges ---
	r.NewGauge("nexus_pnl_realized_usd",
		"Realized PnL in USD",
		map[string]string{"strategy": ""})

	r.NewGauge("nexus_exposure_total_usd",
		"Total exposure in USD",
		nil)

	r.NewGauge("nexus_feed_lag_ms",
		"Feed lag in milliseconds",
		map[string]string{"exchange": "", "symbol": ""})

	r.NewGauge("nexus_intel_cost_usd",
		"Cumulative intel API cost in USD",
		nil)

	// --- Histograms ---
	r.NewHistogram("nexus_execution_latency_ms",
		"Order execution latency in milliseconds",
		nil,
		DefaultLatencyBuckets)

	r.NewHistogram("nexus_feature_calc_latency_ms",
		"Feature calculation latency in milliseconds",
		nil,
		DefaultLatencyBuckets)

	return r
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

func copyLabels(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// sortedKeys is a generic helper that returns sorted keys for any map[string]V.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
