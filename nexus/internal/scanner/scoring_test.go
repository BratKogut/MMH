package scanner

import (
	"testing"
	"time"

	"github.com/nexus-trading/nexus/internal/copytrade"
	"github.com/nexus-trading/nexus/internal/correlation"
	"github.com/nexus-trading/nexus/internal/graph"
	"github.com/nexus-trading/nexus/internal/honeypot"
	"github.com/nexus-trading/nexus/internal/liquidity"
	"github.com/nexus-trading/nexus/internal/narrative"
	"github.com/nexus-trading/nexus/internal/solana"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
)

func newTestScoringInput() ScoringInput {
	return ScoringInput{
		Analysis: TokenAnalysis{
			Mint:        solana.Pubkey("test-mint"),
			SafetyScore: 70,
			Verdict:     VerdictBuy,
			Pool: solana.PoolInfo{
				PoolAddress:  solana.Pubkey("test-pool"),
				LiquidityUSD: decimal.NewFromFloat(25000),
				PriceUSD:     decimal.NewFromFloat(0.001),
				CreatedAt:    time.Now().Add(-2 * time.Minute),
			},
			HolderAnalysis: &HolderAnalysis{
				Top10Pct:      30,
				UniqueHolders: 100,
			},
		},
	}
}

func TestScorer_BasicScore(t *testing.T) {
	scorer := NewScorer(DefaultScoringConfig())
	input := newTestScoringInput()

	score := scorer.Score(input)

	assert.Greater(t, score.Total, 0.0)
	assert.LessOrEqual(t, score.Total, 100.0)
	assert.Greater(t, score.Safety, 0.0)
	assert.NotEmpty(t, score.Recommendation)
}

func TestScorer_InstantKill_SellSimFailed(t *testing.T) {
	scorer := NewScorer(DefaultScoringConfig())
	input := newTestScoringInput()
	input.SellSim = &SellSimResult{
		CanSell: false,
	}

	score := scorer.Score(input)

	assert.True(t, score.InstantKill)
	assert.Equal(t, 0.0, score.Total)
	assert.Equal(t, "SKIP", score.Recommendation)
	assert.Contains(t, score.KillReason, "sell_simulation_failed")
}

func TestScorer_InstantKill_HighTax(t *testing.T) {
	scorer := NewScorer(DefaultScoringConfig())
	input := newTestScoringInput()
	input.SellSim = &SellSimResult{
		CanSell:         true,
		EstimatedTaxPct: 15.0,
	}

	score := scorer.Score(input)

	assert.True(t, score.InstantKill)
	assert.Equal(t, "SKIP", score.Recommendation)
}

func TestScorer_InstantKill_Honeypot(t *testing.T) {
	scorer := NewScorer(DefaultScoringConfig())
	input := newTestScoringInput()
	input.Analysis.Verdict = VerdictHoneypot

	score := scorer.Score(input)

	assert.True(t, score.InstantKill)
	assert.Equal(t, "SKIP", score.Recommendation)
}

func TestScorer_SellSimPass_BonusScore(t *testing.T) {
	scorer := NewScorer(DefaultScoringConfig())
	input := newTestScoringInput()
	input.SellSim = &SellSimResult{
		CanSell:         true,
		EstimatedTaxPct: 2.0,
	}

	score := scorer.Score(input)

	assert.False(t, score.InstantKill)
	assert.Greater(t, score.Safety, float64(input.Analysis.SafetyScore))
}

func TestScorer_EntityDimension(t *testing.T) {
	scorer := NewScorer(DefaultScoringConfig())

	t.Run("nil report = neutral 50", func(t *testing.T) {
		input := newTestScoringInput()
		score := scorer.Score(input)
		assert.Equal(t, 50.0, score.Entity)
	})

	t.Run("1-hop to rugger = 0", func(t *testing.T) {
		input := newTestScoringInput()
		input.EntityReport = &graph.EntityReport{
			HopsToRugger:  1,
			HopsToInsider: -1,
		}
		score := scorer.Score(input)
		assert.Equal(t, 0.0, score.Entity)
	})

	t.Run("1-hop to insider = bonus", func(t *testing.T) {
		input := newTestScoringInput()
		input.EntityReport = &graph.EntityReport{
			HopsToRugger:  -1,
			HopsToInsider: 1,
		}
		score := scorer.Score(input)
		assert.Greater(t, score.Entity, 50.0)
	})

	t.Run("clean label = bonus", func(t *testing.T) {
		input := newTestScoringInput()
		input.EntityReport = &graph.EntityReport{
			HopsToRugger:  -1,
			HopsToInsider: -1,
			Labels:        []graph.Label{graph.LabelClean},
		}
		score := scorer.Score(input)
		assert.Greater(t, score.Entity, 70.0)
	})
}

