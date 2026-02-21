package observability

import (
	"context"
	"sync"
	"time"
)

// ComponentStatus represents the health status of a component.
type ComponentStatus string

const (
	StatusHealthy   ComponentStatus = "healthy"
	StatusDegraded  ComponentStatus = "degraded"
	StatusUnhealthy ComponentStatus = "unhealthy"
)

// HealthCheck is a function that checks component health.
type HealthCheck func(ctx context.Context) ComponentHealth

// ComponentHealth is the health report for a single component.
type ComponentHealth struct {
	Name        string          `json:"name"`
	Status      ComponentStatus `json:"status"`
	Message     string          `json:"message,omitempty"`
	LastChecked time.Time       `json:"last_checked"`
	Latency     time.Duration   `json:"latency_ms"`
	Details     map[string]any  `json:"details,omitempty"`
}

// SystemHealth is the aggregate health of the entire system.
type SystemHealth struct {
	Status     ComponentStatus            `json:"status"`
	Components map[string]ComponentHealth `json:"components"`
	Timestamp  time.Time                  `json:"ts"`
	Uptime     time.Duration              `json:"uptime"`
}

// Alert represents a health-related alert.
type Alert struct {
	Level     string    `json:"level"`     // info|warn|critical
	Component string    `json:"component"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"ts"`
}

// HealthMonitor checks all registered components periodically.
type HealthMonitor struct {
	mu        sync.RWMutex
	checks    map[string]HealthCheck
	results   map[string]ComponentHealth
	startTime time.Time
	interval  time.Duration
	alertCh   chan Alert
	stopCh    chan struct{}
	stopped   sync.Once
}

// NewHealthMonitor creates a new HealthMonitor that checks components at
// the given interval.
func NewHealthMonitor(interval time.Duration) *HealthMonitor {
	return &HealthMonitor{
		checks:    make(map[string]HealthCheck),
		results:   make(map[string]ComponentHealth),
		startTime: time.Now(),
		interval:  interval,
		alertCh:   make(chan Alert, 256),
		stopCh:    make(chan struct{}),
	}
}

// Register adds a named health check. Must be called before Start.
func (m *HealthMonitor) Register(name string, check HealthCheck) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checks[name] = check
}

// Start begins the periodic health check loop. It blocks until the context
// is cancelled or Stop is called.
func (m *HealthMonitor) Start(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	// Run an initial check immediately.
	m.runChecks(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.runChecks(ctx)
		}
	}
}

// Stop signals the monitor to cease periodic checks.
func (m *HealthMonitor) Stop() {
	m.stopped.Do(func() {
		close(m.stopCh)
	})
}

// Check runs all registered health checks synchronously and returns the
// aggregate system health. This can be called independently of the
// periodic loop (e.g. for an HTTP handler).
func (m *HealthMonitor) Check(ctx context.Context) SystemHealth {
	m.runChecks(ctx)
	return m.snapshot()
}

// Alerts returns a read-only channel for receiving health alerts.
func (m *HealthMonitor) Alerts() <-chan Alert {
	return m.alertCh
}

// ComponentStatus returns the most recent health result for a named component.
func (m *HealthMonitor) ComponentStatus(name string) (ComponentHealth, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	h, ok := m.results[name]
	return h, ok
}

// -----------------------------------------------------------------------
// Internal
// -----------------------------------------------------------------------

// runChecks executes every registered health check and stores results.
// If a component transitions to unhealthy or degraded, an alert is emitted.
func (m *HealthMonitor) runChecks(ctx context.Context) {
	m.mu.RLock()
	checks := make(map[string]HealthCheck, len(m.checks))
	for name, fn := range m.checks {
		checks[name] = fn
	}
	m.mu.RUnlock()

	newResults := make(map[string]ComponentHealth, len(checks))

	for name, fn := range checks {
		start := time.Now()
		result := fn(ctx)
		result.Name = name
		result.LastChecked = time.Now()
		result.Latency = time.Since(start)
		newResults[name] = result
	}

	m.mu.Lock()
	oldResults := m.results
	m.results = newResults
	m.mu.Unlock()

	// Emit alerts for status transitions.
	for name, cur := range newResults {
		prev, existed := oldResults[name]
		if !existed || prev.Status != cur.Status {
			m.emitAlert(name, cur)
		}
	}
}

// emitAlert sends an alert on the alert channel (non-blocking).
func (m *HealthMonitor) emitAlert(name string, h ComponentHealth) {
	var level string
	switch h.Status {
	case StatusUnhealthy:
		level = "critical"
	case StatusDegraded:
		level = "warn"
	default:
		level = "info"
	}

	msg := h.Message
	if msg == "" {
		msg = "status changed to " + string(h.Status)
	}

	alert := Alert{
		Level:     level,
		Component: name,
		Message:   msg,
		Timestamp: time.Now(),
	}

	// Non-blocking send; drop if the channel is full.
	select {
	case m.alertCh <- alert:
	default:
	}
}

// snapshot builds a SystemHealth from the current results.
func (m *HealthMonitor) snapshot() SystemHealth {
	m.mu.RLock()
	defer m.mu.RUnlock()

	components := make(map[string]ComponentHealth, len(m.results))
	worstStatus := StatusHealthy

	for name, h := range m.results {
		components[name] = h
		if statusSeverity(h.Status) > statusSeverity(worstStatus) {
			worstStatus = h.Status
		}
	}

	return SystemHealth{
		Status:     worstStatus,
		Components: components,
		Timestamp:  time.Now(),
		Uptime:     time.Since(m.startTime),
	}
}

// statusSeverity returns a numeric severity for comparison.
func statusSeverity(s ComponentStatus) int {
	switch s {
	case StatusHealthy:
		return 0
	case StatusDegraded:
		return 1
	case StatusUnhealthy:
		return 2
	default:
		return -1
	}
}
