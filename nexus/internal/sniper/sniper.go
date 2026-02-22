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
	MaxBuySOL            float64 `yaml:"max_buy_sol"`
	SlippageBps          int     `yaml:"slippage_bps"`
	TakeProfitMultiplier float64 `yaml:"take_profit_multiplier"`
	StopLossPct          float64 `yaml:"stop_loss_pct"`
	TrailingStopEnabled  bool    `yaml:"trailing_stop_enabled"`
	TrailingStopPct      float64 `yaml:"trailing_stop_pct"`
	MaxPositions         int     `yaml:"max_positions"`
	MaxDailyLossSOL      float64 `yaml:"max_daily_loss_sol"`
	MaxDailySpendSOL     float64 `yaml:"max_daily_spend_sol"`
	PriceCheckIntervalMs int     `yaml:"price_check_interval_ms"`
	MinSafetyScore       int     `yaml:"min_safety_score"`
	AutoSellAfterMinutes int     `yaml:"auto_sell_after_minutes"`
	DryRun               bool    `yaml:"dry_run"`
	PriorityFee          uint64  `yaml:"priority_fee"`
	UseJito              bool    `yaml:"use_jito"`
	MaxSellRetries       int     `yaml:"max_sell_retries"`
}

// DefaultConfig returns conservative defaults.
func DefaultConfig() Config {
	return Config{
		MaxBuySOL:            0.1,
		SlippageBps:          200,
		TakeProfitMultiplier: 2.0,
		StopLossPct:          50,
		TrailingStopEnabled:  true,
		TrailingStopPct:      20,
		MaxPositions:         5,
		MaxDailyLossSOL:      1.0,
		MaxDailySpendSOL:     2.0,
		PriceCheckIntervalMs: 3000,
		MinSafetyScore:       40,
		AutoSellAfterMinutes: 60,
		DryRun:               true,
		PriorityFee:          100_000,
		UseJito:              false,
		MaxSellRetries:       3,
	}
}

// Position tracks an active snipe position.
type Position struct {
	ID            string           `json:"id"`
	TokenMint     solana.Pubkey    `json:"token_mint"`
	PoolAddress   solana.Pubkey    `json:"pool_address"`
	DEX           string           `json:"dex"`
	EntryPriceUSD decimal.Decimal  `json:"entry_price_usd"`
	AmountToken   decimal.Decimal  `json:"amount_token"`
	CostSOL       decimal.Decimal  `json:"cost_sol"`
	CurrentPrice  decimal.Decimal  `json:"current_price_usd"`
	HighestPrice  decimal.Decimal  `json:"highest_price_usd"`
	PnLPct        float64          `json:"pnl_pct"`
	SafetyScore   int              `json:"safety_score"`
	BuySignature  solana.Signature `json:"buy_signature"`
	SellSignature solana.Signature `json:"sell_signature,omitempty"`
	Status        PositionStatus   `json:"status"`
	OpenedAt      time.Time        `json:"opened_at"`
	ClosedAt      *time.Time       `json:"closed_at,omitempty"`
	CloseReason   string           `json:"close_reason,omitempty"`
	SellRetries   int              `json:"sell_retries"`
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
	config  Config
	jupiter *jupiter.Adapter
	rpc     solana.RPCClient

	mu        sync.RWMutex
	positions map[string]*Position
	trackers  map[string]*ExitTracker
	csmCancel map[string]context.CancelFunc // per-position CSM cancel
	running   atomic.Bool

	// Sub-engines.
	exitEngine *ExitEngine
	sellSim    *scanner.SellSimulator
	csmConfig  CSMConfig

	// Daily budget tracking (protected by mu).
	dailySpentSOL  decimal.Decimal
	dailyLossSOL   decimal.Decimal
	dailyResetTime time.Time

	// Stats.
	totalSnipes    atomic.Int64
	totalSells     atomic.Int64
	totalProfitSOL atomic.Int64
	winCount       atomic.Int64
	lossCount      atomic.Int64

	// Callbacks.
	onPositionOpen  func(pos *Position)
	onPositionClose func(pos *Position)
}

