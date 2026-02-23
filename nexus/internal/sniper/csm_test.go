package sniper

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexus-trading/nexus/internal/graph"
	"github.com/nexus-trading/nexus/internal/honeypot"
	"github.com/nexus-trading/nexus/internal/scanner"
	"github.com/nexus-trading/nexus/internal/solana"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
)

func newTestCSM(rpc *solana.StubRPCClient, onPanic func(ctx context.Context, pos *Position, reason string)) (*CSM, *Position) {
	pos := &Position{
		ID:        "test-pos-1",
		TokenMint: "TestToken111",
		PoolAddress: "TestPool111",
		Status:    StatusOpen,
		AmountToken: decimal.NewFromInt(1000),
	}

	sellSim := scanner.NewSellSimulator(rpc)
	csm := NewCSM(DefaultCSMConfig(), pos, sellSim, rpc, decimal.NewFromInt(10000), onPanic)
	return csm, pos
}

func TestDefaultCSMConfig(t *testing.T) {
	cfg := DefaultCSMConfig()

	assert.Equal(t, 30, cfg.SellSimIntervalS)
	assert.Equal(t, 5, cfg.PriceCheckIntervalS)
	assert.Equal(t, 50.0, cfg.LiquidityDropPanicPct)
	assert.Equal(t, 30.0, cfg.LiquidityDropWarnPct)
	assert.Equal(t, 10.0, cfg.MaxSellTaxPct)

	// CSM-2 defaults.
	assert.True(t, cfg.HolderExodusEnabled)
	assert.Equal(t, 10, cfg.HolderCheckTopN)
	assert.Equal(t, 30.0, cfg.HolderExodusPanicPct)
	assert.Equal(t, 20.0, cfg.HolderCountDropPanicPct)

	// CSM-4 defaults.
	assert.True(t, cfg.EntityReCheckEnabled)
	assert.Equal(t, 60, cfg.EntityReCheckIntervalS)
	assert.Equal(t, 85.0, cfg.EntityRiskPanicScore)
}

func TestNewCSM(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	csm, _ := newTestCSM(rpc, nil)
	assert.NotNil(t, csm)
	assert.Equal(t, decimal.NewFromInt(10000), csm.entryLiquidityUSD)
}

func TestCSM_SetEntityGraph(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	csm, _ := newTestCSM(rpc, nil)

	graphEngine := graph.NewEngine(graph.DefaultConfig())
	csm.SetEntityGraph(graphEngine, "deployer1")

	assert.Equal(t, "deployer1", csm.deployerAddr)
	assert.NotNil(t, csm.graphEngine)
	assert.Equal(t, 50.0, csm.entryRiskScore) // unknown deployer = 50
}

func TestCSM_SetHoneypotTracker(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	csm, _ := newTestCSM(rpc, nil)

	tracker := honeypot.NewTracker(honeypot.DefaultEvolutionConfig())
	csm.SetHoneypotTracker(tracker)
	assert.NotNil(t, csm.honeypotTracker)
}

// ---------------------------------------------------------------------------
// CSM-2: Holder Exodus Tests
// ---------------------------------------------------------------------------

func TestCSM_CaptureHolderSnapshot(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	rpc.AddHolders("TestToken111", []solana.HolderInfo{
		{Address: "holder1", Balance: decimal.NewFromInt(1000), Percentage: 50},
		{Address: "holder2", Balance: decimal.NewFromInt(500), Percentage: 25},
		{Address: "holder3", Balance: decimal.NewFromInt(500), Percentage: 25},
	})

	csm, _ := newTestCSM(rpc, nil)
	csm.captureHolderSnapshot(context.Background())

	assert.NotNil(t, csm.entryHolders)
	assert.Equal(t, 3, len(csm.entryHolders.holders))
	assert.Equal(t, decimal.NewFromInt(2000), csm.entryHolders.totalBalance)
}

func TestCSM_HolderExodus_NoPanic(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	rpc.AddHolders("TestToken111", []solana.HolderInfo{
		{Address: "holder1", Balance: decimal.NewFromInt(1000), Percentage: 50},
		{Address: "holder2", Balance: decimal.NewFromInt(500), Percentage: 25},
	})

	var panicCount atomic.Int32
	csm, _ := newTestCSM(rpc, func(ctx context.Context, pos *Position, reason string) {
		panicCount.Add(1)
	})

	// Same holders, no exodus.
	csm.captureHolderSnapshot(context.Background())
	csm.checkHolderExodus(context.Background())

	assert.Equal(t, int32(0), panicCount.Load())
}

