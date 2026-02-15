package momentum

import (
	"encoding/json"
	"fmt"
	"math"
	"sync"

	"github.com/google/uuid"
	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/nexus-trading/nexus/internal/strategy"
	"github.com/shopspring/decimal"
)

// Parameter keys used in StrategyConfig.Params.
const (
	ParamMomentumThreshold = "momentum_threshold"
	ParamMaxVolatility     = "max_volatility"
	ParamImbalanceThreshold = "imbalance_threshold"
	ParamCooldownSeconds   = "cooldown_seconds"
	ParamMaxHoldSeconds    = "max_hold_seconds"
	ParamPositionSizeUSD   = "position_size_usd"
)

// Default parameter values.
const (
	DefaultMomentumThreshold = 0.001
	DefaultMaxVolatility     = 0.5
	DefaultImbalanceThreshold = 0.2
	DefaultCooldownSeconds   = 60
	DefaultMaxHoldSeconds    = 3600
	DefaultPositionSizeUSD   = 100.0
)

// Feature names expected from the feature engine.
const (
	FeatureVWAP       = "vwap"
	FeatureMomentum   = "momentum"
	FeatureVolatility = "volatility"
	FeatureImbalance  = "imbalance"
)

// SimpleMomentum is an MVP momentum-based strategy.
//
// Rules:
//   - Deterministic: same inputs produce same outputs.
//   - No I/O: does not call time.Now(), network, or disk. Uses event timestamps.
//   - All parameters come from config, not hardcoded magic numbers.
type SimpleMomentum struct {
	mu       sync.Mutex
	config   strategy.StrategyConfig
	features map[string]map[string]float64 // symbol -> feature_name -> value
	state    stateData

	// Resolved parameters (from config or defaults).
	momentumThreshold float64
	maxVolatility     float64
	imbalanceThreshold float64
	cooldownSeconds   int64
	maxHoldSeconds    int64
	positionSizeUSD   float64

	// intelBias is a per-symbol directional bias from intel events.
	// Positive = bullish, negative = bearish.
	intelBias map[string]float64
}

// stateData holds serializable strategy state for snapshot/restore.
type stateData struct {
	LastSignalTimeMs map[string]int64   `json:"last_signal_time_ms"` // symbol -> last signal timestamp (ms)
	TradeCount       int                `json:"trade_count"`
	Positions        map[string]float64 `json:"positions"`           // symbol -> current position size (USD)
	EntryTimeMs      map[string]int64   `json:"entry_time_ms"`       // symbol -> entry time (ms)
}

// New creates a new SimpleMomentum strategy instance.
func New() *SimpleMomentum {
	return &SimpleMomentum{}
}

// Name returns the strategy name.
func (s *SimpleMomentum) Name() string {
	return "simple_momentum"
}

// Init initializes the strategy with configuration.
func (s *SimpleMomentum) Init(config strategy.StrategyConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.config = config
	s.features = make(map[string]map[string]float64)
	s.intelBias = make(map[string]float64)
	s.state = stateData{
		LastSignalTimeMs: make(map[string]int64),
		Positions:        make(map[string]float64),
		EntryTimeMs:      make(map[string]int64),
	}

	// Resolve parameters with defaults.
	s.momentumThreshold = paramFloat64(config.Params, ParamMomentumThreshold, DefaultMomentumThreshold)
	s.maxVolatility = paramFloat64(config.Params, ParamMaxVolatility, DefaultMaxVolatility)
	s.imbalanceThreshold = paramFloat64(config.Params, ParamImbalanceThreshold, DefaultImbalanceThreshold)
	s.cooldownSeconds = int64(paramFloat64(config.Params, ParamCooldownSeconds, DefaultCooldownSeconds))
	s.maxHoldSeconds = int64(paramFloat64(config.Params, ParamMaxHoldSeconds, DefaultMaxHoldSeconds))
	s.positionSizeUSD = paramFloat64(config.Params, ParamPositionSizeUSD, DefaultPositionSizeUSD)

	return nil
}

// OnEvent processes a market event and optionally emits signals.
func (s *SimpleMomentum) OnEvent(event strategy.MarketEvent) []bus.StrategySignal {
	s.mu.Lock()
	defer s.mu.Unlock()

	var eventTs int64

	switch event.Type {
	case "feature":
		if event.Feature == nil {
			return nil
		}
		s.storeFeature(event.Feature.Symbol, event.Feature.Name, event.Feature.Value)
		eventTs = event.Feature.Ts
		return s.evaluate(event.Feature.Symbol, eventTs)

	case "tick":
		if event.Tick == nil {
			return nil
		}
		eventTs = event.Tick.Timestamp.UnixMilli()
		return s.evaluate(event.Tick.Symbol, eventTs)

	case "trade":
		if event.Trade == nil {
			return nil
		}
		eventTs = event.Trade.Timestamp.UnixMilli()
		return s.evaluate(event.Trade.Symbol, eventTs)

	case "fill":
		if event.Fill == nil {
			return nil
		}
		s.handleFill(event.Fill)
		return nil

	default:
		return nil
	}
}

