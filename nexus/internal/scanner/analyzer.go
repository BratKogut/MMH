package scanner

import (
	"context"
	"fmt"
	"time"

	"github.com/nexus-trading/nexus/internal/solana"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Token Analyzer — safety scoring, rug detection, buy worthiness
// ---------------------------------------------------------------------------

// AnalyzerConfig configures the token analyzer.
type AnalyzerConfig struct {
	// Minimum safety score (0-100) to consider a token for sniping.
	MinSafetyScore int `yaml:"min_safety_score"`

	// Maximum top holder concentration (%). If top 10 holders have > this %, skip.
	MaxTop10HolderPct float64 `yaml:"max_top10_holder_pct"`

	// Maximum single holder concentration (%). Any single holder > this = skip.
	MaxSingleHolderPct float64 `yaml:"max_single_holder_pct"`

	// Require mint authority to be renounced.
	RequireMintRenounced bool `yaml:"require_mint_renounced"`

	// Require freeze authority to be renounced.
	RequireFreezeRenounced bool `yaml:"require_freeze_renounced"`

	// Require LP burned or locked.
	RequireLPSafe bool `yaml:"require_lp_safe"`

	// Minimum liquidity for analysis.
	MinLiquidityUSD float64 `yaml:"min_liquidity_usd"`

	// Number of top holders to check.
	TopHoldersToCheck int `yaml:"top_holders_to_check"`
}

// DefaultAnalyzerConfig returns sensible defaults.
func DefaultAnalyzerConfig() AnalyzerConfig {
	return AnalyzerConfig{
		MinSafetyScore:         40,    // Conservative minimum
		MaxTop10HolderPct:      50.0,  // Top 10 holders < 50%
		MaxSingleHolderPct:     15.0,  // No single holder > 15%
		RequireMintRenounced:   false, // Recommended but many tokens don't
		RequireFreezeRenounced: true,  // Freeze authority is a hard red flag
		RequireLPSafe:          false, // Many early tokens don't have this
		MinLiquidityUSD:        500,
		TopHoldersToCheck:      10,
	}
}

// TokenAnalysis is the result of analyzing a token.
type TokenAnalysis struct {
	Mint           solana.Pubkey `json:"mint"`
	Token          *solana.TokenInfo `json:"token,omitempty"`
	Pool           solana.PoolInfo   `json:"pool"`
	SafetyScore    int              `json:"safety_score"`     // 0-100
	Flags          []SafetyFlag     `json:"flags"`
	HolderAnalysis *HolderAnalysis  `json:"holder_analysis,omitempty"`
	Verdict        Verdict          `json:"verdict"`
	AnalyzedAt     time.Time        `json:"analyzed_at"`
	LatencyMs      int64            `json:"latency_ms"`
}

// SafetyFlag is a specific safety concern or positive signal.
type SafetyFlag struct {
	Code        string `json:"code"`
	Description string `json:"description"`
	Severity    string `json:"severity"` // critical|warning|info|positive
	ScoreImpact int    `json:"score_impact"` // negative = bad, positive = good
}

// HolderAnalysis summarizes holder distribution.
type HolderAnalysis struct {
	TopHolders       []solana.HolderInfo `json:"top_holders"`
	Top10Pct         float64             `json:"top_10_pct"`
	MaxSinglePct     float64             `json:"max_single_pct"`
	CreatorPct       float64             `json:"creator_pct"`
	UniqueHolders    int                 `json:"unique_holders"`
}

// Verdict is the final recommendation.
type Verdict string

const (
	VerdictBuy      Verdict = "BUY"       // Safe enough to snipe
	VerdictWait     Verdict = "WAIT"      // Wait for more data/liquidity
	VerdictSkip     Verdict = "SKIP"      // Too risky, skip
	VerdictRug      Verdict = "RUG"       // High rug probability
	VerdictHoneypot Verdict = "HONEYPOT"  // Likely honeypot
)

// Analyzer performs safety analysis on tokens.
type Analyzer struct {
	config AnalyzerConfig
	rpc    solana.RPCClient
}

// NewAnalyzer creates a new token analyzer.
func NewAnalyzer(config AnalyzerConfig, rpc solana.RPCClient) *Analyzer {
	return &Analyzer{
		config: config,
		rpc:    rpc,
	}
}

// Analyze performs a full safety analysis on a discovered pool/token.
func (a *Analyzer) Analyze(ctx context.Context, discovery PoolDiscovery) TokenAnalysis {
	start := time.Now()

	analysis := TokenAnalysis{
		Mint:        discovery.Pool.TokenMint,
		Token:       discovery.Token,
		Pool:        discovery.Pool,
		SafetyScore: 50, // Start neutral
		AnalyzedAt:  time.Now(),
	}

	// If we don't have token info, try to fetch it.
	if analysis.Token == nil {
		if info, err := a.rpc.GetTokenInfo(ctx, discovery.Pool.TokenMint); err == nil {
			analysis.Token = info
		}
	}

	// Run all checks (nil-safe: checkMintAuthority/checkFreezeAuthority handle nil Token).
	a.checkMintAuthority(&analysis)
	a.checkFreezeAuthority(&analysis)
	a.checkLiquidity(&analysis)
	a.checkLPSafety(&analysis)

	// Use timeout context for RPC calls in analysis.
	analysisCtx, analysisCancel := context.WithTimeout(ctx, 10*time.Second)
	defer analysisCancel()
	a.checkHolders(analysisCtx, &analysis)
	a.checkPoolAge(&analysis)

	// Clamp score.
	if analysis.SafetyScore < 0 {
		analysis.SafetyScore = 0
	}
	if analysis.SafetyScore > 100 {
		analysis.SafetyScore = 100
	}

	// Determine verdict.
	analysis.Verdict = a.determineVerdict(&analysis)
	analysis.LatencyMs = time.Since(start).Milliseconds()

	log.Info().
		Str("mint", string(analysis.Mint)).
		Int("safety_score", analysis.SafetyScore).
		Str("verdict", string(analysis.Verdict)).
		Int("flags", len(analysis.Flags)).
		Int64("analysis_ms", analysis.LatencyMs).
		Msg("analyzer: token analysis complete")

	return analysis
}

// checkMintAuthority checks if mint authority is renounced.
func (a *Analyzer) checkMintAuthority(analysis *TokenAnalysis) {
	if analysis.Token == nil {
		analysis.Flags = append(analysis.Flags, SafetyFlag{
			Code:        "NO_TOKEN_INFO",
			Description: "Could not fetch token metadata",
			Severity:    "warning",
			ScoreImpact: -10,
		})
		analysis.SafetyScore -= 10
		return
	}

	if analysis.Token.IsMintRenounced() {
		analysis.Flags = append(analysis.Flags, SafetyFlag{
			Code:        "MINT_RENOUNCED",
			Description: "Mint authority is renounced - cannot mint more tokens",
			Severity:    "positive",
			ScoreImpact: 15,
		})
		analysis.SafetyScore += 15
	} else {
		flag := SafetyFlag{
			Code:        "MINT_NOT_RENOUNCED",
			Description: "Mint authority is active - creator can mint more tokens",
			Severity:    "warning",
			ScoreImpact: -15,
		}
		if a.config.RequireMintRenounced {
			flag.Severity = "critical"
			flag.ScoreImpact = -30
		}
		analysis.Flags = append(analysis.Flags, flag)
		analysis.SafetyScore += flag.ScoreImpact
	}
}

// checkFreezeAuthority checks if freeze authority is renounced.
func (a *Analyzer) checkFreezeAuthority(analysis *TokenAnalysis) {
	if analysis.Token == nil {
		return
	}

	if analysis.Token.IsFreezeRenounced() {
		analysis.Flags = append(analysis.Flags, SafetyFlag{
			Code:        "FREEZE_RENOUNCED",
			Description: "Freeze authority is renounced - cannot freeze accounts",
			Severity:    "positive",
			ScoreImpact: 10,
		})
		analysis.SafetyScore += 10
	} else {
		flag := SafetyFlag{
			Code:        "FREEZE_ACTIVE",
			Description: "Freeze authority is active - creator can freeze your tokens",
			Severity:    "critical",
			ScoreImpact: -25,
		}
		analysis.Flags = append(analysis.Flags, flag)
		analysis.SafetyScore -= 25
	}
}

// checkLiquidity evaluates pool liquidity depth.
func (a *Analyzer) checkLiquidity(analysis *TokenAnalysis) {
	liq := analysis.Pool.LiquidityUSD

	switch {
	case liq.GreaterThan(decimal.NewFromInt(50000)):
		analysis.Flags = append(analysis.Flags, SafetyFlag{
			Code:        "HIGH_LIQUIDITY",
			Description: "Strong liquidity > $50k",
			Severity:    "positive",
			ScoreImpact: 15,
		})
		analysis.SafetyScore += 15
	case liq.GreaterThan(decimal.NewFromInt(10000)):
		analysis.Flags = append(analysis.Flags, SafetyFlag{
			Code:        "MODERATE_LIQUIDITY",
			Description: "Moderate liquidity $10k-$50k",
			Severity:    "positive",
			ScoreImpact: 10,
		})
		analysis.SafetyScore += 10
	case liq.GreaterThan(decimal.NewFromInt(2000)):
		analysis.Flags = append(analysis.Flags, SafetyFlag{
			Code:        "LOW_LIQUIDITY",
			Description: "Low liquidity $2k-$10k - higher slippage risk",
			Severity:    "info",
			ScoreImpact: 0,
		})
	default:
		analysis.Flags = append(analysis.Flags, SafetyFlag{
			Code:        "DUST_LIQUIDITY",
			Description: "Very low liquidity < $2k - extreme slippage",
			Severity:    "warning",
			ScoreImpact: -10,
		})
		analysis.SafetyScore -= 10
	}
}

// checkLPSafety checks if LP tokens are burned or locked.
func (a *Analyzer) checkLPSafety(analysis *TokenAnalysis) {
	if analysis.Pool.LPBurned {
		analysis.Flags = append(analysis.Flags, SafetyFlag{
			Code:        "LP_BURNED",
			Description: "LP tokens burned - liquidity permanently locked",
			Severity:    "positive",
			ScoreImpact: 20,
		})
		analysis.SafetyScore += 20
	} else if analysis.Pool.LPLocked {
		analysis.Flags = append(analysis.Flags, SafetyFlag{
			Code:        "LP_LOCKED",
			Description: "LP tokens locked in locker contract",
			Severity:    "positive",
			ScoreImpact: 15,
		})
		analysis.SafetyScore += 15
	} else {
		flag := SafetyFlag{
			Code:        "LP_UNLOCKED",
			Description: "LP tokens not locked - rug pull possible",
			Severity:    "warning",
			ScoreImpact: -10,
		}
		if a.config.RequireLPSafe {
			flag.Severity = "critical"
			flag.ScoreImpact = -25
		}
		analysis.Flags = append(analysis.Flags, flag)
		analysis.SafetyScore += flag.ScoreImpact
	}
}

// checkHolders analyzes top holder concentration.
func (a *Analyzer) checkHolders(ctx context.Context, analysis *TokenAnalysis) {
	holders, err := a.rpc.GetTopHolders(ctx, analysis.Mint, a.config.TopHoldersToCheck)
	if err != nil {
		analysis.Flags = append(analysis.Flags, SafetyFlag{
			Code:        "HOLDER_CHECK_FAILED",
			Description: "Could not fetch holder data",
			Severity:    "warning",
			ScoreImpact: -5,
		})
		analysis.SafetyScore -= 5
		return
	}

	ha := &HolderAnalysis{
		TopHolders:    holders,
		UniqueHolders: len(holders),
	}

	for _, h := range holders {
		ha.Top10Pct += h.Percentage
		if h.Percentage > ha.MaxSinglePct {
			ha.MaxSinglePct = h.Percentage
		}
		if h.IsCreator {
			ha.CreatorPct += h.Percentage
		}
	}

	analysis.HolderAnalysis = ha

	// Check top 10 concentration.
	if ha.Top10Pct > a.config.MaxTop10HolderPct {
		analysis.Flags = append(analysis.Flags, SafetyFlag{
			Code:        "HIGH_CONCENTRATION",
			Description: fmt.Sprintf("Top 10 holders own %.1f%% (max: %.1f%%)", ha.Top10Pct, a.config.MaxTop10HolderPct),
			Severity:    "critical",
			ScoreImpact: -20,
		})
		analysis.SafetyScore -= 20
	} else {
		analysis.Flags = append(analysis.Flags, SafetyFlag{
			Code:        "GOOD_DISTRIBUTION",
			Description: fmt.Sprintf("Top 10 holders own %.1f%%", ha.Top10Pct),
			Severity:    "positive",
			ScoreImpact: 10,
		})
		analysis.SafetyScore += 10
	}

	// Check single holder dominance.
	if ha.MaxSinglePct > a.config.MaxSingleHolderPct {
		analysis.Flags = append(analysis.Flags, SafetyFlag{
			Code:        "WHALE_HOLDER",
			Description: fmt.Sprintf("Single holder owns %.1f%% (max: %.1f%%)", ha.MaxSinglePct, a.config.MaxSingleHolderPct),
			Severity:    "warning",
			ScoreImpact: -15,
		})
		analysis.SafetyScore -= 15
	}

	// Check if creator holds too much.
	if ha.CreatorPct > 10.0 {
		analysis.Flags = append(analysis.Flags, SafetyFlag{
			Code:        "CREATOR_HEAVY",
			Description: fmt.Sprintf("Creator holds %.1f%% of supply", ha.CreatorPct),
			Severity:    "warning",
			ScoreImpact: -10,
		})
		analysis.SafetyScore -= 10
	}
}

// checkPoolAge evaluates how new the pool is.
func (a *Analyzer) checkPoolAge(analysis *TokenAnalysis) {
	age := time.Since(analysis.Pool.CreatedAt)

	switch {
	case age < 2*time.Minute:
		analysis.Flags = append(analysis.Flags, SafetyFlag{
			Code:        "VERY_NEW",
			Description: "Pool created < 2 minutes ago - highest risk/reward",
			Severity:    "info",
			ScoreImpact: 5, // Bonus for speed
		})
		analysis.SafetyScore += 5
	case age < 10*time.Minute:
		analysis.Flags = append(analysis.Flags, SafetyFlag{
			Code:        "FRESH",
			Description: "Pool created < 10 minutes ago",
			Severity:    "info",
			ScoreImpact: 0,
		})
	default:
		analysis.Flags = append(analysis.Flags, SafetyFlag{
			Code:        "AGING",
			Description: fmt.Sprintf("Pool is %.0f minutes old", age.Minutes()),
			Severity:    "info",
			ScoreImpact: -5,
		})
		analysis.SafetyScore -= 5
	}
}

// determineVerdict decides the final recommendation.
func (a *Analyzer) determineVerdict(analysis *TokenAnalysis) Verdict {
	// Check for critical flags that force a verdict.
	for _, flag := range analysis.Flags {
		if flag.Severity == "critical" {
			switch flag.Code {
			case "FREEZE_ACTIVE":
				return VerdictHoneypot
			case "HIGH_CONCENTRATION":
				if analysis.SafetyScore < 30 {
					return VerdictRug
				}
			}
		}
	}

	// Score-based verdict.
	switch {
	case analysis.SafetyScore >= a.config.MinSafetyScore:
		return VerdictBuy
	case analysis.SafetyScore >= 25:
		return VerdictWait
	default:
		return VerdictSkip
	}
}

