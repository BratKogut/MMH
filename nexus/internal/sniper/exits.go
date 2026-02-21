package sniper

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Advanced Exit Logic — multi-level TP, panic exits, time-based exits
// Plan v3.1 L5-L6: 4-level TP, trailing stop, panic sell, time decay
// ---------------------------------------------------------------------------

// TPLevel defines a take-profit tier with partial sell.
type TPLevel struct {
	Multiplier float64 `yaml:"multiplier"` // e.g. 1.5 = +50%
	SellPct    float64 `yaml:"sell_pct"`   // % of remaining position to sell
}

// ExitConfig configures the advanced exit engine.
type ExitConfig struct {
	// Multi-level take profit (replaces single TakeProfitMultiplier).
	TPLevels []TPLevel `yaml:"tp_levels"`

	// Stop loss.
	StopLossPct float64 `yaml:"stop_loss_pct"` // e.g. 50 = sell at -50%

	// Trailing stop.
	TrailingStopEnabled bool    `yaml:"trailing_stop_enabled"`
	TrailingStopPct     float64 `yaml:"trailing_stop_pct"` // e.g. 20 = trail 20% from high

	// Time-based exits.
	TimedExits []TimedExit `yaml:"timed_exits"`

	// Panic exit thresholds.
	PanicLiqDropPct   float64 `yaml:"panic_liq_drop_pct"`   // liquidity drop % (default 50)
	PanicWhaleSellSOL float64 `yaml:"panic_whale_sell_sol"`  // whale sell size (default 100 SOL)
}

// TimedExit defines a time-based forced exit.
type TimedExit struct {
	AfterMinutes int     `yaml:"after_minutes"` // close after N minutes
	SellPct      float64 `yaml:"sell_pct"`      // sell this % of remaining position
}

// DefaultExitConfig returns plan v3.1 exit configuration.
func DefaultExitConfig() ExitConfig {
	return ExitConfig{
		TPLevels: []TPLevel{
			{Multiplier: 1.5, SellPct: 25},  // +50%: sell 25%
			{Multiplier: 2.0, SellPct: 25},  // +100%: sell 25%
			{Multiplier: 3.0, SellPct: 25},  // +200%: sell 25%
			{Multiplier: 6.0, SellPct: 100}, // +500%: sell remaining
		},
		StopLossPct:         50,
		TrailingStopEnabled: true,
		TrailingStopPct:     20,
		TimedExits: []TimedExit{
			{AfterMinutes: 240, SellPct: 50},  // 4h: sell 50%
			{AfterMinutes: 720, SellPct: 75},  // 12h: sell 75%
			{AfterMinutes: 1440, SellPct: 100}, // 24h: sell all
		},
		PanicLiqDropPct:   50,
		PanicWhaleSellSOL: 100,
	}
}

// ExitTracker tracks exit state for a position (which TP levels hit, partial sells done).
type ExitTracker struct {
	PositionID     string          `json:"position_id"`
	TPLevelsHit    []bool          `json:"tp_levels_hit"`     // which levels have been triggered
	TimedExitsHit  []bool          `json:"timed_exits_hit"`   // which timed exits have been triggered
	RemainingPct   float64         `json:"remaining_pct"`     // % of original position remaining
	OriginalAmount decimal.Decimal `json:"original_amount"`   // original token amount
}

// NewExitTracker creates a new exit tracker for a position.
func NewExitTracker(posID string, tokenAmount decimal.Decimal, config ExitConfig) *ExitTracker {
	return &ExitTracker{
		PositionID:     posID,
		TPLevelsHit:    make([]bool, len(config.TPLevels)),
		TimedExitsHit:  make([]bool, len(config.TimedExits)),
		RemainingPct:   100.0,
		OriginalAmount: tokenAmount,
	}
}

