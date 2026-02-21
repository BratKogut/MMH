package intel

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// triggerCollector captures triggers fired by the engine.
type triggerCollector struct {
	mu       sync.Mutex
	triggers []Trigger
}

func (c *triggerCollector) handler(_ context.Context, t Trigger) (*IntelEvent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.triggers = append(c.triggers, t)
	return &IntelEvent{EventID: "test-event"}, nil
}

func (c *triggerCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.triggers)
}

func (c *triggerCollector) last() Trigger {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.triggers[len(c.triggers)-1]
}

// ---------------------------------------------------------------------------
// TestTriggerEngine_VolatilitySpike
// ---------------------------------------------------------------------------

func TestTriggerEngine_VolatilitySpike(t *testing.T) {
	collector := &triggerCollector{}
	config := DefaultTriggerEngineConfig()
	config.VolWindowSize = 10
	config.VolSpikeThreshold = 2.0

	engine := NewTriggerEngine(config, collector.handler)
	ctx := context.Background()

	// Feed normal volatility (around 0.01).
	for i := 0; i < 10; i++ {
		engine.OnVolatility(ctx, "BTC-USDT", 0.01)
	}
	assert.Equal(t, 0, collector.count(), "Normal vol should not trigger")

	// Feed a spike (3x normal).
	engine.OnVolatility(ctx, "BTC-USDT", 0.03)
	assert.Equal(t, 1, collector.count(), "Vol spike should trigger")

	trigger := collector.last()
	assert.Equal(t, TriggerMarketAnomaly, trigger.Type)
	assert.Equal(t, TopicVolatilitySpike, trigger.Topic)
	assert.Equal(t, []string{"BTC-USDT"}, trigger.AssetTags)
	assert.Equal(t, PriorityP0, trigger.Priority)
	assert.Contains(t, trigger.Source, "vol_spike")
}

// ---------------------------------------------------------------------------
// TestTriggerEngine_VolSpike_Cooldown
// ---------------------------------------------------------------------------

func TestTriggerEngine_VolSpike_Cooldown(t *testing.T) {
	collector := &triggerCollector{}
	config := DefaultTriggerEngineConfig()
	config.VolWindowSize = 10
	config.VolSpikeThreshold = 2.0

	engine := NewTriggerEngine(config, collector.handler)
	engine.cooldownMs = 100 // short cooldown for testing
	ctx := context.Background()

	// Fill rolling window with normal vol.
	for i := 0; i < 10; i++ {
		engine.OnVolatility(ctx, "BTC-USDT", 0.01)
	}

	// First spike triggers.
	engine.OnVolatility(ctx, "BTC-USDT", 0.03)
	assert.Equal(t, 1, collector.count())

	// Immediate second spike should be cooldown-suppressed.
	engine.OnVolatility(ctx, "BTC-USDT", 0.03)
	assert.Equal(t, 1, collector.count(), "Second spike within cooldown should be suppressed")

	// Wait for cooldown.
	time.Sleep(150 * time.Millisecond)

	// Reset the window with fresh normal data to get a clean mean.
	for i := 0; i < 10; i++ {
		engine.OnVolatility(ctx, "BTC-USDT", 0.01)
	}
	// Now spike again - mean is back to ~0.01, spike at 0.03 is 3x.
	engine.OnVolatility(ctx, "BTC-USDT", 0.03)
	assert.Equal(t, 2, collector.count(), "After cooldown, new spike should trigger")
}

// ---------------------------------------------------------------------------
// TestTriggerEngine_PriceGap
// ---------------------------------------------------------------------------

func TestTriggerEngine_PriceGap(t *testing.T) {
	collector := &triggerCollector{}
	config := DefaultTriggerEngineConfig()
	config.GapThresholdPct = 2.0

	engine := NewTriggerEngine(config, collector.handler)
	ctx := context.Background()

	// First price establishes baseline.
	engine.OnPrice(ctx, "ETH-USDT", 3000.0)
	assert.Equal(t, 0, collector.count())

	// Small move: 0.5%.
	engine.OnPrice(ctx, "ETH-USDT", 3015.0)
	assert.Equal(t, 0, collector.count(), "Small price move should not trigger")

	// Large gap: > 2%.
	engine.OnPrice(ctx, "ETH-USDT", 3100.0)
	assert.Equal(t, 1, collector.count(), "2.8% gap should trigger")

	trigger := collector.last()
	assert.Equal(t, TriggerMarketAnomaly, trigger.Type)
	assert.Contains(t, trigger.Prompt, "Price gap")
	assert.Contains(t, trigger.Source, "price_gap")
}

// ---------------------------------------------------------------------------
// TestTriggerEngine_PriceGap_Down
// ---------------------------------------------------------------------------

func TestTriggerEngine_PriceGap_Down(t *testing.T) {
	collector := &triggerCollector{}
	config := DefaultTriggerEngineConfig()
	config.GapThresholdPct = 3.0

	engine := NewTriggerEngine(config, collector.handler)
	ctx := context.Background()

	engine.OnPrice(ctx, "SOL-USDT", 100.0)
	engine.OnPrice(ctx, "SOL-USDT", 96.0) // 4% drop

	require.Equal(t, 1, collector.count())
	assert.Contains(t, collector.last().Prompt, "down")
}

// ---------------------------------------------------------------------------
// TestTriggerEngine_Schedule
// ---------------------------------------------------------------------------

