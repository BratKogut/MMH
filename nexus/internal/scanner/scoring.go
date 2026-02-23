package scanner

import (
	"time"

	"github.com/nexus-trading/nexus/internal/correlation"
	"github.com/nexus-trading/nexus/internal/graph"
	"github.com/nexus-trading/nexus/internal/honeypot"
	"github.com/nexus-trading/nexus/internal/liquidity"
	"github.com/nexus-trading/nexus/internal/narrative"
	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// 5-Dimensional Scoring Engine
// Plan v3.1 L4: Safety 30% + Entity 15% + Social 20% + OnChain 20% + Timing 15%
// ---------------------------------------------------------------------------

// TokenScore is the full 5-dimensional score for a token.
type TokenScore struct {
	Total            float64 `json:"total"`              // 0-100 weighted composite
	Safety           float64 `json:"safety"`             // 0-100
	Entity           float64 `json:"entity"`             // 0-100
	Social           float64 `json:"social"`             // 0-100
	OnChain          float64 `json:"onchain"`            // 0-100
	Timing           float64 `json:"timing"`             // 0-100
	CorrelationBonus float64 `json:"correlation_bonus"`
	CorrelationType  string  `json:"correlation_type"`
	Recommendation   string  `json:"recommendation"`     // STRONG_BUY|BUY|WAIT|SKIP|RUG|HONEYPOT
	Reasons          []string `json:"reasons"`
	InstantKill      bool    `json:"instant_kill"`
	KillReason       string  `json:"kill_reason,omitempty"`
}

// ScoringWeights defines the weight of each dimension.
type ScoringWeights struct {
	Safety  float64 `yaml:"safety"`  // default 0.30
	Entity  float64 `yaml:"entity"`  // default 0.15
	Social  float64 `yaml:"social"`  // default 0.20
	OnChain float64 `yaml:"onchain"` // default 0.20
	Timing  float64 `yaml:"timing"`  // default 0.15
}

// DefaultWeights returns the plan v3.1 weights.
func DefaultWeights() ScoringWeights {
	return ScoringWeights{
		Safety:  0.30,
		Entity:  0.15,
		Social:  0.20,
		OnChain: 0.20,
		Timing:  0.15,
	}
}

// ScoringConfig configures the scoring engine.
type ScoringConfig struct {
	Weights          ScoringWeights `yaml:"weights"`
	MinTotalScore    float64        `yaml:"min_total_score"`    // default 65
	StrongBuyScore   float64        `yaml:"strong_buy_score"`   // default 85
}

// DefaultScoringConfig returns defaults per plan v3.1.
func DefaultScoringConfig() ScoringConfig {
	return ScoringConfig{
		Weights:        DefaultWeights(),
		MinTotalScore:  65,
		StrongBuyScore: 85,
	}
}

// ScoringInput contains all data needed for 5D scoring.
type ScoringInput struct {
	Analysis     TokenAnalysis
	EntityReport *graph.EntityReport
	SellSim      *SellSimResult
	WhaleSignals []WhaleSignal
	SocialData   *SocialData

	// v3.2 Advanced Analysis inputs.
	LiquidityFlow    *liquidity.LiquidityFlow       // from FlowAnalyzer.Analyze()
	NarrativeState   *narrative.NarrativeState       // from Engine.GetTokenNarrative()
	CrossTokenState  *correlation.CrossTokenState    // from Detector.GetClusterState()
	HoneypotMatch    *honeypot.Signature             // from Tracker.CheckContract()
}

// WhaleSignal represents a whale wallet event.
type WhaleSignal struct {
	Type    string  `json:"type"` // WHALE_BUY|SMART_MONEY_BUY|WHALE_SELL|KOL_BUY
	Address string  `json:"address"`
	Amount  float64 `json:"amount"`
}

// SocialData represents aggregated social signals.
type SocialData struct {
	TelegramVelocity float64 `json:"telegram_velocity"` // msgs/min
	TwitterMentions  int     `json:"twitter_mentions"`
	DiscordMentions  int     `json:"discord_mentions"`
	Sentiment        float64 `json:"sentiment"`         // -1 to 1
	UniqueSources    int     `json:"unique_sources"`
	HasKOLMention    bool    `json:"has_kol_mention"`
}

// Scorer computes 5-dimensional token scores.
type Scorer struct {
	config ScoringConfig
}

// NewScorer creates a new 5D scorer.
func NewScorer(config ScoringConfig) *Scorer {
	return &Scorer{config: config}
}

// Score computes the full 5D score for a token.
func (s *Scorer) Score(input ScoringInput) TokenScore {
	ts := TokenScore{}

	// ── v3.2 Instant-Kill Checks (before scoring) ──
	if kill, reason := s.v32InstantKill(input); kill {
		ts.InstantKill = true
		ts.KillReason = reason
		ts.Total = 0
		ts.Recommendation = "SKIP"
		ts.Reasons = append(ts.Reasons, "INSTANT_KILL: "+reason)
		return ts
	}

	// Dimension 1: SAFETY (30%).
	ts.Safety, ts.InstantKill, ts.KillReason = s.scoreSafety(input)
	if ts.InstantKill {
		ts.Total = 0
		ts.Recommendation = "SKIP"
		ts.Reasons = append(ts.Reasons, "INSTANT_KILL: "+ts.KillReason)
		return ts
	}

	// Dimension 2: ENTITY (15%).
	ts.Entity = s.scoreEntity(input)

	// Dimension 3: SOCIAL (20%).
	ts.Social = s.scoreSocial(input)

	// Dimension 4: ON-CHAIN (20%).
	ts.OnChain = s.scoreOnChain(input)

	// Dimension 5: TIMING (15%).
	ts.Timing = s.scoreTiming(input)

	// ── v3.2 Score Adjustments ──
	v32Adj := s.v32ScoreAdjust(input)
	ts.Safety += v32Adj.safetyAdj
	ts.OnChain += v32Adj.onChainAdj
	ts.Timing += v32Adj.timingAdj
	ts.Entity += v32Adj.entityAdj

	// Clamp all dimensions.
	ts.Safety = clampScore(ts.Safety)
	ts.Entity = clampScore(ts.Entity)
	ts.Social = clampScore(ts.Social)
	ts.OnChain = clampScore(ts.OnChain)
	ts.Timing = clampScore(ts.Timing)

	// Add v3.2 reasons.
	ts.Reasons = append(ts.Reasons, v32Adj.reasons...)

	// Correlation bonus.
	ts.CorrelationBonus, ts.CorrelationType = s.correlationBonus(input, ts)

	// Weighted total.
	w := s.config.Weights
	ts.Total = (ts.Safety * w.Safety) +
		(ts.Entity * w.Entity) +
		(ts.Social * w.Social) +
		(ts.OnChain * w.OnChain) +
		(ts.Timing * w.Timing) +
		ts.CorrelationBonus

	// Clamp.
	ts.Total = clampScore(ts.Total)

	// Recommendation.
	ts.Recommendation = s.recommend(ts)

	return ts
}

// v32Adjustment holds score adjustments from v3.2 modules.
type v32Adjustment struct {
	safetyAdj  float64
	entityAdj  float64
	onChainAdj float64
	timingAdj  float64
	reasons    []string
}

// v32InstantKill checks v3.2 inputs for instant-kill conditions.
func (s *Scorer) v32InstantKill(input ScoringInput) (bool, string) {
	// Honeypot evolution match with high confidence.
	if input.HoneypotMatch != nil && input.HoneypotMatch.Confidence >= 0.7 {
		return true, "honeypot_pattern_match:" + input.HoneypotMatch.PatternID
	}

	// Liquidity rug precursor.
	if input.LiquidityFlow != nil && input.LiquidityFlow.IsRugPrecursor() {
		return true, "rug_precursor_detected"
	}

	// Cross-token critical risk.
	if input.CrossTokenState != nil && input.CrossTokenState.RiskLevel == correlation.RiskCritical {
		return true, "cross_token_critical:" + string(input.CrossTokenState.Pattern)
	}

	return false, ""
}

// v32ScoreAdjust applies v3.2 module score impacts to dimensions.
func (s *Scorer) v32ScoreAdjust(input ScoringInput) v32Adjustment {
	adj := v32Adjustment{}

	// Liquidity flow impacts safety dimension.
	if input.LiquidityFlow != nil {
		impact := input.LiquidityFlow.ScoreImpact()
		if impact != 0 {
			adj.safetyAdj += impact
			if impact < 0 {
				adj.reasons = append(adj.reasons, "v32_liquidity:"+string(input.LiquidityFlow.Pattern))
			}
		}
	}

	// Narrative phase impacts timing dimension.
	if input.NarrativeState != nil {
		impact := narrative.ScoreImpact(input.NarrativeState)
		if impact != 0 {
			adj.timingAdj += impact
			adj.reasons = append(adj.reasons, "v32_narrative:"+input.NarrativeState.Phase.String())
		}
	}

	// Cross-token correlation impacts entity dimension.
	if input.CrossTokenState != nil {
		impact := input.CrossTokenState.ScoreImpact()
		if impact != 0 {
			adj.entityAdj += impact
			adj.reasons = append(adj.reasons, "v32_correlation:"+string(input.CrossTokenState.Pattern))
		}
	}

	// Honeypot match with moderate confidence impacts safety.
	if input.HoneypotMatch != nil && input.HoneypotMatch.Confidence >= 0.3 && input.HoneypotMatch.Confidence < 0.7 {
		adj.safetyAdj -= 20
		adj.reasons = append(adj.reasons, "v32_honeypot_partial:"+input.HoneypotMatch.PatternID)
	}

	return adj
}

func clampScore(v float64) float64 {
	if v > 100 {
		return 100
	}
	if v < 0 {
		return 0
	}
	return v
}

// scoreSafety maps our existing safety analysis to a 0-100 score.
func (s *Scorer) scoreSafety(input ScoringInput) (score float64, kill bool, killReason string) {
	score = float64(input.Analysis.SafetyScore)

	// Sell simulation: PASS = +15, FAIL = INSTANT_KILL.
	if input.SellSim != nil {
		if !input.SellSim.CanSell {
			return 0, true, "sell_simulation_failed"
		}
		score += 15
		if input.SellSim.EstimatedTaxPct > 10 {
			return 0, true, "sell_tax_too_high"
		}
	}

	// Check for honeypot verdict.
	if input.Analysis.Verdict == VerdictHoneypot {
		return 0, true, "honeypot_detected"
	}

	return clampScore(score), false, ""
}

// scoreEntity computes the entity dimension from the graph report.
func (s *Scorer) scoreEntity(input ScoringInput) float64 {
	if input.EntityReport == nil {
		return 50 // neutral if no data
	}

	er := input.EntityReport
	score := 50.0

	// 1-hop to SERIAL_RUGGER = INSTANT_KILL (handled above as high risk).
	if er.HopsToRugger == 1 {
		return 0
	}

	// 2-hop to rugger.
	if er.HopsToRugger == 2 {
		score -= 40
	}

	// Connected to profitable insider.
	if er.HopsToInsider == 1 {
		score += 25
	} else if er.HopsToInsider == 2 {
		score += 10
	}

	// Deployer clean.
	for _, label := range er.Labels {
		switch label {
		case graph.LabelClean:
			score += 30
		case graph.LabelSerialRugger:
			return 0
		case graph.LabelWashTrader:
			score -= 30
		case graph.LabelSybilCluster:
			score -= 20
		}
	}

	// Seed funder labels.
	for _, label := range er.SeedFunderLabels {
		switch label {
		case graph.LabelClean:
			score += 15
		case graph.LabelSerialRugger:
			score -= 25
		case graph.LabelWashTrader:
			score -= 15
		}
	}

	// Sybil cluster size.
	if er.ClusterSize > 20 {
		score -= 20
	}

	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	return score
}

// scoreSocial computes the social dimension.
func (s *Scorer) scoreSocial(input ScoringInput) float64 {
	if input.SocialData == nil {
		return 30 // low baseline without social data
	}

	sd := input.SocialData
	score := 0.0

	// Telegram velocity (0-25).
	switch {
	case sd.TelegramVelocity > 10:
		score += 25
	case sd.TelegramVelocity > 5:
		score += 18
	case sd.TelegramVelocity > 1:
		score += 10
	}

	// Twitter mentions (0-20).
	switch {
	case sd.TwitterMentions > 50:
		score += 20
	case sd.TwitterMentions > 20:
		score += 15
	case sd.TwitterMentions > 5:
		score += 8
	}

	// Discord (0-15).
	if sd.DiscordMentions > 10 {
		score += 15
	} else if sd.DiscordMentions > 3 {
		score += 8
	}

	// Unique sources (0-15).
	switch {
	case sd.UniqueSources >= 3:
		score += 15
	case sd.UniqueSources >= 2:
		score += 10
	case sd.UniqueSources >= 1:
		score += 5
	}

	// KOL mention (0-15).
	if sd.HasKOLMention {
		score += 15
	}

	// Multi-source correlation bonus.
	sourceCount := 0
	if sd.TelegramVelocity > 1 {
		sourceCount++
	}
	if sd.TwitterMentions > 5 {
		sourceCount++
	}
	if sd.DiscordMentions > 3 {
		sourceCount++
	}
	if sourceCount >= 2 {
		score *= 1.3 // 30% bonus for multi-source
	}

	// Sentiment modifier.
	if sd.Sentiment < -0.3 {
		score *= 0.5 // Negative sentiment halves score
	}

	if score > 100 {
		score = 100
	}
	return score
}

// scoreOnChain computes the on-chain activity dimension.
func (s *Scorer) scoreOnChain(input ScoringInput) float64 {
	score := 30.0 // baseline

	// Whale signals.
	for _, ws := range input.WhaleSignals {
		switch ws.Type {
		case "WHALE_BUY":
			score += 30
		case "SMART_MONEY_BUY":
			score += 25
		case "KOL_BUY":
			score += 20
		case "FRESH_WALLET_BIG_BUY":
			score += 10
		case "WHALE_SELL":
			score -= 40
		}
	}

	// Holder analysis from safety report.
	if input.Analysis.HolderAnalysis != nil {
		ha := input.Analysis.HolderAnalysis
		// Good distribution.
		if ha.Top10Pct < 40 {
			score += 15
		}
		// Decent holder count.
		if ha.UniqueHolders > 50 {
			score += 10
		}
	}

	// Liquidity health.
	liq := input.Analysis.Pool.LiquidityUSD
	if liq.GreaterThan(decimal.NewFromInt(50000)) {
		score += 15
	} else if liq.GreaterThan(decimal.NewFromInt(10000)) {
		score += 10
	}

	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	return score
}

// scoreTiming computes the timing dimension.
func (s *Scorer) scoreTiming(input ScoringInput) float64 {
	score := 30.0

	// Token age.
	age := time.Since(input.Analysis.Pool.CreatedAt)
	switch {
	case age < 5*time.Minute:
		score += 30
	case age < 30*time.Minute:
		score += 20
	case age < 2*time.Hour:
		score += 10
	}

	// Market cap estimate.
	mcap := input.Analysis.Pool.LiquidityUSD.Mul(decimal.NewFromInt(2))
	switch {
	case mcap.LessThan(decimal.NewFromInt(50000)):
		score += 25
	case mcap.LessThan(decimal.NewFromInt(500000)):
		score += 15
	case mcap.LessThan(decimal.NewFromInt(5000000)):
		score += 5
	}

	if score > 100 {
		score = 100
	}
	return score
}

// correlationBonus computes scenario-based bonus/penalty.
func (s *Scorer) correlationBonus(input ScoringInput, ts TokenScore) (bonus float64, corrType string) {
	hasWhale := len(input.WhaleSignals) > 0
	hasSocial := input.SocialData != nil && input.SocialData.TelegramVelocity > 1
	hasEntity := input.EntityReport != nil && input.EntityReport.HopsToInsider > 0
	hasChain := ts.OnChain > 50

	// Scenario A: Perfect Storm.
	if hasChain && hasWhale && hasSocial && ts.Entity > 60 {
		return 20, "PERFECT_STORM"
	}

	// Scenario B: Smart Money + Social.
	if hasEntity && hasSocial {
		return 15, "SMART_MONEY_SOCIAL"
	}

	// Scenario C: Social First.
	if hasSocial && hasChain {
		return 10, "SOCIAL_CHAIN"
	}

	// Scenario E: Social Only, No Chain.
	if hasSocial && !hasChain {
		return -10, "SOCIAL_ONLY"
	}

	// Scenario F: High social but entity has rug history.
	if hasSocial && input.EntityReport != nil && input.EntityReport.HopsToRugger > 0 {
		return -20, "INSIDER_RUG_HISTORY"
	}

	return 0, "NEUTRAL"
}

// recommend produces the final recommendation string.
func (s *Scorer) recommend(ts TokenScore) string {
	if ts.InstantKill {
		return "SKIP"
	}
	switch {
	case ts.Total >= s.config.StrongBuyScore:
		return "STRONG_BUY"
	case ts.Total >= s.config.MinTotalScore:
		return "BUY"
	case ts.Total >= 45:
		return "WAIT"
	default:
		return "SKIP"
	}
}
