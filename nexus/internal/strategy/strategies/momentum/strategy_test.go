package momentum

import (
	"encoding/json"
	"testing"

	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/nexus-trading/nexus/internal/strategy"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func defaultConfig() strategy.StrategyConfig {
	return strategy.StrategyConfig{
		StrategyID: "test-momentum-1",
		Symbols:    []string{"BTC-USDT"},
		Exchanges:  []string{"binance"},
		Params: map[string]interface{}{
			ParamMomentumThreshold:  0.001,
			ParamMaxVolatility:      0.5,
			ParamImbalanceThreshold: 0.2,
			ParamCooldownSeconds:    60.0,
			ParamMaxHoldSeconds:     3600.0,
			ParamPositionSizeUSD:    100.0,
		},
	}
}

func featureEvent(symbol, name string, value float64, tsMs int64) strategy.MarketEvent {
	return strategy.MarketEvent{
		Type: "feature",
		Feature: &strategy.FeatureUpdate{
			Symbol: symbol,
			Name:   name,
			Value:  value,
			Ts:     tsMs,
		},
	}
}

// preloadFeatures sets features directly on the strategy's internal map
// without triggering evaluation. This avoids premature signal emission
// during test setup. Caller must hold no lock.
func preloadFeatures(s *SimpleMomentum, symbol string, feats map[string]float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.features[symbol]; !ok {
		s.features[symbol] = make(map[string]float64)
	}
	for name, value := range feats {
		s.features[symbol][name] = value
	}
}

func TestMomentum_Init(t *testing.T) {
	s := New()
	cfg := defaultConfig()

	err := s.Init(cfg)
	require.NoError(t, err)

	assert.Equal(t, "simple_momentum", s.Name())
	assert.Equal(t, "test-momentum-1", s.config.StrategyID)
	assert.Equal(t, 0.001, s.momentumThreshold)
	assert.Equal(t, 0.5, s.maxVolatility)
	assert.Equal(t, 0.2, s.imbalanceThreshold)
	assert.Equal(t, int64(60), s.cooldownSeconds)
	assert.Equal(t, int64(3600), s.maxHoldSeconds)
	assert.Equal(t, 100.0, s.positionSizeUSD)
	assert.NotNil(t, s.features)
	assert.NotNil(t, s.state.Positions)
	assert.NotNil(t, s.state.LastSignalTimeMs)
	assert.NotNil(t, s.state.EntryTimeMs)
}

func TestMomentum_Init_Defaults(t *testing.T) {
	s := New()
	cfg := strategy.StrategyConfig{
		StrategyID: "test-defaults",
		Symbols:    []string{"ETH-USDT"},
		Exchanges:  []string{"binance"},
		Params:     map[string]interface{}{},
	}

	err := s.Init(cfg)
	require.NoError(t, err)

	assert.Equal(t, DefaultMomentumThreshold, s.momentumThreshold)
	assert.Equal(t, DefaultMaxVolatility, s.maxVolatility)
	assert.Equal(t, DefaultImbalanceThreshold, s.imbalanceThreshold)
	assert.Equal(t, int64(DefaultCooldownSeconds), s.cooldownSeconds)
	assert.Equal(t, int64(DefaultMaxHoldSeconds), s.maxHoldSeconds)
	assert.Equal(t, DefaultPositionSizeUSD, s.positionSizeUSD)
}

func TestMomentum_BuySignal(t *testing.T) {
	s := New()
	require.NoError(t, s.Init(defaultConfig()))

	symbol := "BTC-USDT"
	tsMs := int64(1000000)

	// Pre-populate features that should trigger a buy:
	// momentum > 0.001, imbalance > 0.2, volatility < 0.5
	preloadFeatures(s, symbol, map[string]float64{
		FeatureMomentum:   0.005,
		FeatureImbalance:  0.3,
		FeatureVolatility: 0.1,
	})

	// Trigger evaluation with one more feature event.
	signals := s.OnEvent(featureEvent(symbol, FeatureVWAP, 50000.0, tsMs))

	require.Len(t, signals, 1, "Expected exactly one buy signal")

	sig := signals[0]
	assert.Equal(t, "test-momentum-1", sig.StrategyID)
	assert.Equal(t, symbol, sig.Symbol)
	assert.Equal(t, "order_intent", sig.SignalType)
	assert.Greater(t, sig.Confidence, 0.0)
	assert.LessOrEqual(t, sig.Confidence, 1.0)
	require.Len(t, sig.OrdersIntent, 1)
	assert.Equal(t, "buy", sig.OrdersIntent[0].Side)
	assert.Equal(t, "binance", sig.OrdersIntent[0].Exchange)
	assert.Equal(t, symbol, sig.OrdersIntent[0].Symbol)
	assert.Equal(t, "market", sig.OrdersIntent[0].OrderType)
	assert.Contains(t, sig.ReasonCodes, "momentum_buy")
}

