package liquidity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNewFlowAnalyzer(t *testing.T) {
	a := NewFlowAnalyzer(DefaultFlowAnalyzerConfig())
	assert.NotNil(t, a)
	assert.NotNil(t, a.samples)
}

func TestFlowDirection_String(t *testing.T) {
	assert.Equal(t, "STABLE", FlowStable.String())
	assert.Equal(t, "INFLOW", FlowInflow.String())
	assert.Equal(t, "OUTFLOW", FlowOutflow.String())
}

func TestVelocity_String(t *testing.T) {
	assert.Equal(t, "STABLE", VelocityStable.String())
	assert.Equal(t, "ACCELERATING", VelocityAccelerating.String())
	assert.Equal(t, "DECELERATING", VelocityDecelerating.String())
}

func TestSourceQuality_String(t *testing.T) {
	assert.Equal(t, "UNKNOWN", QualityUnknown.String())
	assert.Equal(t, "HIGH", QualityHigh.String())
	assert.Equal(t, "MEDIUM", QualityMedium.String())
	assert.Equal(t, "LOW", QualityLow.String())
}

func TestRecordLiquidityChange(t *testing.T) {
	a := NewFlowAnalyzer(DefaultFlowAnalyzerConfig())

	a.RecordLiquidityChange("token1", 1000, true, QualityHigh)
	a.RecordLiquidityChange("token1", 500, false, QualityMedium)
	a.RecordLiquidityChange("token2", 2000, true, QualityLow)

	stats := a.Stats()
	assert.Equal(t, 2, stats.TrackedTokens)
	assert.Equal(t, 3, stats.TotalSamples)
}

func TestRecordLiquidityChange_RingBuffer(t *testing.T) {
	cfg := DefaultFlowAnalyzerConfig()
	cfg.MaxSamplesPerPool = 3
	a := NewFlowAnalyzer(cfg)

	for i := 0; i < 5; i++ {
		a.RecordLiquidityChange("token1", float64(i*100), true, QualityHigh)
	}

	a.mu.RLock()
	assert.Equal(t, 3, len(a.samples["token1"]))
	a.mu.RUnlock()
}

func TestAnalyze_EmptyPool(t *testing.T) {
	a := NewFlowAnalyzer(DefaultFlowAnalyzerConfig())

	flow := a.Analyze("nonexistent")
	assert.Equal(t, "nonexistent", flow.TokenAddress)
	assert.Equal(t, FlowStable, flow.Direction)
	assert.Equal(t, PatternNormal, flow.Pattern)
}

func TestAnalyze_Inflow(t *testing.T) {
	cfg := DefaultFlowAnalyzerConfig()
	cfg.StableThreshUSD = 100
	a := NewFlowAnalyzer(cfg)

	// Add significant inflow from high quality sources.
	a.RecordLiquidityChange("token1", 5000, true, QualityHigh)
	a.RecordLiquidityChange("token1", 3000, true, QualityHigh)

	flow := a.Analyze("token1")
	assert.Equal(t, FlowInflow, flow.Direction)
	assert.True(t, flow.NetFlow5min > 0)
	assert.Equal(t, 2, flow.LPMintCount)
	assert.Equal(t, 0, flow.LPBurnCount)
}

func TestAnalyze_Outflow(t *testing.T) {
	cfg := DefaultFlowAnalyzerConfig()
	cfg.StableThreshUSD = 100
	a := NewFlowAnalyzer(cfg)

	// Remove liquidity.
	a.RecordLiquidityChange("token1", 5000, false, QualityMedium)
	a.RecordLiquidityChange("token1", 3000, false, QualityMedium)

	flow := a.Analyze("token1")
	assert.Equal(t, FlowOutflow, flow.Direction)
	assert.True(t, flow.NetFlow5min < 0)
	assert.Equal(t, 0, flow.LPMintCount)
	assert.Equal(t, 2, flow.LPBurnCount)
}

func TestAnalyze_HealthyGrowthPattern(t *testing.T) {
	cfg := DefaultFlowAnalyzerConfig()
	cfg.StableThreshUSD = 100
	a := NewFlowAnalyzer(cfg)

	a.RecordLiquidityChange("token1", 5000, true, QualityHigh)

	flow := a.Analyze("token1")
	assert.Equal(t, PatternHealthyGrowth, flow.Pattern)
}

