package regime

import (
	"math"
	"sync"

	"github.com/nexus-trading/nexus/internal/bus"
)

// Regime represents a market regime classification.
type Regime string

const (
	RegimeTrendingUp    Regime = "TRENDING_UP"
	RegimeTrendingDown  Regime = "TRENDING_DOWN"
	RegimeMeanReverting Regime = "MEAN_REVERTING"
	RegimeHighVol       Regime = "HIGH_VOLATILITY"
	RegimeLowVol        Regime = "LOW_VOLATILITY"
	RegimeBreakout      Regime = "BREAKOUT"
	RegimeUnknown       Regime = "UNKNOWN"
)

// RegimeConfig holds thresholds for the threshold-based regime detector.
type RegimeConfig struct {
	// MomentumThresholdUp: momentum above this value classifies as TRENDING_UP.
	MomentumThresholdUp float64
	// MomentumThresholdDown: momentum below this (negative) value classifies as TRENDING_DOWN.
	MomentumThresholdDown float64
	// VolatilityHighPct: volatility above this percentile classifies as HIGH_VOLATILITY.
	VolatilityHighPct float64
	// VolatilityLowPct: volatility below this percentile classifies as LOW_VOLATILITY.
	VolatilityLowPct float64
	// BreakoutMomentumMult: if current |momentum| > BreakoutMomentumMult * avg(|momentum_hist|),
	// and imbalance exceeds BreakoutImbalanceMin, classify as BREAKOUT.
	BreakoutMomentumMult float64
	// BreakoutImbalanceMin: minimum absolute imbalance to confirm breakout.
	BreakoutImbalanceMin float64
	// MinSamples: minimum number of Update calls before classifying (returns UNKNOWN before this).
	MinSamples int
	// MomentumHistSize: number of historical momentum values to keep for breakout detection.
	MomentumHistSize int
}

// DefaultConfig returns a sensible default configuration.
func DefaultConfig() RegimeConfig {
	return RegimeConfig{
		MomentumThresholdUp:   0.001,  // 0.1% positive ROC
		MomentumThresholdDown: -0.001, // -0.1% negative ROC
		VolatilityHighPct:     0.02,   // 2% volatility is high
		VolatilityLowPct:      0.005,  // 0.5% volatility is low
		BreakoutMomentumMult:  3.0,    // 3x average momentum
		BreakoutImbalanceMin:  0.3,    // 30% order imbalance
		MinSamples:            10,
		MomentumHistSize:      20,
	}
}

// symbolState tracks per-symbol state for regime detection.
type symbolState struct {
	regime        Regime
	confidence    float64
	lastMomentum  float64
	lastVolatility float64
	lastImbalance float64
	sampleCount   int
	momentumHist  []float64 // rolling window of recent momentum values
	histHead      int       // write position in circular buffer
	histCount     int       // valid entries in the buffer
}

// Detector classifies the current market regime for each symbol using
// threshold-based rules applied to feature values from the Feature Engine.
type Detector struct {
	config RegimeConfig
	mu     sync.RWMutex
	states map[string]*symbolState
}

// NewDetector creates a new threshold-based regime detector.
func NewDetector(config RegimeConfig) *Detector {
	if config.MomentumHistSize <= 0 {
		config.MomentumHistSize = 20
	}
	if config.MinSamples <= 0 {
		config.MinSamples = 1
	}
	return &Detector{
		config: config,
		states: make(map[string]*symbolState),
	}
}

// Update processes a new set of feature values for a symbol and returns a
// RegimeUpdate event. The features map should contain keys such as "momentum",
// "volatility", "imbalance", and "spread" from the Feature Engine.
func (d *Detector) Update(symbol string, features map[string]float64) bus.RegimeUpdate {
	d.mu.Lock()
	defer d.mu.Unlock()

	s := d.getOrCreate(symbol)

	// Extract feature values.
	momentum := features["momentum"]
	volatility := features["volatility"]
	imbalance := features["imbalance"]

	// Update state.
	s.lastMomentum = momentum
	s.lastVolatility = volatility
	s.lastImbalance = imbalance
	s.sampleCount++

	// Record momentum in the history buffer for breakout detection.
	s.momentumHist[s.histHead] = momentum
	s.histHead = (s.histHead + 1) % d.config.MomentumHistSize
	if s.histCount < d.config.MomentumHistSize {
		s.histCount++
	}

	prevRegime := s.regime

	// If not enough samples, return UNKNOWN.
	if s.sampleCount < d.config.MinSamples {
		s.regime = RegimeUnknown
		s.confidence = 0.0
		return d.buildEvent(symbol, s, prevRegime)
	}

	// Classify the regime.
	newRegime, confidence := d.classify(s)
	s.regime = newRegime
	s.confidence = confidence

	return d.buildEvent(symbol, s, prevRegime)
}

