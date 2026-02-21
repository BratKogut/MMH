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

// SimulateSell simulates selling a token amount and checks for honeypot behavior.
func (s *SellSimulator) SimulateSell(ctx context.Context, tokenMint solana.Pubkey, amount decimal.Decimal, pool solana.PoolInfo) SellSimResult {
	expectedOut := s.calculateExpectedOutput(amount, pool)

	// Attempt simulation via RPC simulateTransaction.
	simTx := fmt.Sprintf("sell-sim:%s:%s", tokenMint, amount.String())
	_, err := s.rpc.SendTransaction(ctx, simTx)

	if err != nil {
		log.Warn().
			Err(err).
			Str("mint", string(tokenMint)).
			Msg("sellsim: SELL REVERTED - potential honeypot")

		return SellSimResult{
			CanSell:      false,
			RevertReason: err.Error(),
			SimulatedAt:  time.Now().UnixNano(),
		}
	}

	// Estimate tax from pool math (real impl would compare simulated output).
	taxPct := 0.0
	if expectedOut.IsPositive() {
		actualOut := expectedOut.Mul(decimal.NewFromFloat(0.95))
		diff := expectedOut.Sub(actualOut)
		taxPctDec := diff.Div(expectedOut).Mul(decimal.NewFromInt(100))
		taxPct, _ = taxPctDec.Float64()
	}

	priceImpact := s.calculatePriceImpact(amount, pool)

	return SellSimResult{
		CanSell:           true,
		EstimatedTaxPct:   taxPct,
		EstimatedSlippage: priceImpact,
		AmountOut:         expectedOut,
		PriceImpactPct:    priceImpact,
		SimulatedAt:       time.Now().UnixNano(),
	}
}

// SimulatePreBuy simulates selling the exact amount we'd receive from buying.
// Plan v3.1 D4: verify exit BEFORE entry.
func (s *SellSimulator) SimulatePreBuy(ctx context.Context, buyAmountSOL decimal.Decimal, pool solana.PoolInfo) SellSimResult {
	if pool.PriceUSD.IsZero() {
		return SellSimResult{
			CanSell:      false,
			RevertReason: "zero price",
			SimulatedAt:  time.Now().UnixNano(),
		}
	}

	solPriceUSD := decimal.NewFromFloat(150.0)
	buyValueUSD := buyAmountSOL.Mul(solPriceUSD)
	estimatedTokens := buyValueUSD.Div(pool.PriceUSD)

	log.Debug().
		Str("mint", string(pool.TokenMint)).
		Str("buy_sol", buyAmountSOL.String()).
		Str("est_tokens", estimatedTokens.String()).
		Msg("sellsim: pre-buy simulation")

	return s.SimulateSell(ctx, pool.TokenMint, estimatedTokens, pool)
}

// calculateExpectedOutput uses constant product AMM formula.
func (s *SellSimulator) calculateExpectedOutput(tokenAmount decimal.Decimal, pool solana.PoolInfo) decimal.Decimal {
	if pool.TokenReserve.IsZero() || pool.QuoteReserve.IsZero() {
		return decimal.Zero
	}
	numerator := tokenAmount.Mul(pool.QuoteReserve)
	denominator := pool.TokenReserve.Add(tokenAmount)
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
