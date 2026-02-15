package quality

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// FeedStats tracks data quality statistics for a single exchange+symbol feed.
type FeedStats struct {
	Exchange      string    `json:"exchange"`
	Symbol        string    `json:"symbol"`
	LastEventTime time.Time `json:"last_event_time"`
	EventCount    int64     `json:"event_count"`
	GapCount      int64     `json:"gap_count"`
	MaxLagMs      float64   `json:"max_lag_ms"`
	AvgLagMs      float64   `json:"avg_lag_ms"`
	LastSeqNum    int64     `json:"last_seq_num"`
	StartTime     time.Time `json:"start_time"`

	// internal: running sum for avg calculation
	totalLagMs float64
}

// Alert represents a data quality alert for a specific feed.
type Alert struct {
	Level    string    `json:"level"`    // warn|critical
	Exchange string    `json:"exchange"`
	Symbol   string    `json:"symbol"`
	Message  string    `json:"message"`
	Ts       time.Time `json:"ts"`
}

// Monitor tracks data quality across all market data feeds.
// It detects lag, stale feeds, and sequence gaps.
type Monitor struct {
	mu              sync.RWMutex
	stats           map[string]*FeedStats // key: "exchange.symbol"
	alertCh         chan Alert
	lagThresholdMs  int
	staleTimeoutSec int
}

// NewMonitor creates a new data quality monitor.
// lagThresholdMs sets the lag threshold in milliseconds that triggers a warning.
func NewMonitor(lagThresholdMs int) *Monitor {
	return &Monitor{
		stats:           make(map[string]*FeedStats),
		alertCh:         make(chan Alert, 256),
		lagThresholdMs:  lagThresholdMs,
		staleTimeoutSec: 30,
	}
}

// feedKey returns the canonical key for an exchange+symbol pair.
func feedKey(exchange, symbol string) string {
	return fmt.Sprintf("%s.%s", exchange, symbol)
}

// getOrCreate returns existing stats or initializes new ones for the feed.
// Caller must hold m.mu write lock.
func (m *Monitor) getOrCreate(exchange, symbol string) *FeedStats {
	key := feedKey(exchange, symbol)
	stats, ok := m.stats[key]
	if !ok {
		stats = &FeedStats{
			Exchange:  exchange,
			Symbol:    symbol,
			StartTime: time.Now(),
		}
		m.stats[key] = stats
	}
	return stats
}

// recordEvent is the shared logic for recording any market data event.
func (m *Monitor) recordEvent(exchange, symbol string, eventTime time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stats := m.getOrCreate(exchange, symbol)
	stats.LastEventTime = time.Now()
	stats.EventCount++

	// Calculate lag: difference between now and the exchange event timestamp.
	lagMs := float64(time.Since(eventTime).Milliseconds())
	if lagMs < 0 {
		lagMs = 0
	}

	stats.totalLagMs += lagMs
	stats.AvgLagMs = stats.totalLagMs / float64(stats.EventCount)

	if lagMs > stats.MaxLagMs {
		stats.MaxLagMs = lagMs
	}

	// Check lag threshold and emit alert.
	if m.lagThresholdMs > 0 && lagMs > float64(m.lagThresholdMs) {
		m.emitAlert(Alert{
			Level:    "warn",
			Exchange: exchange,
			Symbol:   symbol,
			Message:  fmt.Sprintf("Feed lag exceeds threshold: %.1fms > %dms", lagMs, m.lagThresholdMs),
			Ts:       time.Now(),
		})
	}
}

// RecordTrade records a trade event and tracks lag.
func (m *Monitor) RecordTrade(exchange, symbol string, eventTime time.Time) {
	m.recordEvent(exchange, symbol, eventTime)
}

// RecordTick records a tick event and tracks lag.
func (m *Monitor) RecordTick(exchange, symbol string, eventTime time.Time) {
	m.recordEvent(exchange, symbol, eventTime)
}

// RecordGap increments the gap counter for the specified feed.
func (m *Monitor) RecordGap(exchange, symbol string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stats := m.getOrCreate(exchange, symbol)
	stats.GapCount++

	m.emitAlert(Alert{
		Level:    "warn",
		Exchange: exchange,
		Symbol:   symbol,
		Message:  fmt.Sprintf("Sequence gap detected (total gaps: %d)", stats.GapCount),
		Ts:       time.Now(),
	})
}

// Alerts returns the read-only alert channel.
func (m *Monitor) Alerts() <-chan Alert {
	return m.alertCh
}

// Snapshot returns a copy of all current feed stats.
func (m *Monitor) Snapshot() map[string]FeedStats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	snap := make(map[string]FeedStats, len(m.stats))
	for k, v := range m.stats {
		snap[k] = *v
	}
	return snap
}

// Start begins the background goroutine that checks for stale feeds every 10s.
// It blocks until the context is cancelled.
func (m *Monitor) Start(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	log.Info().
		Int("lag_threshold_ms", m.lagThresholdMs).
		Int("stale_timeout_sec", m.staleTimeoutSec).
		Msg("Quality monitor started")

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("Quality monitor stopped")
			return
		case <-ticker.C:
			m.checkStaleFeeds()
		}
	}
}

// checkStaleFeeds inspects all tracked feeds and emits critical alerts
// for any feed that has not received an event for more than staleTimeoutSec.
func (m *Monitor) checkStaleFeeds() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	now := time.Now()
	for _, stats := range m.stats {
		if stats.LastEventTime.IsZero() {
			continue
		}
		staleDur := now.Sub(stats.LastEventTime)
		if staleDur > time.Duration(m.staleTimeoutSec)*time.Second {
			m.emitAlert(Alert{
				Level:    "critical",
				Exchange: stats.Exchange,
				Symbol:   stats.Symbol,
				Message:  fmt.Sprintf("Feed stale for >%ds (last event %.1fs ago)", m.staleTimeoutSec, staleDur.Seconds()),
				Ts:       now,
			})
		}
	}
}

// emitAlert sends an alert to the channel without blocking.
// If the channel is full, the alert is dropped and a warning is logged.
func (m *Monitor) emitAlert(alert Alert) {
	select {
	case m.alertCh <- alert:
	default:
		log.Warn().
			Str("exchange", alert.Exchange).
			Str("symbol", alert.Symbol).
			Str("level", alert.Level).
			Str("message", alert.Message).
			Msg("Alert channel full, dropping alert")
	}
}
