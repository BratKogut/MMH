package regime

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testConfig returns a deterministic config for testing.
func testConfig() RegimeConfig {
	return RegimeConfig{
		MomentumThresholdUp:   0.001,
		MomentumThresholdDown: -0.001,
		VolatilityHighPct:     0.02,
		VolatilityLowPct:      0.005,
		BreakoutMomentumMult:  3.0,
		BreakoutImbalanceMin:  0.3,
		MinSamples:            5,
		MomentumHistSize:      10,
	}
}

// feedSamples pushes n identical feature sets into the detector for a symbol.
func feedSamples(d *Detector, symbol string, features map[string]float64, n int) {
	for i := 0; i < n; i++ {
		d.Update(symbol, features)
	}
}

func TestMinSamplesGuard(t *testing.T) {
	cfg := testConfig()
	cfg.MinSamples = 5
	d := NewDetector(cfg)

	// Strong trending-up features, but not enough samples yet.
	features := map[string]float64{
		"momentum":   0.01,
		"volatility": 0.01,
		"imbalance":  0.0,
	}

	// First 4 updates should return UNKNOWN.
	for i := 0; i < 4; i++ {
		ev := d.Update("BTC-USD", features)
		assert.Equal(t, string(RegimeUnknown), ev.Regime, "sample %d should be UNKNOWN", i)
		assert.Equal(t, 0.0, ev.Confidence)
	}

	// 5th update should classify properly.
	ev := d.Update("BTC-USD", features)
	assert.NotEqual(t, string(RegimeUnknown), ev.Regime, "should classify after MinSamples")
}

func TestTrendingUpClassification(t *testing.T) {
	cfg := testConfig()
	cfg.MinSamples = 1 // disable min-samples guard for this test
	d := NewDetector(cfg)

	features := map[string]float64{
		"momentum":   0.005, // clearly above 0.001
		"volatility": 0.01,  // medium (between 0.005 and 0.02)
		"imbalance":  0.1,
	}

	ev := d.Update("BTC-USD", features)
	assert.Equal(t, string(RegimeTrendingUp), ev.Regime)
	assert.True(t, ev.Confidence >= 0.5, "confidence should be >= 0.5")
}

func TestTrendingDownClassification(t *testing.T) {
	cfg := testConfig()
	cfg.MinSamples = 1
	d := NewDetector(cfg)

	features := map[string]float64{
		"momentum":   -0.005, // clearly below -0.001
		"volatility": 0.01,
		"imbalance":  -0.1,
	}

	ev := d.Update("BTC-USD", features)
	assert.Equal(t, string(RegimeTrendingDown), ev.Regime)
	assert.True(t, ev.Confidence >= 0.5)
}

func TestHighVolatilityClassification(t *testing.T) {
	cfg := testConfig()
	cfg.MinSamples = 1
	d := NewDetector(cfg)

	features := map[string]float64{
		"momentum":   0.005, // also trending, but high vol takes priority
		"volatility": 0.04,  // well above 0.02
		"imbalance":  0.0,
	}

	ev := d.Update("BTC-USD", features)
	assert.Equal(t, string(RegimeHighVol), ev.Regime)
	assert.True(t, ev.Confidence >= 0.5)
}

func TestLowVolatilityClassification(t *testing.T) {
	cfg := testConfig()
	cfg.MinSamples = 1
	d := NewDetector(cfg)

	features := map[string]float64{
		"momentum":   0.0005, // within threshold (not trending)
		"volatility": 0.001,  // well below 0.005
		"imbalance":  0.0,
	}

	ev := d.Update("BTC-USD", features)
	assert.Equal(t, string(RegimeLowVol), ev.Regime)
	assert.True(t, ev.Confidence >= 0.5)
}

