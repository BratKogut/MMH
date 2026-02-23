package sniper

import (
	"context"
	"time"

	"github.com/nexus-trading/nexus/internal/graph"
	"github.com/nexus-trading/nexus/internal/honeypot"
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
	SellSimIntervalS      int     `yaml:"sell_sim_interval_s"`      // sell simulation interval (default 30s)
	PriceCheckIntervalS   int     `yaml:"price_check_interval_s"`   // price check interval (default 5s)
	LiquidityDropPanicPct float64 `yaml:"liquidity_drop_panic_pct"` // panic sell if liq drops > this %
	LiquidityDropWarnPct  float64 `yaml:"liquidity_drop_warn_pct"`  // warn if liq drops > this %
	MaxSellTaxPct         float64 `yaml:"max_sell_tax_pct"`         // panic if sell tax > this %

	// CSM-2: Holder Exodus.
	HolderExodusEnabled     bool    `yaml:"holder_exodus_enabled"`       // enable holder exodus check
	HolderCheckTopN         int     `yaml:"holder_check_top_n"`          // how many top holders to track (default 10)
	HolderExodusPanicPct    float64 `yaml:"holder_exodus_panic_pct"`     // panic if top holders drop > this % of their balance
	HolderCountDropPanicPct float64 `yaml:"holder_count_drop_panic_pct"` // panic if unique holder count drops > this %

	// CSM-4: Entity Graph Re-Check.
	EntityReCheckEnabled   bool    `yaml:"entity_recheck_enabled"`    // enable entity graph re-check
	EntityReCheckIntervalS int     `yaml:"entity_recheck_interval_s"` // re-check every N seconds (default 60)
	EntityRiskPanicScore   float64 `yaml:"entity_risk_panic_score"`   // panic if risk score >= this (default 85)
}

// DefaultCSMConfig returns defaults per plan v3.1.
func DefaultCSMConfig() CSMConfig {
	return CSMConfig{
		SellSimIntervalS:      30,
		PriceCheckIntervalS:   5,
		LiquidityDropPanicPct: 50,
		LiquidityDropWarnPct:  30,
		MaxSellTaxPct:         10,

		HolderExodusEnabled:     true,
		HolderCheckTopN:         10,
		HolderExodusPanicPct:    30,
		HolderCountDropPanicPct: 20,

		EntityReCheckEnabled:   true,
		EntityReCheckIntervalS: 60,
		EntityRiskPanicScore:   85,
	}
}

// holderSnapshot captures holder state at a point in time.
type holderSnapshot struct {
	holders      []solana.HolderInfo
	totalBalance decimal.Decimal // sum of tracked holders' balances
}

// CSM monitors a single position's safety continuously.
type CSM struct {
	config    CSMConfig
	pos       *Position
	sellSim   *scanner.SellSimulator
	rpc       solana.RPCClient
	onPanic   func(ctx context.Context, pos *Position, reason string)
	onWarning func(ctx context.Context, pos *Position, reason string, severity float64) // graduated response

	entryLiquidityUSD decimal.Decimal
	lastSellSim       scanner.SellSimResult
	warningFired      map[string]bool // prevent duplicate warnings per reason

	// CSM-2: Holder Exodus.
	entryHolders *holderSnapshot
	deployerAddr string // token deployer address for entity graph

	// CSM-4: Entity Graph Re-Check.
	graphEngine      *graph.Engine
	honeypotTracker  *honeypot.Tracker
	entryRiskScore   float64
	lastEntityCheck  time.Time
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
		warningFired:      make(map[string]bool),
	}
}

// SetOnWarning sets the graduated response callback for partial exits.
// severity: 0.0-1.0 (how close to panic threshold).
func (c *CSM) SetOnWarning(fn func(ctx context.Context, pos *Position, reason string, severity float64)) {
	c.onWarning = fn
}

// SetEntityGraph wires in the entity graph engine for CSM-4 re-checks.
func (c *CSM) SetEntityGraph(engine *graph.Engine, deployerAddr string) {
	c.graphEngine = engine
	c.deployerAddr = deployerAddr
	if engine != nil && deployerAddr != "" {
		report := engine.QueryDeployer(deployerAddr)
		c.entryRiskScore = report.RiskScore
	}
}

