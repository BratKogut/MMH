package scanner

import (
	"context"
	"testing"
	"time"

	"github.com/nexus-trading/nexus/internal/solana"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestPoolState(addr string, liqUSD, priceUSD float64) solana.PoolInfo {
	return solana.PoolInfo{
		PoolAddress:  solana.Pubkey(addr),
		DEX:          "raydium",
		TokenMint:    solana.Pubkey("test-token"),
		QuoteMint:    solana.SOLMint,
		LiquidityUSD: decimal.NewFromFloat(liqUSD),
		PriceUSD:     decimal.NewFromFloat(priceUSD),
		CreatedAt:    time.Now(),
	}
}

func TestPoolStateManager_TrackPool(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	mgr := NewPoolStateManager(DefaultPoolStateConfig(), rpc)

	pool := newTestPoolState("pool-1", 10000, 0.001)
	mgr.TrackPool(pool)

	state := mgr.GetState(solana.Pubkey("pool-1"))
	require.NotNil(t, state)
	assert.True(t, state.IsActive)
	assert.Equal(t, "10000", state.InitialLiqUSD.String())
	assert.Equal(t, "0.001", state.PriceAtDetect.String())

	stats := mgr.Stats()
	assert.Equal(t, 1, stats.TrackedPools)
	assert.Equal(t, 1, stats.ActivePools)
}

func TestPoolStateManager_TrackPool_Deduplicate(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	mgr := NewPoolStateManager(DefaultPoolStateConfig(), rpc)

	pool := newTestPoolState("pool-1", 10000, 0.001)
	mgr.TrackPool(pool)
	mgr.TrackPool(pool) // duplicate

	stats := mgr.Stats()
	assert.Equal(t, 1, stats.TrackedPools)
}

func TestPoolStateManager_MaxTrackedPools(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	config := DefaultPoolStateConfig()
	config.MaxTrackedPools = 3
	mgr := NewPoolStateManager(config, rpc)

	for i := 0; i < 5; i++ {
		pool := newTestPoolState("pool-"+string(rune('a'+i)), 10000, 0.001)
		mgr.TrackPool(pool)
		time.Sleep(time.Millisecond) // ensure different timestamps
	}

	stats := mgr.Stats()
	assert.LessOrEqual(t, stats.TrackedPools, 3, "Should respect max limit")
}

func TestPoolStateManager_RecordTrade(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	mgr := NewPoolStateManager(DefaultPoolStateConfig(), rpc)

	pool := newTestPoolState("pool-1", 10000, 0.001)
	mgr.TrackPool(pool)

	// Record buys and sells.
	mgr.RecordTrade(solana.Pubkey("pool-1"), true, decimal.NewFromFloat(5.0))
	mgr.RecordTrade(solana.Pubkey("pool-1"), true, decimal.NewFromFloat(10.0))
	mgr.RecordTrade(solana.Pubkey("pool-1"), false, decimal.NewFromFloat(3.0))

	state := mgr.GetState(solana.Pubkey("pool-1"))
	require.NotNil(t, state)
	assert.Equal(t, 2, state.BuyCount)
	assert.Equal(t, 1, state.SellCount)
	assert.Equal(t, "10", state.LargestBuySOL.String())
	assert.Equal(t, "3", state.LargestSellSOL.String())
}

func TestPoolStateManager_RecordTrade_UnknownPool(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	mgr := NewPoolStateManager(DefaultPoolStateConfig(), rpc)

	// Should not panic on unknown pool.
	mgr.RecordTrade(solana.Pubkey("nonexistent"), true, decimal.NewFromFloat(1.0))
}

func TestPoolStateManager_GetActiveStates(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	mgr := NewPoolStateManager(DefaultPoolStateConfig(), rpc)

	mgr.TrackPool(newTestPoolState("pool-1", 10000, 0.001))
	mgr.TrackPool(newTestPoolState("pool-2", 20000, 0.002))

	active := mgr.GetActiveStates()
	assert.Len(t, active, 2)
}

func TestPoolStateManager_EvictExpired(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	config := DefaultPoolStateConfig()
	config.EvictAfterMinutes = 0 // evict immediately
	mgr := NewPoolStateManager(config, rpc)

	pool := newTestPoolState("pool-1", 10000, 0.001)
	mgr.TrackPool(pool)

	// Manually set detected time in the past.
	mgr.mu.Lock()
	state := mgr.pools[solana.Pubkey("pool-1")]
	state.DetectedAt = time.Now().Add(-1 * time.Hour)
	mgr.mu.Unlock()

	mgr.evictExpired()

	stats := mgr.Stats()
	assert.Equal(t, 0, stats.TrackedPools)
	assert.Greater(t, stats.Evictions, int64(0))
}

func TestPoolStateManager_LiquidityChangeCallback(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	config := DefaultPoolStateConfig()
	config.PriceAlertPct = 10.0
	mgr := NewPoolStateManager(config, rpc)

	var callbackCalled bool
	var callbackPct float64
	mgr.SetOnLiquidityChange(func(pool *PoolState, pct float64) {
		callbackCalled = true
		callbackPct = pct
	})

	pool := newTestPoolState("pool-1", 10000, 0.001)
	mgr.TrackPool(pool)

	// Simulate a significant liquidity drop.
	updatedPool := pool
	updatedPool.LiquidityUSD = decimal.NewFromFloat(5000) // -50%
	rpc.AddPool(updatedPool)

	mgr.refreshAll(context.Background())

	// Callback should fire for >10% change.
	assert.True(t, callbackCalled)
	assert.Less(t, callbackPct, -10.0)
}

func TestPoolStateManager_DefaultConfig(t *testing.T) {
	config := DefaultPoolStateConfig()
	assert.Equal(t, 1000, config.MaxTrackedPools)
	assert.Equal(t, 5000, config.RefreshIntervalMs)
	assert.Equal(t, 120, config.EvictAfterMinutes)
}