func TestMeanRevertingClassification(t *testing.T) {
	cfg := testConfig()
	cfg.MinSamples = 1
	d := NewDetector(cfg)

	features := map[string]float64{
		"momentum":   0.0002, // very close to zero, within [-0.001, 0.001]
		"volatility": 0.01,   // medium: between 0.005 and 0.02
		"imbalance":  0.0,
	}

	ev := d.Update("BTC-USD", features)
	assert.Equal(t, string(RegimeMeanReverting), ev.Regime)
	assert.True(t, ev.Confidence >= 0.5, "confidence should be >= 0.5, got %f", ev.Confidence)
}

func TestBreakoutDetection(t *testing.T) {
	cfg := testConfig()
	cfg.MinSamples = 1
	cfg.BreakoutMomentumMult = 3.0
	cfg.BreakoutImbalanceMin = 0.3
	d := NewDetector(cfg)

	// Build up a history of small momentum values.
	mildFeatures := map[string]float64{
		"momentum":   0.0005,
		"volatility": 0.01,
		"imbalance":  0.1,
	}
	// Feed enough to fill the history buffer.
	feedSamples(d, "BTC-USD", mildFeatures, 6)

	// Verify we're not in breakout.
	regime, _ := d.CurrentRegime("BTC-USD")
	assert.NotEqual(t, RegimeBreakout, regime, "should not be breakout with mild momentum")

	// Now spike the momentum: 0.005 is 10x the historical avg of ~0.0005.
	// Also high imbalance to confirm breakout.
	breakoutFeatures := map[string]float64{
		"momentum":   0.005,
		"volatility": 0.01,
		"imbalance":  0.5, // above 0.3 threshold
	}

	ev := d.Update("BTC-USD", breakoutFeatures)
	assert.Equal(t, string(RegimeBreakout), ev.Regime)
	assert.True(t, ev.Confidence >= 0.6, "breakout confidence should be >= 0.6, got %f", ev.Confidence)
}

func TestBreakoutRequiresImbalance(t *testing.T) {
	cfg := testConfig()
	cfg.MinSamples = 1
	cfg.BreakoutMomentumMult = 3.0
	cfg.BreakoutImbalanceMin = 0.3
	d := NewDetector(cfg)

	// Build history.
	mildFeatures := map[string]float64{
		"momentum":   0.0005,
		"volatility": 0.01,
		"imbalance":  0.1,
	}
	feedSamples(d, "BTC-USD", mildFeatures, 6)

	// Spike momentum but LOW imbalance -- should NOT be breakout.
	spikeNoImbalance := map[string]float64{
		"momentum":   0.005,
		"volatility": 0.01,
		"imbalance":  0.1, // below 0.3 threshold
	}

	ev := d.Update("BTC-USD", spikeNoImbalance)
	assert.NotEqual(t, string(RegimeBreakout), ev.Regime,
		"breakout requires imbalance >= BreakoutImbalanceMin")
}

func TestRegimeTransitionTracking(t *testing.T) {
	cfg := testConfig()
	cfg.MinSamples = 1
	d := NewDetector(cfg)

	// Start trending up.
	upFeatures := map[string]float64{
		"momentum":   0.005,
		"volatility": 0.01,
		"imbalance":  0.0,
	}
	ev1 := d.Update("BTC-USD", upFeatures)
	assert.Equal(t, string(RegimeTrendingUp), ev1.Regime)
	// First event from UNKNOWN -> TRENDING_UP should have PrevRegime set.
	assert.Equal(t, string(RegimeUnknown), ev1.PrevRegime)

	// Transition to trending down.
	downFeatures := map[string]float64{
		"momentum":   -0.005,
		"volatility": 0.01,
		"imbalance":  0.0,
	}
	ev2 := d.Update("BTC-USD", downFeatures)
	assert.Equal(t, string(RegimeTrendingDown), ev2.Regime)
	assert.Equal(t, string(RegimeTrendingUp), ev2.PrevRegime)
}

