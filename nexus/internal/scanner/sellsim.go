package scanner

import (
	"context"
	"time"

	"github.com/nexus-trading/nexus/internal/solana"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Sell Simulator — verify exit BEFORE entry + continuous safety checks
// Plan v3.1: L3-E6 pre-entry sell simulation + L7 CSM sell sim co 30s
// ---------------------------------------------------------------------------

// SellSimResult is the result of simulating a sell.
type SellSimResult struct {
	CanSell           bool            `json:"can_sell"`
	EstimatedTaxPct   float64         `json:"estimated_tax_pct"`
	EstimatedSlippage float64         `json:"estimated_slippage_pct"`
	RevertReason      string          `json:"revert_reason,omitempty"`
	AmountOut         decimal.Decimal `json:"amount_out"`
	PriceImpactPct    float64         `json:"price_impact_pct"`
	SimulatedAt       int64           `json:"simulated_at_ns"`
}

// SellSimulator simulates sell transactions to detect honeypots.
type SellSimulator struct {
	rpc solana.RPCClient
}

// NewSellSimulator creates a new sell simulator.
func NewSellSimulator(rpc solana.RPCClient) *SellSimulator {
	return &SellSimulator{rpc: rpc}
}

// SimulateSell estimates whether selling a token is possible using pool math.
// NOTE: Uses AMM math estimation since RPCClient lacks simulateTransaction.
// When real simulation is available, replace with actual tx simulation.
func (s *SellSimulator) SimulateSell(ctx context.Context, tokenMint solana.Pubkey, amount decimal.Decimal, pool solana.PoolInfo) SellSimResult {
	// Check if pool has enough reserves for any sell.
	if pool.QuoteReserve.IsZero() || pool.TokenReserve.IsZero() {
		return SellSimResult{
			CanSell:      false,
			RevertReason: "pool reserves are zero - cannot sell",
			SimulatedAt:  time.Now().UnixNano(),
		}
	}

	expectedOut := s.calculateExpectedOutput(amount, pool)

	// A sell that would drain >90% of quote reserves is infeasible.
	if expectedOut.GreaterThan(pool.QuoteReserve.Mul(decimal.NewFromFloat(0.9))) {
		return SellSimResult{
			CanSell:      false,
			RevertReason: "sell would drain >90% of pool quote reserves",
			SimulatedAt:  time.Now().UnixNano(),
		}
	}

	// Estimate tax/honeypot risk from price impact.
	priceImpact := s.calculatePriceImpact(amount, pool)
	taxPct := 0.0
	if priceImpact > 50.0 {
		// Extremely high price impact suggests hidden sell tax or honeypot.
		taxPct = priceImpact - 50.0
	}

	return SellSimResult{
		CanSell:           true,
		EstimatedTaxPct:   taxPct,
		EstimatedSlippage: priceImpact,
		AmountOut:         expectedOut,
		PriceImpactPct:    priceImpact,
		SimulatedAt:       time.Now().UnixNano(),
	}
}

// SimulatePreBuy estimates sell feasibility for the token amount we'd receive from buying.
// Plan v3.1 D4: verify exit BEFORE entry.
// Uses AMM constant-product formula (buy SOL -> token, then simulate reverse).
func (s *SellSimulator) SimulatePreBuy(ctx context.Context, buyAmountSOL decimal.Decimal, pool solana.PoolInfo) SellSimResult {
	if pool.QuoteReserve.IsZero() || pool.TokenReserve.IsZero() {
		return SellSimResult{
			CanSell:      false,
			RevertReason: "pool reserves are zero",
			SimulatedAt:  time.Now().UnixNano(),
		}
	}

	// Estimate tokens received from buying (constant product: x * y = k).
	// buyAmountSOL goes into QuoteReserve, we get tokens out of TokenReserve.
	// Apply pool fee to buy amount.
	effectiveBuy := buyAmountSOL.Mul(poolFeeMultiplier)
	numerator := effectiveBuy.Mul(pool.TokenReserve)
	denominator := pool.QuoteReserve.Add(effectiveBuy)
	if denominator.IsZero() {
		return SellSimResult{
			CanSell:      false,
			RevertReason: "zero denominator in buy estimation",
			SimulatedAt:  time.Now().UnixNano(),
		}
	}
	estimatedTokens := numerator.Div(denominator)

	log.Debug().
		Str("mint", string(pool.TokenMint)).
		Str("buy_sol", buyAmountSOL.String()).
		Str("est_tokens", estimatedTokens.String()).
		Msg("sellsim: pre-buy simulation")

	return s.SimulateSell(ctx, pool.TokenMint, estimatedTokens, pool)
}

// poolFeeRate is the standard AMM trading fee (0.25% for Raydium/Orca).
var poolFeeMultiplier = decimal.NewFromFloat(0.9975)

// calculateExpectedOutput uses constant product AMM formula with pool fee.
func (s *SellSimulator) calculateExpectedOutput(tokenAmount decimal.Decimal, pool solana.PoolInfo) decimal.Decimal {
	if pool.TokenReserve.IsZero() || pool.QuoteReserve.IsZero() {
		return decimal.Zero
	}
	// Apply pool fee to input amount before constant product calculation.
	effectiveAmount := tokenAmount.Mul(poolFeeMultiplier)
	numerator := effectiveAmount.Mul(pool.QuoteReserve)
	denominator := pool.TokenReserve.Add(effectiveAmount)
	if denominator.IsZero() {
		return decimal.Zero
	}
	return numerator.Div(denominator)
}

// calculatePriceImpact estimates the price impact of a sell.
func (s *SellSimulator) calculatePriceImpact(tokenAmount decimal.Decimal, pool solana.PoolInfo) float64 {
	if pool.TokenReserve.IsZero() {
		return 100.0
	}
	impact := tokenAmount.Div(pool.TokenReserve).Mul(decimal.NewFromInt(100))
	val, _ := impact.Float64()
	return val
}