// ExitDecision represents what the exit engine wants to do.
type ExitDecision struct {
	ShouldSell  bool            // should we sell?
	SellAmount  decimal.Decimal // how much to sell
	SellPct     float64         // what % of remaining
	Reason      string          // TAKE_PROFIT_L1, STOP_LOSS, TRAILING_STOP, TIMED_EXIT, PANIC_*
	IsFullClose bool            // is this closing the entire position?
}

// ExitEngine evaluates exit conditions for a position.
type ExitEngine struct {
	config ExitConfig
}

// NewExitEngine creates a new exit engine.
func NewExitEngine(config ExitConfig) *ExitEngine {
	return &ExitEngine{config: config}
}

// Evaluate checks all exit conditions and returns an ExitDecision.
func (ee *ExitEngine) Evaluate(pos *Position, tracker *ExitTracker) ExitDecision {
	if pos.Status != StatusOpen || tracker.RemainingPct <= 0 {
		return ExitDecision{}
	}

	// Priority 1: Stop loss (always full close).
	if decision := ee.checkStopLoss(pos, tracker); decision.ShouldSell {
		return decision
	}

	// Priority 2: Multi-level take profit.
	if decision := ee.checkMultiTP(pos, tracker); decision.ShouldSell {
		return decision
	}

	// Priority 3: Trailing stop (only if we've passed at least TP level 1).
	if decision := ee.checkTrailingStop(pos, tracker); decision.ShouldSell {
		return decision
	}

	// Priority 4: Time-based exits.
	if decision := ee.checkTimedExits(pos, tracker); decision.ShouldSell {
		return decision
	}

	return ExitDecision{}
}

// checkStopLoss checks if we've hit the stop loss.
func (ee *ExitEngine) checkStopLoss(pos *Position, tracker *ExitTracker) ExitDecision {
	if ee.config.StopLossPct <= 0 || !pos.EntryPriceUSD.IsPositive() {
		return ExitDecision{}
	}

	slPrice := pos.EntryPriceUSD.Mul(decimal.NewFromFloat(1.0 - ee.config.StopLossPct/100.0))
	if pos.CurrentPrice.LessThanOrEqual(slPrice) {
		sellAmount := ee.remainingTokens(tracker)
		return ExitDecision{
			ShouldSell:  true,
			SellAmount:  sellAmount,
			SellPct:     100,
			Reason:      "STOP_LOSS",
			IsFullClose: true,
		}
	}
	return ExitDecision{}
}

// checkMultiTP checks multi-level take profit targets.
func (ee *ExitEngine) checkMultiTP(pos *Position, tracker *ExitTracker) ExitDecision {
	if !pos.EntryPriceUSD.IsPositive() {
		return ExitDecision{}
	}

	for i, level := range ee.config.TPLevels {
		if tracker.TPLevelsHit[i] {
			continue
		}

		tpPrice := pos.EntryPriceUSD.Mul(decimal.NewFromFloat(level.Multiplier))
		if pos.CurrentPrice.GreaterThanOrEqual(tpPrice) {
			sellPct := level.SellPct
			remaining := ee.remainingTokens(tracker)
			sellAmount := remaining.Mul(decimal.NewFromFloat(sellPct / 100.0))

			isFullClose := sellPct >= 100 || tracker.RemainingPct*(1-sellPct/100.0) < 1.0

			return ExitDecision{
				ShouldSell:  true,
				SellAmount:  sellAmount,
				SellPct:     sellPct,
				Reason:      tpLevelReason(i),
				IsFullClose: isFullClose,
			}
		}
	}
	return ExitDecision{}
}

