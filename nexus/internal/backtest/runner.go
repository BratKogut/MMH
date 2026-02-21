package backtest

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/nexus-trading/nexus/internal/execution"
	"github.com/nexus-trading/nexus/internal/risk"
	"github.com/nexus-trading/nexus/internal/strategy"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// BacktestConfig
// ---------------------------------------------------------------------------

// BacktestConfig configures a backtest run.
type BacktestConfig struct {
	StartTime       time.Time
	EndTime         time.Time
	Symbols         []string
	Exchanges       []string
	InitialCapital  float64
	SlippageModel   SlippageModel
	FeeModel        FeeModel
	TimerIntervalMs int // strategy timer interval in ms; 0 = no timers
}

// ---------------------------------------------------------------------------
// DataSource
// ---------------------------------------------------------------------------

// DataSource provides historical market data for backtesting.
type DataSource interface {
	// LoadTrades returns trades for a symbol in a time range, ordered by timestamp.
	LoadTrades(ctx context.Context, exchange, symbol string, start, end time.Time) ([]bus.Trade, error)
	// LoadTicks returns ticks for a symbol in a time range, ordered by timestamp.
	LoadTicks(ctx context.Context, exchange, symbol string, start, end time.Time) ([]bus.Tick, error)
}

// ---------------------------------------------------------------------------
// InMemoryDataSource
// ---------------------------------------------------------------------------

// InMemoryDataSource holds data in memory for testing or small datasets.
type InMemoryDataSource struct {
	Trades map[string][]bus.Trade // key: "exchange.symbol"
	Ticks  map[string][]bus.Tick  // key: "exchange.symbol"
}

// NewInMemoryDataSource creates a new empty in-memory data source.
func NewInMemoryDataSource() *InMemoryDataSource {
	return &InMemoryDataSource{
		Trades: make(map[string][]bus.Trade),
		Ticks:  make(map[string][]bus.Tick),
	}
}

// dsKey builds the map key for an exchange+symbol pair.
func dsKey(exchange, symbol string) string {
	return exchange + "." + symbol
}

// AddTrade appends a trade to the in-memory store.
func (ds *InMemoryDataSource) AddTrade(trade bus.Trade) {
	k := dsKey(trade.Exchange, trade.Symbol)
	ds.Trades[k] = append(ds.Trades[k], trade)
}

// AddTick appends a tick to the in-memory store.
func (ds *InMemoryDataSource) AddTick(tick bus.Tick) {
	k := dsKey(tick.Exchange, tick.Symbol)
	ds.Ticks[k] = append(ds.Ticks[k], tick)
}

// LoadTrades returns trades within the given time range, sorted by timestamp.
func (ds *InMemoryDataSource) LoadTrades(_ context.Context, exchange, symbol string, start, end time.Time) ([]bus.Trade, error) {
	k := dsKey(exchange, symbol)
	all := ds.Trades[k]
	var result []bus.Trade
	for _, t := range all {
		if !t.Timestamp.Before(start) && !t.Timestamp.After(end) {
			result = append(result, t)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Timestamp.Before(result[j].Timestamp)
	})
	return result, nil
}

// LoadTicks returns ticks within the given time range, sorted by timestamp.
func (ds *InMemoryDataSource) LoadTicks(_ context.Context, exchange, symbol string, start, end time.Time) ([]bus.Tick, error) {
	k := dsKey(exchange, symbol)
	all := ds.Ticks[k]
	var result []bus.Tick
	for _, t := range all {
		if !t.Timestamp.Before(start) && !t.Timestamp.After(end) {
			result = append(result, t)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Timestamp.Before(result[j].Timestamp)
	})
	return result, nil
}

// ---------------------------------------------------------------------------
// BacktestResult
// ---------------------------------------------------------------------------

// BacktestResult holds the results of a backtest run.
type BacktestResult struct {
	Config     BacktestConfig
	Metrics    PerformanceMetrics
	Trades     []TradeRecord
	Equity     []EquityPoint
	Signals    []bus.StrategySignal
	Duration   time.Duration
	EventCount int
}