func TestAnalyze_ArtificialPumpPattern(t *testing.T) {
	cfg := DefaultFlowAnalyzerConfig()
	cfg.StableThreshUSD = 100
	a := NewFlowAnalyzer(cfg)

	// Single large inflow from low quality source.
	a.RecordLiquidityChange("token1", 10000, true, QualityLow)

	flow := a.Analyze("token1")
	assert.Equal(t, PatternArtificialPump, flow.Pattern)
}

func TestAnalyze_RugPrecursorPattern(t *testing.T) {
	cfg := DefaultFlowAnalyzerConfig()
	cfg.StableThreshUSD = 100
	cfg.ShortWindowMin = 5
	cfg.LongWindowMin = 30
	a := NewFlowAnalyzer(cfg)

	// Need accelerating outflow: short rate > long rate * 1.5.
	// We'll add recent large outflows to create accelerating pattern.
	// Since all samples are recent (within 5min), both windows should have same data.
	// To get acceleration, we need short rate >> long rate, which means
	// recent outflows are much larger than older ones.
	// Add old outflows (30min ago) - manually inject into samples map.
	a.mu.Lock()
	oldTime := time.Now().Add(-20 * time.Minute)
	a.samples["token1"] = []flowSample{
		{amountUSD: 100, isAdd: false, sourceQuality: QualityLow, timestamp: oldTime},
		{amountUSD: 5000, isAdd: false, sourceQuality: QualityLow, timestamp: time.Now()},
		{amountUSD: 5000, isAdd: false, sourceQuality: QualityLow, timestamp: time.Now()},
	}
	a.mu.Unlock()

	flow := a.Analyze("token1")
	assert.Equal(t, FlowOutflow, flow.Direction)
	// shortRate = -10000/5 = -2000, longRate = -10100/30 = -336
	// abs(shortRate)=2000 > abs(longRate)*1.5 = 504 → accelerating
	assert.Equal(t, VelocityAccelerating, flow.Velocity)
	assert.Equal(t, PatternRugPrecursor, flow.Pattern)
}

func TestScoreImpact(t *testing.T) {
	tests := []struct {
		pattern  FlowPattern
		velocity Velocity
		expected float64
	}{
		{PatternRugPrecursor, VelocityStable, -100},
		{PatternArtificialPump, VelocityStable, -15},
		{PatternSlowBleed, VelocityStable, -10},
		{PatternHealthyGrowth, VelocityAccelerating, 15},
		{PatternHealthyGrowth, VelocityStable, 5},
		{PatternNormal, VelocityStable, 0},
	}

	for _, tt := range tests {
		flow := &LiquidityFlow{Pattern: tt.pattern, Velocity: tt.velocity}
		assert.Equal(t, tt.expected, flow.ScoreImpact(), "pattern=%s velocity=%s", tt.pattern, tt.velocity)
	}
}

func TestIsRugPrecursor(t *testing.T) {
	flow := &LiquidityFlow{Pattern: PatternRugPrecursor}
	assert.True(t, flow.IsRugPrecursor())

	flow2 := &LiquidityFlow{Pattern: PatternNormal}
	assert.False(t, flow2.IsRugPrecursor())
}

func TestCleanup(t *testing.T) {
	a := NewFlowAnalyzer(DefaultFlowAnalyzerConfig())

	// Add a token with old samples.
	a.mu.Lock()
	a.samples["old_token"] = []flowSample{
		{amountUSD: 100, isAdd: true, timestamp: time.Now().Add(-2 * time.Hour)},
	}
	a.samples["fresh_token"] = []flowSample{
		{amountUSD: 200, isAdd: true, timestamp: time.Now()},
	}
	a.mu.Unlock()

	removed := a.Cleanup(1 * time.Hour)
	assert.Equal(t, 1, removed)

	stats := a.Stats()
	assert.Equal(t, 1, stats.TrackedTokens)
}

func TestDefaultFlowAnalyzerConfig(t *testing.T) {
	cfg := DefaultFlowAnalyzerConfig()
	assert.Equal(t, 5, cfg.ShortWindowMin)
	assert.Equal(t, 30, cfg.LongWindowMin)
	assert.Equal(t, 500.0, cfg.StableThreshUSD)
	assert.Equal(t, 500, cfg.MaxSamplesPerPool)
}

func TestAbs(t *testing.T) {
	assert.Equal(t, 5.0, abs(5.0))
	assert.Equal(t, 5.0, abs(-5.0))
	assert.Equal(t, 0.0, abs(0.0))
}
