package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// -----------------------------------------------------------------------
// Counter Tests
// -----------------------------------------------------------------------

func TestCounter_IncAndAdd(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("test_counter", "A test counter", nil)

	assert.Equal(t, 0.0, c.Value())

	c.Inc()
	assert.Equal(t, 1.0, c.Value())

	c.Inc()
	c.Inc()
	assert.Equal(t, 3.0, c.Value())

	c.Add(2.5)
	assert.Equal(t, 5.5, c.Value())

	c.Add(0.001)
	assert.InDelta(t, 5.501, c.Value(), 0.0001)

	// Negative delta should be ignored.
	c.Add(-10)
	assert.InDelta(t, 5.501, c.Value(), 0.0001)

	// Verify Entry().
	entry := c.Entry()
	assert.Equal(t, "test_counter", entry.Name)
	assert.Equal(t, MetricCounter, entry.Type)
	assert.Equal(t, "A test counter", entry.Help)
	assert.InDelta(t, 5.501, entry.Value, 0.0001)
}

func TestCounter_ConcurrentAccess(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("concurrent_counter", "counter for concurrency test", nil)

	var wg sync.WaitGroup
	n := 1000
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			c.Inc()
		}()
	}
	wg.Wait()

	assert.Equal(t, float64(n), c.Value())
}

// -----------------------------------------------------------------------
// Gauge Tests
// -----------------------------------------------------------------------

func TestGauge_SetAndAdd(t *testing.T) {
	r := NewRegistry()
	g := r.NewGauge("test_gauge", "A test gauge", nil)

	assert.Equal(t, 0.0, g.Value())

	g.Set(42.5)
	assert.Equal(t, 42.5, g.Value())

	g.Inc()
	assert.Equal(t, 43.5, g.Value())

	g.Dec()
	assert.Equal(t, 42.5, g.Value())

	g.Add(-50)
	assert.Equal(t, -7.5, g.Value())

	g.Set(0)
	assert.Equal(t, 0.0, g.Value())

	// Verify Entry().
	entry := g.Entry()
	assert.Equal(t, "test_gauge", entry.Name)
	assert.Equal(t, MetricGauge, entry.Type)
}

func TestGauge_ConcurrentAccess(t *testing.T) {
	r := NewRegistry()
	g := r.NewGauge("concurrent_gauge", "gauge for concurrency test", nil)

	var wg sync.WaitGroup
	n := 1000
	wg.Add(n * 2)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			g.Inc()
		}()
		go func() {
			defer wg.Done()
			g.Dec()
		}()
	}
	wg.Wait()

	// After equal increments and decrements, value should be 0.
	assert.Equal(t, 0.0, g.Value())
}

// -----------------------------------------------------------------------
// Histogram Tests
// -----------------------------------------------------------------------

func TestHistogram_Observe(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogram("test_hist", "A test histogram", nil,
		[]float64{10, 25, 50, 100})

	h.Observe(5)
	h.Observe(15)
	h.Observe(30)
	h.Observe(75)
	h.Observe(200)

	assert.Equal(t, int64(5), h.Count())
	assert.InDelta(t, 325.0, h.Sum(), 0.001)

	// Check bucket distribution.
	buckets, counts, sum, count := h.BucketCounts()
	assert.Equal(t, []float64{10, 25, 50, 100}, buckets)
	// Cumulative: <=10: 1, <=25: 2, <=50: 3, <=100: 4
	assert.Equal(t, []int64{1, 2, 3, 4}, counts)
	assert.InDelta(t, 325.0, sum, 0.001)
	assert.Equal(t, int64(5), count)

	// Verify Entry().
	entry := h.Entry()
	assert.Equal(t, "test_hist", entry.Name)
	assert.Equal(t, MetricHistogram, entry.Type)
	assert.Equal(t, float64(5), entry.Value) // Entry.Value = count
}