// EquityPoint represents a single point on the equity curve.
type EquityPoint struct {
	Time  time.Time
	Value float64
}

// ---------------------------------------------------------------------------
// backtestEvent: unified event wrapper for time-ordered replay
// ---------------------------------------------------------------------------

type backtestEvent struct {
	ts    time.Time
	trade *bus.Trade
	tick  *bus.Tick
}

// ---------------------------------------------------------------------------
// backtestExecutor: captures signals and routes through paper fill logic
// ---------------------------------------------------------------------------

// backtestExecutor implements strategy.Executor for backtest mode.
// It applies risk checks, slippage/fee models, and records fills.
type backtestExecutor struct {
	riskEngine    *risk.Engine
	posMgr        *execution.PositionManager
	slippageModel SlippageModel
	feeModel      FeeModel
	signals       []bus.StrategySignal
	trades        []TradeRecord
	fees          float64
	realizedPnL   float64

	// Track open entries for round-trip trade recording.
	// key: "strategy.exchange.symbol"
	openEntries map[string]*openEntry
}

type openEntry struct {
	side       string
	entryPrice float64
	qty        float64
	entryTime  time.Time
	totalFee   float64
	symbol     string
}

func newBacktestExecutor(riskEngine *risk.Engine, posMgr *execution.PositionManager, slipModel SlippageModel, feeModel FeeModel) *backtestExecutor {
	return &backtestExecutor{
		riskEngine:    riskEngine,
		posMgr:        posMgr,
		slippageModel: slipModel,
		feeModel:      feeModel,
		openEntries:   make(map[string]*openEntry),
	}
}

func (e *backtestExecutor) Execute(_ context.Context, signal bus.StrategySignal) error {
	e.signals = append(e.signals, signal)

	intents := e.signalToIntents(signal)
	for _, intent := range intents {
		// Risk check.
		decision := e.riskEngine.Check(intent)
		if !decision.Allowed {
			log.Debug().
				Str("intent_id", intent.IntentID).
				Strs("reasons", decision.ReasonCodes).
				Msg("backtest: risk denied")
			continue
		}

		// Simulate fill with slippage + fees.
		e.simulateFill(intent)
	}
	return nil
}

func (e *backtestExecutor) signalToIntents(signal bus.StrategySignal) []bus.OrderIntent {
	// If explicit order intents are provided, use them.
	if len(signal.OrdersIntent) > 0 {
		intents := make([]bus.OrderIntent, len(signal.OrdersIntent))
		copy(intents, signal.OrdersIntent)
		for i := range intents {
			if intents[i].IntentID == "" {
				intents[i].IntentID = uuid.New().String()
			}
			if intents[i].StrategyID == "" {
				intents[i].StrategyID = signal.StrategyID
			}
		}
		return intents
	}

	// For target_position signals, compute the delta.
	if signal.SignalType == "target_position" {
		return e.computeTargetIntents(signal)
	}

	return nil
}

func (e *backtestExecutor) computeTargetIntents(signal bus.StrategySignal) []bus.OrderIntent {
	exchange := ""
	if ex, ok := signal.Metadata["exchange"].(string); ok {
		exchange = ex
	}
	if exchange == "" {
		return nil
	}

	currentQty := decimal.Zero
	if pos := e.posMgr.GetPosition(signal.StrategyID, exchange, signal.Symbol); pos != nil {
		currentQty = pos.Qty
	}

	diff := signal.TargetPosition.Sub(currentQty)
	if diff.IsZero() {
		return nil
	}

	side := "buy"
	qty := diff
	if diff.IsNegative() {
		side = "sell"
		qty = diff.Abs()
	}

	intent := bus.OrderIntent{
		BaseEvent:   bus.NewBaseEvent("backtest-engine", "1.0"),
		IntentID:    uuid.New().String(),
		StrategyID:  signal.StrategyID,
		Exchange:    exchange,
		Symbol:      signal.Symbol,
		Side:        side,
		OrderType:   "market",
		Qty:         qty,
		TimeInForce: "IOC",
	}
	intent.TraceID = signal.TraceID
	intent.CausationID = signal.EventID

	return []bus.OrderIntent{intent}
}

