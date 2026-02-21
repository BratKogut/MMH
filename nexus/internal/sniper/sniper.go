package sniper

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nexus-trading/nexus/internal/adapters/jupiter"
	"github.com/nexus-trading/nexus/internal/scanner"
	"github.com/nexus-trading/nexus/internal/solana"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Sniper Engine — fast memecoin buy/sell with auto TP/SL
// ---------------------------------------------------------------------------

// Config configures the sniper engine.
type Config struct {
	// Maximum SOL to spend per snipe.
	MaxBuySOL float64 `yaml:"max_buy_sol"`

	// Default slippage tolerance in bps.
	SlippageBps int `yaml:"slippage_bps"`

	// Take profit target (multiplier). 2.0 = 2x = 100% gain.
	TakeProfitMultiplier float64 `yaml:"take_profit_multiplier"`

	// Stop loss percentage (0-100). 50 = sell if price drops 50%.
	StopLossPct float64 `yaml:"stop_loss_pct"`

	// Enable trailing stop loss.
	TrailingStopEnabled bool `yaml:"trailing_stop_enabled"`

	// Trailing stop distance in percentage.
	TrailingStopPct float64 `yaml:"trailing_stop_pct"`

	// Maximum concurrent positions.
	MaxPositions int `yaml:"max_positions"`

	// Maximum daily loss in SOL before stopping.
	MaxDailyLossSOL float64 `yaml:"max_daily_loss_sol"`

	// Maximum daily spend in SOL.
	MaxDailySpendSOL float64 `yaml:"max_daily_spend_sol"`

	// Price check interval for TP/SL monitoring.
	PriceCheckIntervalMs int `yaml:"price_check_interval_ms"`

	// Minimum analyzer safety score to auto-snipe.
	MinSafetyScore int `yaml:"min_safety_score"`

	// Auto-sell after this duration if no TP/SL hit (0 = disabled).
	AutoSellAfterMinutes int `yaml:"auto_sell_after_minutes"`

	// Dry run mode - log but don't execute.
	DryRun bool `yaml:"dry_run"`
}

// DefaultConfig returns conservative defaults.
func DefaultConfig() Config {
	return Config{
		MaxBuySOL:            0.1,   // 0.1 SOL per snipe
		SlippageBps:          200,   // 2%
		TakeProfitMultiplier: 2.0,   // 2x target
		StopLossPct:          50,    // -50% stop loss
		TrailingStopEnabled:  true,
		TrailingStopPct:      20,    // 20% trailing stop
		MaxPositions:         5,
		MaxDailyLossSOL:      1.0,
		MaxDailySpendSOL:     2.0,
		PriceCheckIntervalMs: 3000,  // Check every 3s
		MinSafetyScore:       40,
		AutoSellAfterMinutes: 60,    // Auto-sell after 1 hour
		DryRun:               true,  // Default to dry run
	}
}

// Position tracks an active snipe position.
type Position struct {
	ID             string          `json:"id"`
	TokenMint      solana.Pubkey   `json:"token_mint"`
	PoolAddress    solana.Pubkey   `json:"pool_address"`
	DEX            string          `json:"dex"`
	EntryPriceUSD  decimal.Decimal `json:"entry_price_usd"`
	AmountToken    decimal.Decimal `json:"amount_token"`
	CostSOL        decimal.Decimal `json:"cost_sol"`
	CurrentPrice   decimal.Decimal `json:"current_price_usd"`
	HighestPrice   decimal.Decimal `json:"highest_price_usd"` // for trailing stop
	PnLPct         float64         `json:"pnl_pct"`
	SafetyScore    int             `json:"safety_score"`
	BuySignature   solana.Signature `json:"buy_signature"`
	SellSignature  solana.Signature `json:"sell_signature,omitempty"`
	Status         PositionStatus  `json:"status"`
	OpenedAt       time.Time       `json:"opened_at"`
	ClosedAt       *time.Time      `json:"closed_at,omitempty"`
	CloseReason    string          `json:"close_reason,omitempty"`
}

// PositionStatus represents the state of a snipe position.
type PositionStatus string

const (
	StatusOpen    PositionStatus = "OPEN"
	StatusClosing PositionStatus = "CLOSING"
	StatusClosed  PositionStatus = "CLOSED"
	StatusFailed  PositionStatus = "FAILED"
)