func TestHistogram_Quantile(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogram("quantile_hist", "quantile test", nil,
		[]float64{10, 25, 50, 100, 250})

	// Add 100 values distributed across buckets:
	// 20 values in (0,10], 30 in (10,25], 20 in (25,50], 20 in (50,100], 10 in (100,250]
	for i := 0; i < 20; i++ {
		h.Observe(5) // <= 10
	}
	for i := 0; i < 30; i++ {
		h.Observe(20) // <= 25
	}
	for i := 0; i < 20; i++ {
		h.Observe(40) // <= 50
	}
	for i := 0; i < 20; i++ {
		h.Observe(75) // <= 100
	}
	for i := 0; i < 10; i++ {
		h.Observe(200) // <= 250
	}

	assert.Equal(t, int64(100), h.Count())

	// p50 should be around the 50th observation which falls in the (10,25] bucket range.
	p50 := h.Quantile(0.5)
	assert.True(t, p50 >= 10 && p50 <= 25,
		"p50 should be between 10 and 25, got %f", p50)

	// p90 should be in the (50,100] range.
	p90 := h.Quantile(0.9)
	assert.True(t, p90 >= 50 && p90 <= 100,
		"p90 should be between 50 and 100, got %f", p90)

	// p99 should be in the (100,250] range.
	p99 := h.Quantile(0.99)
	assert.True(t, p99 >= 100 && p99 <= 250,
		"p99 should be between 100 and 250, got %f", p99)

	// Edge cases.
	assert.Equal(t, 0.0, h.Quantile(-0.1))
	assert.Equal(t, 0.0, h.Quantile(1.5))

	// Empty histogram.
	empty := r.NewHistogram("empty_hist", "empty", nil, []float64{10, 50})
	assert.Equal(t, 0.0, empty.Quantile(0.5))
}

// -----------------------------------------------------------------------
// Registry Tests
// -----------------------------------------------------------------------

func TestRegistry_NewAndGet(t *testing.T) {
	r := NewRegistry()

	c := r.NewCounter("my_counter", "help", map[string]string{"env": "test"})
	assert.NotNil(t, c)
	assert.Equal(t, c, r.GetCounter("my_counter"))
	assert.Nil(t, r.GetCounter("nonexistent"))

	g := r.NewGauge("my_gauge", "help", nil)
	assert.NotNil(t, g)
	assert.Equal(t, g, r.GetGauge("my_gauge"))
	assert.Nil(t, r.GetGauge("nonexistent"))

	h := r.NewHistogram("my_hist", "help", nil, DefaultLatencyBuckets)
	assert.NotNil(t, h)
	assert.Equal(t, h, r.GetHistogram("my_hist"))
	assert.Nil(t, r.GetHistogram("nonexistent"))

	// Registering same name returns existing metric.
	c2 := r.NewCounter("my_counter", "different help", nil)
	assert.Equal(t, c, c2)

	// AllMetrics should return all registered entries.
	all := r.AllMetrics()
	assert.Len(t, all, 3)
}

func TestRegistry_AllMetrics_Order(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("z_counter", "z", nil)
	r.NewCounter("a_counter", "a", nil)
	r.NewGauge("m_gauge", "m", nil)

	all := r.AllMetrics()
	require.Len(t, all, 3)
	// Counters first (sorted), then gauges.
	assert.Equal(t, "a_counter", all[0].Name)
	assert.Equal(t, "z_counter", all[1].Name)
	assert.Equal(t, "m_gauge", all[2].Name)
}

// -----------------------------------------------------------------------
// NexusMetrics Tests
// -----------------------------------------------------------------------

func TestNexusMetrics_AllRegistered(t *testing.T) {
	r := NexusMetrics()

	expectedCounters := []string{
		"nexus_trades_total",
		"nexus_ticks_total",
		"nexus_orders_submitted_total",
		"nexus_orders_filled_total",
		"nexus_orders_rejected_total",
		"nexus_risk_checks_total",
		"nexus_strategy_signals_total",
		"nexus_intel_events_total",
	}
	for _, name := range expectedCounters {
		c := r.GetCounter(name)
		require.NotNilf(t, c, "counter %s should be registered", name)
		assert.Equal(t, 0.0, c.Value())
	}

	expectedGauges := []string{
		"nexus_pnl_realized_usd",
		"nexus_exposure_total_usd",
		"nexus_feed_lag_ms",
		"nexus_intel_cost_usd",
	}
	for _, name := range expectedGauges {
		g := r.GetGauge(name)
		require.NotNilf(t, g, "gauge %s should be registered", name)
		assert.Equal(t, 0.0, g.Value())
	}

	expectedHistograms := []string{
		"nexus_execution_latency_ms",
		"nexus_feature_calc_latency_ms",
	}
	for _, name := range expectedHistograms {
		h := r.GetHistogram(name)
		require.NotNilf(t, h, "histogram %s should be registered", name)
		assert.Equal(t, int64(0), h.Count())
	}

	// Total metric count: 8 counters + 4 gauges + 2 histograms = 14.
	all := r.AllMetrics()
	assert.Len(t, all, 14)
}