// checkTrailingStop checks trailing stop, active only after first TP level.
func (ee *ExitEngine) checkTrailingStop(pos *Position, tracker *ExitTracker) ExitDecision {
	if !ee.config.TrailingStopEnabled || !pos.HighestPrice.IsPositive() {
		return ExitDecision{}
	}

	// Only activate trailing stop after at least one TP level hit.
	anyTPHit := false
	for _, hit := range tracker.TPLevelsHit {
		if hit {
			anyTPHit = true
			break
		}
	}
	if !anyTPHit {
		return ExitDecision{}
	}

	trailDist := pos.HighestPrice.Mul(decimal.NewFromFloat(ee.config.TrailingStopPct / 100.0))
	trailStop := pos.HighestPrice.Sub(trailDist)

	// Only trigger if still in profit.
	if pos.CurrentPrice.LessThanOrEqual(trailStop) && pos.CurrentPrice.GreaterThan(pos.EntryPriceUSD) {
		sellAmount := ee.remainingTokens(tracker)
		return ExitDecision{
			ShouldSell:  true,
			SellAmount:  sellAmount,
			SellPct:     100,
			Reason:      "TRAILING_STOP",
			IsFullClose: true,
		}
	}
	return ExitDecision{}
}

// checkTimedExits checks time-based position expiry.
func (ee *ExitEngine) checkTimedExits(pos *Position, tracker *ExitTracker) ExitDecision {
	age := time.Since(pos.OpenedAt)

	for i, te := range ee.config.TimedExits {
		if tracker.TimedExitsHit[i] {
			continue
		}

		deadline := time.Duration(te.AfterMinutes) * time.Minute
		if age >= deadline {
			sellPct := te.SellPct
			remaining := ee.remainingTokens(tracker)
			sellAmount := remaining.Mul(decimal.NewFromFloat(sellPct / 100.0))

			isFullClose := sellPct >= 100 || tracker.RemainingPct*(1-sellPct/100.0) < 1.0

			return ExitDecision{
				ShouldSell:  true,
				SellAmount:  sellAmount,
				SellPct:     sellPct,
				Reason:      timedExitReason(te.AfterMinutes),
				IsFullClose: isFullClose,
			}
		}
	}
	return ExitDecision{}
}

// PanicSell checks for panic exit conditions.
func (ee *ExitEngine) PanicSell(_ context.Context, pos *Position, tracker *ExitTracker, reason string) ExitDecision {
	sellAmount := ee.remainingTokens(tracker)

	log.Warn().
		Str("pos_id", pos.ID).
		Str("mint", string(pos.TokenMint)).
		Str("reason", reason).
		Str("sell_amount", sellAmount.String()).
		Msg("exit: PANIC SELL triggered")

	return ExitDecision{
		ShouldSell:  true,
		SellAmount:  sellAmount,
		SellPct:     100,
		Reason:      reason,
		IsFullClose: true,
	}
}

// ApplyDecision updates the tracker after a successful partial/full sell.
func (ee *ExitEngine) ApplyDecision(tracker *ExitTracker, decision ExitDecision) {
	if decision.IsFullClose {
		tracker.RemainingPct = 0
		return
	}

	// Reduce remaining by sold percentage.
	soldFraction := decision.SellPct / 100.0
	tracker.RemainingPct *= (1 - soldFraction)

	// Mark TP level as hit.
	for i := range ee.config.TPLevels {
		if decision.Reason == tpLevelReason(i) {
			tracker.TPLevelsHit[i] = true
		}
	}

	// Mark timed exit as hit.
	for i, te := range ee.config.TimedExits {
		if decision.Reason == timedExitReason(te.AfterMinutes) {
			tracker.TimedExitsHit[i] = true
		}
	}
}

// remainingTokens calculates the remaining token amount.
func (ee *ExitEngine) remainingTokens(tracker *ExitTracker) decimal.Decimal {
	return tracker.OriginalAmount.Mul(decimal.NewFromFloat(tracker.RemainingPct / 100.0))
}

func tpLevelReason(level int) string {
	return "TAKE_PROFIT_L" + string(rune('1'+level))
}

func timedExitReason(minutes int) string {
	switch {
	case minutes >= 1440:
		return "TIMED_EXIT_24H"
	case minutes >= 720:
		return "TIMED_EXIT_12H"
	case minutes >= 240:
		return "TIMED_EXIT_4H"
	default:
		return "TIMED_EXIT"
	}
}