// OnTimer is called periodically. Checks if any position should be closed
// due to exceeding max hold time.
func (s *SimpleMomentum) OnTimer(ts int64, timerID string) []bus.StrategySignal {
	s.mu.Lock()
	defer s.mu.Unlock()

	var signals []bus.StrategySignal
	maxHoldMs := s.maxHoldSeconds * 1000

	for symbol, entryTs := range s.state.EntryTimeMs {
		pos, hasPos := s.state.Positions[symbol]
		if !hasPos || pos == 0 {
			continue
		}
		holdMs := ts - entryTs
		if holdMs > maxHoldMs {
			sig := s.buildCloseSignal(symbol, ts, "max_hold_time_exceeded")
			if sig != nil {
				signals = append(signals, *sig)
			}
		}
	}
	return signals
}

// OnIntel processes an intel event. If directional_bias is strong (>0.7),
// it biases the momentum threshold for relevant symbols.
func (s *SimpleMomentum) OnIntel(event strategy.IntelEvent) []bus.StrategySignal {
	s.mu.Lock()
	defer s.mu.Unlock()

	if event.Confidence < 0.7 {
		return nil
	}

	var bias float64
	switch event.DirectionalBias {
	case "bullish":
		bias = event.Confidence
	case "bearish":
		bias = -event.Confidence
	default:
		return nil
	}

	// Apply bias to all tagged assets.
	for _, tag := range event.AssetTags {
		s.intelBias[tag] = bias
	}

	return nil
}

// SnapshotState serializes the strategy's internal state for checkpointing.
func (s *SimpleMomentum) SnapshotState() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return json.Marshal(s.state)
}

// RestoreState restores the strategy's internal state from a checkpoint.
func (s *SimpleMomentum) RestoreState(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var restored stateData
	if err := json.Unmarshal(data, &restored); err != nil {
		return fmt.Errorf("unmarshal state: %w", err)
	}

	// Ensure maps are initialized even if they were empty in the snapshot.
	if restored.LastSignalTimeMs == nil {
		restored.LastSignalTimeMs = make(map[string]int64)
	}
	if restored.Positions == nil {
		restored.Positions = make(map[string]float64)
	}
	if restored.EntryTimeMs == nil {
		restored.EntryTimeMs = make(map[string]int64)
	}

	s.state = restored
	return nil
}

// --- Internal helpers (called with lock held) ---

// storeFeature stores a feature value for a symbol.
func (s *SimpleMomentum) storeFeature(symbol, name string, value float64) {
	if _, ok := s.features[symbol]; !ok {
		s.features[symbol] = make(map[string]float64)
	}
	s.features[symbol][name] = value
}

// evaluate runs the momentum decision logic for a symbol.
func (s *SimpleMomentum) evaluate(symbol string, eventTsMs int64) []bus.StrategySignal {
	feats, ok := s.features[symbol]
	if !ok {
		return nil
	}

	momentum, hasMomentum := feats[FeatureMomentum]
	imbalance, hasImbalance := feats[FeatureImbalance]
	volatility, hasVol := feats[FeatureVolatility]

	// Need at minimum momentum and imbalance features.
	if !hasMomentum || !hasImbalance {
		return nil
	}

	// Apply cooldown check.
	cooldownMs := s.cooldownSeconds * 1000
	if lastTs, exists := s.state.LastSignalTimeMs[symbol]; exists {
		if eventTsMs-lastTs < cooldownMs {
			return nil
		}
	}

	// Get effective momentum threshold (potentially biased by intel).
	threshold := s.effectiveMomentumThreshold(symbol)

	currentPos := s.state.Positions[symbol]

	// BUY logic: momentum > threshold AND imbalance > imbalance_threshold AND volatility < max_vol
	if momentum > threshold && imbalance > s.imbalanceThreshold {
		// Volatility filter: if we have volatility data, check it.
		if hasVol && volatility >= s.maxVolatility {
			return nil
		}

		// Only enter if we don't already have a long position.
		if currentPos > 0 {
			return nil
		}

		confidence := math.Min(momentum*100, 1.0)
		sig := s.buildBuySignal(symbol, eventTsMs, confidence)
		return []bus.StrategySignal{sig}
	}

	// SELL logic: momentum < -threshold AND imbalance < -imbalance_threshold
	if momentum < -threshold && imbalance < -s.imbalanceThreshold {
		// Only close if we have a long position.
		if currentPos <= 0 {
			return nil
		}

		closeSig := s.buildCloseSignal(symbol, eventTsMs, "momentum_reversal")
		if closeSig != nil {
			return []bus.StrategySignal{*closeSig}
		}
	}

	return nil
}