// SetHoneypotTracker wires in the honeypot evolution tracker.
func (c *CSM) SetHoneypotTracker(tracker *honeypot.Tracker) {
	c.honeypotTracker = tracker
}

// Run starts the CSM loop. Blocks until ctx is cancelled or panic is triggered.
func (c *CSM) Run(ctx context.Context) {
	sellSimTicker := time.NewTicker(time.Duration(c.config.SellSimIntervalS) * time.Second)
	defer sellSimTicker.Stop()

	// Take initial holder snapshot.
	if c.config.HolderExodusEnabled {
		c.captureHolderSnapshot(ctx)
	}

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

	// CSM-2: Holder Exodus.
	if c.config.HolderExodusEnabled {
		c.checkHolderExodus(ctx)
	}

	// CSM-3: Liquidity Health.
	c.checkLiquidity(ctx)

	// CSM-4: Entity Graph Re-Check.
	if c.config.EntityReCheckEnabled {
		c.checkEntityGraph(ctx)
	}
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

		// Graduated response: partial exit proportional to severity.
		if c.onWarning != nil && !c.warningFired["CSM_LIQUIDITY_WARNING"] {
			c.warningFired["CSM_LIQUIDITY_WARNING"] = true
			severity := (dropVal - c.config.LiquidityDropWarnPct) /
				(c.config.LiquidityDropPanicPct - c.config.LiquidityDropWarnPct)
			if severity > 1.0 {
				severity = 1.0
			}
			c.onWarning(ctx, c.pos, "CSM_LIQUIDITY_WARNING", severity)
		}
	}
}

// ---------------------------------------------------------------------------
// CSM-2: Holder Exodus Detection
// ---------------------------------------------------------------------------

// captureHolderSnapshot takes an initial holder distribution snapshot.
func (c *CSM) captureHolderSnapshot(ctx context.Context) {
	holders, err := c.rpc.GetTopHolders(ctx, c.pos.TokenMint, c.config.HolderCheckTopN)
	if err != nil {
		log.Warn().Err(err).Str("pos_id", c.pos.ID).Msg("csm: failed to capture initial holder snapshot")
		return
	}

	total := decimal.Zero
	for _, h := range holders {
		total = total.Add(h.Balance)
	}

	c.entryHolders = &holderSnapshot{
		holders:      holders,
		totalBalance: total,
	}

	log.Debug().
		Str("pos_id", c.pos.ID).
		Int("holders", len(holders)).
		Str("total_balance", total.String()).
		Msg("csm: captured initial holder snapshot")
}

// checkHolderExodus detects if top holders are rapidly exiting.
func (c *CSM) checkHolderExodus(ctx context.Context) {
	if c.entryHolders == nil || len(c.entryHolders.holders) == 0 {
		return
	}

	currentHolders, err := c.rpc.GetTopHolders(ctx, c.pos.TokenMint, c.config.HolderCheckTopN)
	if err != nil {
		return
	}

	// Build lookup of current balances.
	currentMap := make(map[solana.Pubkey]decimal.Decimal, len(currentHolders))
	for _, h := range currentHolders {
		currentMap[h.Address] = h.Balance
	}

	// Track how much the original top holders have sold.
	totalSold := decimal.Zero
	for _, entryHolder := range c.entryHolders.holders {
		if entryHolder.Balance.IsZero() {
			continue
		}
		currentBal, exists := currentMap[entryHolder.Address]
		if !exists {
			// Holder completely exited.
			totalSold = totalSold.Add(entryHolder.Balance)
			continue
		}
		if currentBal.LessThan(entryHolder.Balance) {
			totalSold = totalSold.Add(entryHolder.Balance.Sub(currentBal))
		}
	}

	if c.entryHolders.totalBalance.IsZero() {
		return
	}

	// Calculate exodus percentage.
	exodusPct, _ := totalSold.Div(c.entryHolders.totalBalance).
		Mul(decimal.NewFromInt(100)).Float64()

	if exodusPct >= c.config.HolderExodusPanicPct {
		log.Error().
			Str("pos_id", c.pos.ID).
			Float64("exodus_pct", exodusPct).
			Str("total_sold", totalSold.String()).
			Msg("csm: HOLDER EXODUS - top holders dumping!")

		c.onPanic(ctx, c.pos, "CSM_HOLDER_EXODUS")
		return
	}

	if exodusPct > c.config.HolderExodusPanicPct*0.6 {
		log.Warn().
			Str("pos_id", c.pos.ID).
			Float64("exodus_pct", exodusPct).
			Msg("csm: holder exodus warning - significant selling by top holders")

		// Graduated response: partial exit when holders are dumping.
		if c.onWarning != nil && !c.warningFired["CSM_HOLDER_EXODUS_WARNING"] {
			c.warningFired["CSM_HOLDER_EXODUS_WARNING"] = true
			warnThreshold := c.config.HolderExodusPanicPct * 0.6
			severity := (exodusPct - warnThreshold) / (c.config.HolderExodusPanicPct - warnThreshold)
			if severity > 1.0 {
				severity = 1.0
			}
			c.onWarning(ctx, c.pos, "CSM_HOLDER_EXODUS_WARNING", severity)
		}
	}
}