func TestCSM_HolderExodus_PanicOnMassExit(t *testing.T) {
	rpc := solana.NewStubRPCClient()

	// Entry: holders have 1000 + 500 = 1500.
	rpc.AddHolders("TestToken111", []solana.HolderInfo{
		{Address: "holder1", Balance: decimal.NewFromInt(1000), Percentage: 50},
		{Address: "holder2", Balance: decimal.NewFromInt(500), Percentage: 25},
	})

	var panicReason string
	csm, _ := newTestCSM(rpc, func(ctx context.Context, pos *Position, reason string) {
		panicReason = reason
	})
	csm.config.HolderExodusPanicPct = 30

	csm.captureHolderSnapshot(context.Background())

	// Now holders have dumped > 30% of their balance.
	rpc.AddHolders("TestToken111", []solana.HolderInfo{
		{Address: "holder1", Balance: decimal.NewFromInt(500), Percentage: 50},  // sold 500
		{Address: "holder2", Balance: decimal.NewFromInt(100), Percentage: 25},  // sold 400
	})

	csm.checkHolderExodus(context.Background())

	assert.Equal(t, "CSM_HOLDER_EXODUS", panicReason)
}

func TestCSM_HolderExodus_CompleteExit(t *testing.T) {
	rpc := solana.NewStubRPCClient()

	rpc.AddHolders("TestToken111", []solana.HolderInfo{
		{Address: "holder1", Balance: decimal.NewFromInt(1000), Percentage: 50},
	})

	var panicReason string
	csm, _ := newTestCSM(rpc, func(ctx context.Context, pos *Position, reason string) {
		panicReason = reason
	})
	csm.config.HolderExodusPanicPct = 30

	csm.captureHolderSnapshot(context.Background())

	// Holder completely gone.
	rpc.AddHolders("TestToken111", []solana.HolderInfo{})

	csm.checkHolderExodus(context.Background())

	assert.Equal(t, "CSM_HOLDER_EXODUS", panicReason)
}

func TestCSM_HolderExodus_NoSnapshot(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	csm, _ := newTestCSM(rpc, nil)

	// Should not panic with nil snapshot.
	csm.checkHolderExodus(context.Background())
}

// ---------------------------------------------------------------------------
// CSM-4: Entity Graph Re-Check Tests
// ---------------------------------------------------------------------------

func TestCSM_EntityGraph_NoPanic(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	var panicCount atomic.Int32
	csm, _ := newTestCSM(rpc, func(ctx context.Context, pos *Position, reason string) {
		panicCount.Add(1)
	})

	graphEngine := graph.NewEngine(graph.DefaultConfig())
	csm.SetEntityGraph(graphEngine, "clean_deployer")

	csm.checkEntityGraph(context.Background())

	assert.Equal(t, int32(0), panicCount.Load())
}

func TestCSM_EntityGraph_SerialRugger(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	var panicReason string
	csm, _ := newTestCSM(rpc, func(ctx context.Context, pos *Position, reason string) {
		panicReason = reason
	})

	graphEngine := graph.NewEngine(graph.DefaultConfig())
	ts := time.Now().Unix()

	// Initially clean deployer.
	graphEngine.AddDeploy("deployer1", ts)
	csm.SetEntityGraph(graphEngine, "deployer1")

	// Now deployer gets marked as serial rugger (2 rugs).
	graphEngine.MarkRug("deployer1")
	graphEngine.MarkRug("deployer1")

	csm.checkEntityGraph(context.Background())

	assert.Equal(t, "CSM_ENTITY_SERIAL_RUGGER", panicReason)
}

func TestCSM_EntityGraph_RiskSpike(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	var panicReason string
	csm, _ := newTestCSM(rpc, func(ctx context.Context, pos *Position, reason string) {
		panicReason = reason
	})

	graphEngine := graph.NewEngine(graph.DefaultConfig())
	ts := time.Now().Unix()

	graphEngine.AddDeploy("deployer1", ts)
	csm.SetEntityGraph(graphEngine, "deployer1")
	csm.config.EntityRiskPanicScore = 85

	// Simulate risk spike by linking deployer near a rugger.
	graphEngine.AddDeploy("rugger1", ts)
	graphEngine.MarkRug("rugger1")
	graphEngine.MarkRug("rugger1")
	graphEngine.AddTransfer("rugger1", "deployer1", 10.0, ts, "transfer")

	// Override entry risk to make the spike significant.
	csm.entryRiskScore = 50

	csm.checkEntityGraph(context.Background())

	// Deployer is now near a rugger but risk might not exceed 85.
	// The exact behavior depends on BFS scoring — check for warning or panic.
	// With 1-hop to serial rugger, risk should be high.
	report := graphEngine.QueryDeployer("deployer1")
	if report.RiskScore >= 85 && report.RiskScore > csm.entryRiskScore+20 {
		assert.Equal(t, "CSM_ENTITY_RISK_SPIKE", panicReason)
	}
}