func TestScorer_SocialDimension(t *testing.T) {
	scorer := NewScorer(DefaultScoringConfig())

	t.Run("no social = low baseline", func(t *testing.T) {
		input := newTestScoringInput()
		score := scorer.Score(input)
		assert.Equal(t, 30.0, score.Social)
	})

	t.Run("high social = high score", func(t *testing.T) {
		input := newTestScoringInput()
		input.SocialData = &SocialData{
			TelegramVelocity: 15,
			TwitterMentions:  60,
			DiscordMentions:  20,
			UniqueSources:    3,
			HasKOLMention:    true,
			Sentiment:        0.8,
		}
		score := scorer.Score(input)
		assert.Greater(t, score.Social, 80.0)
	})

	t.Run("negative sentiment penalizes", func(t *testing.T) {
		input := newTestScoringInput()
		input.SocialData = &SocialData{
			TelegramVelocity: 10,
			TwitterMentions:  30,
			Sentiment:        -0.5,
		}
		score := scorer.Score(input)
		assert.Less(t, score.Social, 30.0, "Negative sentiment should reduce score")
	})
}

func TestScorer_OnChainDimension(t *testing.T) {
	scorer := NewScorer(DefaultScoringConfig())

	t.Run("whale buy = bonus", func(t *testing.T) {
		input := newTestScoringInput()
		input.WhaleSignals = []WhaleSignal{
			{Type: "WHALE_BUY", Amount: 50},
		}
		score := scorer.Score(input)
		assert.Greater(t, score.OnChain, 50.0)
	})

	t.Run("whale sell = penalty", func(t *testing.T) {
		input := newTestScoringInput()
		input.WhaleSignals = []WhaleSignal{
			{Type: "WHALE_SELL", Amount: 100},
		}
		score := scorer.Score(input)
		assert.Less(t, score.OnChain, 30.0)
	})
}

func TestScorer_TimingDimension(t *testing.T) {
	scorer := NewScorer(DefaultScoringConfig())

	t.Run("very new = high score", func(t *testing.T) {
		input := newTestScoringInput()
		input.Analysis.Pool.CreatedAt = time.Now().Add(-1 * time.Minute)
		input.Analysis.Pool.LiquidityUSD = decimal.NewFromFloat(20000) // mcap=40k < 50k
		score := scorer.Score(input)
		assert.Greater(t, score.Timing, 70.0)
	})

	t.Run("old pool = lower timing", func(t *testing.T) {
		input := newTestScoringInput()
		input.Analysis.Pool.CreatedAt = time.Now().Add(-5 * time.Hour)
		input.Analysis.Pool.LiquidityUSD = decimal.NewFromFloat(3000000) // mcap=6M
		score := scorer.Score(input)
		assert.Equal(t, 30.0, score.Timing)
	})
}

func TestScorer_CorrelationBonus_PerfectStorm(t *testing.T) {
	scorer := NewScorer(DefaultScoringConfig())

	input := newTestScoringInput()
	input.WhaleSignals = []WhaleSignal{{Type: "WHALE_BUY", Amount: 50}}
	input.SocialData = &SocialData{TelegramVelocity: 10, UniqueSources: 3}
	input.EntityReport = &graph.EntityReport{
		HopsToRugger:  -1,
		HopsToInsider: 1,
		Labels:        []graph.Label{graph.LabelClean},
	}

	score := scorer.Score(input)
	assert.Equal(t, "PERFECT_STORM", score.CorrelationType)
	assert.Equal(t, 20.0, score.CorrelationBonus)
}

