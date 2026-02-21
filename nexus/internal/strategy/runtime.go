package strategy

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/rs/zerolog/log"
)

// Executor is the interface to the execution engine (decoupled).
// The runtime sends validated signals to the executor for downstream processing
// (risk check, order placement, etc.).
type Executor interface {
	Execute(ctx context.Context, signal bus.StrategySignal) error
}

// RuntimeConfig holds runtime-level configuration.
type RuntimeConfig struct {
	// DryRun when true prevents signals from being sent to the executor.
	DryRun bool
	// TimerIntervalMs is the interval in milliseconds for the timer tick.
	// Defaults to 1000 (1 second) if zero.
	TimerIntervalMs int
}

// registeredStrategy wraps a strategy with its config and runtime counters.
type registeredStrategy struct {
	strategy    Strategy
	config      StrategyConfig
	enabled     bool
	signalCount int64
	errorCount  int64
	// symbolSet is a set for O(1) symbol lookup.
	symbolSet map[string]struct{}
}

// StrategyStats exposes per-strategy runtime statistics.
type StrategyStats struct {
	StrategyID  string `json:"strategy_id"`
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	SignalCount int64  `json:"signal_count"`
	ErrorCount  int64  `json:"error_count"`
}

// Runtime hosts strategies, routes events, validates and dispatches signals.
type Runtime struct {
	mu         sync.RWMutex
	strategies map[string]*registeredStrategy
	producer   bus.Producer
	executor   Executor
	config     RuntimeConfig

	cancel context.CancelFunc
	wg     sync.WaitGroup

	started atomic.Bool
}

// NewRuntime creates a new strategy runtime.
func NewRuntime(producer bus.Producer, executor Executor, config RuntimeConfig) *Runtime {
	if config.TimerIntervalMs <= 0 {
		config.TimerIntervalMs = 1000
	}
	return &Runtime{
		strategies: make(map[string]*registeredStrategy),
		producer:   producer,
		executor:   executor,
		config:     config,
	}
}

