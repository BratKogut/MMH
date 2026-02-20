package scanner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nexus-trading/nexus/internal/solana"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Scanner Tests
// ---------------------------------------------------------------------------

func newTestPool(address string, mint string, liqUSD float64, ageSeconds int) solana.PoolInfo {
	return solana.PoolInfo{
		PoolAddress:  solana.Pubkey(address),
		DEX:          "raydium",
		TokenMint:    solana.Pubkey(mint),
		QuoteMint:    solana.SOLMint,
		TokenReserve: decimal.NewFromInt(1000000),
		QuoteReserve: decimal.NewFromFloat(10.0),
		LiquidityUSD: decimal.NewFromFloat(liqUSD),
		PriceUSD:     decimal.NewFromFloat(0.001),
		CreatedAt:    time.Now().Add(-time.Duration(ageSeconds) * time.Second),
	}
}

func newTestToken(mint string, mintRenounced, freezeRenounced bool) solana.TokenInfo {
	info := solana.TokenInfo{
		Mint:     solana.Pubkey(mint),
		Symbol:   "TEST",
		Name:     "Test Token",
		Decimals: 9,
		Supply:   decimal.NewFromInt(1000000000),
	}
	if !mintRenounced {
		info.MintAuthority = solana.Pubkey("CreatorAddr123")
	}
	if !freezeRenounced {
		info.FreezeAuthority = solana.Pubkey("CreatorAddr123")
	}
	return info
}

func TestScanner_FiltersLowLiquidity(t *testing.T) {
	rpc := solana.NewStubRPCClient()

	var mu sync.Mutex
	var discoveries []PoolDiscovery

	config := DefaultScannerConfig()
	config.MinLiquidityUSD = 1000

	s := NewScanner(config, rpc, func(_ context.Context, d PoolDiscovery) {
		mu.Lock()
		discoveries = append(discoveries, d)
		mu.Unlock()
	})

	ctx := context.Background()

	// Low liquidity pool should be rejected.
	lowLiqPool := newTestPool("pool-1", "token-1", 500, 60)
	s.handlePool(ctx, lowLiqPool, "test")

	mu.Lock()
	assert.Empty(t, discoveries, "Low liquidity pool should be rejected")
	mu.Unlock()

	assert.Equal(t, int64(1), s.poolsRejected.Load())
}

func TestScanner_AcceptsGoodPool(t *testing.T) {
	rpc := solana.NewStubRPCClient()

	token := newTestToken("mint-good", true, true)
	rpc.AddToken(token)

	var mu sync.Mutex
	var discoveries []PoolDiscovery

	config := DefaultScannerConfig()
	config.MinLiquidityUSD = 500

	s := NewScanner(config, rpc, func(_ context.Context, d PoolDiscovery) {
		mu.Lock()
		discoveries = append(discoveries, d)
		mu.Unlock()
	})

	ctx := context.Background()

	goodPool := newTestPool("pool-good", "mint-good", 5000, 120)
	s.handlePool(ctx, goodPool, "test")

	mu.Lock()
	require.Len(t, discoveries, 1)
	assert.Equal(t, solana.Pubkey("pool-good"), discoveries[0].Pool.PoolAddress)
	assert.NotNil(t, discoveries[0].Token)
	mu.Unlock()

	assert.Equal(t, int64(1), s.poolsAccepted.Load())
}

func TestScanner_DeduplicatesPools(t *testing.T) {
	rpc := solana.NewStubRPCClient()

	var mu sync.Mutex
	var discoveries []PoolDiscovery

	config := DefaultScannerConfig()
	s := NewScanner(config, rpc, func(_ context.Context, d PoolDiscovery) {
		mu.Lock()
		discoveries = append(discoveries, d)
		mu.Unlock()
	})

	ctx := context.Background()

	pool := newTestPool("pool-dup", "mint-dup", 5000, 60)

	// Send same pool twice.
	s.handlePool(ctx, pool, "test")
	s.handlePool(ctx, pool, "test")

	mu.Lock()
	assert.Len(t, discoveries, 1, "Duplicate pool should be filtered")
	mu.Unlock()
}