// -----------------------------------------------------------------------
// HealthMonitor Tests
// -----------------------------------------------------------------------

func TestHealthMonitor_RegisterAndCheck(t *testing.T) {
	mon := NewHealthMonitor(time.Second)

	mon.Register("database", func(ctx context.Context) ComponentHealth {
		return ComponentHealth{
			Status:  StatusHealthy,
			Message: "connected",
		}
	})

	mon.Register("redis", func(ctx context.Context) ComponentHealth {
		return ComponentHealth{
			Status:  StatusHealthy,
			Message: "ok",
		}
	})

	ctx := context.Background()
	health := mon.Check(ctx)

	assert.Equal(t, StatusHealthy, health.Status)
	assert.Len(t, health.Components, 2)

	dbHealth, ok := health.Components["database"]
	assert.True(t, ok)
	assert.Equal(t, StatusHealthy, dbHealth.Status)
	assert.Equal(t, "database", dbHealth.Name)
	assert.Equal(t, "connected", dbHealth.Message)
	assert.False(t, dbHealth.LastChecked.IsZero())
	assert.True(t, dbHealth.Latency >= 0)

	// Also test ComponentStatus retrieval.
	comp, ok := mon.ComponentStatus("database")
	assert.True(t, ok)
	assert.Equal(t, StatusHealthy, comp.Status)

	_, ok = mon.ComponentStatus("nonexistent")
	assert.False(t, ok)
}

func TestHealthMonitor_AggregateStatus(t *testing.T) {
	tests := []struct {
		name     string
		statuses []ComponentStatus
		expected ComponentStatus
	}{
		{
			name:     "all healthy",
			statuses: []ComponentStatus{StatusHealthy, StatusHealthy, StatusHealthy},
			expected: StatusHealthy,
		},
		{
			name:     "one degraded",
			statuses: []ComponentStatus{StatusHealthy, StatusDegraded, StatusHealthy},
			expected: StatusDegraded,
		},
		{
			name:     "one unhealthy",
			statuses: []ComponentStatus{StatusHealthy, StatusDegraded, StatusUnhealthy},
			expected: StatusUnhealthy,
		},
		{
			name:     "all unhealthy",
			statuses: []ComponentStatus{StatusUnhealthy, StatusUnhealthy},
			expected: StatusUnhealthy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mon := NewHealthMonitor(time.Minute)

			for i, s := range tt.statuses {
				status := s // capture
				name := string(rune('a' + i))
				mon.Register(name, func(ctx context.Context) ComponentHealth {
					return ComponentHealth{Status: status}
				})
			}

			ctx := context.Background()
			health := mon.Check(ctx)
			assert.Equal(t, tt.expected, health.Status)
			assert.True(t, health.Uptime > 0)
		})
	}
}

func TestHealthMonitor_Alerts(t *testing.T) {
	mon := NewHealthMonitor(time.Minute)

	callCount := 0
	mon.Register("flaky", func(ctx context.Context) ComponentHealth {
		callCount++
		if callCount == 1 {
			return ComponentHealth{Status: StatusHealthy, Message: "ok"}
		}
		return ComponentHealth{Status: StatusUnhealthy, Message: "connection lost"}
	})

	ctx := context.Background()

	// First check: component is new, so an alert is emitted for the initial state.
	mon.Check(ctx)
	alert := drainAlert(t, mon.Alerts())
	assert.Equal(t, "info", alert.Level)
	assert.Equal(t, "flaky", alert.Component)

	// Second check: transition healthy -> unhealthy should fire a critical alert.
	mon.Check(ctx)
	alert = drainAlert(t, mon.Alerts())
	assert.Equal(t, "critical", alert.Level)
	assert.Equal(t, "flaky", alert.Component)
	assert.Contains(t, alert.Message, "connection lost")
}