func TestMomentum_BuySignal_ConfidenceCapped(t *testing.T) {
	s := New()
	require.NoError(t, s.Init(defaultConfig()))

	symbol := "BTC-USDT"
	tsMs := int64(1000000)

	// High momentum: 0.05 * 100 = 5.0, should be capped to 1.0
	preloadFeatures(s, symbol, map[string]float64{
		FeatureMomentum:   0.05,
		FeatureImbalance:  0.5,
		FeatureVolatility: 0.1,
	})

	signals := s.OnEvent(featureEvent(symbol, FeatureVWAP, 50000.0, tsMs))
	require.Len(t, signals, 1)
	assert.Equal(t, 1.0, signals[0].Confidence, "Confidence should be capped at 1.0")
}

func TestMomentum_SellSignal(t *testing.T) {
	s := New()
	require.NoError(t, s.Init(defaultConfig()))

	symbol := "BTC-USDT"
	tsMs := int64(1000000)

	// Establish a position.
	s.mu.Lock()
	s.state.Positions[symbol] = 100.0
	s.state.EntryTimeMs[symbol] = tsMs
	s.mu.Unlock()

	// Pre-populate features that trigger sell.
	preloadFeatures(s, symbol, map[string]float64{
		FeatureMomentum:  -0.005,
		FeatureImbalance: -0.3,
	})

	// Trigger evaluation.
	signals := s.OnEvent(featureEvent(symbol, FeatureVolatility, 0.1, tsMs+70000))

	require.Len(t, signals, 1, "Expected exactly one sell signal")

	sig := signals[0]
	assert.Equal(t, "test-momentum-1", sig.StrategyID)
	assert.Equal(t, symbol, sig.Symbol)
	assert.Equal(t, "order_intent", sig.SignalType)
	assert.Equal(t, 1.0, sig.Confidence) // close signal has confidence 1.0
	require.Len(t, sig.OrdersIntent, 1)
	assert.Equal(t, "sell", sig.OrdersIntent[0].Side)
	assert.Contains(t, sig.ReasonCodes, "momentum_reversal")
}

func TestMomentum_SellSignal_NoPosition(t *testing.T) {
	s := New()
	require.NoError(t, s.Init(defaultConfig()))

	symbol := "BTC-USDT"
	tsMs := int64(1000000)

	// No position - sell conditions met but nothing to close.
	preloadFeatures(s, symbol, map[string]float64{
		FeatureMomentum:  -0.005,
		FeatureImbalance: -0.3,
	})

	signals := s.OnEvent(featureEvent(symbol, FeatureVolatility, 0.1, tsMs))
	assert.Len(t, signals, 0, "Should not sell when no position exists")
}

func TestMomentum_Cooldown(t *testing.T) {
	s := New()
	require.NoError(t, s.Init(defaultConfig()))

	symbol := "BTC-USDT"
	tsMs := int64(1000000)

	// Pre-populate buy-triggering features.
	preloadFeatures(s, symbol, map[string]float64{
		FeatureMomentum:   0.005,
		FeatureImbalance:  0.3,
		FeatureVolatility: 0.1,
	})

	// First signal should succeed.
	signals := s.OnEvent(featureEvent(symbol, FeatureVWAP, 50000.0, tsMs))
	require.Len(t, signals, 1, "First signal should be generated")

	// Second signal within cooldown (60s = 60000ms) should not fire.
	signals = s.OnEvent(featureEvent(symbol, FeatureMomentum, 0.006, tsMs+30000))
	assert.Len(t, signals, 0, "Signal within cooldown should be suppressed")

	// Signal after cooldown expires should work.
	// The first signal didn't change Positions (that requires a fill), so no
	// position blocking issue. But the buy signal called buildBuySignal which
	// does NOT set Positions (positions only update via fills).
	signals = s.OnEvent(featureEvent(symbol, FeatureMomentum, 0.005, tsMs+61000))
	assert.Len(t, signals, 1, "Signal after cooldown should be allowed")
}