func TestConfidenceScoring(t *testing.T) {
	cfg := testConfig()
	cfg.MinSamples = 1
	d := NewDetector(cfg)

	// Barely above threshold: low confidence.
	weakTrend := map[string]float64{
		"momentum":   0.0011, // just above 0.001
		"volatility": 0.01,
		"imbalance":  0.0,
	}
	ev1 := d.Update("SYM1", weakTrend)
	assert.Equal(t, string(RegimeTrendingUp), ev1.Regime)
	weakConf := ev1.Confidence

	// Far above threshold: higher confidence.
	strongTrend := map[string]float64{
		"momentum":   0.01, // 10x the threshold
		"volatility": 0.01,
		"imbalance":  0.0,
	}
	ev2 := d.Update("SYM2", strongTrend)
	assert.Equal(t, string(RegimeTrendingUp), ev2.Regime)
	strongConf := ev2.Confidence

	assert.True(t, strongConf > weakConf,
		"stronger momentum should produce higher confidence: strong=%f weak=%f", strongConf, weakConf)

	// Both should be in valid range [0, 1].
	assert.True(t, weakConf >= 0 && weakConf <= 1)
	assert.True(t, strongConf >= 0 && strongConf <= 1)
}

func TestMultipleSymbolsIndependent(t *testing.T) {
	cfg := testConfig()
	cfg.MinSamples = 1
	d := NewDetector(cfg)

	btcFeatures := map[string]float64{
		"momentum":   0.005,
		"volatility": 0.01,
		"imbalance":  0.0,
	}
	ethFeatures := map[string]float64{
		"momentum":   -0.005,
		"volatility": 0.01,
		"imbalance":  0.0,
	}

	d.Update("BTC-USD", btcFeatures)
	d.Update("ETH-USD", ethFeatures)

	btcRegime, _ := d.CurrentRegime("BTC-USD")
	ethRegime, _ := d.CurrentRegime("ETH-USD")

	assert.Equal(t, RegimeTrendingUp, btcRegime)
	assert.Equal(t, RegimeTrendingDown, ethRegime)
}

func TestCurrentRegimeUnknownSymbol(t *testing.T) {
	d := NewDetector(testConfig())

	regime, confidence := d.CurrentRegime("NONEXISTENT")
	assert.Equal(t, RegimeUnknown, regime)
	assert.Equal(t, 0.0, confidence)
}

func TestVolatilityPriorityOverTrend(t *testing.T) {
	cfg := testConfig()
	cfg.MinSamples = 1
	d := NewDetector(cfg)

	// Both high momentum AND high volatility: vol should win.
	features := map[string]float64{
		"momentum":   0.01,  // strongly trending
		"volatility": 0.05,  // very high vol
		"imbalance":  0.0,
	}

	ev := d.Update("BTC-USD", features)
	assert.Equal(t, string(RegimeHighVol), ev.Regime,
		"high volatility should take priority over trending")
}

func TestEventFieldsPopulated(t *testing.T) {
	cfg := testConfig()
	cfg.MinSamples = 1
	d := NewDetector(cfg)

	features := map[string]float64{
		"momentum":   0.005,
		"volatility": 0.01,
		"imbalance":  0.0,
	}

	ev := d.Update("BTC-USD", features)

	// Verify bus event fields are populated.
	assert.NotEmpty(t, ev.EventID)
	assert.NotEmpty(t, ev.SchemaVersion)
	assert.Equal(t, "regime-detector", ev.Producer)
	assert.Equal(t, "BTC-USD", ev.Symbol)
	assert.NotZero(t, ev.Timestamp)
}

// --- HMM Detector Tests ---

func TestHMMDetectorBasic(t *testing.T) {
	cfg := DefaultHMMConfig()
	h := NewHMMDetector(cfg)

	// Feed a sequence of "flat" observations: HMM should gravitate toward MeanReverting.
	var state HMMState
	var conf float64
	for i := 0; i < 20; i++ {
		state, conf = h.Observe("BTC-USD", 2) // flat
	}

	assert.Equal(t, HMMMeanReverting, state, "repeated flat observations should -> MeanReverting")
	assert.True(t, conf > 0.3, "confidence should be meaningful, got %f", conf)
}