// effectiveMomentumThreshold returns the momentum threshold, adjusted by intel bias.
// A strong bullish bias lowers the threshold (easier to buy).
// A strong bearish bias raises the threshold (harder to buy).
func (s *SimpleMomentum) effectiveMomentumThreshold(symbol string) float64 {
	bias, hasBias := s.intelBias[symbol]
	if !hasBias {
		return s.momentumThreshold
	}

	// Scale threshold: bullish bias (positive) reduces threshold by up to 50%,
	// bearish bias (negative) increases threshold by up to 50%.
	adjustment := 1.0 - (bias * 0.5)
	return s.momentumThreshold * adjustment
}

// buildBuySignal constructs a buy signal with an OrderIntent.
func (s *SimpleMomentum) buildBuySignal(symbol string, eventTsMs int64, confidence float64) bus.StrategySignal {
	exchange := ""
	if len(s.config.Exchanges) > 0 {
		exchange = s.config.Exchanges[0]
	}

	intentID := uuid.New().String()
	posSize := decimal.NewFromFloat(s.positionSizeUSD)

	signal := bus.StrategySignal{
		BaseEvent:  bus.NewBaseEvent("strategy."+s.config.StrategyID, "1.0"),
		StrategyID: s.config.StrategyID,
		Symbol:     symbol,
		SignalType: "order_intent",
		Confidence: confidence,
		OrdersIntent: []bus.OrderIntent{
			{
				BaseEvent:   bus.NewBaseEvent("strategy."+s.config.StrategyID, "1.0"),
				IntentID:    intentID,
				StrategyID:  s.config.StrategyID,
				Exchange:    exchange,
				Symbol:      symbol,
				Side:        "buy",
				OrderType:   "market",
				Qty:         posSize,
				TimeInForce: "IOC",
				Tags:        []string{"momentum", "entry"},
			},
		},
		ReasonCodes: []string{"momentum_buy"},
		Metadata: map[string]any{
			"position_size_usd": s.positionSizeUSD,
		},
	}

	// Record state.
	s.state.LastSignalTimeMs[symbol] = eventTsMs
	s.state.TradeCount++

	return signal
}

// buildCloseSignal constructs a sell signal to close a long position.
func (s *SimpleMomentum) buildCloseSignal(symbol string, eventTsMs int64, reason string) *bus.StrategySignal {
	currentPos := s.state.Positions[symbol]
	if currentPos <= 0 {
		return nil
	}

	exchange := ""
	if len(s.config.Exchanges) > 0 {
		exchange = s.config.Exchanges[0]
	}

	intentID := uuid.New().String()
	qty := decimal.NewFromFloat(currentPos)

	signal := bus.StrategySignal{
		BaseEvent:  bus.NewBaseEvent("strategy."+s.config.StrategyID, "1.0"),
		StrategyID: s.config.StrategyID,
		Symbol:     symbol,
		SignalType: "order_intent",
		Confidence: 1.0,
		OrdersIntent: []bus.OrderIntent{
			{
				BaseEvent:   bus.NewBaseEvent("strategy."+s.config.StrategyID, "1.0"),
				IntentID:    intentID,
				StrategyID:  s.config.StrategyID,
				Exchange:    exchange,
				Symbol:      symbol,
				Side:        "sell",
				OrderType:   "market",
				Qty:         qty,
				TimeInForce: "IOC",
				Tags:        []string{"momentum", "exit", reason},
			},
		},
		ReasonCodes: []string{reason},
		Metadata: map[string]any{
			"close_size_usd": currentPos,
		},
	}

	// Record state.
	s.state.LastSignalTimeMs[symbol] = eventTsMs
	s.state.TradeCount++

	return &signal
}

// handleFill updates internal position tracking based on a fill.
func (s *SimpleMomentum) handleFill(fill *bus.Fill) {
	if fill.StrategyID != s.config.StrategyID {
		return
	}

	fillQty := fill.Qty.InexactFloat64()
	switch fill.Side {
	case "buy":
		s.state.Positions[fill.Symbol] += fillQty
		if _, exists := s.state.EntryTimeMs[fill.Symbol]; !exists {
			s.state.EntryTimeMs[fill.Symbol] = fill.Timestamp.UnixMilli()
		}
	case "sell":
		s.state.Positions[fill.Symbol] -= fillQty
		if s.state.Positions[fill.Symbol] <= 0 {
			s.state.Positions[fill.Symbol] = 0
			delete(s.state.EntryTimeMs, fill.Symbol)
		}
	}
}

// paramFloat64 extracts a float64 parameter from the params map, returning
// the default if missing or not convertible.
func paramFloat64(params map[string]interface{}, key string, defaultVal float64) float64 {
	v, ok := params[key]
	if !ok {
		return defaultVal
	}

	switch val := v.(type) {
	case float64:
		return val
	case int:
		return float64(val)
	case int64:
		return float64(val)
	case json.Number:
		f, err := val.Float64()
		if err != nil {
			return defaultVal
		}
		return f
	default:
		return defaultVal
	}
}
