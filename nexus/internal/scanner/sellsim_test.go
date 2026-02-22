package scanner

import (
	"context"
	"testing"

	"github.com/nexus-trading/nexus/internal/solana"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
)

func newTestPoolForSellSim() solana.PoolInfo {
	return solana.PoolInfo{
		PoolAddress:  solana.Pubkey("pool-sellsim"),
		TokenMint:    solana.Pubkey("token-sellsim"),
		QuoteMint:    solana.SOLMint,
		TokenReserve: decimal.NewFromFloat(1000000),
		QuoteReserve: decimal.NewFromFloat(100),
		LiquidityUSD: decimal.NewFromFloat(15000),
		PriceUSD:     decimal.NewFromFloat(0.001),
	}
}

func TestSellSimulator_SuccessfulSell(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	sim := NewSellSimulator(rpc)

	pool := newTestPoolForSellSim()
	amount := decimal.NewFromFloat(1000)

	result := sim.SimulateSell(context.Background(), solana.Pubkey("token-sellsim"), amount, pool)

	assert.True(t, result.CanSell)
	assert.Greater(t, result.AmountOut.InexactFloat64(), 0.0)
	assert.Greater(t, result.SimulatedAt, int64(0))
	assert.Empty(t, result.RevertReason)
}

func TestSellSimulator_FailedSell_ZeroReserves(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	sim := NewSellSimulator(rpc)

	pool := newTestPoolForSellSim()
	pool.QuoteReserve = decimal.Zero // no liquidity
	pool.TokenReserve = decimal.Zero
	amount := decimal.NewFromFloat(1000)

	result := sim.SimulateSell(context.Background(), solana.Pubkey("token-sellsim"), amount, pool)

	assert.False(t, result.CanSell)
	assert.NotEmpty(t, result.RevertReason)
}

func TestSellSimulator_FailedSell_DrainReserves(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	sim := NewSellSimulator(rpc)

	pool := newTestPoolForSellSim()
	// Selling so many tokens that output > 90% of quote reserves.
	amount := decimal.NewFromFloat(100000000) // 100x token reserve

	result := sim.SimulateSell(context.Background(), solana.Pubkey("token-sellsim"), amount, pool)

	assert.False(t, result.CanSell)
	assert.Contains(t, result.RevertReason, "90%")
}

func TestSellSimulator_ExpectedOutput_AMM(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	sim := NewSellSimulator(rpc)

	pool := newTestPoolForSellSim()
	amount := decimal.NewFromFloat(1000) // selling 1000 tokens

	// Constant product: dy = y * dx / (x + dx)
	// = 100 * 1000 / (1000000 + 1000) = 100000 / 1001000 ≈ 0.0999
	expected := sim.calculateExpectedOutput(amount, pool)

	assert.True(t, expected.IsPositive())
	val, _ := expected.Float64()
	assert.InDelta(t, 0.0999, val, 0.01)
}

func TestSellSimulator_PriceImpact(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	sim := NewSellSimulator(rpc)

	pool := newTestPoolForSellSim()

	// Small sell: 0.1% of reserves.
	small := decimal.NewFromFloat(1000) // 0.1% of 1M
	impact := sim.calculatePriceImpact(small, pool)
	assert.InDelta(t, 0.1, impact, 0.01)

	// Large sell: 10% of reserves.
	large := decimal.NewFromFloat(100000) // 10% of 1M
	impact2 := sim.calculatePriceImpact(large, pool)
	assert.InDelta(t, 10.0, impact2, 0.1)
}

func TestSellSimulator_PreBuy(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	sim := NewSellSimulator(rpc)

	pool := newTestPoolForSellSim()
	buySOL := decimal.NewFromFloat(0.5)

	result := sim.SimulatePreBuy(context.Background(), buySOL, pool)

	assert.True(t, result.CanSell)
	assert.Greater(t, result.AmountOut.InexactFloat64(), 0.0)
}

func TestSellSimulator_PreBuy_ZeroReserves(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	sim := NewSellSimulator(rpc)

	pool := newTestPoolForSellSim()
	pool.QuoteReserve = decimal.Zero
	pool.TokenReserve = decimal.Zero

	result := sim.SimulatePreBuy(context.Background(), decimal.NewFromFloat(0.5), pool)

	assert.False(t, result.CanSell)
	assert.NotEmpty(t, result.RevertReason)
}

func TestSellSimulator_EmptyReserves(t *testing.T) {
	rpc := solana.NewStubRPCClient()
	sim := NewSellSimulator(rpc)

	pool := newTestPoolForSellSim()
	pool.TokenReserve = decimal.Zero
	pool.QuoteReserve = decimal.Zero

	output := sim.calculateExpectedOutput(decimal.NewFromFloat(1000), pool)
	assert.True(t, output.IsZero())
}
