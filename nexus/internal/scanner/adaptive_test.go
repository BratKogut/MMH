package scanner

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestDefaultAdaptiveWeightConfig(t *testing.T) {
	cfg := DefaultAdaptiveWeightConfig()
	assert.True(t, cfg.Enabled)
	assert.Equal(t, 0.05, cfg.LearningRate)
	assert.Equal(t, 0.05, cfg.MinWeight)
	assert.Equal(t, 0.50, cfg.MaxWeight)
	assert.Equal(t, 50, cfg.WindowSize)
	assert.Equal(t, 10, cfg.MinSamplesForAdjust)
}

func TestNewAdaptiveWeightEngine(t *testing.T) {
	baseline := DefaultWeights()
	e := NewAdaptiveWeightEngine(DefaultAdaptiveWeightConfig(), baseline)
	assert.NotNil(t, e)

	weights := e.GetWeights()
	assert.Equal(t, baseline.Safety, weights.Safety)
	assert.Equal(t, baseline.Entity, weights.Entity)
}

func TestRecordOutcome(t *testing.T) {
	e := NewAdaptiveWeightEngine(DefaultAdaptiveWeightConfig(), DefaultWeights())

	e.RecordOutcome(TradeOutcome{
		Score: TokenScore{Safety: 80, Entity: 60, Social: 50, OnChain: 70, Timing: 65},
		PnLPct: 150,
		IsWin:  true,
	})

	stats := e.Stats()
	assert.Equal(t, 1, stats.SampleCount)
	assert.Equal(t, 1.0, stats.WinRate)
}

func TestRecordOutcome_RingBuffer(t *testing.T) {
	cfg := DefaultAdaptiveWeightConfig()
	cfg.WindowSize = 3
	e := NewAdaptiveWeightEngine(cfg, DefaultWeights())

	for i := 0; i < 5; i++ {
		e.RecordOutcome(TradeOutcome{
			Score: TokenScore{Safety: float64(i * 20)},
			IsWin: i%2 == 0,
		})
	}

	stats := e.Stats()
	assert.Equal(t, 3, stats.SampleCount)
}

func TestRecalculate_NotEnoughSamples(t *testing.T) {
	cfg := DefaultAdaptiveWeightConfig()
	cfg.MinSamplesForAdjust = 5
	cfg.RecalcIntervalMin = 0 // no rate limit for test
	e := NewAdaptiveWeightEngine(cfg, DefaultWeights())

	// Only 2 samples, should not adjust.
	e.RecordOutcome(TradeOutcome{Score: TokenScore{Safety: 80}, IsWin: true})
	e.RecordOutcome(TradeOutcome{Score: TokenScore{Safety: 20}, IsWin: false})

	original := e.GetWeights()
	e.Recalculate()
	after := e.GetWeights()

	assert.Equal(t, original.Safety, after.Safety)
}

func TestRecalculate_AdjustsWeights(t *testing.T) {
	cfg := DefaultAdaptiveWeightConfig()
	cfg.MinSamplesForAdjust = 3
	cfg.RecalcIntervalMin = 0
	e := NewAdaptiveWeightEngine(cfg, DefaultWeights())

	// Create pattern: wins have high safety, losses have low safety.
	for i := 0; i < 5; i++ {
		e.RecordOutcome(TradeOutcome{
			Score: TokenScore{Safety: 90, Entity: 50, Social: 50, OnChain: 50, Timing: 50},
			IsWin: true,
		})
	}
	for i := 0; i < 5; i++ {
		e.RecordOutcome(TradeOutcome{
			Score: TokenScore{Safety: 20, Entity: 50, Social: 50, OnChain: 50, Timing: 50},
			IsWin: false,
		})
	}

	e.lastCalc = time.Time{} // reset rate limit
	e.Recalculate()

	weights := e.GetWeights()
	// Safety should have higher weight since it predicts wins.
	// Other dimensions should have lower weights since they're equal for wins/losses.
	assert.True(t, weights.Safety > weights.Social, "safety=%f should be > social=%f", weights.Safety, weights.Social)
}

