package scanner

import (
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// ---------------------------------------------------------------------------
// Adaptive Scoring Weights
// v3.2: Dynamically adjust 5D scoring weights based on recent performance.
// If a dimension consistently predicts winners, increase its weight.
// If a dimension fails to predict rugs, decrease its weight.
// ---------------------------------------------------------------------------

// AdaptiveWeightConfig configures the adaptive weight engine.
type AdaptiveWeightConfig struct {
	Enabled             bool    `yaml:"enabled"`
	LearningRate        float64 `yaml:"learning_rate"`         // how fast weights adjust (0.01-0.1)
	MinWeight           float64 `yaml:"min_weight"`            // minimum weight for any dimension
	MaxWeight           float64 `yaml:"max_weight"`            // maximum weight for any dimension
	WindowSize          int     `yaml:"window_size"`           // number of trades to consider
	RecalcIntervalMin   int     `yaml:"recalc_interval_min"`   // recalculate every N minutes
	MinSamplesForAdjust int     `yaml:"min_samples_for_adjust"` // minimum trades before adjusting
}

// DefaultAdaptiveWeightConfig returns defaults.
func DefaultAdaptiveWeightConfig() AdaptiveWeightConfig {
	return AdaptiveWeightConfig{
		Enabled:             true,
		LearningRate:        0.05,
		MinWeight:           0.05,
		MaxWeight:           0.50,
		WindowSize:          50,
		RecalcIntervalMin:   30,
		MinSamplesForAdjust: 10,
	}
}

// TradeOutcome records the outcome of a scored trade for weight learning.
type TradeOutcome struct {
	Score     TokenScore `json:"score"`
	PnLPct   float64    `json:"pnl_pct"`   // realized PnL (positive = win)
	IsWin    bool       `json:"is_win"`
	IsRug    bool       `json:"is_rug"`     // was it a rug pull?
	Duration time.Duration `json:"duration"` // how long position was held
	RecordedAt time.Time `json:"recorded_at"`
}

// AdaptiveWeightEngine dynamically adjusts scoring weights based on performance.
type AdaptiveWeightEngine struct {
	config   AdaptiveWeightConfig
	mu       sync.RWMutex
	outcomes []TradeOutcome // ring buffer
	current  ScoringWeights // current adaptive weights
	baseline ScoringWeights // original weights (for drift limits)
	lastCalc time.Time
}

// NewAdaptiveWeightEngine creates a new adaptive weight engine.
func NewAdaptiveWeightEngine(config AdaptiveWeightConfig, baseline ScoringWeights) *AdaptiveWeightEngine {
	return &AdaptiveWeightEngine{
		config:   config,
		current:  baseline,
		baseline: baseline,
		lastCalc: time.Now(),
	}
}

// RecordOutcome records a trade outcome for weight learning.
func (e *AdaptiveWeightEngine) RecordOutcome(outcome TradeOutcome) {
	e.mu.Lock()
	defer e.mu.Unlock()

	outcome.RecordedAt = time.Now()

	// Ring buffer.
	if len(e.outcomes) >= e.config.WindowSize {
		e.outcomes = e.outcomes[1:]
	}
	e.outcomes = append(e.outcomes, outcome)
}

// GetWeights returns the current adaptive weights.
func (e *AdaptiveWeightEngine) GetWeights() ScoringWeights {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.current
}

// Recalculate adjusts weights based on recent trade outcomes.
// Should be called periodically (every RecalcIntervalMin minutes).
func (e *AdaptiveWeightEngine) Recalculate() {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.config.Enabled {
		return
	}

	if len(e.outcomes) < e.config.MinSamplesForAdjust {
		return
	}

	// Rate limit.
	if time.Since(e.lastCalc) < time.Duration(e.config.RecalcIntervalMin)*time.Minute {
		return
	}
	e.lastCalc = time.Now()

	// Calculate dimension-outcome correlations.
	// For each dimension, compute correlation between score and outcome.
	safetyCorr := e.dimensionCorrelation(func(o TradeOutcome) float64 { return o.Score.Safety })
	entityCorr := e.dimensionCorrelation(func(o TradeOutcome) float64 { return o.Score.Entity })
	socialCorr := e.dimensionCorrelation(func(o TradeOutcome) float64 { return o.Score.Social })
	onchainCorr := e.dimensionCorrelation(func(o TradeOutcome) float64 { return o.Score.OnChain })
	timingCorr := e.dimensionCorrelation(func(o TradeOutcome) float64 { return o.Score.Timing })

	// Adjust weights based on correlation.
	lr := e.config.LearningRate
	e.current.Safety = e.adjustWeight(e.current.Safety, safetyCorr, lr)
	e.current.Entity = e.adjustWeight(e.current.Entity, entityCorr, lr)
	e.current.Social = e.adjustWeight(e.current.Social, socialCorr, lr)
	e.current.OnChain = e.adjustWeight(e.current.OnChain, onchainCorr, lr)
	e.current.Timing = e.adjustWeight(e.current.Timing, timingCorr, lr)

	// Normalize to sum to 1.0.
	e.normalize()

	log.Info().
		Float64("safety", e.current.Safety).
		Float64("entity", e.current.Entity).
		Float64("social", e.current.Social).
		Float64("onchain", e.current.OnChain).
		Float64("timing", e.current.Timing).
		Int("samples", len(e.outcomes)).
		Msg("adaptive_weights: recalculated")
}

// dimensionCorrelation computes how well a dimension score predicts winning trades.
// Returns value in [-1, 1]: positive = higher score correlates with wins.
func (e *AdaptiveWeightEngine) dimensionCorrelation(dimFn func(TradeOutcome) float64) float64 {
	if len(e.outcomes) < 2 {
		return 0
	}

	// Simple approach: compare average dimension score for wins vs losses.
	var winSum, lossSum float64
	var winCount, lossCount int

	for _, o := range e.outcomes {
		val := dimFn(o)
		if o.IsWin {
			winSum += val
			winCount++
		} else {
			lossSum += val
			lossCount++
		}
	}

	if winCount == 0 || lossCount == 0 {
		return 0
	}

	winAvg := winSum / float64(winCount)
	lossAvg := lossSum / float64(lossCount)

	// Normalize difference to [-1, 1].
	diff := (winAvg - lossAvg) / 100.0
	if diff > 1 {
		diff = 1
	}
	if diff < -1 {
		diff = -1
	}
	return diff
}

// adjustWeight adjusts a weight based on correlation.
func (e *AdaptiveWeightEngine) adjustWeight(current, correlation, lr float64) float64 {
	// Positive correlation → increase weight. Negative → decrease.
	adjusted := current + (correlation * lr)

	// Clamp.
	if adjusted < e.config.MinWeight {
		adjusted = e.config.MinWeight
	}
	if adjusted > e.config.MaxWeight {
		adjusted = e.config.MaxWeight
	}
	return adjusted
}

// normalize ensures weights sum to 1.0.
func (e *AdaptiveWeightEngine) normalize() {
	total := e.current.Safety + e.current.Entity + e.current.Social +
		e.current.OnChain + e.current.Timing

	if total == 0 {
		e.current = e.baseline
		return
	}

	e.current.Safety /= total
	e.current.Entity /= total
	e.current.Social /= total
	e.current.OnChain /= total
	e.current.Timing /= total
}

// Reset restores baseline weights.
func (e *AdaptiveWeightEngine) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.current = e.baseline
	e.outcomes = nil
	log.Info().Msg("adaptive_weights: reset to baseline")
}

// Stats returns adaptive weight statistics.
type AdaptiveStats struct {
	CurrentWeights ScoringWeights `json:"current_weights"`
	BaselineWeights ScoringWeights `json:"baseline_weights"`
	SampleCount    int             `json:"sample_count"`
	WinRate        float64         `json:"win_rate"`
	LastRecalc     time.Time       `json:"last_recalc"`
	Enabled        bool            `json:"enabled"`
}

func (e *AdaptiveWeightEngine) Stats() AdaptiveStats {
	e.mu.RLock()
	defer e.mu.RUnlock()

	winCount := 0
	for _, o := range e.outcomes {
		if o.IsWin {
			winCount++
		}
	}

	winRate := 0.0
	if len(e.outcomes) > 0 {
		winRate = float64(winCount) / float64(len(e.outcomes))
	}

	return AdaptiveStats{
		CurrentWeights:  e.current,
		BaselineWeights: e.baseline,
		SampleCount:     len(e.outcomes),
		WinRate:         winRate,
		LastRecalc:      e.lastCalc,
		Enabled:         e.config.Enabled,
	}
}