func (e *backtestExecutor) simulateFill(intent bus.OrderIntent) {
	price := intent.LimitPrice.InexactFloat64()
	if price == 0 {
		// Market orders without a reference price cannot be filled meaningfully.
		log.Warn().Str("intent_id", intent.IntentID).Msg("backtest: no reference price, skipping fill")
		return
	}
	qty := intent.Qty.InexactFloat64()

	// Compute slippage.
	slippage := 0.0
	if e.slippageModel != nil {
		slippage = e.slippageModel.EstimateSlippage(price, qty, intent.Side, intent.Symbol)
	}
	fillPrice := price + slippage

	// Compute fee.
	fee := 0.0
	if e.feeModel != nil {
		// Backtest fills are always taker (crossing the spread).
		fee = e.feeModel.EstimateFee(fillPrice, qty, intent.Side, intent.Exchange, false)
	}
	e.fees += fee

	// Build bus.Fill and apply to position manager.
	fill := bus.Fill{
		BaseEvent:     bus.NewBaseEvent("backtest-broker", "1.0"),
		Exchange:      intent.Exchange,
		Symbol:        intent.Symbol,
		OrderID:       "BT-" + intent.IntentID[:8],
		IntentID:      intent.IntentID,
		StrategyID:    intent.StrategyID,
		Side:          intent.Side,
		Qty:           intent.Qty,
		Price:         decimal.NewFromFloat(fillPrice),
		Fee:           decimal.NewFromFloat(fee),
		FeeCurrency:   "USD",
		ExpectedPrice: intent.LimitPrice,
		SlippageBps:   math.Abs(slippage) / price * 10000.0,
	}
	fill.TraceID = intent.TraceID
	fill.Timestamp = intent.Timestamp

	if err := e.posMgr.ApplyFill(fill); err != nil {
		log.Error().Err(err).Str("intent_id", intent.IntentID).Msg("backtest: apply fill failed")
		return
	}

	// Track round-trip trades.
	e.recordTradeEntry(intent, fillPrice, qty, fee, slippage)
}

func (e *backtestExecutor) recordTradeEntry(intent bus.OrderIntent, fillPrice, qty, fee, slippage float64) {
	key := fmt.Sprintf("%s.%s.%s", intent.StrategyID, intent.Exchange, intent.Symbol)
	entry, hasOpen := e.openEntries[key]

	if !hasOpen {
		// Opening a new position.
		e.openEntries[key] = &openEntry{
			side:       intent.Side,
			entryPrice: fillPrice,
			qty:        qty,
			entryTime:  intent.Timestamp,
			totalFee:   fee,
			symbol:     intent.Symbol,
		}
		return
	}

	// Check if this fill is closing the position (opposite side).
	if entry.side != intent.Side {
		// Closing trade -- compute round-trip PnL.
		closeQty := math.Min(qty, entry.qty)
		var pnl float64
		if entry.side == "buy" {
			// Was long, now selling: pnl = (exit - entry) * qty
			pnl = (fillPrice - entry.entryPrice) * closeQty
		} else {
			// Was short, now buying: pnl = (entry - exit) * qty
			pnl = (entry.entryPrice - fillPrice) * closeQty
		}
		totalFee := entry.totalFee + fee
		pnl -= totalFee

		tr := TradeRecord{
			Symbol:     intent.Symbol,
			Side:       entry.side,
			EntryPrice: entry.entryPrice,
			ExitPrice:  fillPrice,
			Qty:        closeQty,
			PnL:        pnl,
			Fees:       totalFee,
			Slippage:   math.Abs(slippage),
			EntryTime:  entry.entryTime,
			ExitTime:   intent.Timestamp,
		}
		e.trades = append(e.trades, tr)
		e.realizedPnL += pnl

		remaining := entry.qty - qty
		if remaining <= 1e-12 {
			// Fully closed.
			delete(e.openEntries, key)

			// If the fill qty exceeded the previous position, open a new one
			// in the opposite direction with the excess.
			excess := qty - entry.qty
			if excess > 1e-12 {
				e.openEntries[key] = &openEntry{
					side:       intent.Side,
					entryPrice: fillPrice,
					qty:        excess,
					entryTime:  intent.Timestamp,
					totalFee:   0,
					symbol:     intent.Symbol,
				}
			}
		} else {
			// Partially closed; reduce the open entry qty.
			entry.qty = remaining
		}
	} else {
		// Adding to existing position -- update weighted average entry.
		totalQty := entry.qty + qty
		entry.entryPrice = (entry.entryPrice*entry.qty + fillPrice*qty) / totalQty
		entry.qty = totalQty
		entry.totalFee += fee
	}
}

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