func TestTriggerEngine_Schedule(t *testing.T) {
	collector := &triggerCollector{}
	config := DefaultTriggerEngineConfig()
	config.ScheduleCheckIntervalMs = 10 // very fast for testing

	engine := NewTriggerEngine(config, collector.handler)

	// Add an event that fires immediately.
	engine.AddScheduledEvent(ScheduledEvent{
		Name:      "FOMC Decision",
		Topic:     TopicMacroEvent,
		Time:      time.Now().Add(-1 * time.Second), // already past
		AssetTags: []string{"BTC-USDT", "ETH-USDT"},
		Priority:  PriorityP0,
		Prompt:    "FOMC just announced. Analyze market impact on crypto.",
	})

	// Add a future event that shouldn't fire yet.
	engine.AddScheduledEvent(ScheduledEvent{
		Name:      "CPI Release",
		Topic:     TopicMacroEvent,
		Time:      time.Now().Add(1 * time.Hour),
		AssetTags: []string{"BTC-USDT"},
		Priority:  PriorityP1,
		Prompt:    "CPI release imminent.",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	go engine.StartScheduler(ctx)
	time.Sleep(50 * time.Millisecond)

	assert.Equal(t, 1, collector.count(), "Only past event should fire")

	trigger := collector.last()
	assert.Equal(t, TriggerSchedule, trigger.Type)
	assert.Equal(t, TopicMacroEvent, trigger.Topic)
	assert.Contains(t, trigger.Source, "FOMC Decision")
	assert.Equal(t, PriorityP0, trigger.Priority)
}

// ---------------------------------------------------------------------------
// TestTriggerEngine_Cascade
// ---------------------------------------------------------------------------

func TestTriggerEngine_Cascade(t *testing.T) {
	collector := &triggerCollector{}
	config := DefaultTriggerEngineConfig()
	config.MaxCascadeDepth = 2

	engine := NewTriggerEngine(config, collector.handler)
	ctx := context.Background()

	parent := &IntelEvent{
		EventID:   "parent-001",
		Topic:     TopicRegulatoryAction,
		AssetTags: []string{"BTC-USDT"},
		Priority:  PriorityP1,
	}

	// Depth 0: should fire.
	engine.OnCascade(ctx, parent, "Deep dive on regulatory impact", 0)
	assert.Equal(t, 1, collector.count())

	// Depth 1: should fire.
	engine.OnCascade(ctx, parent, "Even deeper analysis", 1)
	assert.Equal(t, 2, collector.count())

	// Depth 2: should be blocked (MaxCascadeDepth=2).
	engine.OnCascade(ctx, parent, "Too deep", 2)
	assert.Equal(t, 2, collector.count(), "Cascade at max depth should be blocked")
}

// ---------------------------------------------------------------------------
// TestTriggerEngine_StrategyRequest
// ---------------------------------------------------------------------------

func TestTriggerEngine_StrategyRequest(t *testing.T) {
	collector := &triggerCollector{}
	config := DefaultTriggerEngineConfig()
	engine := NewTriggerEngine(config, collector.handler)
	ctx := context.Background()

	engine.OnStrategyRequest(ctx, "strat-alpha", TopicSentimentShift, []string{"BTC-USDT"}, "What is current BTC sentiment?")

	require.Equal(t, 1, collector.count())
	trigger := collector.last()
	assert.Equal(t, TriggerStrategyReq, trigger.Type)
	assert.Equal(t, TopicSentimentShift, trigger.Topic)
	assert.Contains(t, trigger.Source, "strat-alpha")
}

// ---------------------------------------------------------------------------
// TestTriggerEngine_Stats
// ---------------------------------------------------------------------------

func TestTriggerEngine_Stats(t *testing.T) {
	collector := &triggerCollector{}
	config := DefaultTriggerEngineConfig()
	config.VolWindowSize = 5
	config.VolSpikeThreshold = 2.0

	engine := NewTriggerEngine(config, collector.handler)
	ctx := context.Background()

	// Feed data for 2 symbols.
	for i := 0; i < 5; i++ {
		engine.OnVolatility(ctx, "BTC-USDT", 0.01)
		engine.OnVolatility(ctx, "ETH-USDT", 0.02)
	}

	// One spike.
	engine.OnVolatility(ctx, "BTC-USDT", 0.05)

	stats := engine.Stats()
	assert.Equal(t, 2, stats.SymbolsTracked)
	assert.Equal(t, int64(1), stats.TriggersEmitted)
	assert.Equal(t, int64(1), stats.AnomaliesFound)
}

// ---------------------------------------------------------------------------
// TestTriggerEngine_MultiSymbol_Independent
// ---------------------------------------------------------------------------

func TestTriggerEngine_MultiSymbol_Independent(t *testing.T) {
	collector := &triggerCollector{}
	config := DefaultTriggerEngineConfig()
	config.VolWindowSize = 5
	config.VolSpikeThreshold = 2.0

	engine := NewTriggerEngine(config, collector.handler)
	ctx := context.Background()

	// Build up BTC history.
	for i := 0; i < 5; i++ {
		engine.OnVolatility(ctx, "BTC-USDT", 0.01)
	}
	// ETH has its own history with higher baseline vol.
	for i := 0; i < 5; i++ {
		engine.OnVolatility(ctx, "ETH-USDT", 0.05)
	}

	// BTC spike at 0.03 (3x its normal 0.01) should trigger.
	engine.OnVolatility(ctx, "BTC-USDT", 0.03)
	// ETH at 0.06 is only 1.2x its normal 0.05 -- should NOT trigger.
	engine.OnVolatility(ctx, "ETH-USDT", 0.06)

	assert.Equal(t, 1, collector.count())
	assert.Equal(t, []string{"BTC-USDT"}, collector.last().AssetTags)
}