func TestHMMDetectorTrendingSequence(t *testing.T) {
	cfg := DefaultHMMConfig()
	h := NewHMMDetector(cfg)

	// Feed a sequence of strong_up observations: should converge to Trending.
	var state HMMState
	for i := 0; i < 20; i++ {
		state, _ = h.Observe("ETH-USD", 0) // strong_up
	}

	assert.Equal(t, HMMTrending, state, "repeated strong_up should -> Trending")
}

func TestHMMDetectorHighVolSequence(t *testing.T) {
	cfg := DefaultHMMConfig()
	h := NewHMMDetector(cfg)

	// Feed a sequence of strong_down observations: HMM HighVol state has highest
	// emission probability for strong_down.
	var state HMMState
	for i := 0; i < 30; i++ {
		state, _ = h.Observe("SOL-USD", 4) // strong_down
	}

	assert.Equal(t, HMMHighVol, state, "repeated strong_down should -> HighVol")
}

func TestHMMCurrentStateUnknownSymbol(t *testing.T) {
	h := NewHMMDetector(DefaultHMMConfig())

	state, conf := h.CurrentState("NONEXISTENT")
	assert.Equal(t, HMMTrending, state) // default
	assert.Equal(t, 0.0, conf)
}

func TestHMMInvalidObservation(t *testing.T) {
	h := NewHMMDetector(DefaultHMMConfig())

	state, conf := h.Observe("BTC-USD", -1)
	assert.Equal(t, HMMTrending, state)
	assert.Equal(t, 0.0, conf)

	state, conf = h.Observe("BTC-USD", 100)
	assert.Equal(t, HMMTrending, state)
	assert.Equal(t, 0.0, conf)
}

func TestStateToRegimeMapping(t *testing.T) {
	assert.Equal(t, RegimeTrendingUp, StateToRegime(HMMTrending))
	assert.Equal(t, RegimeMeanReverting, StateToRegime(HMMMeanReverting))
	assert.Equal(t, RegimeHighVol, StateToRegime(HMMHighVol))
	assert.Equal(t, RegimeUnknown, StateToRegime(HMMState(99)))
}

func TestDiscretizeMomentum(t *testing.T) {
	tests := []struct {
		momentum float64
		expected int
	}{
		{0.01, 0},    // strong_up
		{0.003, 1},   // mild_up
		{0.0, 2},     // flat
		{-0.003, 3},  // mild_down
		{-0.01, 4},   // strong_down
	}

	for _, tt := range tests {
		result := DiscretizeMomentum(tt.momentum)
		assert.Equal(t, tt.expected, result, "momentum=%f", tt.momentum)
	}
}

func TestClampConfidence(t *testing.T) {
	assert.Equal(t, 0.0, clampConfidence(-0.5))
	assert.Equal(t, 0.5, clampConfidence(0.5))
	assert.Equal(t, 1.0, clampConfidence(1.5))
}

func TestHMMConfidenceRange(t *testing.T) {
	h := NewHMMDetector(DefaultHMMConfig())

	// Feed a mixed sequence and verify confidence is always in [0, 1].
	observations := []int{0, 1, 2, 3, 4, 0, 2, 4, 1, 3, 0, 0, 0, 4, 4, 4}
	for _, obs := range observations {
		_, conf := h.Observe("BTC-USD", obs)
		assert.True(t, conf >= 0 && conf <= 1, "confidence out of range: %f", conf)
		assert.False(t, math.IsNaN(conf), "confidence should not be NaN")
	}
}

func TestDetectorDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	require.NotNil(t, d)
	assert.Equal(t, 20, d.config.MomentumHistSize)
	assert.Equal(t, 10, d.config.MinSamples)
}