func TestMomentum_HighVolatility(t *testing.T) {
	s := New()
	require.NoError(t, s.Init(defaultConfig()))

	symbol := "BTC-USDT"
	tsMs := int64(1000000)

	// Pre-populate features where momentum + imbalance would trigger buy,
	// but volatility exceeds max.
	preloadFeatures(s, symbol, map[string]float64{
		FeatureMomentum:   0.005,
		FeatureImbalance:  0.3,
		FeatureVolatility: 0.6, // > 0.5 max
	})

	// Trigger evaluation.
	signals := s.OnEvent(featureEvent(symbol, FeatureVWAP, 50000.0, tsMs))
	assert.Len(t, signals, 0, "High volatility should prevent signal")
}

func TestMomentum_NoSignal_LowMomentum(t *testing.T) {
	s := New()
	require.NoError(t, s.Init(defaultConfig()))

	symbol := "BTC-USDT"
	tsMs := int64(1000000)

	// Momentum below threshold.
	preloadFeatures(s, symbol, map[string]float64{
		FeatureMomentum:   0.0005,
		FeatureImbalance:  0.3,
		FeatureVolatility: 0.1,
	})

	signals := s.OnEvent(featureEvent(symbol, FeatureVWAP, 50000.0, tsMs))
	assert.Len(t, signals, 0, "Low momentum should not trigger signal")
}

func TestMomentum_NoSignal_LowImbalance(t *testing.T) {
	s := New()
	require.NoError(t, s.Init(defaultConfig()))

	symbol := "BTC-USDT"
	tsMs := int64(1000000)

	// Imbalance below threshold.
	preloadFeatures(s, symbol, map[string]float64{
		FeatureMomentum:   0.005,
		FeatureImbalance:  0.1, // < 0.2 threshold
		FeatureVolatility: 0.1,
	})

	signals := s.OnEvent(featureEvent(symbol, FeatureVWAP, 50000.0, tsMs))
	assert.Len(t, signals, 0, "Low imbalance should not trigger signal")
}

func TestMomentum_SnapshotRestore(t *testing.T) {
	s := New()
	require.NoError(t, s.Init(defaultConfig()))

	symbol := "BTC-USDT"
	tsMs := int64(1000000)

	// Build up some state.
	s.mu.Lock()
	s.state.Positions[symbol] = 150.0
	s.state.EntryTimeMs[symbol] = tsMs
	s.state.LastSignalTimeMs[symbol] = tsMs + 5000
	s.state.TradeCount = 3
	s.mu.Unlock()

	// Snapshot.
	data, err := s.SnapshotState()
	require.NoError(t, err)
	require.NotEmpty(t, data)

	// Verify it's valid JSON.
	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &parsed))

	// Create a new strategy and restore.
	s2 := New()
	require.NoError(t, s2.Init(defaultConfig()))

	err = s2.RestoreState(data)
	require.NoError(t, err)

	// Verify state is preserved.
	assert.Equal(t, 150.0, s2.state.Positions[symbol])
	assert.Equal(t, tsMs, s2.state.EntryTimeMs[symbol])
	assert.Equal(t, tsMs+5000, s2.state.LastSignalTimeMs[symbol])
	assert.Equal(t, 3, s2.state.TradeCount)
}

func TestMomentum_SnapshotRestore_EmptyState(t *testing.T) {
	s := New()
	require.NoError(t, s.Init(defaultConfig()))

	// Snapshot with empty state.
	data, err := s.SnapshotState()
	require.NoError(t, err)

	s2 := New()
	require.NoError(t, s2.Init(defaultConfig()))
	require.NoError(t, s2.RestoreState(data))

	// Maps should be initialized, not nil.
	assert.NotNil(t, s2.state.Positions)
	assert.NotNil(t, s2.state.LastSignalTimeMs)
	assert.NotNil(t, s2.state.EntryTimeMs)
}