func TestScorer_Recommendation(t *testing.T) {
	config := DefaultScoringConfig()
	scorer := NewScorer(config)

	t.Run("high score = STRONG_BUY", func(t *testing.T) {
		input := newTestScoringInput()
		input.Analysis.SafetyScore = 95
		input.SellSim = &SellSimResult{CanSell: true, EstimatedTaxPct: 0}
		input.SocialData = &SocialData{TelegramVelocity: 15, TwitterMentions: 60, DiscordMentions: 20, UniqueSources: 3, HasKOLMention: true}
		input.WhaleSignals = []WhaleSignal{{Type: "WHALE_BUY", Amount: 50}, {Type: "SMART_MONEY_BUY", Amount: 30}}
		input.EntityReport = &graph.EntityReport{HopsToRugger: -1, HopsToInsider: 1, Labels: []graph.Label{graph.LabelClean}}
		input.Analysis.Pool.CreatedAt = time.Now().Add(-1 * time.Minute)
		input.Analysis.Pool.LiquidityUSD = decimal.NewFromFloat(20000)

		score := scorer.Score(input)
		assert.Equal(t, "STRONG_BUY", score.Recommendation)
	})

	t.Run("low score = SKIP", func(t *testing.T) {
		input := newTestScoringInput()
		input.Analysis.SafetyScore = 20
		input.WhaleSignals = []WhaleSignal{{Type: "WHALE_SELL", Amount: 100}}
		input.Analysis.Pool.CreatedAt = time.Now().Add(-5 * time.Hour)
		input.Analysis.Pool.LiquidityUSD = decimal.NewFromFloat(3000000)

		score := scorer.Score(input)
		assert.Equal(t, "SKIP", score.Recommendation)
	})
}

func TestScorer_TotalClamped(t *testing.T) {
	scorer := NewScorer(DefaultScoringConfig())

	input := newTestScoringInput()
	input.Analysis.SafetyScore = 100
	input.SellSim = &SellSimResult{CanSell: true, EstimatedTaxPct: 0}

	score := scorer.Score(input)

	assert.LessOrEqual(t, score.Total, 100.0)
	assert.GreaterOrEqual(t, score.Total, 0.0)
}

// ---------------------------------------------------------------------------
// v3.2 Instant-Kill Tests
// ---------------------------------------------------------------------------

func TestScorer_v32InstantKill_HoneypotHighConfidence(t *testing.T) {
	scorer := NewScorer(DefaultScoringConfig())
	input := newTestScoringInput()
	input.HoneypotMatch = &honeypot.Signature{
		PatternID:  "hp-001",
		Confidence: 0.8,
	}

	score := scorer.Score(input)

	assert.True(t, score.InstantKill)
	assert.Equal(t, 0.0, score.Total)
	assert.Equal(t, "SKIP", score.Recommendation)
	assert.Contains(t, score.KillReason, "honeypot_pattern_match")
}

func TestScorer_v32InstantKill_RugPrecursor(t *testing.T) {
	scorer := NewScorer(DefaultScoringConfig())
	input := newTestScoringInput()
	input.LiquidityFlow = &liquidity.LiquidityFlow{
		Pattern: liquidity.PatternRugPrecursor,
	}

	score := scorer.Score(input)

	assert.True(t, score.InstantKill)
	assert.Equal(t, 0.0, score.Total)
	assert.Contains(t, score.KillReason, "rug_precursor_detected")
}

func TestScorer_v32InstantKill_CrossTokenCritical(t *testing.T) {
	scorer := NewScorer(DefaultScoringConfig())
	input := newTestScoringInput()
	input.CrossTokenState = &correlation.CrossTokenState{
		RiskLevel: correlation.RiskCritical,
		Pattern:   correlation.PatternSerial,
	}

	score := scorer.Score(input)

	assert.True(t, score.InstantKill)
	assert.Equal(t, 0.0, score.Total)
	assert.Contains(t, score.KillReason, "cross_token_critical")
}

// ---------------------------------------------------------------------------
// v3.2 Score Adjustment Tests
// ---------------------------------------------------------------------------

func TestScorer_v32ScoreAdjust_LiquidityFlow(t *testing.T) {
	scorer := NewScorer(DefaultScoringConfig())

	t.Run("healthy growth boosts safety", func(t *testing.T) {
		input := newTestScoringInput()
		input.LiquidityFlow = &liquidity.LiquidityFlow{
			Pattern: liquidity.PatternHealthyGrowth,
		}

		withFlow := scorer.Score(input)

		input2 := newTestScoringInput()
		withoutFlow := scorer.Score(input2)

		assert.Greater(t, withFlow.Safety, withoutFlow.Safety)
	})

	t.Run("slow bleed penalizes safety", func(t *testing.T) {
		input := newTestScoringInput()
		input.LiquidityFlow = &liquidity.LiquidityFlow{
			Pattern: liquidity.PatternSlowBleed,
		}

		withFlow := scorer.Score(input)

		input2 := newTestScoringInput()
		withoutFlow := scorer.Score(input2)

		assert.Less(t, withFlow.Safety, withoutFlow.Safety)
	})
}

