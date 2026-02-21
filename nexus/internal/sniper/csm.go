package sniper

import (
	"context"
	"time"

	"github.com/nexus-trading/nexus/internal/scanner"
	"github.com/nexus-trading/nexus/internal/solana"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Continuous Safety Monitor (CSM)
// Plan v3.1 L7: dedicated goroutine per position, sell sim co 30s,
// liquidity health, holder exodus, entity graph re-check
// ---------------------------------------------------------------------------

// CSMConfig configures the Continuous Safety Monitor.
type CSMConfig struct {
	SellSimIntervalS   int     `yaml:"sell_sim_interval_s"`   // sell simulation interval (default 30s)
	PriceCheckIntervalS int    `yaml:"price_check_interval_s"` // price check interval (default 5s)
	LiquidityDropPanicPct float64 `yaml:"liquidity_drop_panic_pct"` // panic sell if liq drops > this %
	LiquidityDropWarnPct  float64 `yaml:"liquidity_drop_warn_pct"`  // warn if liq drops > this %
	MaxSellTaxPct        float64 `yaml:"max_sell_tax_pct"`         // panic if sell tax > this %
}

// DefaultCSMConfig returns defaults per plan v3.1.
func DefaultCSMConfig() CSMConfig {
	return CSMConfig{
		SellSimIntervalS:      30,
		PriceCheckIntervalS:   5,
		LiquidityDropPanicPct: 50,
		LiquidityDropWarnPct:  30,
		MaxSellTaxPct:         10,
	}
}

// CSM monitors a single position's safety continuously.
type CSM struct {
	config    CSMConfig
	pos       *Position
	sellSim   *scanner.SellSimulator
	rpc       solana.RPCClient
	onPanic   func(ctx context.Context, pos *Position, reason string)

	entryLiquidityUSD decimal.Decimal
	lastSellSim       scanner.SellSimResult
}

// NewCSM creates a new Continuous Safety Monitor for a position.
func NewCSM(config CSMConfig, pos *Position, sellSim *scanner.SellSimulator, rpc solana.RPCClient, entryLiq decimal.Decimal, onPanic func(ctx context.Context, pos *Position, reason string)) *CSM {
	return &CSM{
		config:            config,
		pos:               pos,
		sellSim:           sellSim,
		rpc:               rpc,
		onPanic:           onPanic,
		entryLiquidityUSD: entryLiq,
	}
}

// Run starts the CSM loop. Blocks until ctx is cancelled or panic is triggered.
func (c *CSM) Run(ctx context.Context) {
	sellSimTicker := time.NewTicker(time.Duration(c.config.SellSimIntervalS) * time.Second)
	defer sellSimTicker.Stop()

	log.Debug().
		Str("pos_id", c.pos.ID).
		Str("mint", string(c.pos.TokenMint)).
		Msg("csm: started monitoring")

	for {
		select {
		case <-ctx.Done():
			return
		case <-sellSimTicker.C:
			if c.pos.Status != StatusOpen {
				return
			}
			c.runChecks(ctx)
		}
	}
}

// runChecks runs all CSM safety checks.
func (c *CSM) runChecks(ctx context.Context) {
	// CSM-1: Sell Simulation.
	c.checkSellSim(ctx)

	// CSM-3: Liquidity Health.
	c.checkLiquidity(ctx)
}

// checkSellSim simulates selling our position to detect honeypot transitions.
func (c *CSM) checkSellSim(ctx context.Context) {
	pool, err := c.rpc.GetPoolInfo(ctx, c.pos.PoolAddress)
	if err != nil {
		log.Warn().Err(err).Str("pos_id", c.pos.ID).Msg("csm: failed to get pool info")
		return
	}

	result := c.sellSim.SimulateSell(ctx, c.pos.TokenMint, c.pos.AmountToken, *pool)
	c.lastSellSim = result

	// Check: sell reverted = honeypot activated.
	if !result.CanSell {
		log.Error().
			Str("pos_id", c.pos.ID).
			Str("mint", string(c.pos.TokenMint)).
			Str("reason", result.RevertReason).
			Msg("csm: SELL SIMULATION FAILED - honeypot detected!")

		c.onPanic(ctx, c.pos, "CSM_SELL_SIM_FAILED")
		return
	}

	// Check: tax increased above threshold.
	if result.EstimatedTaxPct > c.config.MaxSellTaxPct {
		log.Error().
			Str("pos_id", c.pos.ID).
			Float64("tax_pct", result.EstimatedTaxPct).
			Float64("max_tax", c.config.MaxSellTaxPct).
			Msg("csm: TAX INCREASE DETECTED - panic sell!")

		c.onPanic(ctx, c.pos, "CSM_TAX_INCREASE")
		return
	}

	log.Debug().
		Str("pos_id", c.pos.ID).
		Bool("can_sell", result.CanSell).
		Float64("tax_pct", result.EstimatedTaxPct).
		Msg("csm: sell sim OK")
}

// checkLiquidity checks if pool liquidity has dropped dangerously.
func (c *CSM) checkLiquidity(ctx context.Context) {
	pool, err := c.rpc.GetPoolInfo(ctx, c.pos.PoolAddress)
	if err != nil {
		return
	}

	if c.entryLiquidityUSD.IsZero() {
		return
	}

	dropPct := c.entryLiquidityUSD.Sub(pool.LiquidityUSD).
		Div(c.entryLiquidityUSD).
		Mul(decimal.NewFromInt(100))
	dropVal, _ := dropPct.Float64()

	if dropVal >= c.config.LiquidityDropPanicPct {
		log.Error().
			Str("pos_id", c.pos.ID).
			Float64("drop_pct", dropVal).
			Str("current_liq", pool.LiquidityUSD.String()).
			Str("entry_liq", c.entryLiquidityUSD.String()).
			Msg("csm: LIQUIDITY PANIC - major liquidity removal!")

		c.onPanic(ctx, c.pos, "CSM_LIQUIDITY_PANIC")
		return
	}

	if dropVal >= c.config.LiquidityDropWarnPct {
		log.Warn().
			Str("pos_id", c.pos.ID).
			Float64("drop_pct", dropVal).
			Msg("csm: liquidity warning - significant drop")
	}
}

// LastSellSim returns the latest sell simulation result.
func (c *CSM) LastSellSim() scanner.SellSimResult {
	return c.lastSellSim
}