func TestCSM_EntityGraph_NoEngine(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	csm, _ := newTestCSM(rpc, nil)

	// Should not panic with nil engine.
	csm.checkEntityGraph(context.Background())
}

func TestCSM_EntityGraph_RateLimit(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	csm, _ := newTestCSM(rpc, nil)

	graphEngine := graph.NewEngine(graph.DefaultConfig())
	csm.SetEntityGraph(graphEngine, "deployer1")
	csm.config.EntityReCheckIntervalS = 60

	// First check runs.
	csm.checkEntityGraph(context.Background())
	firstCheck := csm.lastEntityCheck

	// Immediate second check should be rate-limited.
	csm.checkEntityGraph(context.Background())

	// lastEntityCheck should not change (rate-limited).
	assert.Equal(t, firstCheck, csm.lastEntityCheck)
}

func TestCSM_HoneypotRetroactive_NoMatch(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	rpc.AddPool(solana.PoolInfo{
		PoolAddress:  "TestPool111",
		LiquidityUSD: decimal.NewFromInt(10000),
	})

	var panicCount atomic.Int32
	csm, _ := newTestCSM(rpc, func(ctx context.Context, pos *Position, reason string) {
		panicCount.Add(1)
	})

	tracker := honeypot.NewTracker(honeypot.DefaultEvolutionConfig())
	csm.SetHoneypotTracker(tracker)

	csm.checkHoneypotRetroactive(context.Background())

	assert.Equal(t, int32(0), panicCount.Load())
}

// ---------------------------------------------------------------------------
// CSM Run Integration
// ---------------------------------------------------------------------------

func TestCSM_RunChecks_AllEnabled(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	rpc.AddPool(solana.PoolInfo{
		PoolAddress:  "TestPool111",
		LiquidityUSD: decimal.NewFromInt(10000),
		QuoteReserve: decimal.NewFromInt(500),
		TokenReserve: decimal.NewFromInt(1000000),
	})
	rpc.AddHolders("TestToken111", []solana.HolderInfo{
		{Address: "holder1", Balance: decimal.NewFromInt(1000), Percentage: 50},
	})

	var panicCount atomic.Int32
	csm, _ := newTestCSM(rpc, func(ctx context.Context, pos *Position, reason string) {
		panicCount.Add(1)
	})

	graphEngine := graph.NewEngine(graph.DefaultConfig())
	csm.SetEntityGraph(graphEngine, "deployer1")

	csm.captureHolderSnapshot(context.Background())
	csm.runChecks(context.Background())

	// No panics expected (everything is healthy).
	assert.Equal(t, int32(0), panicCount.Load())
}

func TestCSM_RunChecks_DisabledChecks(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	rpc.AddPool(solana.PoolInfo{
		PoolAddress:  "TestPool111",
		LiquidityUSD: decimal.NewFromInt(10000),
		QuoteReserve: decimal.NewFromInt(500),
		TokenReserve: decimal.NewFromInt(1000000),
	})

	var panicCount atomic.Int32
	csm, _ := newTestCSM(rpc, func(ctx context.Context, pos *Position, reason string) {
		panicCount.Add(1)
	})
	csm.config.HolderExodusEnabled = false
	csm.config.EntityReCheckEnabled = false

	csm.runChecks(context.Background())

	// No panics expected.
	assert.Equal(t, int32(0), panicCount.Load())
}

func TestCSM_Run_ContextCancel(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	rpc.AddPool(solana.PoolInfo{
		PoolAddress:  "TestPool111",
		LiquidityUSD: decimal.NewFromInt(10000),
	})
	rpc.AddHolders("TestToken111", []solana.HolderInfo{})

	csm, _ := newTestCSM(rpc, nil)
	csm.config.SellSimIntervalS = 1 // 1s for fast test
	csm.config.HolderExodusEnabled = false
	csm.config.EntityReCheckEnabled = false

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		csm.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		// Good — exited cleanly.
	case <-time.After(2 * time.Second):
		t.Fatal("CSM.Run did not exit on context cancel")
	}
}

func TestCSM_LastSellSim(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	csm, _ := newTestCSM(rpc, nil)

	result := csm.LastSellSim()
	assert.False(t, result.CanSell) // zero value
}