// Engine is the sniper engine that manages buy/sell decisions and positions.
type Engine struct {
	config   Config
	jupiter  *jupiter.Adapter
	rpc      solana.RPCClient

	mu        sync.RWMutex
	positions map[string]*Position // ID -> Position
	trackers  map[string]*ExitTracker // ID -> ExitTracker
	running   atomic.Bool

	// Sub-engines.
	exitEngine *ExitEngine
	sellSim    *scanner.SellSimulator
	csmConfig  CSMConfig

	// Daily budget tracking.
	dailySpentSOL   decimal.Decimal
	dailyLossSOL    decimal.Decimal
	dailyResetTime  time.Time

	// Stats.
	totalSnipes    atomic.Int64
	totalSells     atomic.Int64
	totalProfitSOL atomic.Int64 // stored as micro-SOL for atomic
	winCount       atomic.Int64
	lossCount      atomic.Int64

	// Callbacks.
	onPositionOpen  func(pos *Position)
	onPositionClose func(pos *Position)
}

// NewEngine creates a new sniper engine.
func NewEngine(config Config, jup *jupiter.Adapter, rpc solana.RPCClient) *Engine {
	return &Engine{
		config:         config,
		jupiter:        jup,
		rpc:            rpc,
		positions:      make(map[string]*Position),
		trackers:       make(map[string]*ExitTracker),
		exitEngine:     NewExitEngine(DefaultExitConfig()),
		sellSim:        scanner.NewSellSimulator(rpc),
		csmConfig:      DefaultCSMConfig(),
		dailyResetTime: startOfDay(),
	}
}

// SetExitConfig sets the exit configuration.
func (e *Engine) SetExitConfig(config ExitConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.exitEngine = NewExitEngine(config)
}

// SetCSMConfig sets the CSM configuration.
func (e *Engine) SetCSMConfig(config CSMConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.csmConfig = config
}

// SetOnPositionOpen sets callback for new positions.
func (e *Engine) SetOnPositionOpen(fn func(pos *Position)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onPositionOpen = fn
}

// SetOnPositionClose sets callback for closed positions.
func (e *Engine) SetOnPositionClose(fn func(pos *Position)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onPositionClose = fn
}

// OnDiscovery is the callback for the scanner. Decides whether to snipe a new token.
func (e *Engine) OnDiscovery(ctx context.Context, analysis scanner.TokenAnalysis) {
	// Check daily limits.
	if !e.checkDailyLimits() {
		log.Warn().Msg("sniper: daily limit reached, skipping")
		return
	}

	// Check position limit.
	e.mu.RLock()
	openCount := 0
	for _, p := range e.positions {
		if p.Status == StatusOpen {
			openCount++
		}
	}
	e.mu.RUnlock()

	if openCount >= e.config.MaxPositions {
		log.Warn().Int("open", openCount).Int("max", e.config.MaxPositions).
			Msg("sniper: max positions reached, skipping")
		return
	}

	// Check safety score.
	if analysis.SafetyScore < e.config.MinSafetyScore {
		log.Info().
			Int("score", analysis.SafetyScore).
			Int("min", e.config.MinSafetyScore).
			Str("verdict", string(analysis.Verdict)).
			Msg("sniper: safety score too low, skipping")
		return
	}

	// Check verdict.
	if analysis.Verdict != scanner.VerdictBuy {
		log.Info().
			Str("verdict", string(analysis.Verdict)).
			Msg("sniper: verdict is not BUY, skipping")
		return
	}

	// Execute the snipe.
	e.executeBuy(ctx, analysis)
}