func TestScorer_v32ScoreAdjust_NarrativePhase(t *testing.T) {
	scorer := NewScorer(DefaultScoringConfig())

	t.Run("emerging narrative boosts timing", func(t *testing.T) {
		input := newTestScoringInput()
		input.NarrativeState = &narrative.NarrativeState{
			Phase: narrative.PhaseEmerging,
		}

		withNarr := scorer.Score(input)

		input2 := newTestScoringInput()
		withoutNarr := scorer.Score(input2)

		assert.Greater(t, withNarr.Timing, withoutNarr.Timing)
	})

	t.Run("dead narrative penalizes timing", func(t *testing.T) {
		input := newTestScoringInput()
		input.NarrativeState = &narrative.NarrativeState{
			Phase: narrative.PhaseDead,
		}

		withNarr := scorer.Score(input)

		input2 := newTestScoringInput()
		withoutNarr := scorer.Score(input2)

		assert.Less(t, withNarr.Timing, withoutNarr.Timing)
	})
}

func TestScorer_v32ScoreAdjust_CrossTokenCorrelation(t *testing.T) {
	scorer := NewScorer(DefaultScoringConfig())

	input := newTestScoringInput()
	input.CrossTokenState = &correlation.CrossTokenState{
		RiskLevel: correlation.RiskHigh,
		Pattern:   correlation.PatternRotation,
	}

	withCorr := scorer.Score(input)

	input2 := newTestScoringInput()
	withoutCorr := scorer.Score(input2)

	assert.Less(t, withCorr.Entity, withoutCorr.Entity)
	assert.Contains(t, withCorr.Reasons, "v32_correlation:ROTATION")
}

func TestScorer_v32ScoreAdjust_HoneypotPartial(t *testing.T) {
	scorer := NewScorer(DefaultScoringConfig())

	input := newTestScoringInput()
	input.HoneypotMatch = &honeypot.Signature{
		PatternID:  "hp-partial",
		Confidence: 0.5, // between 0.3 and 0.7 = partial penalty
	}

	withPartial := scorer.Score(input)

	input2 := newTestScoringInput()
	withoutPartial := scorer.Score(input2)

	assert.Less(t, withPartial.Safety, withoutPartial.Safety)
	assert.Contains(t, withPartial.Reasons, "v32_honeypot_partial:hp-partial")
}

func TestScorer_v32ScoreAdjust_CopyTrade(t *testing.T) {
	scorer := NewScorer(DefaultScoringConfig())

	t.Run("first move boosts onchain", func(t *testing.T) {
		input := newTestScoringInput()
		input.CopyTradeSignal = &copytrade.CopySignal{
			Signal:     copytrade.SignalFirstMove,
			Tier:       copytrade.TierWhale,
			Confidence: 0.8,
		}

		withSignal := scorer.Score(input)

		input2 := newTestScoringInput()
		withoutSignal := scorer.Score(input2)

		assert.Greater(t, withSignal.OnChain, withoutSignal.OnChain)
		assert.Contains(t, withSignal.Reasons, "v32_copytrade:FIRST_MOVE")
	})

	t.Run("dump penalizes onchain", func(t *testing.T) {
		input := newTestScoringInput()
		input.CopyTradeSignal = &copytrade.CopySignal{
			Signal:     copytrade.SignalDump,
			Tier:       copytrade.TierWhale,
			Confidence: 0.9,
		}

		withSignal := scorer.Score(input)

		input2 := newTestScoringInput()
		withoutSignal := scorer.Score(input2)

		assert.Less(t, withSignal.OnChain, withoutSignal.OnChain)
	})
}

func TestScorer_v32HoneypotBelowThreshold_NoKill(t *testing.T) {
	scorer := NewScorer(DefaultScoringConfig())
	input := newTestScoringInput()
	input.HoneypotMatch = &honeypot.Signature{
		PatternID:  "hp-low",
		Confidence: 0.2, // below 0.3 = no impact
	}

	score := scorer.Score(input)
	assert.False(t, score.InstantKill)
	assert.Greater(t, score.Total, 0.0)
}

func TestScorer_SetWeights(t *testing.T) {
	scorer := NewScorer(DefaultScoringConfig())

	newWeights := ScoringWeights{
		Safety: 0.50, Entity: 0.10, Social: 0.15, OnChain: 0.15, Timing: 0.10,
	}
	scorer.SetWeights(newWeights)

	got := scorer.GetWeights()
	assert.Equal(t, 0.50, got.Safety)
	assert.Equal(t, 0.10, got.Entity)

	// Verify Score uses new weights.
	input := newTestScoringInput()
	input.Analysis.SafetyScore = 100
	score := scorer.Score(input)
	assert.Greater(t, score.Total, 0.0)
}
