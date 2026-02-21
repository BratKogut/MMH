package features

import (
	"math"
	"testing"
	"time"

	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Helpers ---

func makeTrade(symbol string, price, qty float64, side string, ts time.Time) bus.Trade {
	return bus.Trade{
		BaseEvent: bus.BaseEvent{Timestamp: ts},
		Symbol:    symbol,
		Price:     decimal.NewFromFloat(price),
		Qty:       decimal.NewFromFloat(qty),
		Side:      side,
	}
}

func makeTick(symbol string, bid, ask float64, ts time.Time) bus.Tick {
	return bus.Tick{
		BaseEvent: bus.BaseEvent{Timestamp: ts},
		Symbol:    symbol,
		Bid:       decimal.NewFromFloat(bid),
		Ask:       decimal.NewFromFloat(ask),
		BidSize:   decimal.NewFromFloat(1),
		AskSize:   decimal.NewFromFloat(1),
	}
}

var baseTime = time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

// --- VWAP Tests ---

func TestVWAP_BasicCalculation(t *testing.T) {
	v := NewVWAP(5 * time.Minute)

	// Three trades:
	// T1: price=100, qty=10 => pq=1000
	// T2: price=102, qty=20 => pq=2040
	// T3: price=101, qty=30 => pq=3030
	// VWAP = (1000 + 2040 + 3030) / (10 + 20 + 30) = 6070 / 60 = 101.1667

	trades := []struct {
		price, qty float64
	}{
		{100, 10},
		{102, 20},
		{101, 30},
	}

	for i, tr := range trades {
		ts := baseTime.Add(time.Duration(i) * time.Second)
		v.OnTrade("BTCUSD", makeTrade("BTCUSD", tr.price, tr.qty, "buy", ts))
	}

	expected := 6070.0 / 60.0
	got := v.Value("BTCUSD")
	assert.InDelta(t, expected, got, 0.001, "VWAP should equal sum(p*q)/sum(q)")
	assert.True(t, v.Ready("BTCUSD"))
}

func TestVWAP_ColdStart(t *testing.T) {
	v := NewVWAP(5 * time.Minute)
	assert.Equal(t, 0.0, v.Value("BTCUSD"))
	assert.False(t, v.Ready("BTCUSD"))
}

func TestVWAP_RollingWindow(t *testing.T) {
	// Use a 10-second window so trades drop off quickly.
	v := NewVWAP(10 * time.Second)

	// Trade 1 at t=0: price=100, qty=10
	t1 := baseTime
	v.OnTrade("ETH", makeTrade("ETH", 100, 10, "buy", t1))

	// Trade 2 at t=5s: price=200, qty=10
	t2 := baseTime.Add(5 * time.Second)
	v.OnTrade("ETH", makeTrade("ETH", 200, 10, "buy", t2))

	// Both trades in window: VWAP = (1000+2000)/20 = 150
	assert.InDelta(t, 150.0, v.Value("ETH"), 0.001)

	// Trade 3 at t=15s: price=300, qty=10. Now trade 1 (at t=0) should be evicted.
	t3 := baseTime.Add(15 * time.Second)
	v.OnTrade("ETH", makeTrade("ETH", 300, 10, "buy", t3))

	// After eviction: trade 2 (t=5s) + trade 3 (t=15s)
	// VWAP = (2000 + 3000) / 20 = 250
	assert.InDelta(t, 250.0, v.Value("ETH"), 0.001)
}

func TestVWAP_Reset(t *testing.T) {
	v := NewVWAP(5 * time.Minute)
	v.OnTrade("BTC", makeTrade("BTC", 100, 1, "buy", baseTime))
	assert.True(t, v.Ready("BTC"))

	v.Reset("BTC")
	assert.False(t, v.Ready("BTC"))
	assert.Equal(t, 0.0, v.Value("BTC"))
}

func TestVWAP_MultipleSymbols(t *testing.T) {
	v := NewVWAP(5 * time.Minute)
	v.OnTrade("BTC", makeTrade("BTC", 50000, 1, "buy", baseTime))
	v.OnTrade("ETH", makeTrade("ETH", 3000, 2, "buy", baseTime))

	assert.InDelta(t, 50000.0, v.Value("BTC"), 0.001)
	assert.InDelta(t, 3000.0, v.Value("ETH"), 0.001)
}

// --- Spread Tests ---

func TestSpread_FromTicks(t *testing.T) {
	sp := NewSpread(1.0) // alpha=1 means no smoothing, pure current value.

	// bid=100, ask=100.50
	// mid = 100.25
	// spread = 0.50
	// spread_bps = 0.50 / 100.25 * 10000 = 49.875...
	sp.OnTick("BTC", makeTick("BTC", 100.0, 100.50, baseTime))

	expected := (0.50 / 100.25) * 10000.0
	got := sp.Value("BTC")
	assert.InDelta(t, expected, got, 0.01, "spread_bps should be (ask-bid)/mid*10000")
	assert.True(t, sp.Ready("BTC"))
}

func TestSpread_ColdStart(t *testing.T) {
	sp := NewSpread(0.1)
	assert.Equal(t, 0.0, sp.Value("BTC"))
	assert.False(t, sp.Ready("BTC"))
}

func TestSpread_EMA(t *testing.T) {
	alpha := 0.1
	sp := NewSpread(alpha)

	// Tick 1: bid=100, ask=101 => spread_bps = 1/100.5*10000 = 99.50
	tick1 := makeTick("BTC", 100.0, 101.0, baseTime)
	sp.OnTick("BTC", tick1)
	firstBps := (1.0 / 100.5) * 10000.0
	assert.InDelta(t, firstBps, sp.Value("BTC"), 0.01, "first tick seeds EMA")

	// Tick 2: bid=100, ask=100.20 => spread_bps = 0.20/100.10*10000 = 19.98
	tick2 := makeTick("BTC", 100.0, 100.20, baseTime.Add(time.Second))
	sp.OnTick("BTC", tick2)
	secondBps := (0.20 / 100.10) * 10000.0
	expectedEMA := alpha*secondBps + (1-alpha)*firstBps
	assert.InDelta(t, expectedEMA, sp.Value("BTC"), 0.01, "EMA should smooth the spread")

	// EMA should be between the two raw values (closer to first due to small alpha).
	assert.True(t, sp.Value("BTC") > secondBps, "EMA should be > smaller spread")
	assert.True(t, sp.Value("BTC") < firstBps, "EMA should be < larger spread")
}

func TestSpread_InvalidTick(t *testing.T) {
	sp := NewSpread(0.5)

	// ask < bid is invalid, should be ignored
	sp.OnTick("BTC", makeTick("BTC", 101.0, 100.0, baseTime))
	assert.False(t, sp.Ready("BTC"))

	// Zero bid is invalid
	sp.OnTick("BTC", makeTick("BTC", 0.0, 100.0, baseTime))
	assert.False(t, sp.Ready("BTC"))
}

func TestSpread_MidPrice(t *testing.T) {
	sp := NewSpread(1.0)
	sp.OnTick("BTC", makeTick("BTC", 100.0, 102.0, baseTime))
	assert.InDelta(t, 101.0, sp.MidPrice("BTC"), 0.001)
}

// --- Volatility Tests ---

func TestVolatility_ColdStart(t *testing.T) {
	vol := NewVolatility(100)
	assert.Equal(t, 0.0, vol.Value("BTC"))
	assert.False(t, vol.Ready("BTC"))

	// One trade is not enough.
	vol.OnTrade("BTC", makeTrade("BTC", 100, 1, "buy", baseTime))
	assert.Equal(t, 0.0, vol.Value("BTC"))
	assert.False(t, vol.Ready("BTC"))
}

func TestVolatility_ConstantPrice(t *testing.T) {
	vol := NewVolatility(100)

	// 50 trades all at the same price => all log returns are 0 => vol = 0.
	for i := 0; i < 50; i++ {
		ts := baseTime.Add(time.Duration(i) * time.Second)
		vol.OnTrade("BTC", makeTrade("BTC", 100.0, 1, "buy", ts))
	}

	assert.True(t, vol.Ready("BTC"))
	assert.InDelta(t, 0.0, vol.Value("BTC"), 1e-10,
		"constant price should produce zero volatility")
}

func TestVolatility_IncreasingPrices(t *testing.T) {
	vol := NewVolatility(100)

	// Linearly increasing prices: 100, 101, 102, ..., 119
	for i := 0; i < 20; i++ {
		price := 100.0 + float64(i)
		ts := baseTime.Add(time.Duration(i) * time.Second)
		vol.OnTrade("BTC", makeTrade("BTC", price, 1, "buy", ts))
	}

	v := vol.Value("BTC")
	assert.True(t, vol.Ready("BTC"))
	assert.True(t, v > 0, "increasing prices should produce positive volatility, got %f", v)
}

func TestVolatility_KnownValues(t *testing.T) {
	// Test with known prices to verify the math.
	vol := NewVolatility(10)

	prices := []float64{100, 102, 99, 103, 101}
	for i, p := range prices {
		ts := baseTime.Add(time.Duration(i) * time.Second)
		vol.OnTrade("BTC", makeTrade("BTC", p, 1, "buy", ts))
	}

	// Manual calculation of log returns:
	// r0 = ln(102/100) = 0.019803
	// r1 = ln(99/102)  = -0.029853
	// r2 = ln(103/99)  = 0.039608
	// r3 = ln(101/103) = -0.019608
	// mean = (0.019803 - 0.029853 + 0.039608 - 0.019608) / 4 = 0.002487
	// variance (Bessel) = sum((r-mean)^2) / 3
	// stddev * sqrt(365*24*60)

	r := []float64{
		math.Log(102.0 / 100.0),
		math.Log(99.0 / 102.0),
		math.Log(103.0 / 99.0),
		math.Log(101.0 / 103.0),
	}
	mean := 0.0
	for _, ri := range r {
		mean += ri
	}
	mean /= float64(len(r))

	sumSq := 0.0
	for _, ri := range r {
		d := ri - mean
		sumSq += d * d
	}
	variance := sumSq / float64(len(r)-1) // Bessel's correction
	expected := math.Sqrt(variance) * math.Sqrt(365*24*60)

	assert.InDelta(t, expected, vol.Value("BTC"), 0.001,
		"volatility should match manual calculation")
}

// --- Imbalance Tests ---

func TestImbalance_ColdStart(t *testing.T) {
	im := NewImbalance(60 * time.Second)
	assert.Equal(t, 0.0, im.Value("BTC"))
	assert.False(t, im.Ready("BTC"))
}

func TestImbalance_AllBuys(t *testing.T) {
	im := NewImbalance(60 * time.Second)

	for i := 0; i < 10; i++ {
		ts := baseTime.Add(time.Duration(i) * time.Second)
		im.OnTrade("BTC", makeTrade("BTC", 100, 1, "buy", ts))
	}

	assert.True(t, im.Ready("BTC"))
	assert.InDelta(t, 1.0, im.Value("BTC"), 1e-10,
		"all buy trades should produce imbalance = 1.0")
}

func TestImbalance_AllSells(t *testing.T) {
	im := NewImbalance(60 * time.Second)

	for i := 0; i < 10; i++ {
		ts := baseTime.Add(time.Duration(i) * time.Second)
		im.OnTrade("BTC", makeTrade("BTC", 100, 1, "sell", ts))
	}

	assert.InDelta(t, -1.0, im.Value("BTC"), 1e-10,
		"all sell trades should produce imbalance = -1.0")
}

func TestImbalance_Balanced(t *testing.T) {
	im := NewImbalance(60 * time.Second)

	// 5 buy trades and 5 sell trades with equal volume.
	for i := 0; i < 5; i++ {
		ts := baseTime.Add(time.Duration(i*2) * time.Second)
		im.OnTrade("BTC", makeTrade("BTC", 100, 2, "buy", ts))
		ts2 := baseTime.Add(time.Duration(i*2+1) * time.Second)
		im.OnTrade("BTC", makeTrade("BTC", 100, 2, "sell", ts2))
	}

	assert.InDelta(t, 0.0, im.Value("BTC"), 1e-10,
		"equal buy/sell volume should produce imbalance = 0")
}

func TestImbalance_UnknownSideIgnored(t *testing.T) {
	im := NewImbalance(60 * time.Second)

	im.OnTrade("BTC", makeTrade("BTC", 100, 5, "unknown", baseTime))
	assert.False(t, im.Ready("BTC"), "unknown side trades should not count")
	assert.Equal(t, 0.0, im.Value("BTC"))
}

func TestImbalance_RollingWindow(t *testing.T) {
	im := NewImbalance(10 * time.Second)

	// Buy at t=0
	im.OnTrade("BTC", makeTrade("BTC", 100, 10, "buy", baseTime))
	assert.InDelta(t, 1.0, im.Value("BTC"), 1e-10)

	// Sell at t=15s. The buy at t=0 is now >10s old and should be evicted.
	ts := baseTime.Add(15 * time.Second)
	im.OnTrade("BTC", makeTrade("BTC", 100, 10, "sell", ts))

	assert.InDelta(t, -1.0, im.Value("BTC"), 1e-10,
		"after eviction, only the sell should remain")
}

// --- Momentum Tests ---

func TestMomentum_ColdStart(t *testing.T) {
	m := NewMomentum(time.Second, 60)
	assert.Equal(t, 0.0, m.Value("BTC"))
	assert.False(t, m.Ready("BTC"))
}

func TestMomentum_PositiveROC(t *testing.T) {
	m := NewMomentum(time.Second, 60)

	// Price goes from 100 to 110 over 10 seconds.
	for i := 0; i <= 10; i++ {
		price := 100.0 + float64(i)
		ts := baseTime.Add(time.Duration(i) * time.Second)
		m.OnTrade("BTC", makeTrade("BTC", price, 1, "buy", ts))
	}

	// ROC = (110 - 100) / 100 = 0.10
	assert.True(t, m.Ready("BTC"))
	assert.InDelta(t, 0.10, m.Value("BTC"), 0.001,
		"10% price increase should produce ROC = 0.10")
}

func TestMomentum_NegativeROC(t *testing.T) {
	m := NewMomentum(time.Second, 60)

	// Price goes from 100 to 90 over 10 seconds.
	for i := 0; i <= 10; i++ {
		price := 100.0 - float64(i)
		ts := baseTime.Add(time.Duration(i) * time.Second)
		m.OnTrade("BTC", makeTrade("BTC", price, 1, "buy", ts))
	}

	// ROC = (90 - 100) / 100 = -0.10
	assert.InDelta(t, -0.10, m.Value("BTC"), 0.001)
}

func TestMomentum_FlatPrice(t *testing.T) {
	m := NewMomentum(time.Second, 60)

	for i := 0; i < 20; i++ {
		ts := baseTime.Add(time.Duration(i) * time.Second)
		m.OnTrade("BTC", makeTrade("BTC", 100.0, 1, "buy", ts))
	}

	assert.InDelta(t, 0.0, m.Value("BTC"), 1e-10,
		"constant price should produce zero momentum")
}

func TestMomentum_SamplingRate(t *testing.T) {
	// Sampling at 2-second intervals means trades within the same 2s bucket
	// update the same slot.
	m := NewMomentum(2*time.Second, 60)

	// Two trades in the same 2s bucket: only the latest should be kept.
	m.OnTrade("BTC", makeTrade("BTC", 100, 1, "buy", baseTime))
	m.OnTrade("BTC", makeTrade("BTC", 105, 1, "buy", baseTime.Add(500*time.Millisecond)))

	// Only one sample point, so not ready.
	// Now add a trade 3 seconds later (new bucket).
	m.OnTrade("BTC", makeTrade("BTC", 110, 1, "buy", baseTime.Add(3*time.Second)))

	// The first sample should have been updated to 105 (latest in that bucket).
	// ROC = (110 - 105) / 105 = 0.04762
	assert.True(t, m.Ready("BTC"))
	assert.InDelta(t, (110.0-105.0)/105.0, m.Value("BTC"), 0.001)
}

// --- Engine Integration Tests ---

func TestEngine_ProcessTrade(t *testing.T) {
	engine := NewEngine([]string{"BTCUSD"})

	trade := makeTrade("BTCUSD", 50000, 1.5, "buy", baseTime)
	results := engine.ProcessTrade(trade)

	require.NotEmpty(t, results, "ProcessTrade should return results for all calculators")

	// We registered 5 calculators.
	assert.Len(t, results, 5, "should have one result per calculator")

	// Verify all results have correct symbol and timestamp.
	for _, r := range results {
		assert.Equal(t, "BTCUSD", r.Symbol)
		assert.Equal(t, baseTime.UnixMilli(), r.Timestamp)
		assert.NotEmpty(t, r.Name)
	}

	// Find the VWAP result: should equal trade price (only one trade).
	var vwapResult *FeatureResult
	for i := range results {
		if results[i].Name == "vwap" {
			vwapResult = &results[i]
			break
		}
	}
	require.NotNil(t, vwapResult)
	assert.InDelta(t, 50000.0, vwapResult.Value, 0.01)
}

func TestEngine_ProcessTick(t *testing.T) {
	engine := NewEngine([]string{"BTCUSD"})

	tick := makeTick("BTCUSD", 49999.0, 50001.0, baseTime)
	results := engine.ProcessTick(tick)

	require.NotEmpty(t, results)
	assert.Len(t, results, 5)

	// Find spread result.
	var spreadResult *FeatureResult
	for i := range results {
		if results[i].Name == "spread_bps" {
			spreadResult = &results[i]
			break
		}
	}
	require.NotNil(t, spreadResult)

	// mid = 50000, spread = 2, bps = 2/50000 * 10000 = 0.4
	assert.InDelta(t, 0.4, spreadResult.Value, 0.01)
}

func TestEngine_Snapshot(t *testing.T) {
	engine := NewEngine([]string{"BTCUSD"})

	// Feed some data first.
	for i := 0; i < 5; i++ {
		ts := baseTime.Add(time.Duration(i) * time.Second)
		engine.ProcessTrade(makeTrade("BTCUSD", 50000+float64(i*100), 1, "buy", ts))
		engine.ProcessTick(makeTick("BTCUSD", 49999+float64(i*100), 50001+float64(i*100), ts))
	}

	snap := engine.Snapshot("BTCUSD")

	// Should have all 5 features.
	assert.Len(t, snap, 5)
	assert.Contains(t, snap, "vwap")
	assert.Contains(t, snap, "spread_bps")
	assert.Contains(t, snap, "volatility")
	assert.Contains(t, snap, "trade_imbalance")
	assert.Contains(t, snap, "momentum")

	// VWAP should be reasonable (around 50200 for ascending prices).
	assert.True(t, snap["vwap"] > 49000, "VWAP should be > 49000")
	assert.True(t, snap["vwap"] < 52000, "VWAP should be < 52000")

	// Imbalance should be 1.0 (all buys).
	assert.InDelta(t, 1.0, snap["trade_imbalance"], 0.001)
}

func TestEngine_SnapshotEmptySymbol(t *testing.T) {
	engine := NewEngine([]string{"BTCUSD"})
	snap := engine.Snapshot("ETHUSD") // no data for this symbol

	assert.Len(t, snap, 5, "snapshot should still have all feature keys")
	for _, v := range snap {
		assert.Equal(t, 0.0, v, "all values should be 0 for unknown symbol")
	}
}

func TestEngine_ResetSymbol(t *testing.T) {
	engine := NewEngine([]string{"BTCUSD"})

	engine.ProcessTrade(makeTrade("BTCUSD", 50000, 1, "buy", baseTime))
	snap := engine.Snapshot("BTCUSD")
	assert.True(t, snap["vwap"] > 0)

	engine.ResetSymbol("BTCUSD")
	snap = engine.Snapshot("BTCUSD")
	assert.Equal(t, 0.0, snap["vwap"])
}