// CurrentRegime returns the current regime and confidence for a symbol.
// Returns (UNKNOWN, 0.0) if the symbol has not been seen.
func (d *Detector) CurrentRegime(symbol string) (Regime, float64) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	s, ok := d.states[symbol]
	if !ok {
		return RegimeUnknown, 0.0
	}
	return s.regime, s.confidence
}

// classify applies threshold-based rules to determine the regime.
// Priority order: Breakout > HighVol/LowVol > Trending > MeanReverting > Unknown.
func (d *Detector) classify(s *symbolState) (Regime, float64) {
	momentum := s.lastMomentum
	volatility := s.lastVolatility
	imbalance := s.lastImbalance
	cfg := d.config

	// 1. Check for BREAKOUT: sudden momentum spike + volume/imbalance surge.
	if s.histCount >= 3 {
		avgAbsMomentum := d.avgAbsMomentum(s)
		absMomentum := math.Abs(momentum)

		if avgAbsMomentum > 0 && absMomentum > cfg.BreakoutMomentumMult*avgAbsMomentum &&
			math.Abs(imbalance) >= cfg.BreakoutImbalanceMin {
			// Confidence: how much the spike exceeds the threshold.
			ratio := absMomentum / (cfg.BreakoutMomentumMult * avgAbsMomentum)
			conf := clampConfidence(0.6 + 0.4*math.Min(ratio-1.0, 1.0))
			return RegimeBreakout, conf
		}
	}

	// 2. Check volatility extremes.
	if volatility > cfg.VolatilityHighPct {
		// High volatility takes priority over trend classification.
		excess := (volatility - cfg.VolatilityHighPct) / cfg.VolatilityHighPct
		conf := clampConfidence(0.5 + 0.5*math.Min(excess, 1.0))
		return RegimeHighVol, conf
	}
	if volatility < cfg.VolatilityLowPct && volatility >= 0 {
		deficit := (cfg.VolatilityLowPct - volatility) / cfg.VolatilityLowPct
		conf := clampConfidence(0.5 + 0.5*math.Min(deficit, 1.0))
		return RegimeLowVol, conf
	}

	// 3. Check trend (momentum) thresholds.
	if momentum > cfg.MomentumThresholdUp {
		excess := (momentum - cfg.MomentumThresholdUp) / math.Abs(cfg.MomentumThresholdUp)
		conf := clampConfidence(0.5 + 0.4*math.Min(excess, 1.0))
		return RegimeTrendingUp, conf
	}
	if momentum < cfg.MomentumThresholdDown {
		excess := (cfg.MomentumThresholdDown - momentum) / math.Abs(cfg.MomentumThresholdDown)
		conf := clampConfidence(0.5 + 0.4*math.Min(excess, 1.0))
		return RegimeTrendingDown, conf
	}

	// 4. If momentum is within thresholds and volatility is medium, it's mean-reverting.
	if math.Abs(momentum) <= math.Abs(cfg.MomentumThresholdUp) &&
		volatility >= cfg.VolatilityLowPct && volatility <= cfg.VolatilityHighPct {
		// Confidence based on how centered momentum is (closer to zero = more confident).
		momentumNorm := math.Abs(momentum) / math.Abs(cfg.MomentumThresholdUp)
		conf := clampConfidence(0.5 + 0.4*(1.0-momentumNorm))
		return RegimeMeanReverting, conf
	}

	return RegimeUnknown, 0.3
}

