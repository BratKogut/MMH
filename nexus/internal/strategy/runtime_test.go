package strategy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus-trading/nexus/internal/bus"
)

// ---------------------------------------------------------------------------
// stubStrategy is a minimal strategy implementation for runtime tests.
// ---------------------------------------------------------------------------

type stubStrategy struct {
	name       string
	initErr    error
	intelCalls []IntelEvent
	signals    []bus.StrategySignal // pre-loaded signals to return from OnIntel
}

func (s *stubStrategy) Init(_ StrategyConfig) error            { return s.initErr }
func (s *stubStrategy) OnEvent(_ MarketEvent) []bus.StrategySignal { return nil }
func (s *stubStrategy) OnTimer(_ int64, _ string) []bus.StrategySignal { return nil }
func (s *stubStrategy) SnapshotState() ([]byte, error)         { return nil, nil }
func (s *stubStrategy) RestoreState(_ []byte) error             { return nil }
func (s *stubStrategy) Name() string                            { return s.name }

func (s *stubStrategy) OnIntel(event IntelEvent) []bus.StrategySignal {
	s.intelCalls = append(s.intelCalls, event)
	return s.signals
}

// stubExecutor captures executed signals.
type stubExecutor struct {
	signals []bus.StrategySignal
}

func (e *stubExecutor) Execute(_ context.Context, sig bus.StrategySignal) error {
	e.signals = append(e.signals, sig)
	return nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestRuntime_OnIntelEvent_RoutesToMatchingStrategies(t *testing.T) {
	producer := bus.NewStubProducer()
	executor := &stubExecutor{}
	rt := NewRuntime(producer, executor, RuntimeConfig{TimerIntervalMs: 1000})

	btcStrat := &stubStrategy{name: "btc-momentum"}
	ethStrat := &stubStrategy{name: "eth-momentum"}
	allStrat := &stubStrategy{name: "all-symbols"}

	require.NoError(t, rt.Register(StrategyConfig{
		StrategyID: "strat-btc",
		Symbols:    []string{"BTC-USDT"},
	}, btcStrat))

	require.NoError(t, rt.Register(StrategyConfig{
		StrategyID: "strat-eth",
		Symbols:    []string{"ETH-USDT"},
	}, ethStrat))

	require.NoError(t, rt.Register(StrategyConfig{
		StrategyID: "strat-all",
		Symbols:    []string{}, // subscribes to all
	}, allStrat))

	ctx := context.Background()

	// Intel event about BTC should reach btc-strategy and all-symbols.
	intelBTC := IntelEvent{
		EventID:         "intel-001",
		Topic:           "volatility_spike",
		Confidence:      0.85,
		DirectionalBias: "bullish",
		AssetTags:       []string{"BTC-USDT"},
	}
	err := rt.OnIntelEvent(ctx, intelBTC)
	require.NoError(t, err)

	assert.Len(t, btcStrat.intelCalls, 1)
	assert.Len(t, ethStrat.intelCalls, 0, "ETH strategy should not receive BTC intel")
	assert.Len(t, allStrat.intelCalls, 1)

	// Intel event about ETH should reach eth-strategy and all-symbols.
	intelETH := IntelEvent{
		EventID:         "intel-002",
		Topic:           "sentiment_shift",
		Confidence:      0.72,
		DirectionalBias: "bearish",
		AssetTags:       []string{"ETH-USDT"},
	}
	err = rt.OnIntelEvent(ctx, intelETH)
	require.NoError(t, err)

	assert.Len(t, btcStrat.intelCalls, 1, "BTC strategy should not receive ETH intel")
	assert.Len(t, ethStrat.intelCalls, 1)
	assert.Len(t, allStrat.intelCalls, 2)
}

func TestRuntime_OnIntelEvent_MultiAssetTags(t *testing.T) {
	producer := bus.NewStubProducer()
	executor := &stubExecutor{}
	rt := NewRuntime(producer, executor, RuntimeConfig{TimerIntervalMs: 1000})

	btcStrat := &stubStrategy{name: "btc-only"}
	ethStrat := &stubStrategy{name: "eth-only"}

	require.NoError(t, rt.Register(StrategyConfig{
		StrategyID: "strat-btc",
		Symbols:    []string{"BTC-USDT"},
	}, btcStrat))
	require.NoError(t, rt.Register(StrategyConfig{
		StrategyID: "strat-eth",
		Symbols:    []string{"ETH-USDT"},
	}, ethStrat))

	ctx := context.Background()

	// Intel event about both BTC and ETH should reach both strategies.
	event := IntelEvent{
		EventID:   "intel-multi",
		Topic:     "macro_event",
		AssetTags: []string{"BTC-USDT", "ETH-USDT"},
	}
	err := rt.OnIntelEvent(ctx, event)
	require.NoError(t, err)

	assert.Len(t, btcStrat.intelCalls, 1)
	assert.Len(t, ethStrat.intelCalls, 1)
}

func TestRuntime_OnIntelEvent_DisabledStrategySkipped(t *testing.T) {
	producer := bus.NewStubProducer()
	executor := &stubExecutor{}
	rt := NewRuntime(producer, executor, RuntimeConfig{TimerIntervalMs: 1000})

	strat := &stubStrategy{name: "disabled-strat"}
	require.NoError(t, rt.Register(StrategyConfig{
		StrategyID: "strat-disabled",
		Symbols:    []string{"BTC-USDT"},
	}, strat))

	require.NoError(t, rt.Disable("strat-disabled"))

	ctx := context.Background()
	event := IntelEvent{
		EventID:   "intel-003",
		AssetTags: []string{"BTC-USDT"},
	}
	err := rt.OnIntelEvent(ctx, event)
	require.NoError(t, err)

	assert.Len(t, strat.intelCalls, 0, "Disabled strategy should not receive intel events")
}

func TestRuntime_OnIntelEvent_SignalsDispatched(t *testing.T) {
	producer := bus.NewStubProducer()
	executor := &stubExecutor{}
	rt := NewRuntime(producer, executor, RuntimeConfig{TimerIntervalMs: 1000})

	sig := bus.StrategySignal{
		BaseEvent:  bus.NewBaseEvent("test", "1.0.0"),
		StrategyID: "strat-with-signal",
		Symbol:     "BTC-USDT",
		SignalType: "target_position",
		Confidence: 0.9,
	}
	strat := &stubStrategy{
		name:    "signal-producer",
		signals: []bus.StrategySignal{sig},
	}

	require.NoError(t, rt.Register(StrategyConfig{
		StrategyID: "strat-with-signal",
		Symbols:    []string{"BTC-USDT"},
	}, strat))

	ctx := context.Background()
	event := IntelEvent{
		EventID:         "intel-004",
		Topic:           "volatility_spike",
		Confidence:      0.9,
		DirectionalBias: "bullish",
		AssetTags:       []string{"BTC-USDT"},
	}
	err := rt.OnIntelEvent(ctx, event)
	require.NoError(t, err)

	assert.Len(t, executor.signals, 1, "Executor should receive signal from strategy's OnIntel")
	assert.Equal(t, "strat-with-signal", executor.signals[0].StrategyID)
}