// NewEngine creates a new sniper engine.
func NewEngine(config Config, jup *jupiter.Adapter, rpc solana.RPCClient) *Engine {
	if config.MaxSellRetries <= 0 {
		config.MaxSellRetries = 3
	}
	if config.PriorityFee == 0 {
		config.PriorityFee = 100_000
	}
	return &Engine{
		config:         config,
		jupiter:        jup,
		rpc:            rpc,
		positions:      make(map[string]*Position),
		trackers:       make(map[string]*ExitTracker),
		csmCancel:      make(map[string]context.CancelFunc),
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
	// Atomic check-and-reserve daily budget.
	if !e.reserveDailyBudget() {
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

	if analysis.SafetyScore < e.config.MinSafetyScore {
		log.Info().
			Int("score", analysis.SafetyScore).
			Int("min", e.config.MinSafetyScore).
			Str("verdict", string(analysis.Verdict)).
			Msg("sniper: safety score too low, skipping")
		return
	}

	if analysis.Verdict != scanner.VerdictBuy {
		log.Info().
			Str("verdict", string(analysis.Verdict)).
			Msg("sniper: verdict is not BUY, skipping")
		return
	}

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
		if analysis.Pool.PriceUSD.IsPositive() {
			pos.AmountToken = buyAmount.Div(analysis.Pool.PriceUSD)
		} else {
			pos.AmountToken = buyAmount
		}
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
			PriorityFee:   e.config.PriorityFee,
			UseJitoBundle: e.config.UseJito,
		}

		result, err := e.jupiter.SwapDirect(ctx, params)
		if err != nil {
			log.Error().Err(err).Str("pos_id", posID).Str("mint", string(analysis.Mint)).Msg("sniper: buy FAILED")
			pos.Status = StatusFailed
			// Don't track failed buy positions.
			return
		}

		pos.BuySignature = result.Signature
		pos.AmountToken = result.AmountOut
	}

	// Track spending + create exit tracker under single lock.
	e.mu.Lock()
	e.positions[posID] = pos
	e.trackers[posID] = NewExitTracker(posID, pos.AmountToken, e.exitEngine.config)
	e.dailySpentSOL = e.dailySpentSOL.Add(buyAmount)
	csmCfg := e.csmConfig
	e.mu.Unlock()

	e.totalSnipes.Add(1)

	// Start CSM with cancellable context.
	csmCtx, csmCancel := context.WithCancel(ctx)
	e.mu.Lock()
	e.csmCancel[posID] = csmCancel
	e.mu.Unlock()

	csm := NewCSM(csmCfg, pos, e.sellSim, e.rpc, analysis.Pool.LiquidityUSD,
		func(csmCtx context.Context, p *Position, reason string) {
			e.executeSell(csmCtx, p, reason)
		})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error().Interface("panic", r).Str("pos_id", posID).Msg("sniper: CSM panic recovered")
			}
		}()
		csm.Run(csmCtx)
	}()

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

		if priceUSD.GreaterThan(pos.HighestPrice) {
			pos.HighestPrice = priceUSD
		}

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
				// Mark as closing BEFORE spawning goroutine to prevent double-sell.
				pos.Status = StatusClosing
				if decision.IsFullClose {
					go func(p *Position, r string) {
						defer func() {
							if rec := recover(); rec != nil {
								log.Error().Interface("panic", rec).Str("pos_id", p.ID).Msg("sniper: sell panic recovered")
							}
						}()
						e.executeSell(ctx, p, r)
					}(pos, decision.Reason)
				} else {
					// For partial sells, don't change status to closing.
					pos.Status = StatusOpen
					go func(p *Position, d ExitDecision) {
						defer func() {
							if rec := recover(); rec != nil {
								log.Error().Interface("panic", rec).Str("pos_id", p.ID).Msg("sniper: partial sell panic recovered")
							}
						}()
						e.executePartialSell(ctx, p, d)
					}(pos, decision)
				}
				return
			}
			continue
		}

		// Fallback: simple TP/SL (when no exit engine).
		if e.config.TakeProfitMultiplier > 0 {
			tpPrice := pos.EntryPriceUSD.Mul(decimal.NewFromFloat(e.config.TakeProfitMultiplier))
			if priceUSD.GreaterThanOrEqual(tpPrice) {
				pos.Status = StatusClosing
				go e.executeSell(ctx, pos, "TAKE_PROFIT")
				return
			}
		}

		if e.config.TrailingStopEnabled && pos.HighestPrice.IsPositive() {
			trailDist := pos.HighestPrice.Mul(decimal.NewFromFloat(e.config.TrailingStopPct / 100.0))
			trailStop := pos.HighestPrice.Sub(trailDist)
			if priceUSD.LessThanOrEqual(trailStop) && priceUSD.GreaterThan(pos.EntryPriceUSD) {
				pos.Status = StatusClosing
				go e.executeSell(ctx, pos, "TRAILING_STOP")
				return
			}
		}

		if e.config.StopLossPct > 0 {
			slPrice := pos.EntryPriceUSD.Mul(decimal.NewFromFloat(1.0 - e.config.StopLossPct/100.0))
			if priceUSD.LessThanOrEqual(slPrice) {
				pos.Status = StatusClosing
				go e.executeSell(ctx, pos, "STOP_LOSS")
				return
			}
		}

		if e.config.AutoSellAfterMinutes > 0 {
			maxAge := time.Duration(e.config.AutoSellAfterMinutes) * time.Minute
			if time.Since(pos.OpenedAt) > maxAge {
				pos.Status = StatusClosing
				go e.executeSell(ctx, pos, "AUTO_SELL_TIMEOUT")
				return
			}
		}
	}
}

