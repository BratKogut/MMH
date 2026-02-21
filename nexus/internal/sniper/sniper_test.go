package sniper

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nexus-trading/nexus/internal/adapters/jupiter"
	"github.com/nexus-trading/nexus/internal/scanner"
	"github.com/nexus-trading/nexus/internal/solana"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestEngine(t *testing.T) (*Engine, *solana.StubRPCClient) {
	t.Helper()

	rpc := solana.NewStubRPCClient()
	jupConfig := jupiter.DefaultConfig()
	jup := jupiter.New(jupConfig, rpc, solana.Pubkey("test-wallet"))
	require.NoError(t, jup.Connect(context.Background()))

	config := DefaultConfig()
	config.DryRun = true
	config.MaxBuySOL = 0.5
	config.MaxPositions = 3
	config.MaxDailySpendSOL = 5.0
	config.MaxDailyLossSOL = 2.0
	config.TakeProfitMultiplier = 2.0
	config.StopLossPct = 50
	config.MinSafetyScore = 40
	config.TrailingStopEnabled = true
	config.TrailingStopPct = 20

	engine := NewEngine(config, jup, rpc)
	return engine, rpc
}

func newTestAnalysis(mint string, safetyScore int, verdict scanner.Verdict, priceUSD float64) scanner.TokenAnalysis {
	return scanner.TokenAnalysis{
		Mint:        solana.Pubkey(mint),
		SafetyScore: safetyScore,
		Verdict:     verdict,
		Pool: solana.PoolInfo{
			PoolAddress:  solana.Pubkey("pool-" + mint),
			DEX:          "raydium",
			TokenMint:    solana.Pubkey(mint),
			QuoteMint:    solana.SOLMint,
			LiquidityUSD: decimal.NewFromFloat(10000),
			PriceUSD:     decimal.NewFromFloat(priceUSD),
		},
		AnalyzedAt: time.Now(),
	}
}

func TestSniper_BuyOnGoodAnalysis(t *testing.T) {
	engine, _ := newTestEngine(t)

	var mu sync.Mutex
	var opened []*Position
	engine.SetOnPositionOpen(func(pos *Position) {
		mu.Lock()
		opened = append(opened, pos)
		mu.Unlock()
	})

	ctx := context.Background()
	analysis := newTestAnalysis("good-token", 65, scanner.VerdictBuy, 0.001)

	engine.OnDiscovery(ctx, analysis)

	mu.Lock()
	require.Len(t, opened, 1)
	assert.Equal(t, solana.Pubkey("good-token"), opened[0].TokenMint)
	assert.Equal(t, StatusOpen, opened[0].Status)
	assert.True(t, opened[0].CostSOL.Equal(decimal.NewFromFloat(0.5)))
	mu.Unlock()

	stats := engine.Stats()
	assert.Equal(t, int64(1), stats.TotalSnipes)
	assert.Equal(t, 1, stats.OpenPositions)
}

func TestSniper_SkipsLowSafetyScore(t *testing.T) {
	engine, _ := newTestEngine(t)

	var mu sync.Mutex
	var opened []*Position
	engine.SetOnPositionOpen(func(pos *Position) {
		mu.Lock()
		opened = append(opened, pos)
		mu.Unlock()
	})

	ctx := context.Background()
	analysis := newTestAnalysis("unsafe-token", 20, scanner.VerdictSkip, 0.001)

	engine.OnDiscovery(ctx, analysis)

	mu.Lock()
	assert.Empty(t, opened, "Should not snipe low safety score token")
	mu.Unlock()
}

func TestSniper_SkipsNonBuyVerdict(t *testing.T) {
	engine, _ := newTestEngine(t)

	ctx := context.Background()

	// Even with high safety score, WAIT verdict should not trigger buy.
	analysis := newTestAnalysis("wait-token", 60, scanner.VerdictWait, 0.001)
	engine.OnDiscovery(ctx, analysis)

	assert.Empty(t, engine.OpenPositions())
}

func TestSniper_RespectsMaxPositions(t *testing.T) {
	engine, _ := newTestEngine(t)
	ctx := context.Background()

	// Fill up to max positions (3).
	for i := 0; i < 3; i++ {
		analysis := newTestAnalysis(
			"token-"+string(rune('a'+i)),
			65,
			scanner.VerdictBuy,
			0.001,
		)
		engine.OnDiscovery(ctx, analysis)
	}

	assert.Len(t, engine.OpenPositions(), 3)

	// Fourth should be rejected.
	analysis := newTestAnalysis("token-d", 70, scanner.VerdictBuy, 0.001)
	engine.OnDiscovery(ctx, analysis)

	assert.Len(t, engine.OpenPositions(), 3, "Should not exceed max positions")
}