func TestScanner_RejectsOldPool(t *testing.T) {
	rpc := solana.NewStubRPCClient()

	var mu sync.Mutex
	var discoveries []PoolDiscovery

	config := DefaultScannerConfig()
	config.MaxTokenAgeMinutes = 5

	s := NewScanner(config, rpc, func(_ context.Context, d PoolDiscovery) {
		mu.Lock()
		discoveries = append(discoveries, d)
		mu.Unlock()
	})

	ctx := context.Background()

	// Pool older than 5 minutes.
	oldPool := newTestPool("pool-old", "mint-old", 5000, 600)
	s.handlePool(ctx, oldPool, "test")

	mu.Lock()
	assert.Empty(t, discoveries, "Old pool should be rejected")
	mu.Unlock()
}

func TestScanner_WebsocketSubscription(t *testing.T) {
	rpc := solana.NewStubRPCClient()

	token := newTestToken("mint-ws", true, true)
	rpc.AddToken(token)

	var mu sync.Mutex
	var discoveries []PoolDiscovery

	config := DefaultScannerConfig()
	config.MonitorDEXes = []string{"raydium"}

	s := NewScanner(config, rpc, func(_ context.Context, d PoolDiscovery) {
		mu.Lock()
		discoveries = append(discoveries, d)
		mu.Unlock()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Emit a pool via websocket after a short delay.
	go func() {
		time.Sleep(50 * time.Millisecond)
		pool := newTestPool("pool-ws", "mint-ws", 10000, 30)
		rpc.EmitPool(pool)
	}()

	_ = s.Start(ctx)

	mu.Lock()
	require.Len(t, discoveries, 1)
	assert.Equal(t, solana.Pubkey("pool-ws"), discoveries[0].Pool.PoolAddress)
	assert.Equal(t, "websocket", discoveries[0].Source)
	mu.Unlock()
}

func TestScanner_Stats(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	config := DefaultScannerConfig()

	s := NewScanner(config, rpc, nil)

	ctx := context.Background()

	// Process some pools.
	s.handlePool(ctx, newTestPool("p1", "m1", 100, 60), "test")  // rejected (low liq)
	s.handlePool(ctx, newTestPool("p2", "m2", 5000, 60), "test") // accepted

	stats := s.Stats()
	assert.Equal(t, int64(2), stats.PoolsScanned)
	assert.Equal(t, int64(1), stats.PoolsAccepted)
	assert.Equal(t, int64(1), stats.PoolsRejected)
	assert.Equal(t, 2, stats.TrackedPools) // both tracked for dedup
}

// ---------------------------------------------------------------------------
// Analyzer Tests
// ---------------------------------------------------------------------------

func TestAnalyzer_SafeToken(t *testing.T) {
	rpc := solana.NewStubRPCClient()

	// Safe token: mint renounced, freeze renounced, LP burned, good distribution.
	token := newTestToken("mint-safe", true, true)
	rpc.AddToken(token)

	rpc.AddHolders(solana.Pubkey("mint-safe"), []solana.HolderInfo{
		{Address: "h1", Balance: decimal.NewFromInt(100), Percentage: 5.0},
		{Address: "h2", Balance: decimal.NewFromInt(80), Percentage: 4.0},
		{Address: "h3", Balance: decimal.NewFromInt(60), Percentage: 3.0},
	})

	pool := newTestPool("pool-safe", "mint-safe", 20000, 60)
	pool.LPBurned = true

	config := DefaultAnalyzerConfig()
	analyzer := NewAnalyzer(config, rpc)

	discovery := PoolDiscovery{
		Pool:   pool,
		Token:  &token,
		Source: "test",
	}

	analysis := analyzer.Analyze(context.Background(), discovery)

	assert.Equal(t, VerdictBuy, analysis.Verdict)
	assert.Greater(t, analysis.SafetyScore, 60)

	// Check positive flags.
	var flagCodes []string
	for _, f := range analysis.Flags {
		flagCodes = append(flagCodes, f.Code)
	}
	assert.Contains(t, flagCodes, "MINT_RENOUNCED")
	assert.Contains(t, flagCodes, "FREEZE_RENOUNCED")
	assert.Contains(t, flagCodes, "LP_BURNED")
	assert.Contains(t, flagCodes, "MODERATE_LIQUIDITY")
	assert.Contains(t, flagCodes, "GOOD_DISTRIBUTION")
}

func TestAnalyzer_HoneypotDetection(t *testing.T) {
	rpc := solana.NewStubRPCClient()

	// Freeze authority active = honeypot signal.
	token := newTestToken("mint-hp", true, false)
	rpc.AddToken(token)
	rpc.AddHolders(solana.Pubkey("mint-hp"), []solana.HolderInfo{
		{Address: "h1", Balance: decimal.NewFromInt(100), Percentage: 5.0},
	})

	pool := newTestPool("pool-hp", "mint-hp", 5000, 60)

	config := DefaultAnalyzerConfig()
	analyzer := NewAnalyzer(config, rpc)

	discovery := PoolDiscovery{Pool: pool, Token: &token, Source: "test"}
	analysis := analyzer.Analyze(context.Background(), discovery)

	assert.Equal(t, VerdictHoneypot, analysis.Verdict)
}

func TestAnalyzer_HighConcentrationRug(t *testing.T) {
	rpc := solana.NewStubRPCClient()

	token := newTestToken("mint-rug", true, true)
	rpc.AddToken(token)

	// Top holders own 80% = rug risk.
	rpc.AddHolders(solana.Pubkey("mint-rug"), []solana.HolderInfo{
		{Address: "whale1", Balance: decimal.NewFromInt(500), Percentage: 35.0, IsCreator: true},
		{Address: "whale2", Balance: decimal.NewFromInt(300), Percentage: 25.0},
		{Address: "whale3", Balance: decimal.NewFromInt(200), Percentage: 20.0},
	})

	pool := newTestPool("pool-rug", "mint-rug", 3000, 60)

	config := DefaultAnalyzerConfig()
	config.MaxTop10HolderPct = 50
	analyzer := NewAnalyzer(config, rpc)

	discovery := PoolDiscovery{Pool: pool, Token: &token, Source: "test"}
	analysis := analyzer.Analyze(context.Background(), discovery)

	// Score should be low enough for SKIP or RUG.
	assert.True(t, analysis.Verdict == VerdictRug || analysis.Verdict == VerdictSkip,
		"High concentration should result in RUG or SKIP verdict, got %s", analysis.Verdict)
	assert.NotNil(t, analysis.HolderAnalysis)
	assert.Greater(t, analysis.HolderAnalysis.Top10Pct, 50.0)
}

func TestAnalyzer_LPUnlocked_Warning(t *testing.T) {
	rpc := solana.NewStubRPCClient()

	token := newTestToken("mint-unlp", true, true)
	rpc.AddToken(token)
	rpc.AddHolders(solana.Pubkey("mint-unlp"), []solana.HolderInfo{
		{Address: "h1", Balance: decimal.NewFromInt(100), Percentage: 5.0},
	})

	pool := newTestPool("pool-unlp", "mint-unlp", 5000, 60)
	pool.LPBurned = false
	pool.LPLocked = false

	config := DefaultAnalyzerConfig()
	analyzer := NewAnalyzer(config, rpc)

	discovery := PoolDiscovery{Pool: pool, Token: &token, Source: "test"}
	analysis := analyzer.Analyze(context.Background(), discovery)

	var hasLPFlag bool
	for _, f := range analysis.Flags {
		if f.Code == "LP_UNLOCKED" {
			hasLPFlag = true
			assert.Equal(t, "warning", f.Severity)
		}
	}
	assert.True(t, hasLPFlag, "Should have LP_UNLOCKED flag")
}

func TestAnalyzer_VeryNewPoolBonus(t *testing.T) {
	rpc := solana.NewStubRPCClient()

	token := newTestToken("mint-new", true, true)
	rpc.AddToken(token)
	rpc.AddHolders(solana.Pubkey("mint-new"), []solana.HolderInfo{
		{Address: "h1", Balance: decimal.NewFromInt(100), Percentage: 5.0},
	})

	// Pool created 30 seconds ago.
	pool := newTestPool("pool-new", "mint-new", 5000, 30)
	pool.LPBurned = true

	config := DefaultAnalyzerConfig()
	analyzer := NewAnalyzer(config, rpc)

	discovery := PoolDiscovery{Pool: pool, Token: &token, Source: "test"}
	analysis := analyzer.Analyze(context.Background(), discovery)

	var hasNewFlag bool
	for _, f := range analysis.Flags {
		if f.Code == "VERY_NEW" {
			hasNewFlag = true
			assert.Equal(t, 5, f.ScoreImpact, "Very new pool should get a bonus")
		}
	}
	assert.True(t, hasNewFlag, "Should have VERY_NEW flag")
}
