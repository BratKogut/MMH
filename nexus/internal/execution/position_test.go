package execution

import (
	"testing"
	"time"

	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helper to build a Fill with minimal boilerplate.
func makeFill(strategyID, exchange, symbol, side string, qty, price, fee float64) bus.Fill {
	return bus.Fill{
		BaseEvent:  bus.NewBaseEvent("test", "1.0"),
		Exchange:   exchange,
		Symbol:     symbol,
		OrderID:    "ord-1",
		IntentID:   "intent-1",
		StrategyID: strategyID,
		Side:       side,
		Qty:        decimal.NewFromFloat(qty),
		Price:      decimal.NewFromFloat(price),
		Fee:        decimal.NewFromFloat(fee),
	}
}

// ---------------------------------------------------------------------------
// TestPosition_BuyAndSell
// Buy 1 BTC @ 50000, sell 1 BTC @ 55000 → realized PnL = 5000
// ---------------------------------------------------------------------------
func TestPosition_BuyAndSell(t *testing.T) {
	pm := NewPositionManager()

	// Buy 1 BTC @ 50000
	buyFill := makeFill("strat-1", "kraken", "BTC/USD", "buy", 1.0, 50000.0, 10.0)
	require.NoError(t, pm.ApplyFill(buyFill))

	pos := pm.GetPosition("strat-1", "kraken", "BTC/USD")
	require.NotNil(t, pos)
	assert.Equal(t, "long", pos.Side)
	assert.True(t, pos.Qty.Equal(decimal.NewFromFloat(1.0)), "qty should be 1")
	assert.True(t, pos.AvgEntry.Equal(decimal.NewFromFloat(50000.0)), "avg entry should be 50000")
	assert.True(t, pos.RealizedPnL.IsZero(), "no realized PnL yet")

	// Sell 1 BTC @ 55000 (close position)
	sellFill := makeFill("strat-1", "kraken", "BTC/USD", "sell", 1.0, 55000.0, 10.0)
	require.NoError(t, pm.ApplyFill(sellFill))

	pos = pm.GetPosition("strat-1", "kraken", "BTC/USD")
	require.NotNil(t, pos)
	assert.Equal(t, "flat", pos.Side)
	assert.True(t, pos.Qty.IsZero(), "qty should be 0")
	assert.True(t, pos.RealizedPnL.Equal(decimal.NewFromFloat(5000.0)),
		"realized PnL should be (55000-50000)*1 = 5000, got %s", pos.RealizedPnL)
}

// ---------------------------------------------------------------------------
// TestPosition_PartialClose
// Buy 2 BTC @ 100, sell 1 BTC @ 150 → position 1 BTC, realized 50
// ---------------------------------------------------------------------------
func TestPosition_PartialClose(t *testing.T) {
	pm := NewPositionManager()

	buyFill := makeFill("strat-1", "kraken", "BTC/USD", "buy", 2.0, 100.0, 1.0)
	require.NoError(t, pm.ApplyFill(buyFill))

	sellFill := makeFill("strat-1", "kraken", "BTC/USD", "sell", 1.0, 150.0, 1.0)
	require.NoError(t, pm.ApplyFill(sellFill))

	pos := pm.GetPosition("strat-1", "kraken", "BTC/USD")
	require.NotNil(t, pos)
	assert.Equal(t, "long", pos.Side)
	assert.True(t, pos.Qty.Equal(decimal.NewFromFloat(1.0)),
		"remaining qty should be 1, got %s", pos.Qty)
	assert.True(t, pos.AvgEntry.Equal(decimal.NewFromFloat(100.0)),
		"avg entry should stay 100, got %s", pos.AvgEntry)
	assert.True(t, pos.RealizedPnL.Equal(decimal.NewFromFloat(50.0)),
		"realized PnL should be (150-100)*1 = 50, got %s", pos.RealizedPnL)
}

// ---------------------------------------------------------------------------
// TestPosition_AverageEntry
// Buy 1 @ 100, buy 1 @ 200 → avg = 150, qty = 2
// ---------------------------------------------------------------------------
func TestPosition_AverageEntry(t *testing.T) {
	pm := NewPositionManager()

	fill1 := makeFill("strat-1", "kraken", "ETH/USD", "buy", 1.0, 100.0, 0.5)
	require.NoError(t, pm.ApplyFill(fill1))

	fill2 := makeFill("strat-1", "kraken", "ETH/USD", "buy", 1.0, 200.0, 0.5)
	require.NoError(t, pm.ApplyFill(fill2))

	pos := pm.GetPosition("strat-1", "kraken", "ETH/USD")
	require.NotNil(t, pos)
	assert.Equal(t, "long", pos.Side)
	assert.True(t, pos.Qty.Equal(decimal.NewFromFloat(2.0)),
		"qty should be 2, got %s", pos.Qty)
	assert.True(t, pos.AvgEntry.Equal(decimal.NewFromFloat(150.0)),
		"avg entry should be (100*1 + 200*1) / 2 = 150, got %s", pos.AvgEntry)
	assert.True(t, pos.RealizedPnL.IsZero(), "no realized PnL yet")
}

// ---------------------------------------------------------------------------
// TestPosition_ShortPosition
// Sell 2 @ 1000 (open short) → negative qty, then buy 2 @ 900 to close → PnL = +200
// ---------------------------------------------------------------------------
func TestPosition_ShortPosition(t *testing.T) {
	pm := NewPositionManager()

	// Open short: sell 2 @ 1000
	sellFill := makeFill("strat-1", "kraken", "ETH/USD", "sell", 2.0, 1000.0, 5.0)
	require.NoError(t, pm.ApplyFill(sellFill))

	pos := pm.GetPosition("strat-1", "kraken", "ETH/USD")
	require.NotNil(t, pos)
	assert.Equal(t, "short", pos.Side)
	assert.True(t, pos.Qty.Equal(decimal.NewFromFloat(-2.0)),
		"qty should be -2, got %s", pos.Qty)
	assert.True(t, pos.AvgEntry.Equal(decimal.NewFromFloat(1000.0)),
		"avg entry should be 1000, got %s", pos.AvgEntry)

	// Close short: buy 2 @ 900
	buyFill := makeFill("strat-1", "kraken", "ETH/USD", "buy", 2.0, 900.0, 5.0)
	require.NoError(t, pm.ApplyFill(buyFill))

	pos = pm.GetPosition("strat-1", "kraken", "ETH/USD")
	require.NotNil(t, pos)
	assert.Equal(t, "flat", pos.Side)
	assert.True(t, pos.Qty.IsZero(), "qty should be 0")
	assert.True(t, pos.RealizedPnL.Equal(decimal.NewFromFloat(200.0)),
		"realized PnL should be (1000-900)*2 = 200, got %s", pos.RealizedPnL)
}

// ---------------------------------------------------------------------------
// TestPosition_MultiStrategy
// 2 strategies same symbol → separate positions, global netted
// ---------------------------------------------------------------------------
func TestPosition_MultiStrategy(t *testing.T) {
	pm := NewPositionManager()

	// Strategy A: buy 3 BTC @ 50000
	fillA := makeFill("strat-A", "binance", "BTC/USD", "buy", 3.0, 50000.0, 10.0)
	require.NoError(t, pm.ApplyFill(fillA))

	// Strategy B: sell 1 BTC @ 52000 (open short)
	fillB := makeFill("strat-B", "binance", "BTC/USD", "sell", 1.0, 52000.0, 10.0)
	require.NoError(t, pm.ApplyFill(fillB))

	// Check strategy-level positions are separate.
	posA := pm.GetPosition("strat-A", "binance", "BTC/USD")
	require.NotNil(t, posA)
	assert.Equal(t, "long", posA.Side)
	assert.True(t, posA.Qty.Equal(decimal.NewFromFloat(3.0)),
		"strategy A qty should be 3, got %s", posA.Qty)

	posB := pm.GetPosition("strat-B", "binance", "BTC/USD")
	require.NotNil(t, posB)
	assert.Equal(t, "short", posB.Side)
	assert.True(t, posB.Qty.Equal(decimal.NewFromFloat(-1.0)),
		"strategy B qty should be -1, got %s", posB.Qty)

	// Check global position: net = 3 + (-1) = 2 long.
	globalPos := pm.GetGlobalPosition("binance", "BTC/USD")
	require.NotNil(t, globalPos)
	assert.Equal(t, "long", globalPos.Side)
	assert.True(t, globalPos.Qty.Equal(decimal.NewFromFloat(2.0)),
		"global qty should be 2, got %s", globalPos.Qty)
	// Global avg entry: first fill opens at 50000 with qty 3, second fill (sell 1)
	// reduces by 1. After first fill: avg=50000, qty=3. After sell 1@52000:
	// realized = (52000-50000)*1 = 2000, qty=2, avg stays 50000.
	assert.True(t, globalPos.AvgEntry.Equal(decimal.NewFromFloat(50000.0)),
		"global avg entry should be 50000, got %s", globalPos.AvgEntry)
}

// ---------------------------------------------------------------------------
// TestPosition_PnLCalculation
// Known fills → exact PnL verification
// ---------------------------------------------------------------------------
func TestPosition_PnLCalculation(t *testing.T) {
	pm := NewPositionManager()

	// Buy 10 @ 100
	require.NoError(t, pm.ApplyFill(makeFill("strat-1", "kraken", "ETH/USD", "buy", 10.0, 100.0, 1.0)))
	// Sell 5 @ 120 → realized = (120-100)*5 = 100
	require.NoError(t, pm.ApplyFill(makeFill("strat-1", "kraken", "ETH/USD", "sell", 5.0, 120.0, 1.0)))
	// Sell 3 @ 90 → realized = (90-100)*3 = -30
	require.NoError(t, pm.ApplyFill(makeFill("strat-1", "kraken", "ETH/USD", "sell", 3.0, 90.0, 1.0)))

	pos := pm.GetPosition("strat-1", "kraken", "ETH/USD")
	require.NotNil(t, pos)
	assert.Equal(t, "long", pos.Side)
	assert.True(t, pos.Qty.Equal(decimal.NewFromFloat(2.0)),
		"remaining qty should be 2, got %s", pos.Qty)
	// Total realized = 100 + (-30) = 70
	assert.True(t, pos.RealizedPnL.Equal(decimal.NewFromFloat(70.0)),
		"realized PnL should be 70, got %s", pos.RealizedPnL)
	assert.True(t, pos.AvgEntry.Equal(decimal.NewFromFloat(100.0)),
		"avg entry should still be 100, got %s", pos.AvgEntry)

	// Strategy PnL aggregation.
	stratPnL := pm.StrategyPnL("strat-1")
	assert.True(t, stratPnL.Equal(decimal.NewFromFloat(70.0)),
		"strategy PnL should be 70, got %s", stratPnL)
}

// ---------------------------------------------------------------------------
// TestPosition_Fees
// Fees accumulate correctly across multiple fills.
// ---------------------------------------------------------------------------
func TestPosition_Fees(t *testing.T) {
	pm := NewPositionManager()

	require.NoError(t, pm.ApplyFill(makeFill("strat-1", "kraken", "BTC/USD", "buy", 1.0, 50000.0, 25.0)))
	require.NoError(t, pm.ApplyFill(makeFill("strat-1", "kraken", "BTC/USD", "buy", 0.5, 51000.0, 12.5)))
	require.NoError(t, pm.ApplyFill(makeFill("strat-1", "kraken", "BTC/USD", "sell", 0.5, 52000.0, 15.0)))

	pos := pm.GetPosition("strat-1", "kraken", "BTC/USD")
	require.NotNil(t, pos)
	expectedFees := decimal.NewFromFloat(25.0 + 12.5 + 15.0)
	assert.True(t, pos.TotalFees.Equal(expectedFees),
		"total fees should be %s, got %s", expectedFees, pos.TotalFees)
	assert.Equal(t, 3, pos.TradeCount)
}

// ---------------------------------------------------------------------------
// TestPosition_TotalExposure
// Multiple positions → correct total exposure in USD.
// ---------------------------------------------------------------------------
func TestPosition_TotalExposure(t *testing.T) {
	pm := NewPositionManager()

	// Position 1: buy 2 BTC @ 50000 on kraken → notional = 100,000
	require.NoError(t, pm.ApplyFill(makeFill("strat-1", "kraken", "BTC/USD", "buy", 2.0, 50000.0, 0)))
	// Position 2: sell 10 ETH @ 3000 on binance → notional = 30,000
	require.NoError(t, pm.ApplyFill(makeFill("strat-1", "binance", "ETH/USD", "sell", 10.0, 3000.0, 0)))

	exposure := pm.TotalExposureUSD()
	// Global positions: BTC on kraken (2 * 50000 = 100000), ETH on binance (10 * 3000 = 30000)
	expected := 130000.0
	assert.InDelta(t, expected, exposure, 0.01,
		"total exposure should be %f, got %f", expected, exposure)
}

// ---------------------------------------------------------------------------
// TestPosition_FlipDirection
// Long position flips to short in a single fill.
// ---------------------------------------------------------------------------
func TestPosition_FlipDirection(t *testing.T) {
	pm := NewPositionManager()

	// Open long: buy 2 @ 100
	require.NoError(t, pm.ApplyFill(makeFill("strat-1", "kraken", "ETH/USD", "buy", 2.0, 100.0, 0)))

	// Flip to short: sell 5 @ 120 (closes 2 long, opens 3 short)
	require.NoError(t, pm.ApplyFill(makeFill("strat-1", "kraken", "ETH/USD", "sell", 5.0, 120.0, 0)))

	pos := pm.GetPosition("strat-1", "kraken", "ETH/USD")
	require.NotNil(t, pos)
	assert.Equal(t, "short", pos.Side)
	assert.True(t, pos.Qty.Equal(decimal.NewFromFloat(-3.0)),
		"qty should be -3 after flip, got %s", pos.Qty)
	// Realized PnL from closing 2 long: (120-100)*2 = 40
	assert.True(t, pos.RealizedPnL.Equal(decimal.NewFromFloat(40.0)),
		"realized PnL should be 40, got %s", pos.RealizedPnL)
	// New avg entry for the short portion should be the fill price.
	assert.True(t, pos.AvgEntry.Equal(decimal.NewFromFloat(120.0)),
		"avg entry should be 120 after flip, got %s", pos.AvgEntry)
}

// ---------------------------------------------------------------------------
// TestPosition_OpenedAtUpdatesOnReopen
// Position opened, closed, and reopened → OpenedAt reflects the reopen time.
// ---------------------------------------------------------------------------
func TestPosition_OpenedAtUpdatesOnReopen(t *testing.T) {
	pm := NewPositionManager()

	// Open
	fill1 := makeFill("strat-1", "kraken", "BTC/USD", "buy", 1.0, 100.0, 0)
	fill1.Timestamp = time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	require.NoError(t, pm.ApplyFill(fill1))

	pos := pm.GetPosition("strat-1", "kraken", "BTC/USD")
	require.NotNil(t, pos)
	firstOpen := pos.OpenedAt

	// Close
	fill2 := makeFill("strat-1", "kraken", "BTC/USD", "sell", 1.0, 110.0, 0)
	fill2.Timestamp = time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC)
	require.NoError(t, pm.ApplyFill(fill2))

	// Reopen
	fill3 := makeFill("strat-1", "kraken", "BTC/USD", "buy", 2.0, 120.0, 0)
	fill3.Timestamp = time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, pm.ApplyFill(fill3))

	pos = pm.GetPosition("strat-1", "kraken", "BTC/USD")
	require.NotNil(t, pos)
	assert.True(t, pos.OpenedAt.After(firstOpen),
		"OpenedAt should be updated on reopen")
	assert.Equal(t, fill3.Timestamp, pos.OpenedAt)
}

// ---------------------------------------------------------------------------
// TestPosition_AllPositions
// AllPositions returns all strategy-level positions.
// ---------------------------------------------------------------------------
func TestPosition_AllPositions(t *testing.T) {
	pm := NewPositionManager()

	require.NoError(t, pm.ApplyFill(makeFill("s1", "kraken", "BTC/USD", "buy", 1.0, 100.0, 0)))
	require.NoError(t, pm.ApplyFill(makeFill("s2", "binance", "ETH/USD", "sell", 2.0, 200.0, 0)))
	require.NoError(t, pm.ApplyFill(makeFill("s1", "kraken", "ETH/USD", "buy", 3.0, 300.0, 0)))

	all := pm.AllPositions()
	assert.Len(t, all, 3, "should have 3 strategy-level positions")
}