// avgAbsMomentum computes the average of |momentum| from history,
// excluding the most recent entry (to compare current vs. historical).
func (d *Detector) avgAbsMomentum(s *symbolState) float64 {
	if s.histCount < 2 {
		return 0
	}

	sum := 0.0
	count := 0
	// The most recent entry is at (histHead - 1). We skip it.
	for i := 0; i < s.histCount-1; i++ {
		idx := (s.histHead - 2 - i + d.config.MomentumHistSize) % d.config.MomentumHistSize
		sum += math.Abs(s.momentumHist[idx])
		count++
	}

	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

// buildEvent constructs a bus.RegimeUpdate event.
func (d *Detector) buildEvent(symbol string, s *symbolState, prevRegime Regime) bus.RegimeUpdate {
	ev := bus.RegimeUpdate{
		BaseEvent:  bus.NewBaseEvent("regime-detector", "1.0"),
		Symbol:     symbol,
		Regime:     string(s.regime),
		Confidence: s.confidence,
	}
	if prevRegime != s.regime && prevRegime != "" {
		ev.PrevRegime = string(prevRegime)
	}
	return ev
}

// getOrCreate returns the symbolState for a symbol, creating it if needed.
func (d *Detector) getOrCreate(symbol string) *symbolState {
	s, ok := d.states[symbol]
	if !ok {
		s = &symbolState{
			regime:       RegimeUnknown,
			momentumHist: make([]float64, d.config.MomentumHistSize),
		}
		d.states[symbol] = s
	}
	return s
}

// clampConfidence clamps a confidence value to [0, 1].
func clampConfidence(c float64) float64 {
	if c < 0 {
		return 0
	}
	if c > 1 {
		return 1
	}
	return c
}

// --- HMM-based Detector (research/secondary) ---

// HMMState represents a hidden state in the HMM.
type HMMState int

const (
	HMMTrending      HMMState = 0
	HMMMeanReverting HMMState = 1
	HMMHighVol       HMMState = 2
)

// HMMConfig holds the pre-trained parameters for a 3-state HMM.
type HMMConfig struct {
	// NumStates is the number of hidden states (fixed at 3).
	NumStates int
	// NumObservations is the number of discrete observation symbols.
	NumObservations int
	// Initial state probabilities [NumStates].
	InitialProb []float64
	// Transition matrix [NumStates][NumStates]: TransitionProb[i][j] = P(state_j | state_i).
	TransitionProb [][]float64
	// Emission matrix [NumStates][NumObservations]: EmissionProb[i][k] = P(obs_k | state_i).
	EmissionProb [][]float64
}

// DefaultHMMConfig returns a pre-trained HMM configuration with sensible defaults.
func DefaultHMMConfig() HMMConfig {
	return HMMConfig{
		NumStates:       3,
		NumObservations: 5,
		InitialProb:     []float64{0.4, 0.4, 0.2},
		TransitionProb: [][]float64{
			{0.7, 0.2, 0.1}, // Trending   -> {Trending, MeanRev, HighVol}
			{0.2, 0.7, 0.1}, // MeanRev    -> {Trending, MeanRev, HighVol}
			{0.15, 0.15, 0.7}, // HighVol  -> {Trending, MeanRev, HighVol}
		},
		EmissionProb: [][]float64{
			{0.4, 0.3, 0.1, 0.1, 0.1}, // Trending emits: {strong_up, mild_up, flat, mild_down, strong_down}
			{0.05, 0.2, 0.5, 0.2, 0.05}, // MeanRev emits
			{0.2, 0.1, 0.1, 0.1, 0.5}, // HighVol emits (biased toward extremes)
		},
	}
}

// HMMDetector performs Hidden Markov Model-based regime detection
// using Viterbi decoding.
type HMMDetector struct {
	config HMMConfig
	mu     sync.RWMutex
	states map[string]*hmmSymbolState
}

// hmmSymbolState tracks the HMM decoding state per symbol.
type hmmSymbolState struct {
	// viterbiProb holds the current Viterbi probabilities (log-space) for each state.
	viterbiProb []float64
	// bestState is the most likely current state.
	bestState HMMState
	// observations seen so far.
	obsCount int
}

// NewHMMDetector creates a new HMM-based regime detector.
func NewHMMDetector(config HMMConfig) *HMMDetector {
	return &HMMDetector{
		config: config,
		states: make(map[string]*hmmSymbolState),
	}
}

// Observe processes a new discretized observation for a symbol and returns
// the most likely current state via Viterbi decoding.
//
// observation is an integer in [0, NumObservations).
// Mapping convention:
//
//	0 = strong_up, 1 = mild_up, 2 = flat, 3 = mild_down, 4 = strong_down
func (h *HMMDetector) Observe(symbol string, observation int) (HMMState, float64) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if observation < 0 || observation >= h.config.NumObservations {
		return HMMTrending, 0.0
	}

	s := h.getOrCreate(symbol)
	cfg := h.config

	if s.obsCount == 0 {
		// Initialize with initial probabilities * emission.
		for i := 0; i < cfg.NumStates; i++ {
			s.viterbiProb[i] = math.Log(cfg.InitialProb[i]) + math.Log(cfg.EmissionProb[i][observation])
		}
	} else {
		// Viterbi step: for each state, find the max prev_state transition.
		newProb := make([]float64, cfg.NumStates)
		for j := 0; j < cfg.NumStates; j++ {
			maxVal := math.Inf(-1)
			for i := 0; i < cfg.NumStates; i++ {
				val := s.viterbiProb[i] + math.Log(cfg.TransitionProb[i][j])
				if val > maxVal {
					maxVal = val
				}
			}
			newProb[j] = maxVal + math.Log(cfg.EmissionProb[j][observation])
		}
		copy(s.viterbiProb, newProb)
	}

	s.obsCount++

	// Find the most likely state.
	bestState := 0
	bestProb := s.viterbiProb[0]
	for i := 1; i < cfg.NumStates; i++ {
		if s.viterbiProb[i] > bestProb {
			bestProb = s.viterbiProb[i]
			bestState = i
		}
	}
	s.bestState = HMMState(bestState)

	// Compute confidence as normalized probability (softmax of log probs).
	confidence := h.normalizedConfidence(s)

	return s.bestState, confidence
}