func TestHealthMonitor_StartStop(t *testing.T) {
	mon := NewHealthMonitor(50 * time.Millisecond)

	var mu sync.Mutex
	checkCount := 0
	mon.Register("ticker", func(ctx context.Context) ComponentHealth {
		mu.Lock()
		checkCount++
		mu.Unlock()
		return ComponentHealth{Status: StatusHealthy}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go mon.Start(ctx)

	// Wait for at least 3 check cycles.
	time.Sleep(200 * time.Millisecond)
	mon.Stop()

	mu.Lock()
	count := checkCount
	mu.Unlock()

	// We should have at least 2 checks (initial + at least 1 ticker).
	assert.GreaterOrEqual(t, count, 2,
		"expected at least 2 health checks, got %d", count)
}

// -----------------------------------------------------------------------
// PrometheusExporter Tests
// -----------------------------------------------------------------------

func TestPrometheusExporter_Format(t *testing.T) {
	r := NewRegistry()

	c := r.NewCounter("http_requests_total", "Total HTTP requests",
		map[string]string{"method": "GET", "status": "200"})
	c.Add(1234)

	g := r.NewGauge("temperature", "Current temperature",
		map[string]string{"location": "server_room"})
	g.Set(23.5)

	h := r.NewHistogram("request_duration_ms", "Request duration in ms",
		nil, []float64{10, 50, 100, 500})
	h.Observe(5)
	h.Observe(25)
	h.Observe(75)
	h.Observe(250)

	exp := NewPrometheusExporter(r)
	output := exp.Format()

	// Verify counter output.
	assert.Contains(t, output, "# HELP http_requests_total Total HTTP requests")
	assert.Contains(t, output, "# TYPE http_requests_total counter")
	assert.Contains(t, output, `http_requests_total{method="GET",status="200"} 1234`)

	// Verify gauge output.
	assert.Contains(t, output, "# HELP temperature Current temperature")
	assert.Contains(t, output, "# TYPE temperature gauge")
	assert.Contains(t, output, `temperature{location="server_room"} 23.5`)

	// Verify histogram output.
	assert.Contains(t, output, "# HELP request_duration_ms Request duration in ms")
	assert.Contains(t, output, "# TYPE request_duration_ms histogram")
	assert.Contains(t, output, `request_duration_ms_bucket{le="10"} 1`)
	assert.Contains(t, output, `request_duration_ms_bucket{le="50"} 2`)
	assert.Contains(t, output, `request_duration_ms_bucket{le="100"} 3`)
	assert.Contains(t, output, `request_duration_ms_bucket{le="500"} 4`)
	assert.Contains(t, output, `request_duration_ms_bucket{le="+Inf"} 4`)
	assert.Contains(t, output, "request_duration_ms_sum 355")
	assert.Contains(t, output, "request_duration_ms_count 4")
}

func TestPrometheusExporter_FormatEmpty(t *testing.T) {
	r := NewRegistry()
	exp := NewPrometheusExporter(r)
	output := exp.Format()
	assert.Equal(t, "", output)
}

func TestPrometheusExporter_ServeHTTP(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("test_metric", "A test", nil)
	c.Inc()

	exp := NewPrometheusExporter(r)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()

	exp.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/plain")
	body := rec.Body.String()
	assert.Contains(t, body, "# HELP test_metric A test")
	assert.Contains(t, body, "# TYPE test_metric counter")
	assert.Contains(t, body, "test_metric 1")
}

func TestPrometheusExporter_FormatLabels(t *testing.T) {
	// No labels.
	assert.Equal(t, "", formatLabels(nil))
	assert.Equal(t, "", formatLabels(map[string]string{}))

	// Single label.
	s := formatLabels(map[string]string{"env": "prod"})
	assert.Equal(t, `{env="prod"}`, s)

	// Multiple labels should be sorted.
	s = formatLabels(map[string]string{"z": "last", "a": "first", "m": "mid"})
	assert.Equal(t, `{a="first",m="mid",z="last"}`, s)
}

func TestPrometheusExporter_NexusMetrics(t *testing.T) {
	// Verify that the full NexusMetrics set can be exported without errors.
	r := NexusMetrics()

	// Set some values to make it interesting.
	r.GetCounter("nexus_trades_total").Add(42)
	r.GetGauge("nexus_exposure_total_usd").Set(100000.50)
	r.GetHistogram("nexus_execution_latency_ms").Observe(12.5)

	exp := NewPrometheusExporter(r)
	output := exp.Format()

	// Spot-check a few metrics in the output.
	assert.Contains(t, output, "nexus_trades_total")
	assert.Contains(t, output, "nexus_exposure_total_usd")
	assert.Contains(t, output, "nexus_execution_latency_ms")

	// Count # HELP lines - should be 14 (one per metric).
	helpCount := strings.Count(output, "# HELP ")
	assert.Equal(t, 14, helpCount, "expected 14 HELP lines for all NexusMetrics")
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

// drainAlert reads one alert with a timeout.
func drainAlert(t *testing.T, ch <-chan Alert) Alert {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for alert")
		return Alert{}
	}
}