// ---------------------------------------------------------------------------
// CSM-4: Entity Graph Re-Check
// ---------------------------------------------------------------------------

// checkEntityGraph re-queries the entity graph for updated deployer risk.
func (c *CSM) checkEntityGraph(ctx context.Context) {
	if c.graphEngine == nil || c.deployerAddr == "" {
		return
	}

	// Rate-limit entity re-checks.
	interval := time.Duration(c.config.EntityReCheckIntervalS) * time.Second
	if time.Since(c.lastEntityCheck) < interval {
		return
	}
	c.lastEntityCheck = time.Now()

	report := c.graphEngine.QueryDeployer(c.deployerAddr)

	// Check: deployer marked as serial rugger since entry.
	for _, label := range report.Labels {
		if label == graph.LabelSerialRugger {
			log.Error().
				Str("pos_id", c.pos.ID).
				Str("deployer", c.deployerAddr).
				Float64("risk_score", report.RiskScore).
				Msg("csm: ENTITY GRAPH - deployer now labeled SERIAL_RUGGER!")

			c.onPanic(ctx, c.pos, "CSM_ENTITY_SERIAL_RUGGER")
			return
		}
	}

	// Check: risk score jumped significantly since entry.
	if report.RiskScore >= c.config.EntityRiskPanicScore && report.RiskScore > c.entryRiskScore+20 {
		log.Error().
			Str("pos_id", c.pos.ID).
			Str("deployer", c.deployerAddr).
			Float64("entry_risk", c.entryRiskScore).
			Float64("current_risk", report.RiskScore).
			Msg("csm: ENTITY GRAPH - risk score spiked!")

		c.onPanic(ctx, c.pos, "CSM_ENTITY_RISK_SPIKE")
		return
	}

	// Check: deployer close to newly discovered rugger.
	if report.HopsToRugger >= 0 && report.HopsToRugger <= 2 {
		log.Warn().
			Str("pos_id", c.pos.ID).
			Str("deployer", c.deployerAddr).
			Int("hops", report.HopsToRugger).
			Msg("csm: entity graph warning - deployer near known rugger")
	}

	// Check: honeypot evolution retroactive match (if tracker available).
	if c.honeypotTracker != nil {
		c.checkHoneypotRetroactive(ctx)
	}

	log.Debug().
		Str("pos_id", c.pos.ID).
		Float64("risk_score", report.RiskScore).
		Msg("csm: entity re-check OK")
}

// checkHoneypotRetroactive checks if our held token matches newly discovered honeypot patterns.
func (c *CSM) checkHoneypotRetroactive(ctx context.Context) {
	// Get contract data for our token (using pool info as proxy).
	pool, err := c.rpc.GetPoolInfo(ctx, c.pos.PoolAddress)
	if err != nil {
		return
	}

	// Use pool address bytes as contract data proxy for matching.
	contractData := []byte(pool.PoolAddress)
	sig := c.honeypotTracker.CheckContract(contractData)
	if sig != nil && sig.Confidence >= 0.7 {
		log.Error().
			Str("pos_id", c.pos.ID).
			Str("pattern", sig.PatternID).
			Float64("confidence", sig.Confidence).
			Msg("csm: HONEYPOT EVOLUTION - token matches known rug pattern!")

		c.onPanic(ctx, c.pos, "CSM_HONEYPOT_RETROACTIVE")
	}
}

// LastSellSim returns the latest sell simulation result.
func (c *CSM) LastSellSim() scanner.SellSimResult {
	return c.lastSellSim
}