func TestMomentum_OnTimer_MaxHoldTime(t *testing.T) {
	s := New()
	cfg := defaultConfig()
	cfg.Params[ParamMaxHoldSeconds] = 60.0 // 60 seconds for test
	require.NoError(t, s.Init(cfg))

	symbol := "BTC-USDT"
	entryTs := int64(1000000)

	// Set a position with entry time.
	s.mu.Lock()
	s.state.Positions[symbol] = 100.0
	s.state.EntryTimeMs[symbol] = entryTs
	s.mu.Unlock()

	// Timer before max hold: no signal.
	signals := s.OnTimer(entryTs+30000, "periodic") // 30s
	assert.Len(t, signals, 0, "Should not close before max hold time")

	// Timer after max hold: close signal.
	signals = s.OnTimer(entryTs+61000, "periodic") // 61s
	require.Len(t, signals, 1, "Should close after max hold time")
	assert.Equal(t, "sell", signals[0].OrdersIntent[0].Side)
	assert.Contains(t, signals[0].ReasonCodes, "max_hold_time_exceeded")
}

func TestMomentum_OnIntel_BullishBias(t *testing.T) {
	s := New()
	require.NoError(t, s.Init(defaultConfig()))

	symbol := "BTC-USDT"

	// Send bullish intel with high confidence.
	intel := strategy.IntelEvent{
		EventID:         "intel-1",
		Confidence:      0.9,
		DirectionalBias: "bullish",
		AssetTags:       []string{symbol},
	}

	signals := s.OnIntel(intel)
	assert.Len(t, signals, 0, "Intel should not directly produce signals")

	// Verify threshold is adjusted (bullish lowers threshold).
	s.mu.Lock()
	threshold := s.effectiveMomentumThreshold(symbol)
	s.mu.Unlock()

	assert.Less(t, threshold, s.momentumThreshold,
		"Bullish intel should lower momentum threshold")
}

func TestMomentum_OnIntel_BearishBias(t *testing.T) {
	s := New()
	require.NoError(t, s.Init(defaultConfig()))

	symbol := "BTC-USDT"

	intel := strategy.IntelEvent{
		EventID:         "intel-1",
		Confidence:      0.9,
		DirectionalBias: "bearish",
		AssetTags:       []string{symbol},
	}

	s.OnIntel(intel)

	s.mu.Lock()
	threshold := s.effectiveMomentumThreshold(symbol)
	s.mu.Unlock()

	assert.Greater(t, threshold, s.momentumThreshold,
		"Bearish intel should raise momentum threshold")
}

func TestMomentum_OnIntel_LowConfidence(t *testing.T) {
	s := New()
	require.NoError(t, s.Init(defaultConfig()))

	symbol := "BTC-USDT"

	// Low confidence intel should be ignored.
	intel := strategy.IntelEvent{
		EventID:         "intel-2",
		Confidence:      0.3,
		DirectionalBias: "bullish",
		AssetTags:       []string{symbol},
	}

	s.OnIntel(intel)

	s.mu.Lock()
	_, hasBias := s.intelBias[symbol]
	s.mu.Unlock()

	assert.False(t, hasBias, "Low confidence intel should not set bias")
}

func TestMomentum_FillUpdatesPosition(t *testing.T) {
	s := New()
	require.NoError(t, s.Init(defaultConfig()))

	symbol := "BTC-USDT"

	// Simulate a buy fill.
	fillEvent := strategy.MarketEvent{
		Type: "fill",
		Fill: &bus.Fill{
			BaseEvent:  bus.NewBaseEvent("test", "1.0"),
			Exchange:   "binance",
			Symbol:     symbol,
			StrategyID: "test-momentum-1",
			Side:       "buy",
			Qty:        mustDecimal("50"),
			Price:      mustDecimal("50000"),
		},
	}

	signals := s.OnEvent(fillEvent)
	assert.Len(t, signals, 0, "Fill events should not produce signals")

	s.mu.Lock()
	pos := s.state.Positions[symbol]
	s.mu.Unlock()

	assert.Equal(t, 50.0, pos, "Position should be updated by fill")
}

func TestMomentum_FillSellReducesPosition(t *testing.T) {
	s := New()
	require.NoError(t, s.Init(defaultConfig()))

	symbol := "BTC-USDT"

	// Start with a position.
	s.mu.Lock()
	s.state.Positions[symbol] = 100.0
	s.state.EntryTimeMs[symbol] = 1000000
	s.mu.Unlock()

	// Simulate a sell fill.
	fillEvent := strategy.MarketEvent{
		Type: "fill",
		Fill: &bus.Fill{
			BaseEvent:  bus.NewBaseEvent("test", "1.0"),
			Exchange:   "binance",
			Symbol:     symbol,
			StrategyID: "test-momentum-1",
			Side:       "sell",
			Qty:        mustDecimal("100"),
			Price:      mustDecimal("51000"),
		},
	}

	s.OnEvent(fillEvent)

	s.mu.Lock()
	pos := s.state.Positions[symbol]
	_, hasEntry := s.state.EntryTimeMs[symbol]
	s.mu.Unlock()

	assert.Equal(t, 0.0, pos, "Position should be zeroed after full sell")
	assert.False(t, hasEntry, "Entry time should be cleared when position is closed")
}