// executePartialSell sells a partial amount of a position.
func (e *Engine) executePartialSell(ctx context.Context, pos *Position, decision ExitDecision) {
	// Snapshot sell amount under lock to avoid TOCTOU race.
	e.mu.RLock()
	currentAmount := pos.AmountToken
	posID := pos.ID
	e.mu.RUnlock()

	sellAmount := decision.SellAmount
	if sellAmount.GreaterThan(currentAmount) {
		sellAmount = currentAmount
	}
	if !sellAmount.IsPositive() {
		return
	}

	log.Info().
		Str("pos_id", posID).
		Str("mint", string(pos.TokenMint)).
		Str("reason", decision.Reason).
		Float64("sell_pct", decision.SellPct).
		Str("sell_amount", sellAmount.String()).
		Msg("sniper: PARTIAL SELL")

	if e.config.DryRun {
		log.Info().
			Str("pos_id", posID).
			Str("reason", decision.Reason).
			Float64("sell_pct", decision.SellPct).
			Msg("sniper: DRY RUN partial sell")
	} else {
		params := solana.SwapParams{
			InputMint:     pos.TokenMint,
			OutputMint:    solana.SOLMint,
			AmountIn:      sellAmount,
			SlippageBps:   e.config.SlippageBps,
			PriorityFee:   e.config.PriorityFee,
			UseJitoBundle: e.config.UseJito,
		}

		_, err := e.jupiter.SwapDirect(ctx, params)
		if err != nil {
			log.Error().Err(err).Str("pos_id", posID).Str("reason", decision.Reason).Msg("sniper: partial sell FAILED")
			return
		}
	}

	// Update position token amount.
	e.mu.Lock()
	pos.AmountToken = pos.AmountToken.Sub(sellAmount)
	if pos.AmountToken.IsNegative() {
		pos.AmountToken = decimal.Zero
	}
	remaining := pos.AmountToken.String()
	e.mu.Unlock()

	e.totalSells.Add(1)

	log.Info().
		Str("pos_id", posID).
		Str("remaining", remaining).
		Msg("sniper: partial sell completed")
}