// executeBuy executes a buy order for the analyzed token.
func (e *Engine) executeBuy(ctx context.Context, analysis scanner.TokenAnalysis) {
	posID := uuid.New().String()[:12]
	buyAmount := decimal.NewFromFloat(e.config.MaxBuySOL)

	log.Info().
		Str("pos_id", posID).
		Str("mint", string(analysis.Mint)).
		Str("dex", analysis.Pool.DEX).
		Str("buy_sol", buyAmount.String()).
		Int("safety", analysis.SafetyScore).
		Bool("dry_run", e.config.DryRun).
		Msg("sniper: EXECUTING BUY")

	pos := &Position{
		ID:            posID,
		TokenMint:     analysis.Mint,
		PoolAddress:   analysis.Pool.PoolAddress,
		DEX:           analysis.Pool.DEX,
		EntryPriceUSD: analysis.Pool.PriceUSD,
		CostSOL:       buyAmount,
		CurrentPrice:  analysis.Pool.PriceUSD,
		HighestPrice:  analysis.Pool.PriceUSD,
		SafetyScore:   analysis.SafetyScore,
		Status:        StatusOpen,
		OpenedAt:      time.Now(),
	}

	if e.config.DryRun {
		pos.BuySignature = solana.Signature(fmt.Sprintf("DRYRUN-BUY-%s", posID))
		pos.AmountToken = buyAmount.Div(analysis.Pool.PriceUSD)
		log.Info().
			Str("pos_id", posID).
			Str("token_amount", pos.AmountToken.String()).
			Msg("sniper: DRY RUN buy (no real transaction)")
	} else {
		params := solana.SwapParams{
			InputMint:     solana.SOLMint,
			OutputMint:    analysis.Mint,
			AmountIn:      buyAmount,
			SlippageBps:   e.config.SlippageBps,
			PriorityFee:   100_000,
			UseJitoBundle: false,
		}

		result, err := e.jupiter.SwapDirect(ctx, params)
		if err != nil {
			log.Error().Err(err).Str("pos_id", posID).Msg("sniper: buy FAILED")
			pos.Status = StatusFailed
			return
		}

		pos.BuySignature = result.Signature
		pos.AmountToken = result.AmountOut
	}

	// Track spending + create exit tracker.
	e.mu.Lock()
	e.positions[posID] = pos
	e.trackers[posID] = NewExitTracker(posID, pos.AmountToken, e.exitEngine.config)
	e.dailySpentSOL = e.dailySpentSOL.Add(buyAmount)
	csmCfg := e.csmConfig
	e.mu.Unlock()

	e.totalSnipes.Add(1)

	// Start CSM for this position.
	csm := NewCSM(csmCfg, pos, e.sellSim, e.rpc, analysis.Pool.LiquidityUSD,
		func(ctx context.Context, p *Position, reason string) {
			e.executeSell(ctx, p, reason)
		})
	go csm.Run(context.Background())

	// Notify callback.
	e.mu.RLock()
	cb := e.onPositionOpen
	e.mu.RUnlock()
	if cb != nil {
		cb(pos)
	}

	log.Info().
		Str("pos_id", posID).
		Str("mint", string(pos.TokenMint)).
		Str("amount", pos.AmountToken.String()).
		Str("entry_price", pos.EntryPriceUSD.String()).
		Msg("sniper: position OPENED")
}

// UpdatePrice updates the current price for a token and checks TP/SL.
func (e *Engine) UpdatePrice(ctx context.Context, mint solana.Pubkey, priceUSD decimal.Decimal) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, pos := range e.positions {
		if pos.TokenMint != mint || pos.Status != StatusOpen {
			continue
		}

		pos.CurrentPrice = priceUSD

		// Track highest price for trailing stop.
		if priceUSD.GreaterThan(pos.HighestPrice) {
			pos.HighestPrice = priceUSD
		}

		// Calculate PnL.
		if pos.EntryPriceUSD.IsPositive() {
			pnl := priceUSD.Sub(pos.EntryPriceUSD).Div(pos.EntryPriceUSD).
				Mul(decimal.NewFromInt(100))
			pos.PnLPct, _ = pnl.Float64()
		}

		// Use ExitEngine for multi-level TP/SL/trailing/timed decisions.
		tracker := e.trackers[pos.ID]
		if tracker != nil && e.exitEngine != nil {
			decision := e.exitEngine.Evaluate(pos, tracker)
			if decision.ShouldSell {
				e.exitEngine.ApplyDecision(tracker, decision)
				if decision.IsFullClose {
					go e.executeSell(ctx, pos, decision.Reason)
				} else {
					go e.executePartialSell(ctx, pos, decision)
				}
				return
			}
			continue
		}

		// Fallback: simple TP/SL for backward compat (when no exit engine).
		if e.config.TakeProfitMultiplier > 0 {
			tpPrice := pos.EntryPriceUSD.Mul(decimal.NewFromFloat(e.config.TakeProfitMultiplier))
			if priceUSD.GreaterThanOrEqual(tpPrice) {
				go e.executeSell(ctx, pos, "TAKE_PROFIT")
				return
			}
		}

		if e.config.TrailingStopEnabled && pos.HighestPrice.IsPositive() {
			trailDist := pos.HighestPrice.Mul(decimal.NewFromFloat(e.config.TrailingStopPct / 100.0))
			trailStop := pos.HighestPrice.Sub(trailDist)
			if priceUSD.LessThanOrEqual(trailStop) && priceUSD.GreaterThan(pos.EntryPriceUSD) {
				go e.executeSell(ctx, pos, "TRAILING_STOP")
				return
			}
		}

		if e.config.StopLossPct > 0 {
			slPrice := pos.EntryPriceUSD.Mul(decimal.NewFromFloat(1.0 - e.config.StopLossPct/100.0))
			if priceUSD.LessThanOrEqual(slPrice) {
				go e.executeSell(ctx, pos, "STOP_LOSS")
				return
			}
		}

		if e.config.AutoSellAfterMinutes > 0 {
			maxAge := time.Duration(e.config.AutoSellAfterMinutes) * time.Minute
			if time.Since(pos.OpenedAt) > maxAge {
				go e.executeSell(ctx, pos, "AUTO_SELL_TIMEOUT")
				return
			}
		}
	}
}