// CurrentState returns the current most-likely HMM state for a symbol.
func (h *HMMDetector) CurrentState(symbol string) (HMMState, float64) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	s, ok := h.states[symbol]
	if !ok || s.obsCount == 0 {
		return HMMTrending, 0.0
	}

	confidence := h.normalizedConfidence(s)
	return s.bestState, confidence
}

// StateToRegime converts an HMM state to a Regime string.
func StateToRegime(state HMMState) Regime {
	switch state {
	case HMMTrending:
		return RegimeTrendingUp // HMM does not distinguish direction
	case HMMMeanReverting:
		return RegimeMeanReverting
	case HMMHighVol:
		return RegimeHighVol
	default:
		return RegimeUnknown
	}
}

// normalizedConfidence computes a confidence value from log-space Viterbi probabilities
// using the log-sum-exp trick for numerical stability.
func (h *HMMDetector) normalizedConfidence(s *hmmSymbolState) float64 {
	probs := s.viterbiProb
	n := h.config.NumStates

	// Find the max log prob.
	maxLP := probs[0]
	bestIdx := 0
	for i := 1; i < n; i++ {
		if probs[i] > maxLP {
			maxLP = probs[i]
			bestIdx = i
		}
	}

	// Log-sum-exp to normalize.
	sumExp := 0.0
	for i := 0; i < n; i++ {
		sumExp += math.Exp(probs[i] - maxLP)
	}

	// Confidence = exp(best - max) / sum_exp = 1/sum_exp when bestIdx is max.
	confidence := math.Exp(probs[bestIdx]-maxLP) / sumExp
	return clampConfidence(confidence)
}

// getOrCreate returns or creates HMM state for a symbol.
func (h *HMMDetector) getOrCreate(symbol string) *hmmSymbolState {
	s, ok := h.states[symbol]
	if !ok {
		s = &hmmSymbolState{
			viterbiProb: make([]float64, h.config.NumStates),
		}
		h.states[symbol] = s
	}
	return s
}

// DiscretizeMomentum maps a continuous momentum value to a discrete observation.
// Uses fixed thresholds: strong_up(0), mild_up(1), flat(2), mild_down(3), strong_down(4).
func DiscretizeMomentum(momentum float64) int {
	switch {
	case momentum > 0.005:
		return 0 // strong_up
	case momentum > 0.001:
		return 1 // mild_up
	case momentum > -0.001:
		return 2 // flat
	case momentum > -0.005:
		return 3 // mild_down
	default:
		return 4 // strong_down
	}
}