func TestSniper_TakeProfit(t *testing.T) {
	engine, _ := newTestEngine(t)

	var mu sync.Mutex
	var closed []*Position
	engine.SetOnPositionClose(func(pos *Position) {
		mu.Lock()
		closed = append(closed, pos)
		mu.Unlock()
	})

	ctx := context.Background()
	analysis := newTestAnalysis("tp-token", 65, scanner.VerdictBuy, 0.001)
	engine.OnDiscovery(ctx, analysis)

	require.Len(t, engine.OpenPositions(), 1)

	// Multi-level TP: cycle through all levels at 6x price.
	// Each UpdatePrice triggers one TP level sequentially.
	for i := 0; i < 4; i++ {
		engine.UpdatePrice(ctx, solana.Pubkey("tp-token"), decimal.NewFromFloat(0.006))
		time.Sleep(20 * time.Millisecond)
	}

	// L4 should trigger full close.
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	require.Len(t, closed, 1, "L4 full close should fire")
	assert.Equal(t, "TAKE_PROFIT_L4", closed[0].CloseReason)
	mu.Unlock()
}

func TestSniper_StopLoss(t *testing.T) {
	engine, _ := newTestEngine(t)

	var mu sync.Mutex
	var closed []*Position
	engine.SetOnPositionClose(func(pos *Position) {
		mu.Lock()
		closed = append(closed, pos)
		mu.Unlock()
	})

	ctx := context.Background()
	analysis := newTestAnalysis("sl-token", 65, scanner.VerdictBuy, 0.01)
	engine.OnDiscovery(ctx, analysis)

	// Price drops 60% → below 50% stop loss.
	engine.UpdatePrice(ctx, solana.Pubkey("sl-token"), decimal.NewFromFloat(0.004))

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	require.Len(t, closed, 1)
	assert.Equal(t, "STOP_LOSS", closed[0].CloseReason)
	mu.Unlock()
}

func TestSniper_TrailingStop(t *testing.T) {
	engine, _ := newTestEngine(t)

	var mu sync.Mutex
	var closed []*Position
	engine.SetOnPositionClose(func(pos *Position) {
		mu.Lock()
		closed = append(closed, pos)
		mu.Unlock()
	})

	ctx := context.Background()
	analysis := newTestAnalysis("trail-token", 65, scanner.VerdictBuy, 0.01)
	engine.OnDiscovery(ctx, analysis)

	// Price rises to 0.018 (80% gain).
	engine.UpdatePrice(ctx, solana.Pubkey("trail-token"), decimal.NewFromFloat(0.018))
	time.Sleep(10 * time.Millisecond) // No sell yet

	mu.Lock()
	assert.Empty(t, closed, "Should not trigger trailing stop yet")
	mu.Unlock()

	// Price drops 25% from high (0.018 → 0.0135). Trailing stop is 20% so it triggers.
	engine.UpdatePrice(ctx, solana.Pubkey("trail-token"), decimal.NewFromFloat(0.0135))

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	require.Len(t, closed, 1)
	assert.Equal(t, "TRAILING_STOP", closed[0].CloseReason)
	mu.Unlock()
}

func TestSniper_DailySpendLimit(t *testing.T) {
	engine, _ := newTestEngine(t)
	engine.config.MaxDailySpendSOL = 1.0 // 1 SOL daily limit
	engine.config.MaxBuySOL = 0.5        // 0.5 SOL per trade

	ctx := context.Background()

	// First buy: 0.5 SOL.
	engine.OnDiscovery(ctx, newTestAnalysis("t1", 65, scanner.VerdictBuy, 0.001))
	assert.Len(t, engine.OpenPositions(), 1)

	// Second buy: 1.0 SOL total.
	engine.OnDiscovery(ctx, newTestAnalysis("t2", 65, scanner.VerdictBuy, 0.001))
	assert.Len(t, engine.OpenPositions(), 2)

	// Third should be blocked by daily spend limit.
	engine.OnDiscovery(ctx, newTestAnalysis("t3", 65, scanner.VerdictBuy, 0.001))
	assert.Len(t, engine.OpenPositions(), 2, "Should respect daily spend limit")
}

func TestSniper_ForceClose(t *testing.T) {
	engine, _ := newTestEngine(t)

	var mu sync.Mutex
	var closed []*Position
	engine.SetOnPositionClose(func(pos *Position) {
		mu.Lock()
		closed = append(closed, pos)
		mu.Unlock()
	})

	ctx := context.Background()

	// Open 2 positions.
	engine.OnDiscovery(ctx, newTestAnalysis("fc1", 65, scanner.VerdictBuy, 0.01))
	engine.OnDiscovery(ctx, newTestAnalysis("fc2", 65, scanner.VerdictBuy, 0.01))
	assert.Len(t, engine.OpenPositions(), 2)

	// Force close all.
	engine.ForceClose(ctx)

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	assert.Len(t, closed, 2, "Both positions should be force closed")
	for _, p := range closed {
		assert.Equal(t, "FORCE_CLOSE", p.CloseReason)
	}
	mu.Unlock()
}

func TestSniper_Stats(t *testing.T) {
	engine, _ := newTestEngine(t)
	ctx := context.Background()

	engine.OnDiscovery(ctx, newTestAnalysis("s1", 65, scanner.VerdictBuy, 0.01))

	stats := engine.Stats()
	assert.Equal(t, int64(1), stats.TotalSnipes)
	assert.Equal(t, 1, stats.OpenPositions)
	assert.True(t, stats.DryRun)
	assert.Equal(t, "0.5", stats.DailySpentSOL)
}
