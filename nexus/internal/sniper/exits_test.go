package sniper

import (
	"context"
	"testing"
	"time"

	"github.com/nexus-trading/nexus/internal/solana"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestPosition(entryPrice, currentPrice float64) *Position {
	return &Position{
		ID:            "test-pos-1",
		TokenMint:     solana.Pubkey("test-mint"),
		EntryPriceUSD: decimal.NewFromFloat(entryPrice),
		CurrentPrice:  decimal.NewFromFloat(currentPrice),
		HighestPrice:  decimal.NewFromFloat(currentPrice),
		AmountToken:   decimal.NewFromFloat(1000),
		CostSOL:       decimal.NewFromFloat(0.5),
		Status:        StatusOpen,
		OpenedAt:      time.Now(),
	}
}

func TestExitEngine_StopLoss(t *testing.T) {
	ee := NewExitEngine(DefaultExitConfig())
	pos := newTestPosition(1.0, 0.4) // -60%
	tracker := NewExitTracker(pos.ID, pos.AmountToken, DefaultExitConfig())

	decision := ee.Evaluate(pos, tracker)

	require.True(t, decision.ShouldSell)
	assert.Equal(t, "STOP_LOSS", decision.Reason)
	assert.True(t, decision.IsFullClose)
	assert.Equal(t, 100.0, decision.SellPct)
}

func TestExitEngine_StopLoss_NotTriggered(t *testing.T) {
	ee := NewExitEngine(DefaultExitConfig())
	pos := newTestPosition(1.0, 0.7) // -30%, SL is at -50%
	tracker := NewExitTracker(pos.ID, pos.AmountToken, DefaultExitConfig())

	decision := ee.Evaluate(pos, tracker)

	assert.False(t, decision.ShouldSell)
}

func TestExitEngine_MultiLevelTP(t *testing.T) {
	config := DefaultExitConfig()
	ee := NewExitEngine(config)

	t.Run("TP Level 1: +50%", func(t *testing.T) {
		pos := newTestPosition(1.0, 1.5)
		tracker := NewExitTracker(pos.ID, pos.AmountToken, config)

		decision := ee.Evaluate(pos, tracker)

		require.True(t, decision.ShouldSell)
		assert.Equal(t, "TAKE_PROFIT_L1", decision.Reason)
		assert.Equal(t, 25.0, decision.SellPct)
		assert.False(t, decision.IsFullClose)

		// Apply and check remaining.
		ee.ApplyDecision(tracker, decision)
		assert.InDelta(t, 75.0, tracker.RemainingPct, 0.1)
		assert.True(t, tracker.TPLevelsHit[0])
	})

	t.Run("TP Level 2: +100%", func(t *testing.T) {
		pos := newTestPosition(1.0, 2.0)
		tracker := NewExitTracker(pos.ID, pos.AmountToken, config)
		tracker.TPLevelsHit[0] = true // L1 already hit

		decision := ee.Evaluate(pos, tracker)

		require.True(t, decision.ShouldSell)
		assert.Equal(t, "TAKE_PROFIT_L2", decision.Reason)
		assert.Equal(t, 25.0, decision.SellPct)
	})

	t.Run("TP Level 4: +500% full close", func(t *testing.T) {
		pos := newTestPosition(1.0, 6.0)
		tracker := NewExitTracker(pos.ID, pos.AmountToken, config)
		tracker.TPLevelsHit[0] = true
		tracker.TPLevelsHit[1] = true
		tracker.TPLevelsHit[2] = true

		decision := ee.Evaluate(pos, tracker)

		require.True(t, decision.ShouldSell)
		assert.Equal(t, "TAKE_PROFIT_L4", decision.Reason)
		assert.Equal(t, 100.0, decision.SellPct)
		assert.True(t, decision.IsFullClose)
	})
}

func TestExitEngine_TrailingStop_RequiresTPHit(t *testing.T) {
	config := DefaultExitConfig()
	ee := NewExitEngine(config)

	pos := newTestPosition(1.0, 1.3)
	pos.HighestPrice = decimal.NewFromFloat(1.7) // high was 1.7, now at 1.3 (-23%)
	tracker := NewExitTracker(pos.ID, pos.AmountToken, config)

	// No TP hit yet -> trailing stop should NOT activate.
	decision := ee.Evaluate(pos, tracker)
	assert.False(t, decision.ShouldSell, "Trailing stop should not trigger without TP hit")
}

func TestExitEngine_TrailingStop_AfterTP(t *testing.T) {
	config := DefaultExitConfig()
	ee := NewExitEngine(config)

	pos := newTestPosition(1.0, 1.3)
	pos.HighestPrice = decimal.NewFromFloat(1.7) // dropped 23% from high
	tracker := NewExitTracker(pos.ID, pos.AmountToken, config)
	tracker.TPLevelsHit[0] = true // L1 already hit

	decision := ee.Evaluate(pos, tracker)

	require.True(t, decision.ShouldSell)
	assert.Equal(t, "TRAILING_STOP", decision.Reason)
	assert.True(t, decision.IsFullClose)
}

func TestExitEngine_TimedExit_4H(t *testing.T) {
	config := DefaultExitConfig()
	ee := NewExitEngine(config)

	pos := newTestPosition(1.0, 1.1) // slightly profitable
	pos.OpenedAt = time.Now().Add(-5 * time.Hour) // 5 hours old
	tracker := NewExitTracker(pos.ID, pos.AmountToken, config)

	decision := ee.Evaluate(pos, tracker)

	require.True(t, decision.ShouldSell)
	assert.Equal(t, "TIMED_EXIT_4H", decision.Reason)
	assert.Equal(t, 50.0, decision.SellPct)
}

func TestExitEngine_TimedExit_24H_FullClose(t *testing.T) {
	config := DefaultExitConfig()
	ee := NewExitEngine(config)

	pos := newTestPosition(1.0, 1.1)
	pos.OpenedAt = time.Now().Add(-25 * time.Hour)
	tracker := NewExitTracker(pos.ID, pos.AmountToken, config)
	tracker.TimedExitsHit[0] = true // 4h already hit
	tracker.TimedExitsHit[1] = true // 12h already hit

	decision := ee.Evaluate(pos, tracker)

	require.True(t, decision.ShouldSell)
	assert.Equal(t, "TIMED_EXIT_24H", decision.Reason)
	assert.True(t, decision.IsFullClose)
}

func TestExitEngine_PanicSell(t *testing.T) {
	config := DefaultExitConfig()
	ee := NewExitEngine(config)

	pos := newTestPosition(1.0, 0.8)
	tracker := NewExitTracker(pos.ID, pos.AmountToken, config)

	decision := ee.PanicSell(context.Background(), pos, tracker, "CSM_LIQUIDITY_PANIC")

	require.True(t, decision.ShouldSell)
	assert.Equal(t, "CSM_LIQUIDITY_PANIC", decision.Reason)
	assert.True(t, decision.IsFullClose)
	assert.Equal(t, 100.0, decision.SellPct)
}

func TestExitEngine_ApplyDecision_Partial(t *testing.T) {
	config := DefaultExitConfig()
	ee := NewExitEngine(config)

	tracker := NewExitTracker("pos-1", decimal.NewFromFloat(1000), config)

	// Apply L1 (25% sell).
	decision := ExitDecision{
		ShouldSell: true,
		SellPct:    25,
		Reason:     "TAKE_PROFIT_L1",
	}
	ee.ApplyDecision(tracker, decision)

	assert.InDelta(t, 75.0, tracker.RemainingPct, 0.1)
	assert.True(t, tracker.TPLevelsHit[0])

	// Apply L2 (25% of remaining = 25% of 75% = 18.75% of original).
	decision2 := ExitDecision{
		ShouldSell: true,
		SellPct:    25,
		Reason:     "TAKE_PROFIT_L2",
	}
	ee.ApplyDecision(tracker, decision2)

	assert.InDelta(t, 56.25, tracker.RemainingPct, 0.1)
	assert.True(t, tracker.TPLevelsHit[1])
}

func TestExitEngine_StopLossBeforeTP(t *testing.T) {
	// Stop loss takes priority over everything.
	config := DefaultExitConfig()
	ee := NewExitEngine(config)

	pos := newTestPosition(1.0, 0.3) // -70%, below SL
	pos.OpenedAt = time.Now().Add(-25 * time.Hour) // also past 24h timed exit
	tracker := NewExitTracker(pos.ID, pos.AmountToken, config)

	decision := ee.Evaluate(pos, tracker)

	require.True(t, decision.ShouldSell)
	assert.Equal(t, "STOP_LOSS", decision.Reason, "SL should take priority over timed exit")
}

func TestExitEngine_ClosedPosition_NoDecision(t *testing.T) {
	config := DefaultExitConfig()
	ee := NewExitEngine(config)

	pos := newTestPosition(1.0, 0.3)
	pos.Status = StatusClosed
	tracker := NewExitTracker(pos.ID, pos.AmountToken, config)

	decision := ee.Evaluate(pos, tracker)

	assert.False(t, decision.ShouldSell)
}