// executeSell closes a position.
func (e *Engine) executeSell(ctx context.Context, pos *Position, reason string) {
	e.mu.Lock()
	if pos.Status == StatusClosed || pos.Status == StatusFailed {
		e.mu.Unlock()
		return
	}
	pos.Status = StatusClosing
	posID := pos.ID
	pnlPct := pos.PnLPct
	costSOL := pos.CostSOL
	e.mu.Unlock()

	log.Info().
		Str("pos_id", posID).
		Str("mint", string(pos.TokenMint)).
		Str("reason", reason).
		Float64("pnl_pct", pnlPct).
		Bool("dry_run", e.config.DryRun).
		Msg("sniper: EXECUTING SELL")

	now := time.Now()

	if e.config.DryRun {
		e.mu.Lock()
		pos.SellSignature = solana.Signature(fmt.Sprintf("DRYRUN-SELL-%s", posID))
		e.mu.Unlock()
		log.Info().
			Str("pos_id", posID).
			Msg("sniper: DRY RUN sell (no real transaction)")
	} else {
		e.mu.RLock()
		amountToken := pos.AmountToken
		e.mu.RUnlock()

		params := solana.SwapParams{
			InputMint:     pos.TokenMint,
			OutputMint:    solana.SOLMint,
			AmountIn:      amountToken,
			SlippageBps:   e.config.SlippageBps,
			PriorityFee:   e.config.PriorityFee,
			UseJitoBundle: e.config.UseJito,
		}

		result, err := e.jupiter.SwapDirect(ctx, params)
		if err != nil {
			e.mu.Lock()
			pos.SellRetries++
			if pos.SellRetries >= e.config.MaxSellRetries {
				log.Error().Err(err).Str("pos_id", posID).Int("retries", pos.SellRetries).
					Msg("sniper: sell PERMANENTLY FAILED after max retries")
				pos.Status = StatusFailed
				pos.CloseReason = fmt.Sprintf("SELL_FAILED_%s", reason)
				pos.ClosedAt = &now
			} else {
				log.Warn().Err(err).Str("pos_id", posID).Int("retry", pos.SellRetries).
					Msg("sniper: sell failed, will retry next cycle")
				pos.Status = StatusOpen // allow retry
			}
			e.mu.Unlock()
			return
		}
		e.mu.Lock()
		pos.SellSignature = result.Signature
		e.mu.Unlock()
	}

	e.mu.Lock()
	pos.Status = StatusClosed
	pos.ClosedAt = &now
	pos.CloseReason = reason
	pnlPct = pos.PnLPct // re-read under lock for accurate P&L

	// Track daily loss.
	if pnlPct < 0 {
		lossSOL := costSOL.Mul(decimal.NewFromFloat(-pnlPct / 100.0))
		e.dailyLossSOL = e.dailyLossSOL.Add(lossSOL)
		e.lossCount.Add(1)
	} else {
		e.winCount.Add(1)
	}

	// Cancel CSM for this position.
	if cancel, ok := e.csmCancel[posID]; ok {
		cancel()
		delete(e.csmCancel, posID)
	}

	// Cleanup tracker to prevent memory leak.
	delete(e.trackers, posID)

	cb := e.onPositionClose
	e.mu.Unlock()

	e.totalSells.Add(1)

	if cb != nil {
		cb(pos)
	}

	log.Info().
		Str("pos_id", posID).
		Str("mint", string(pos.TokenMint)).
		Str("reason", reason).
		Float64("pnl_pct", pnlPct).
		Msg("sniper: position CLOSED")
}

// ForceClose forces all open positions to close.
func (e *Engine) ForceClose(ctx context.Context) {
	shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Cancel all CSMs first.
	e.mu.Lock()
	for posID, csmCancel := range e.csmCancel {
		csmCancel()
		delete(e.csmCancel, posID)
	}
	var openPositions []*Position
	for _, p := range e.positions {
		if p.Status == StatusOpen {
			openPositions = append(openPositions, p)
		}
	}
	e.mu.Unlock()

	for _, pos := range openPositions {
		e.executeSell(shutdownCtx, pos, "FORCE_CLOSE")
	}
}

// reserveDailyBudget atomically checks and reserves daily budget.
func (e *Engine) reserveDailyBudget() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Reset if new day.
	if time.Now().After(e.dailyResetTime.Add(24 * time.Hour)) {
		e.dailySpentSOL = decimal.Zero
		e.dailyLossSOL = decimal.Zero
		e.dailyResetTime = startOfDay()
	}

	buyAmount := decimal.NewFromFloat(e.config.MaxBuySOL)
	maxSpend := decimal.NewFromFloat(e.config.MaxDailySpendSOL)
	maxLoss := decimal.NewFromFloat(e.config.MaxDailyLossSOL)

	// Check if we would exceed daily spend.
	if e.dailySpentSOL.Add(buyAmount).GreaterThan(maxSpend) {
		return false
	}

	// Check daily loss.
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
