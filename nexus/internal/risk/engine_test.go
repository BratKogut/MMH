package risk

import (
	"strings"
	"testing"

	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
)

// contains checks if s contains substr.
func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

func defaultConfig() Config {
	return Config{
		MaxDailyLossUSD:      1000,
		MaxTotalExposureUSD:  10000,
		MaxPositionPerSymbol: 5000,
		MaxNotionalPerOrder:  2000,
		CooldownAfterLosses:  3,
		CooldownMinutes:      15,
	}
}

func makeIntent(id, symbol string, qty, price float64) bus.OrderIntent {
	return bus.OrderIntent{
		BaseEvent:  bus.NewBaseEvent("test", "1.0.0"),
		IntentID:   id,
		StrategyID: "test-strategy",
		Exchange:   "kraken",
		Symbol:     symbol,
		Side:       "buy",
		OrderType:  "limit",
		Qty:        decimal.NewFromFloat(qty),
		LimitPrice: decimal.NewFromFloat(price),
	}
}

func TestRiskCheck_AllowValidOrder(t *testing.T) {
	e := New(defaultConfig())
	intent := makeIntent("test-1", "BTC/USD", 0.01, 50000)

	d := e.Check(intent)
	assert.True(t, d.Allowed)
	assert.Empty(t, d.ReasonCodes)
}

func TestRiskCheck_DenyExposureExceeded(t *testing.T) {
	e := New(defaultConfig())
	e.UpdateExposure(9500) // near limit

	intent := makeIntent("test-2", "BTC/USD", 0.1, 50000) // 5000 notional
	d := e.Check(intent)

	assert.False(t, d.Allowed)
	assert.Contains(t, d.ReasonCodes[0], "EXPOSURE_EXCEEDED")
}

func TestRiskCheck_DenyDailyLoss(t *testing.T) {
	e := New(defaultConfig())
	// Set dailyPnL directly to avoid auto-freeze from UpdatePnL,
	// so we can test the daily loss check specifically.
	e.mu.Lock()
	e.dailyPnL = -1500
	e.mu.Unlock()

	intent := makeIntent("test-3", "BTC/USD", 0.001, 50000)
	d := e.Check(intent)

	assert.False(t, d.Allowed)
	assert.Contains(t, d.ReasonCodes[0], "DAILY_LOSS_EXCEEDED")
}

func TestRiskCheck_DenyOrderTooLarge(t *testing.T) {
	e := New(defaultConfig())

	intent := makeIntent("test-4", "BTC/USD", 1.0, 50000) // 50000 notional
	d := e.Check(intent)

	assert.False(t, d.Allowed)
	// Order also exceeds exposure limit, so check that ORDER_TOO_LARGE
	// appears in any of the reason codes.
	found := false
	for _, rc := range d.ReasonCodes {
		if assert.ObjectsAreEqual(true, len(rc) > 0) {
			if contains(rc, "ORDER_TOO_LARGE") {
				found = true
				break
			}
		}
	}
	assert.True(t, found, "expected ORDER_TOO_LARGE in reason codes: %v", d.ReasonCodes)
}

func TestKillSwitch_BlocksAll(t *testing.T) {
	e := New(defaultConfig())
	e.Kill()

	intent := makeIntent("test-5", "BTC/USD", 0.001, 50000)
	d := e.Check(intent)

	assert.False(t, d.Allowed)
	assert.Contains(t, d.ReasonCodes[0], "KILL_SWITCH_ACTIVE")
}

func TestFreeze_BlocksAndResumes(t *testing.T) {
	e := New(defaultConfig())
	e.Freeze("test")

	intent := makeIntent("test-6", "BTC/USD", 0.001, 50000)
	d := e.Check(intent)
	assert.False(t, d.Allowed)

	e.Resume()
	d = e.Check(intent)
	assert.True(t, d.Allowed)
}

func TestKillSwitch_CannotResume(t *testing.T) {
	e := New(defaultConfig())
	e.Kill()
	e.Resume() // should not work

	assert.False(t, e.IsActive())
}

func TestAutoFreeze_OnDailyLoss(t *testing.T) {
	e := New(defaultConfig())
	e.UpdatePnL(-1500) // triggers auto-freeze

	assert.True(t, e.frozen.Load())
}
