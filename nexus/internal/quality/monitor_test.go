package quality

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecordTrade_UpdatesStats(t *testing.T) {
	m := NewMonitor(5000) // 5s threshold

	now := time.Now()
	m.RecordTrade("kraken", "BTC/USD", now)
	m.RecordTrade("kraken", "BTC/USD", now)
	m.RecordTrade("kraken", "BTC/USD", now)

	snap := m.Snapshot()
	stats, ok := snap["kraken.BTC/USD"]
	require.True(t, ok, "Expected feed stats for kraken.BTC/USD")

	assert.Equal(t, "kraken", stats.Exchange)
	assert.Equal(t, "BTC/USD", stats.Symbol)
	assert.Equal(t, int64(3), stats.EventCount)
	assert.False(t, stats.LastEventTime.IsZero())
	assert.False(t, stats.StartTime.IsZero())
}

func TestRecordTick_UpdatesStats(t *testing.T) {
	m := NewMonitor(5000)

	now := time.Now()
	m.RecordTick("binance", "ETHUSDT", now)
	m.RecordTick("binance", "ETHUSDT", now)

	snap := m.Snapshot()
	stats, ok := snap["binance.ETHUSDT"]
	require.True(t, ok, "Expected feed stats for binance.ETHUSDT")

	assert.Equal(t, "binance", stats.Exchange)
	assert.Equal(t, "ETHUSDT", stats.Symbol)
	assert.Equal(t, int64(2), stats.EventCount)
}

func TestRecordTrade_DetectsLag(t *testing.T) {
	m := NewMonitor(100) // 100ms threshold

	// Simulate a trade event that happened 200ms ago to trigger lag alert.
	pastTime := time.Now().Add(-200 * time.Millisecond)
	m.RecordTrade("kraken", "BTC/USD", pastTime)

	// Should have generated a lag alert.
	select {
	case alert := <-m.Alerts():
		assert.Equal(t, "warn", alert.Level)
		assert.Equal(t, "kraken", alert.Exchange)
		assert.Equal(t, "BTC/USD", alert.Symbol)
		assert.Contains(t, alert.Message, "Feed lag exceeds threshold")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Expected a lag alert but none received")
	}

	// Verify stats recorded the lag.
	snap := m.Snapshot()
	stats := snap["kraken.BTC/USD"]
	assert.Greater(t, stats.MaxLagMs, float64(100))
	assert.Greater(t, stats.AvgLagMs, float64(100))
}

func TestRecordTrade_NoAlertUnderThreshold(t *testing.T) {
	m := NewMonitor(5000) // 5s threshold

	// Trade event is "now" so lag should be ~0ms.
	m.RecordTrade("kraken", "BTC/USD", time.Now())

	// No alert should be generated.
	select {
	case alert := <-m.Alerts():
		t.Fatalf("Did not expect an alert but got: %+v", alert)
	case <-time.After(50 * time.Millisecond):
		// Good - no alert.
	}
}

func TestRecordGap_IncrementsCounter(t *testing.T) {
	m := NewMonitor(5000)

	m.RecordTrade("kraken", "BTC/USD", time.Now())
	m.RecordGap("kraken", "BTC/USD")
	m.RecordGap("kraken", "BTC/USD")

	snap := m.Snapshot()
	stats := snap["kraken.BTC/USD"]
	assert.Equal(t, int64(2), stats.GapCount)

	// Should have generated two gap alerts.
	select {
	case alert := <-m.Alerts():
		assert.Equal(t, "warn", alert.Level)
		assert.Contains(t, alert.Message, "Sequence gap detected")
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Expected a gap alert")
	}
}

func TestStaleFeed_GeneratesAlert(t *testing.T) {
	m := NewMonitor(5000)
	// Override stale timeout to 1s for testing.
	m.staleTimeoutSec = 1

	// Record a trade so the feed has a LastEventTime.
	m.RecordTrade("kraken", "BTC/USD", time.Now())

	// Drain any lag alerts from the RecordTrade call.
	drainAlerts(m.Alerts())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the monitor in a background goroutine.
	go m.Start(ctx)

	// Wait for the feed to become stale (>1s) and the check interval to fire.
	// We need to wait for the stale timeout (1s) plus the check interval (10s).
	// Instead, manually trigger the check.
	time.Sleep(1200 * time.Millisecond)
	m.checkStaleFeeds()

	// Should have a critical stale alert.
	select {
	case alert := <-m.Alerts():
		assert.Equal(t, "critical", alert.Level)
		assert.Equal(t, "kraken", alert.Exchange)
		assert.Equal(t, "BTC/USD", alert.Symbol)
		assert.Contains(t, alert.Message, "Feed stale")
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Expected a stale feed alert but none received")
	}
}

func TestSnapshot_ReturnsAllFeeds(t *testing.T) {
	m := NewMonitor(5000)
	now := time.Now()

	m.RecordTrade("kraken", "BTC/USD", now)
	m.RecordTrade("kraken", "ETH/USD", now)
	m.RecordTrade("binance", "BTCUSDT", now)
	m.RecordTick("binance", "ETHUSDT", now)

	snap := m.Snapshot()
	assert.Len(t, snap, 4)

	_, ok1 := snap["kraken.BTC/USD"]
	_, ok2 := snap["kraken.ETH/USD"]
	_, ok3 := snap["binance.BTCUSDT"]
	_, ok4 := snap["binance.ETHUSDT"]

	assert.True(t, ok1, "Missing kraken.BTC/USD")
	assert.True(t, ok2, "Missing kraken.ETH/USD")
	assert.True(t, ok3, "Missing binance.BTCUSDT")
	assert.True(t, ok4, "Missing binance.ETHUSDT")
}

func TestSnapshot_ReturnsCopy(t *testing.T) {
	m := NewMonitor(5000)
	m.RecordTrade("kraken", "BTC/USD", time.Now())

	snap1 := m.Snapshot()
	assert.Equal(t, int64(1), snap1["kraken.BTC/USD"].EventCount)

	// Record more events.
	m.RecordTrade("kraken", "BTC/USD", time.Now())

	// snap1 should not be affected (it's a copy).
	assert.Equal(t, int64(1), snap1["kraken.BTC/USD"].EventCount)

	snap2 := m.Snapshot()
	assert.Equal(t, int64(2), snap2["kraken.BTC/USD"].EventCount)
}

func TestMultipleExchanges_IndependentStats(t *testing.T) {
	m := NewMonitor(5000)
	now := time.Now()

	m.RecordTrade("kraken", "BTC/USD", now)
	m.RecordTrade("kraken", "BTC/USD", now)
	m.RecordTrade("binance", "BTCUSDT", now)

	snap := m.Snapshot()
	assert.Equal(t, int64(2), snap["kraken.BTC/USD"].EventCount)
	assert.Equal(t, int64(1), snap["binance.BTCUSDT"].EventCount)
}

// drainAlerts drains the alert channel without blocking.
func drainAlerts(ch <-chan Alert) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
