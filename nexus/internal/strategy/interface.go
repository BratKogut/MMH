package strategy

import (
	"github.com/nexus-trading/nexus/internal/bus"
)

// MarketEvent wraps all types of events that a strategy can receive.
type MarketEvent struct {
	Type      string              `json:"type"` // tick|trade|orderbook|ohlcv|position|fill|feature|regime
	Tick      *bus.Tick           `json:"tick,omitempty"`
	Trade     *bus.Trade          `json:"trade,omitempty"`
	Orderbook *bus.OrderbookUpdate `json:"orderbook,omitempty"`
	Feature   *FeatureUpdate      `json:"feature,omitempty"`
	Regime    *bus.RegimeUpdate   `json:"regime,omitempty"`
	Fill      *bus.Fill           `json:"fill,omitempty"`
}

// FeatureUpdate represents an update to a computed feature.
type FeatureUpdate struct {
	Symbol string  `json:"symbol"`
	Name   string  `json:"name"`
	Value  float64 `json:"value"`
	Ts     int64   `json:"ts"`
}

// IntelEvent wraps intel events for strategy consumption.
type IntelEvent struct {
	EventID         string   `json:"event_id"`
	Topic           string   `json:"topic"`
	Priority        string   `json:"priority"`
	TTLSeconds      int      `json:"ttl_seconds"`
	Confidence      float64  `json:"confidence"`
	ImpactHorizon   string   `json:"impact_horizon"`
	DirectionalBias string   `json:"directional_bias"`
	AssetTags       []string `json:"asset_tags"`
	ClaimsJSON      string   `json:"claims_json"`
}

// StrategyConfig holds configuration for a strategy instance.
type StrategyConfig struct {
	StrategyID string                 `json:"strategy_id"`
	Params     map[string]interface{} `json:"params"`
	Symbols    []string               `json:"symbols"`
	Exchanges  []string               `json:"exchanges"`
}

// Strategy is the interface that all trading strategies must implement.
// The same implementation works for backtest, paper, and live.
//
// Rules:
// - Strategy NEVER sends orders directly to exchange
// - Strategy NEVER exceeds soft limits in config (hard limits in Risk Engine)
// - Strategy is deterministic: same inputs -> same signals
type Strategy interface {
	// Init initializes the strategy with configuration.
	Init(config StrategyConfig) error

	// OnEvent processes a market event and optionally emits signals.
	OnEvent(event MarketEvent) []bus.StrategySignal

	// OnTimer is called periodically (1s/5s/1m depending on config).
	OnTimer(ts int64, timerID string) []bus.StrategySignal

	// OnIntel processes an intel event and optionally emits signals.
	OnIntel(event IntelEvent) []bus.StrategySignal

	// SnapshotState serializes the strategy's internal state for checkpointing.
	SnapshotState() ([]byte, error)

	// RestoreState restores the strategy's internal state from a checkpoint.
	RestoreState(data []byte) error

	// Name returns the strategy name.
	Name() string
}