// executePartialSell sells a partial amount of a position.
func (e *Engine) executePartialSell(ctx context.Context, pos *Position, decision ExitDecision) {
	log.Info().
		Str("pos_id", pos.ID).
		Str("mint", string(pos.TokenMint)).
		Str("reason", decision.Reason).
		Float64("sell_pct", decision.SellPct).
		Str("sell_amount", decision.SellAmount.String()).
		Msg("sniper: PARTIAL SELL")

	if e.config.DryRun {
		log.Info().
			Str("pos_id", pos.ID).
			Str("reason", decision.Reason).
			Float64("sell_pct", decision.SellPct).
			Msg("sniper: DRY RUN partial sell")
	} else {
		params := solana.SwapParams{
			InputMint:   pos.TokenMint,
			OutputMint:  solana.SOLMint,
			AmountIn:    decision.SellAmount,
			SlippageBps: e.config.SlippageBps,
			PriorityFee: 100_000,
		}

		_, err := e.jupiter.SwapDirect(ctx, params)
		if err != nil {
			log.Error().Err(err).Str("pos_id", pos.ID).Msg("sniper: partial sell FAILED")
			return
		}
	}

	// Update position token amount.
	e.mu.Lock()
	pos.AmountToken = pos.AmountToken.Sub(decision.SellAmount)
	e.mu.Unlock()

	e.totalSells.Add(1)

	log.Info().
		Str("pos_id", pos.ID).
		Str("remaining", pos.AmountToken.String()).
		Msg("sniper: partial sell completed")
}

// executeSell closes a position.
func (e *Engine) executeSell(ctx context.Context, pos *Position, reason string) {
	e.mu.Lock()
	if pos.Status != StatusOpen {
		e.mu.Unlock()
		return
	}
	pos.Status = StatusClosing
	e.mu.Unlock()

	log.Info().
		Str("pos_id", pos.ID).
		Str("mint", string(pos.TokenMint)).
		Str("reason", reason).
		Float64("pnl_pct", pos.PnLPct).
		Bool("dry_run", e.config.DryRun).
		Msg("sniper: EXECUTING SELL")

	now := time.Now()

	if e.config.DryRun {
		pos.SellSignature = solana.Signature(fmt.Sprintf("DRYRUN-SELL-%s", pos.ID))
		log.Info().
			Str("pos_id", pos.ID).
			Msg("sniper: DRY RUN sell (no real transaction)")
	} else {
		params := solana.SwapParams{
			InputMint:   pos.TokenMint,
			OutputMint:  solana.SOLMint,
			AmountIn:    pos.AmountToken,
			SlippageBps: e.config.SlippageBps,
			PriorityFee: 100_000,
		}

		result, err := e.jupiter.SwapDirect(ctx, params)
		if err != nil {
			log.Error().Err(err).Str("pos_id", pos.ID).Msg("sniper: sell FAILED")
			e.mu.Lock()
			pos.Status = StatusOpen // Revert to open, try again next cycle
			e.mu.Unlock()
			return
		}
		pos.SellSignature = result.Signature
	}

	e.mu.Lock()
	pos.Status = StatusClosed
	pos.ClosedAt = &now
	pos.CloseReason = reason

	// Track daily loss.
	if pos.PnLPct < 0 {
		lossSOL := pos.CostSOL.Mul(decimal.NewFromFloat(-pos.PnLPct / 100.0))
		e.dailyLossSOL = e.dailyLossSOL.Add(lossSOL)
		e.lossCount.Add(1)
	} else {
		e.winCount.Add(1)
	}

	cb := e.onPositionClose
	e.mu.Unlock()

	e.totalSells.Add(1)

	if cb != nil {
		cb(pos)
	}

	log.Info().
		Str("pos_id", pos.ID).
		Str("mint", string(pos.TokenMint)).
		Str("reason", reason).
		Float64("pnl_pct", pos.PnLPct).
		Msg("sniper: position CLOSED")
}

