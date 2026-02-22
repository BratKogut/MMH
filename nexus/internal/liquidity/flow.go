package liquidity

import (
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// ---------------------------------------------------------------------------
// Liquidity Flow Direction Analysis — v3.2 Module
// "Nie pytaj ile jest — pytaj dokąd płynie"
// Tracks net flow direction, velocity, and source quality per pool.
// ---------------------------------------------------------------------------

// FlowDirection indicates the net flow direction.
type FlowDirection int

const (
	FlowStable  FlowDirection = iota
	FlowInflow                // net adding liquidity
	FlowOutflow               // net removing liquidity
)

func (f FlowDirection) String() string {
	switch f {
	case FlowInflow:
		return "INFLOW"
	case FlowOutflow:
		return "OUTFLOW"
	default:
		return "STABLE"
	}
}

// Velocity indicates the acceleration of flow.
type Velocity int

const (
	VelocityStable       Velocity = iota
	VelocityAccelerating          // flow is increasing
	VelocityDecelerating          // flow is decreasing
)

func (v Velocity) String() string {
	switch v {
	case VelocityAccelerating:
		return "ACCELERATING"
	case VelocityDecelerating:
		return "DECELERATING"
	default:
		return "STABLE"
	}
}

// SourceQuality rates the quality of liquidity providers.
type SourceQuality int

const (
	QualityUnknown SourceQuality = iota
	QualityHigh                  // locked contracts, CLEAN wallets
	QualityMedium                // unknown but clean wallets
	QualityLow                   // sybil cluster, fresh wallets, wash traders
)

func (q SourceQuality) String() string {
	switch q {
	case QualityHigh:
		return "HIGH"
	case QualityMedium:
		return "MEDIUM"
	case QualityLow:
		return "LOW"
	default:
		return "UNKNOWN"
	}
}

// FlowPattern classifies the liquidity flow pattern.
type FlowPattern string

const (
	PatternHealthyGrowth  FlowPattern = "HEALTHY_GROWTH"
	PatternArtificialPump FlowPattern = "ARTIFICIAL_PUMP"
	PatternSlowBleed      FlowPattern = "SLOW_BLEED"
	PatternRugPrecursor   FlowPattern = "RUG_PRECURSOR"
	PatternLPUnlockRisk   FlowPattern = "LP_UNLOCK_RISK"
	PatternNormal         FlowPattern = "NORMAL"
)

// LiquidityFlow is the analysis result for a single pool.
type LiquidityFlow struct {
	TokenAddress  string          `json:"token_address"`
	NetFlow5min   float64         `json:"net_flow_5min"`   // USD net inflow/outflow
	NetFlow30min  float64         `json:"net_flow_30min"`
	Direction     FlowDirection   `json:"direction"`
	Velocity      Velocity        `json:"velocity"`
	SourceQuality SourceQuality   `json:"source_quality"`
	LPMintCount   int             `json:"lp_mint_count"`
	LPBurnCount   int             `json:"lp_burn_count"`
	LargestFlow   float64         `json:"largest_flow"`
	Pattern       FlowPattern     `json:"pattern"`
	Timestamp     int64           `json:"timestamp"`
}

// flowSample records a liquidity change event.
type flowSample struct {
	amountUSD     float64
	isAdd         bool       // true=add liquidity, false=remove
	sourceQuality SourceQuality
	timestamp     time.Time
}

// FlowAnalyzerConfig configures the flow analyzer.
type FlowAnalyzerConfig struct {
	ShortWindowMin   int     `yaml:"short_window_min"`    // 5-minute window
	LongWindowMin    int     `yaml:"long_window_min"`     // 30-minute window
	StableThreshUSD  float64 `yaml:"stable_thresh_usd"`   // below this = STABLE
	MaxSamplesPerPool int    `yaml:"max_samples_per_pool"` // ring buffer size
}

// DefaultFlowAnalyzerConfig returns production defaults.
func DefaultFlowAnalyzerConfig() FlowAnalyzerConfig {
	return FlowAnalyzerConfig{
		ShortWindowMin:    5,
		LongWindowMin:     30,
		StableThreshUSD:   500,
		MaxSamplesPerPool: 500,
	}
}

// FlowAnalyzer tracks liquidity flow direction per pool.
type FlowAnalyzer struct {
	config FlowAnalyzerConfig

	mu      sync.RWMutex
	samples map[string][]flowSample // tokenAddress -> ring buffer of events
}

// NewFlowAnalyzer creates a new liquidity flow analyzer.
func NewFlowAnalyzer(config FlowAnalyzerConfig) *FlowAnalyzer {
	return &FlowAnalyzer{
		config:  config,
		samples: make(map[string][]flowSample),
	}
}

// RecordLiquidityChange records an add or remove liquidity event.
func (a *FlowAnalyzer) RecordLiquidityChange(tokenAddress string, amountUSD float64, isAdd bool, quality SourceQuality) {
	a.mu.Lock()
	defer a.mu.Unlock()

	s := flowSample{
		amountUSD:     amountUSD,
		isAdd:         isAdd,
		sourceQuality: quality,
		timestamp:     time.Now(),
	}

	samples := a.samples[tokenAddress]
	if len(samples) >= a.config.MaxSamplesPerPool {
		// Drop oldest.
		samples = samples[1:]
	}
	a.samples[tokenAddress] = append(samples, s)
}

// Analyze computes the current liquidity flow for a token.
func (a *FlowAnalyzer) Analyze(tokenAddress string) LiquidityFlow {
	a.mu.RLock()
	samples := a.samples[tokenAddress]
	// Copy to avoid holding lock during analysis.
	copied := make([]flowSample, len(samples))
	copy(copied, samples)
	a.mu.RUnlock()

	now := time.Now()
	shortCutoff := now.Add(-time.Duration(a.config.ShortWindowMin) * time.Minute)
	longCutoff := now.Add(-time.Duration(a.config.LongWindowMin) * time.Minute)

	result := LiquidityFlow{
		TokenAddress: tokenAddress,
		Timestamp:    now.UnixNano(),
		Pattern:      PatternNormal,
	}

	if len(copied) == 0 {
		return result
	}

	// Compute flows per window.
	var netShort, netLong float64
	var shortAdds, shortRemoves int
	var worstQuality SourceQuality
	var largestSingle float64

	for _, s := range copied {
		signed := s.amountUSD
		if !s.isAdd {
			signed = -signed
		}

		if s.timestamp.After(longCutoff) {
			netLong += signed
		}
		if s.timestamp.After(shortCutoff) {
			netShort += signed
			if s.isAdd {
				shortAdds++
			} else {
				shortRemoves++
			}
		}

		if s.amountUSD > largestSingle {
			largestSingle = s.amountUSD
		}
		if s.sourceQuality > worstQuality {
			worstQuality = s.sourceQuality
		}
	}

	result.NetFlow5min = netShort
	result.NetFlow30min = netLong
	result.LPMintCount = shortAdds
	result.LPBurnCount = shortRemoves
	result.LargestFlow = largestSingle
	result.SourceQuality = worstQuality

	// Direction.
	if netShort > a.config.StableThreshUSD {
		result.Direction = FlowInflow
	} else if netShort < -a.config.StableThreshUSD {
		result.Direction = FlowOutflow
	} else {
		result.Direction = FlowStable
	}

	// Velocity: compare short vs long window (normalized per minute).
	shortRate := netShort / float64(a.config.ShortWindowMin)
	longRate := netLong / float64(a.config.LongWindowMin)

	if abs(shortRate) > abs(longRate)*1.5 && abs(shortRate) > 50 {
		result.Velocity = VelocityAccelerating
	} else if abs(shortRate) < abs(longRate)*0.5 && abs(longRate) > 50 {
		result.Velocity = VelocityDecelerating
	} else {
		result.Velocity = VelocityStable
	}

	// Pattern detection.
	result.Pattern = a.detectPattern(result, largestSingle)

	return result
}

// detectPattern classifies the flow into a known pattern.
func (a *FlowAnalyzer) detectPattern(flow LiquidityFlow, largestFlow float64) FlowPattern {
	// RUG PRECURSOR: accelerating outflow from low-quality source.
	if flow.Direction == FlowOutflow && flow.Velocity == VelocityAccelerating {
		return PatternRugPrecursor
	}

	// ARTIFICIAL PUMP: large single inflow from low-quality source.
	if flow.Direction == FlowInflow && largestFlow > abs(flow.NetFlow5min)*0.8 && flow.SourceQuality == QualityLow {
		return PatternArtificialPump
	}

	// SLOW BLEED: stable small outflows from multiple sources.
	if flow.Direction == FlowOutflow && flow.Velocity == VelocityStable {
		return PatternSlowBleed
	}

	// HEALTHY GROWTH: inflow, stable or accelerating, high quality.
	if flow.Direction == FlowInflow && flow.SourceQuality <= QualityMedium {
		return PatternHealthyGrowth
	}

	return PatternNormal
}

// ScoreImpact returns the scoring adjustment for the on-chain dimension.
func (flow *LiquidityFlow) ScoreImpact() float64 {
	switch flow.Pattern {
	case PatternHealthyGrowth:
		if flow.Velocity == VelocityAccelerating {
			return 15
		}
		return 5
	case PatternSlowBleed:
		return -10
	case PatternArtificialPump:
		return -15
	case PatternRugPrecursor:
		return -100 // INSTANT_KILL equivalent
	default:
		return 0
	}
}

// IsRugPrecursor returns true if the pattern indicates imminent rug.
func (flow *LiquidityFlow) IsRugPrecursor() bool {
	return flow.Pattern == PatternRugPrecursor
}

// Cleanup removes samples for pools that haven't had events in the given duration.
func (a *FlowAnalyzer) Cleanup(maxAge time.Duration) int {
	cutoff := time.Now().Add(-maxAge)

	a.mu.Lock()
	defer a.mu.Unlock()

	removed := 0
	for token, samples := range a.samples {
		if len(samples) == 0 {
			delete(a.samples, token)
			removed++
			continue
		}
		// If newest sample is older than cutoff, remove entire token.
		if samples[len(samples)-1].timestamp.Before(cutoff) {
			delete(a.samples, token)
			removed++
		}
	}

	if removed > 0 {
		log.Debug().Int("removed", removed).Msg("liquidity_flow: cleaned up stale tokens")
	}
	return removed
}

// Stats returns analyzer statistics.
type FlowStats struct {
	TrackedTokens int `json:"tracked_tokens"`
	TotalSamples  int `json:"total_samples"`
}

func (a *FlowAnalyzer) Stats() FlowStats {
	a.mu.RLock()
	defer a.mu.RUnlock()

	total := 0
	for _, s := range a.samples {
		total += len(s)
	}
	return FlowStats{
		TrackedTokens: len(a.samples),
		TotalSamples:  total,
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