func TestRecalculate_NormalizesToOne(t *testing.T) {
	cfg := DefaultAdaptiveWeightConfig()
	cfg.MinSamplesForAdjust = 3
	cfg.RecalcIntervalMin = 0
	e := NewAdaptiveWeightEngine(cfg, DefaultWeights())

	for i := 0; i < 10; i++ {
		e.RecordOutcome(TradeOutcome{
			Score: TokenScore{Safety: 80, Entity: 60, Social: 40, OnChain: 70, Timing: 30},
			IsWin: i%2 == 0,
		})
	}

	e.lastCalc = time.Time{}
	e.Recalculate()

	weights := e.GetWeights()
	total := weights.Safety + weights.Entity + weights.Social + weights.OnChain + weights.Timing
	assert.InDelta(t, 1.0, total, 0.001)
}

func TestRecalculate_RateLimit(t *testing.T) {
	cfg := DefaultAdaptiveWeightConfig()
	cfg.MinSamplesForAdjust = 3
	cfg.RecalcIntervalMin = 60 // 60 min rate limit
	e := NewAdaptiveWeightEngine(cfg, DefaultWeights())

	for i := 0; i < 10; i++ {
		e.RecordOutcome(TradeOutcome{
			Score: TokenScore{Safety: 80},
			IsWin: true,
		})
	}

	original := e.GetWeights()
	e.Recalculate() // should be rate-limited (lastCalc was set to now)
	after := e.GetWeights()

	assert.Equal(t, original.Safety, after.Safety)
}

func TestRecalculate_Disabled(t *testing.T) {
	cfg := DefaultAdaptiveWeightConfig()
	cfg.Enabled = false
	e := NewAdaptiveWeightEngine(cfg, DefaultWeights())

	for i := 0; i < 20; i++ {
		e.RecordOutcome(TradeOutcome{
			Score: TokenScore{Safety: 90},
			IsWin: true,
		})
	}

	original := e.GetWeights()
	e.lastCalc = time.Time{}
	e.Recalculate()
	after := e.GetWeights()

	assert.Equal(t, original.Safety, after.Safety)
}

func TestReset(t *testing.T) {
	cfg := DefaultAdaptiveWeightConfig()
	cfg.MinSamplesForAdjust = 3
	cfg.RecalcIntervalMin = 0
	baseline := DefaultWeights()
	e := NewAdaptiveWeightEngine(cfg, baseline)

	for i := 0; i < 10; i++ {
		e.RecordOutcome(TradeOutcome{
			Score: TokenScore{Safety: 90},
			IsWin: true,
		})
	}

	e.lastCalc = time.Time{}
	e.Recalculate()
	e.Reset()

	weights := e.GetWeights()
	assert.Equal(t, baseline.Safety, weights.Safety)
	assert.Equal(t, baseline.Entity, weights.Entity)

	stats := e.Stats()
	assert.Equal(t, 0, stats.SampleCount)
}

func TestStats(t *testing.T) {
	e := NewAdaptiveWeightEngine(DefaultAdaptiveWeightConfig(), DefaultWeights())

	e.RecordOutcome(TradeOutcome{IsWin: true})
	e.RecordOutcome(TradeOutcome{IsWin: true})
	e.RecordOutcome(TradeOutcome{IsWin: false})

	stats := e.Stats()
	assert.Equal(t, 3, stats.SampleCount)
	assert.InDelta(t, 0.667, stats.WinRate, 0.01)
	assert.True(t, stats.Enabled)
}

func TestDimensionCorrelation_AllWins(t *testing.T) {
	e := NewAdaptiveWeightEngine(DefaultAdaptiveWeightConfig(), DefaultWeights())

	// All wins with same score → correlation = 0 (no contrast).
	for i := 0; i < 5; i++ {
		e.RecordOutcome(TradeOutcome{
			Score: TokenScore{Safety: 80},
			IsWin: true,
		})
	}

	corr := e.dimensionCorrelation(func(o TradeOutcome) float64 { return o.Score.Safety })
	assert.Equal(t, 0.0, corr) // no losses to compare
}

func TestDimensionCorrelation_ClearSignal(t *testing.T) {
	e := NewAdaptiveWeightEngine(DefaultAdaptiveWeightConfig(), DefaultWeights())

	// Wins have high safety, losses have low safety.
	for i := 0; i < 5; i++ {
		e.RecordOutcome(TradeOutcome{
			Score: TokenScore{Safety: 90},
			IsWin: true,
		})
		e.RecordOutcome(TradeOutcome{
			Score: TokenScore{Safety: 10},
			IsWin: false,
		})
	}

	corr := e.dimensionCorrelation(func(o TradeOutcome) float64 { return o.Score.Safety })
	assert.True(t, corr > 0, "expected positive correlation, got %f", corr)
}