// Runner executes a backtest.
type Runner struct {
	config      BacktestConfig
	dataSource  DataSource
	strat       strategy.Strategy
	stratConfig strategy.StrategyConfig
	riskCfg     risk.Config
}

// NewRunner creates a new backtest runner.
func NewRunner(config BacktestConfig, dataSource DataSource, strat strategy.Strategy, stratConfig strategy.StrategyConfig, riskCfg risk.Config) *Runner {
	return &Runner{
		config:      config,
		dataSource:  dataSource,
		strat:       strat,
		stratConfig: stratConfig,
		riskCfg:     riskCfg,
	}
}

// Run executes the backtest: loads data, replays events through
// strategy+risk+execution, and returns the result with metrics.
//
// The backtest is deterministic: same inputs produce same outputs.
func (r *Runner) Run(ctx context.Context) (*BacktestResult, error) {
	startWall := time.Now()

	// 1. Create infrastructure stubs.
	producer := bus.NewStubProducer()
	riskEngine := risk.New(r.riskCfg)
	posMgr := execution.NewPositionManager()

	// Apply defaults for models.
	slipModel := r.config.SlippageModel
	if slipModel == nil {
		slipModel = &FixedSlippage{BasisPoints: 0}
	}
	feeModel := r.config.FeeModel
	if feeModel == nil {
		feeModel = &FixedFee{MakerFeePct: 0, TakerFeePct: 0}
	}

	btExecutor := newBacktestExecutor(riskEngine, posMgr, slipModel, feeModel)

	// 2. Create strategy runtime and register strategy.
	runtimeCfg := strategy.RuntimeConfig{
		DryRun:          false,
		TimerIntervalMs: r.config.TimerIntervalMs,
	}
	if runtimeCfg.TimerIntervalMs <= 0 {
		runtimeCfg.TimerIntervalMs = 1000
	}
	rt := strategy.NewRuntime(producer, btExecutor, runtimeCfg)
	if err := rt.Register(r.stratConfig, r.strat); err != nil {
		return nil, fmt.Errorf("register strategy: %w", err)
	}

	// 3. Load all trades and ticks from the data source.
	var allEvents []backtestEvent

	for _, exchange := range r.config.Exchanges {
		for _, symbol := range r.config.Symbols {
			trades, err := r.dataSource.LoadTrades(ctx, exchange, symbol, r.config.StartTime, r.config.EndTime)
			if err != nil {
				return nil, fmt.Errorf("load trades %s.%s: %w", exchange, symbol, err)
			}
			for i := range trades {
				allEvents = append(allEvents, backtestEvent{
					ts:    trades[i].Timestamp,
					trade: &trades[i],
				})
			}

			ticks, err := r.dataSource.LoadTicks(ctx, exchange, symbol, r.config.StartTime, r.config.EndTime)
			if err != nil {
				return nil, fmt.Errorf("load ticks %s.%s: %w", exchange, symbol, err)
			}
			for i := range ticks {
				allEvents = append(allEvents, backtestEvent{
					ts:   ticks[i].Timestamp,
					tick: &ticks[i],
				})
			}
		}
	}

	// 4. Sort all events by timestamp (stable sort for determinism).
	sort.SliceStable(allEvents, func(i, j int) bool {
		return allEvents[i].ts.Before(allEvents[j].ts)
	})

	log.Info().
		Int("event_count", len(allEvents)).
		Time("start", r.config.StartTime).
		Time("end", r.config.EndTime).
		Msg("backtest: events loaded and sorted")

	// 5. Feed events through strategy runtime in timestamp order.
	equity := make([]EquityPoint, 0, len(allEvents)/10+1)
	// Record initial equity point.
	equity = append(equity, EquityPoint{
		Time:  r.config.StartTime,
		Value: r.config.InitialCapital,
	})

	// Track last known prices per symbol for unrealized PnL.
	lastPrices := make(map[string]float64)

	var lastTimerFire time.Time
	timerInterval := time.Duration(runtimeCfg.TimerIntervalMs) * time.Millisecond
	eventCount := 0

	for _, evt := range allEvents {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		eventCount++

		// Update last known prices from this event.
		if evt.trade != nil {
			lastPrices[evt.trade.Symbol] = evt.trade.Price.InexactFloat64()
		} else if evt.tick != nil {
			bid := evt.tick.Bid.InexactFloat64()
			ask := evt.tick.Ask.InexactFloat64()
			if bid > 0 && ask > 0 {
				lastPrices[evt.tick.Symbol] = (bid + ask) / 2.0
			}
		}

		// Fire timers if enough simulated time has passed.
		if r.config.TimerIntervalMs > 0 && !lastTimerFire.IsZero() {
			for !lastTimerFire.Add(timerInterval).After(evt.ts) {
				nextFire := lastTimerFire.Add(timerInterval)
				_ = rt.OnTimer(ctx, nextFire, "periodic")
				lastTimerFire = nextFire
			}
		}
		if lastTimerFire.IsZero() {
			lastTimerFire = evt.ts
		}

		// Build MarketEvent and dispatch.
		var mktEvent strategy.MarketEvent
		if evt.trade != nil {
			mktEvent = strategy.MarketEvent{
				Type:  "trade",
				Trade: evt.trade,
			}
		} else if evt.tick != nil {
			mktEvent = strategy.MarketEvent{
				Type: "tick",
				Tick: evt.tick,
			}
		}

		if err := rt.OnMarketEvent(ctx, mktEvent); err != nil {
			log.Debug().Err(err).Msg("backtest: event processing error")
		}

		// 6. Record equity point after each event.
		// Equity = initial capital + realized PnL + unrealized PnL from open positions.
		currentEquity := r.config.InitialCapital + btExecutor.realizedPnL
		currentEquity += computeUnrealizedPnL(btExecutor.openEntries, lastPrices)

		equity = append(equity, EquityPoint{
			Time:  evt.ts,
			Value: currentEquity,
		})
	}

	// 7. Compute final metrics.
	metrics := computeRunnerMetrics(btExecutor.trades, equity, r.config.InitialCapital)

	result := &BacktestResult{
		Config:     r.config,
		Metrics:    metrics,
		Trades:     btExecutor.trades,
		Equity:     equity,
		Signals:    btExecutor.signals,
		Duration:   time.Since(startWall),
		EventCount: eventCount,
	}

	log.Info().
		Int("total_trades", metrics.TradeCount).
		Float64("net_pnl", metrics.NetPnL).
		Float64("max_drawdown_pct", metrics.MaxDrawdownPct*100).
		Float64("sharpe", metrics.SharpeRatio).
		Float64("win_rate", metrics.WinRate*100).
		Dur("wall_time", result.Duration).
		Msg("backtest: complete")

	return result, nil
}

// computeUnrealizedPnL computes unrealized PnL from open entries using
// the last known prices per symbol.
func computeUnrealizedPnL(entries map[string]*openEntry, lastPrices map[string]float64) float64 {
	unrealized := 0.0
	for _, entry := range entries {
		px, ok := lastPrices[entry.symbol]
		if !ok {
			continue
		}
		if entry.side == "buy" {
			unrealized += (px - entry.entryPrice) * entry.qty
		} else {
			unrealized += (entry.entryPrice - px) * entry.qty
		}
	}
	return unrealized
}