// ForceClose forces all open positions to close.
func (e *Engine) ForceClose(ctx context.Context) {
	e.mu.RLock()
	var openPositions []*Position
	for _, p := range e.positions {
		if p.Status == StatusOpen {
			openPositions = append(openPositions, p)
		}
	}
	e.mu.RUnlock()

	for _, pos := range openPositions {
		e.executeSell(ctx, pos, "FORCE_CLOSE")
	}
}

// checkDailyLimits checks if daily spending/loss limits are exceeded.
func (e *Engine) checkDailyLimits() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Reset if new day.
	if time.Now().After(e.dailyResetTime.Add(24 * time.Hour)) {
		e.dailySpentSOL = decimal.Zero
		e.dailyLossSOL = decimal.Zero
		e.dailyResetTime = startOfDay()
	}

	// Check daily spend.
	maxSpend := decimal.NewFromFloat(e.config.MaxDailySpendSOL)
	if e.dailySpentSOL.GreaterThanOrEqual(maxSpend) {
		return false
	}

	// Check daily loss.
	maxLoss := decimal.NewFromFloat(e.config.MaxDailyLossSOL)
	if e.dailyLossSOL.GreaterThanOrEqual(maxLoss) {
		return false
	}

	return true
}

// Positions returns all positions.
func (e *Engine) Positions() []*Position {
	e.mu.RLock()
	defer e.mu.RUnlock()

	positions := make([]*Position, 0, len(e.positions))
	for _, p := range e.positions {
		positions = append(positions, p)
	}
	return positions
}

// OpenPositions returns only open positions.
func (e *Engine) OpenPositions() []*Position {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var positions []*Position
	for _, p := range e.positions {
		if p.Status == StatusOpen {
			positions = append(positions, p)
		}
	}
	return positions
}

// Stats returns sniper statistics.
type SniperStats struct {
	TotalSnipes   int64   `json:"total_snipes"`
	TotalSells    int64   `json:"total_sells"`
	OpenPositions int     `json:"open_positions"`
	WinCount      int64   `json:"win_count"`
	LossCount     int64   `json:"loss_count"`
	WinRate       float64 `json:"win_rate"`
	DailySpentSOL string  `json:"daily_spent_sol"`
	DailyLossSOL  string  `json:"daily_loss_sol"`
	DryRun        bool    `json:"dry_run"`
}

func (e *Engine) Stats() SniperStats {
	e.mu.RLock()
	openCount := 0
	for _, p := range e.positions {
		if p.Status == StatusOpen {
			openCount++
		}
	}
	spentSOL := e.dailySpentSOL.String()
	lossSOL := e.dailyLossSOL.String()
	e.mu.RUnlock()

	wins := e.winCount.Load()
	losses := e.lossCount.Load()
	total := wins + losses
	winRate := 0.0
	if total > 0 {
		winRate = float64(wins) / float64(total) * 100.0
	}

	return SniperStats{
		TotalSnipes:   e.totalSnipes.Load(),
		TotalSells:    e.totalSells.Load(),
		OpenPositions: openCount,
		WinCount:      wins,
		LossCount:     losses,
		WinRate:       winRate,
		DailySpentSOL: spentSOL,
		DailyLossSOL:  lossSOL,
		DryRun:        e.config.DryRun,
	}
}

func startOfDay() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}