// Register adds a strategy to the runtime. The strategy is initialized with the
// given config. Returns an error if the strategy ID is already registered or
// Init fails.
func (r *Runtime) Register(cfg StrategyConfig, strat Strategy) error {
	if cfg.StrategyID == "" {
		return fmt.Errorf("strategy config must have a non-empty StrategyID")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.strategies[cfg.StrategyID]; exists {
		return fmt.Errorf("strategy %q already registered", cfg.StrategyID)
	}

	if err := strat.Init(cfg); err != nil {
		return fmt.Errorf("init strategy %q: %w", cfg.StrategyID, err)
	}

	symSet := make(map[string]struct{}, len(cfg.Symbols))
	for _, s := range cfg.Symbols {
		symSet[s] = struct{}{}
	}

	r.strategies[cfg.StrategyID] = &registeredStrategy{
		strategy:  strat,
		config:    cfg,
		enabled:   true,
		symbolSet: symSet,
	}

	log.Info().
		Str("strategy_id", cfg.StrategyID).
		Str("name", strat.Name()).
		Strs("symbols", cfg.Symbols).
		Msg("Strategy registered")

	return nil
}

// Unregister removes a strategy from the runtime.
func (r *Runtime) Unregister(strategyID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.strategies[strategyID]; !exists {
		return fmt.Errorf("strategy %q not registered", strategyID)
	}
	delete(r.strategies, strategyID)
	return nil
}

// Enable enables a previously disabled strategy.
func (r *Runtime) Enable(strategyID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	reg, ok := r.strategies[strategyID]
	if !ok {
		return fmt.Errorf("strategy %q not registered", strategyID)
	}
	reg.enabled = true
	return nil
}

// Disable disables a strategy without removing it.
func (r *Runtime) Disable(strategyID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	reg, ok := r.strategies[strategyID]
	if !ok {
		return fmt.Errorf("strategy %q not registered", strategyID)
	}
	reg.enabled = false
	return nil
}

// OnMarketEvent fans the event to all registered strategies that subscribe to
// the event's symbol. Collects signals, validates them, and sends to executor.
func (r *Runtime) OnMarketEvent(ctx context.Context, event MarketEvent) error {
	symbol := eventSymbol(event)

	r.mu.RLock()
	// Collect matching strategies under read lock; release before calling strategy.
	type target struct {
		id  string
		reg *registeredStrategy
	}
	targets := make([]target, 0, len(r.strategies))
	for id, reg := range r.strategies {
		if !reg.enabled {
			continue
		}
		// If the strategy subscribes to this symbol (empty symbolSet = all symbols).
		if len(reg.symbolSet) == 0 {
			targets = append(targets, target{id, reg})
		} else if _, ok := reg.symbolSet[symbol]; ok {
			targets = append(targets, target{id, reg})
		}
	}
	r.mu.RUnlock()

	var firstErr error
	for _, t := range targets {
		signals := t.reg.strategy.OnEvent(event)
		for _, sig := range signals {
			if err := r.processSignal(ctx, t.id, sig); err != nil {
				atomic.AddInt64(&t.reg.errorCount, 1)
				if firstErr == nil {
					firstErr = err
				}
				log.Error().Err(err).
					Str("strategy_id", t.id).
					Msg("Error processing signal")
			} else {
				atomic.AddInt64(&t.reg.signalCount, 1)
			}
		}
	}
	return firstErr
}

// OnTimer calls OnTimer on all enabled strategies.
func (r *Runtime) OnTimer(ctx context.Context, ts time.Time, timerID string) error {
	tsUnix := ts.UnixMilli()

	r.mu.RLock()
	type target struct {
		id  string
		reg *registeredStrategy
	}
	targets := make([]target, 0, len(r.strategies))
	for id, reg := range r.strategies {
		if reg.enabled {
			targets = append(targets, target{id, reg})
		}
	}
	r.mu.RUnlock()

	var firstErr error
	for _, t := range targets {
		signals := t.reg.strategy.OnTimer(tsUnix, timerID)
		for _, sig := range signals {
			if err := r.processSignal(ctx, t.id, sig); err != nil {
				atomic.AddInt64(&t.reg.errorCount, 1)
				if firstErr == nil {
					firstErr = err
				}
			} else {
				atomic.AddInt64(&t.reg.signalCount, 1)
			}
		}
	}
	return firstErr
}

// OnIntelEvent routes an intel event to all enabled strategies whose symbol set
// overlaps with the event's asset tags. This is the bridge between the Intel
// Pipeline (Brain) and the Strategy Runtime.
func (r *Runtime) OnIntelEvent(ctx context.Context, event IntelEvent) error {
	r.mu.RLock()
	type target struct {
		id  string
		reg *registeredStrategy
	}
	targets := make([]target, 0, len(r.strategies))
	for id, reg := range r.strategies {
		if !reg.enabled {
			continue
		}
		// If the strategy subscribes to all symbols (empty set) or any matching asset tag.
		if len(reg.symbolSet) == 0 {
			targets = append(targets, target{id, reg})
		} else {
			for _, tag := range event.AssetTags {
				if _, ok := reg.symbolSet[tag]; ok {
					targets = append(targets, target{id, reg})
					break
				}
			}
		}
	}
	r.mu.RUnlock()

	var firstErr error
	for _, t := range targets {
		signals := t.reg.strategy.OnIntel(event)
		for _, sig := range signals {
			if err := r.processSignal(ctx, t.id, sig); err != nil {
				atomic.AddInt64(&t.reg.errorCount, 1)
				if firstErr == nil {
					firstErr = err
				}
				log.Error().Err(err).
					Str("strategy_id", t.id).
					Str("intel_event_id", event.EventID).
					Msg("Error processing intel signal")
			} else {
				atomic.AddInt64(&t.reg.signalCount, 1)
			}
		}
	}
	return firstErr
}

// OnFeatureUpdate wraps features as a series of MarketEvents and dispatches them.
func (r *Runtime) OnFeatureUpdate(ctx context.Context, symbol string, features map[string]float64) error {
	now := time.Now().UnixMilli()
	var firstErr error
	for name, value := range features {
		event := MarketEvent{
			Type: "feature",
			Feature: &FeatureUpdate{
				Symbol: symbol,
				Name:   name,
				Value:  value,
				Ts:     now,
			},
		}
		if err := r.OnMarketEvent(ctx, event); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Start launches the timer goroutine that periodically calls OnTimer.
func (r *Runtime) Start(ctx context.Context) error {
	if r.started.Load() {
		return fmt.Errorf("runtime already started")
	}
	r.started.Store(true)

	ctx, r.cancel = context.WithCancel(ctx)

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ticker := time.NewTicker(time.Duration(r.config.TimerIntervalMs) * time.Millisecond)
		defer ticker.Stop()

		log.Info().Int("interval_ms", r.config.TimerIntervalMs).Msg("Strategy runtime timer started")

		for {
			select {
			case <-ctx.Done():
				log.Info().Msg("Strategy runtime timer stopped")
				return
			case t := <-ticker.C:
				if err := r.OnTimer(ctx, t, "periodic"); err != nil {
					log.Error().Err(err).Msg("Timer callback error")
				}
			}
		}
	}()

	return nil
}

// Stop shuts down the runtime timer goroutine and waits for it to finish.
func (r *Runtime) Stop() {
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()
	r.started.Store(false)
	log.Info().Msg("Strategy runtime stopped")
}

// Stats returns per-strategy statistics.
func (r *Runtime) Stats() map[string]StrategyStats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	stats := make(map[string]StrategyStats, len(r.strategies))
	for id, reg := range r.strategies {
		stats[id] = StrategyStats{
			StrategyID:  id,
			Name:        reg.strategy.Name(),
			Enabled:     reg.enabled,
			SignalCount: atomic.LoadInt64(&reg.signalCount),
			ErrorCount:  atomic.LoadInt64(&reg.errorCount),
		}
	}
	return stats
}

// processSignal validates a signal and dispatches it to the executor and producer.
func (r *Runtime) processSignal(ctx context.Context, expectedStrategyID string, signal bus.StrategySignal) error {
	if err := r.validateSignal(expectedStrategyID, signal); err != nil {
		return fmt.Errorf("signal validation: %w", err)
	}

	// Publish signal event to the bus.
	if r.producer != nil {
		if err := r.producer.PublishJSON(ctx, "strategy.signals", signal.StrategyID, signal); err != nil {
			log.Error().Err(err).Str("strategy_id", signal.StrategyID).Msg("Failed to publish signal")
			// Non-fatal: continue to executor even if publish fails.
		}
	}

	// In dry-run mode, log but do not execute.
	if r.config.DryRun {
		log.Info().
			Str("strategy_id", signal.StrategyID).
			Str("symbol", signal.Symbol).
			Str("signal_type", signal.SignalType).
			Float64("confidence", signal.Confidence).
			Msg("DRY RUN: signal not executed")
		return nil
	}

	// Dispatch to executor.
	if r.executor != nil {
		if err := r.executor.Execute(ctx, signal); err != nil {
			return fmt.Errorf("executor: %w", err)
		}
	}

	return nil
}

// validateSignal checks that a signal is well-formed.
func (r *Runtime) validateSignal(expectedStrategyID string, signal bus.StrategySignal) error {
	if signal.StrategyID == "" {
		return fmt.Errorf("signal missing strategy_id")
	}
	if signal.StrategyID != expectedStrategyID {
		return fmt.Errorf("signal strategy_id %q does not match registered strategy %q",
			signal.StrategyID, expectedStrategyID)
	}

	r.mu.RLock()
	_, registered := r.strategies[signal.StrategyID]
	r.mu.RUnlock()
	if !registered {
		return fmt.Errorf("signal from unregistered strategy %q", signal.StrategyID)
	}

	if signal.Confidence < 0 || signal.Confidence > 1 {
		return fmt.Errorf("signal confidence %.4f out of range [0, 1]", signal.Confidence)
	}

	for i, intent := range signal.OrdersIntent {
		if intent.Exchange == "" {
			return fmt.Errorf("order intent[%d] missing exchange", i)
		}
		if intent.Symbol == "" {
			return fmt.Errorf("order intent[%d] missing symbol", i)
		}
		if intent.Side != "buy" && intent.Side != "sell" {
			return fmt.Errorf("order intent[%d] invalid side %q (must be buy|sell)", i, intent.Side)
		}
	}

	return nil
}

// eventSymbol extracts the symbol from a MarketEvent.
func eventSymbol(event MarketEvent) string {
	switch event.Type {
	case "tick":
		if event.Tick != nil {
			return event.Tick.Symbol
		}
	case "trade":
		if event.Trade != nil {
			return event.Trade.Symbol
		}
	case "orderbook":
		if event.Orderbook != nil {
			return event.Orderbook.Symbol
		}
	case "feature":
		if event.Feature != nil {
			return event.Feature.Symbol
		}
	case "regime":
		if event.Regime != nil {
			return event.Regime.Symbol
		}
	case "fill":
		if event.Fill != nil {
			return event.Fill.Symbol
		}
	}
	return ""
}