func TestMomentum_Deterministic(t *testing.T) {
	// Same inputs must produce same outputs (modulo UUIDs).
	sym := "BTC-USDT"
	tsMs := int64(1000000)

	run := func() []bus.StrategySignal {
		s := New()
		_ = s.Init(defaultConfig())

		// Pre-populate features, then trigger with one event.
		preloadFeatures(s, sym, map[string]float64{
			FeatureMomentum:   0.005,
			FeatureImbalance:  0.3,
			FeatureVolatility: 0.1,
		})

		return s.OnEvent(featureEvent(sym, FeatureVWAP, 50000.0, tsMs))
	}

	signals1 := run()
	signals2 := run()

	require.Len(t, signals1, 1)
	require.Len(t, signals2, 1)

	// Deterministic fields must match.
	assert.Equal(t, signals1[0].StrategyID, signals2[0].StrategyID)
	assert.Equal(t, signals1[0].Symbol, signals2[0].Symbol)
	assert.Equal(t, signals1[0].SignalType, signals2[0].SignalType)
	assert.Equal(t, signals1[0].Confidence, signals2[0].Confidence)
	assert.Equal(t, signals1[0].OrdersIntent[0].Side, signals2[0].OrdersIntent[0].Side)
	assert.Equal(t, signals1[0].OrdersIntent[0].Exchange, signals2[0].OrdersIntent[0].Exchange)
	assert.Equal(t, signals1[0].OrdersIntent[0].OrderType, signals2[0].OrdersIntent[0].OrderType)
}

func TestMomentum_NoSignal_WithExistingPosition(t *testing.T) {
	s := New()
	require.NoError(t, s.Init(defaultConfig()))

	symbol := "BTC-USDT"
	tsMs := int64(1000000)

	// Already have a long position.
	s.mu.Lock()
	s.state.Positions[symbol] = 100.0
	s.mu.Unlock()

	// Pre-populate bullish features (would normally trigger buy).
	preloadFeatures(s, symbol, map[string]float64{
		FeatureMomentum:   0.005,
		FeatureImbalance:  0.3,
		FeatureVolatility: 0.1,
	})

	signals := s.OnEvent(featureEvent(symbol, FeatureVWAP, 50000.0, tsMs))
	assert.Len(t, signals, 0, "Should not buy when already holding a position")
}

func TestMomentum_NilEventFields(t *testing.T) {
	s := New()
	require.NoError(t, s.Init(defaultConfig()))

	// Events with nil sub-types should not panic.
	signals := s.OnEvent(strategy.MarketEvent{Type: "tick", Tick: nil})
	assert.Nil(t, signals)

	signals = s.OnEvent(strategy.MarketEvent{Type: "trade", Trade: nil})
	assert.Nil(t, signals)

	signals = s.OnEvent(strategy.MarketEvent{Type: "feature", Feature: nil})
	assert.Nil(t, signals)

	signals = s.OnEvent(strategy.MarketEvent{Type: "fill", Fill: nil})
	assert.Nil(t, signals)

	signals = s.OnEvent(strategy.MarketEvent{Type: "unknown"})
	assert.Nil(t, signals)
}

func TestMomentum_FillIgnoresOtherStrategies(t *testing.T) {
	s := New()
	require.NoError(t, s.Init(defaultConfig()))

	symbol := "BTC-USDT"

	// Fill from a different strategy.
	fillEvent := strategy.MarketEvent{
		Type: "fill",
		Fill: &bus.Fill{
			BaseEvent:  bus.NewBaseEvent("test", "1.0"),
			Exchange:   "binance",
			Symbol:     symbol,
			StrategyID: "other-strategy",
			Side:       "buy",
			Qty:        mustDecimal("100"),
			Price:      mustDecimal("50000"),
		},
	}

	s.OnEvent(fillEvent)

	s.mu.Lock()
	pos := s.state.Positions[symbol]
	s.mu.Unlock()

	assert.Equal(t, 0.0, pos, "Should not update position from another strategy's fill")
}

func mustDecimal(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}
