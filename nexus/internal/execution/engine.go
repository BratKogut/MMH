package execution

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/nexus-trading/nexus/internal/adapters"
	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/nexus-trading/nexus/internal/risk"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Execution Engine
// ---------------------------------------------------------------------------

// Engine routes orders to exchanges and manages their full lifecycle.
// Thread-safe: multiple goroutines may call Execute concurrently.
//
// Invariants:
//   - Every order passes risk check BEFORE reaching an exchange.
//   - Intent IDs are de-duplicated (idempotent).
//   - dryRun mode is a first-class citizen: logs exactly what would happen.
//   - Every state change is logged with trace_id and intent_id.
type Engine struct {
	mu       sync.RWMutex
	orders   map[string]*Order                  // intentID -> Order
	adapters map[string]adapters.ExchangeAdapter // exchange name -> adapter

	riskEngine *risk.Engine
	producer   bus.Producer
	posMgr     *PositionManager
	dryRun     bool
}

// NewEngine creates a new execution engine.
// If dryRun is true, orders are logged but never sent to an exchange.
func NewEngine(riskEngine *risk.Engine, producer bus.Producer, posMgr *PositionManager, dryRun bool) *Engine {
	return &Engine{
		orders:     make(map[string]*Order),
		adapters:   make(map[string]adapters.ExchangeAdapter),
		riskEngine: riskEngine,
		producer:   producer,
		posMgr:     posMgr,
		dryRun:     dryRun,
	}
}

// RegisterAdapter registers an exchange adapter for order routing.
func (e *Engine) RegisterAdapter(name string, adapter adapters.ExchangeAdapter) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.adapters[name] = adapter
	log.Info().Str("adapter", name).Msg("Exchange adapter registered")
}

// PositionMgr returns the engine's position manager.
func (e *Engine) PositionMgr() *PositionManager {
	return e.posMgr
}

// ---------------------------------------------------------------------------
// Execute — main entry point for strategy signals
// ---------------------------------------------------------------------------

// Execute converts a StrategySignal into one or more order intents, performs
// risk checks, and routes them to the appropriate exchange adapter.
func (e *Engine) Execute(ctx context.Context, signal bus.StrategySignal) error {
	traceID := signal.TraceID
	if traceID == "" {
		traceID = uuid.New().String()[:16]
	}

	intents := e.signalToIntents(signal, traceID)
	if len(intents) == 0 {
		log.Warn().
			Str("trace_id", traceID).
			Str("strategy_id", signal.StrategyID).
			Str("signal_type", signal.SignalType).
			Msg("No order intents derived from signal")
		return nil
	}

	var firstErr error
	for i := range intents {
		if err := e.executeIntent(ctx, &intents[i], traceID); err != nil {
			log.Error().Err(err).
				Str("trace_id", traceID).
				Str("intent_id", intents[i].IntentID).
				Msg("Failed to execute intent")
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// signalToIntents converts a StrategySignal to concrete OrderIntents.
func (e *Engine) signalToIntents(signal bus.StrategySignal, traceID string) []bus.OrderIntent {
	// If intents are explicitly provided, use them directly.
	if len(signal.OrdersIntent) > 0 {
		intents := make([]bus.OrderIntent, len(signal.OrdersIntent))
		copy(intents, signal.OrdersIntent)
		for i := range intents {
			if intents[i].TraceID == "" {
				intents[i].TraceID = traceID
			}
			if intents[i].StrategyID == "" {
				intents[i].StrategyID = signal.StrategyID
			}
			if intents[i].IntentID == "" {
				intents[i].IntentID = uuid.New().String()
			}
			if intents[i].CausationID == "" {
				intents[i].CausationID = signal.EventID
			}
			if intents[i].CorrelationID == "" {
				intents[i].CorrelationID = signal.CorrelationID
			}
		}
		return intents
	}

	// For target_position signals, compute the delta needed.
	if signal.SignalType == "target_position" {
		return e.computeTargetPositionIntents(signal, traceID)
	}

	return nil
}

// computeTargetPositionIntents computes the order(s) required to move from the
// current position to the target position declared in the signal.
func (e *Engine) computeTargetPositionIntents(signal bus.StrategySignal, traceID string) []bus.OrderIntent {
	// Determine exchange from metadata or fall back to first registered adapter.
	exchange := ""
	if ex, ok := signal.Metadata["exchange"].(string); ok {
		exchange = ex
	} else {
		e.mu.RLock()
		for name := range e.adapters {
			exchange = name
			break
		}
		e.mu.RUnlock()
	}
	if exchange == "" {
		log.Error().
			Str("trace_id", traceID).
			Str("strategy_id", signal.StrategyID).
			Msg("No exchange available for target_position signal")
		return nil
	}

	// Get current position for this strategy+exchange+symbol.
	currentQty := decimal.Zero
	if pos := e.posMgr.GetPosition(signal.StrategyID, exchange, signal.Symbol); pos != nil {
		currentQty = pos.Qty
	}

	diff := signal.TargetPosition.Sub(currentQty)
	if diff.IsZero() {
		log.Debug().
			Str("trace_id", traceID).
			Str("strategy_id", signal.StrategyID).
			Str("symbol", signal.Symbol).
			Msg("Already at target position, no orders needed")
		return nil
	}

	side := "buy"
	qty := diff
	if diff.IsNegative() {
		side = "sell"
		qty = diff.Abs()
	}

	intent := bus.OrderIntent{
		BaseEvent:   bus.NewBaseEvent("execution-engine", "1.0"),
		IntentID:    uuid.New().String(),
		StrategyID:  signal.StrategyID,
		Exchange:    exchange,
		Symbol:      signal.Symbol,
		Side:        side,
		OrderType:   "market",
		Qty:         qty,
		TimeInForce: "IOC",
	}
	intent.TraceID = traceID
	intent.CausationID = signal.EventID
	intent.CorrelationID = signal.CorrelationID

	return []bus.OrderIntent{intent}
}

// executeIntent processes a single OrderIntent through the full lifecycle:
// idempotency check -> risk check -> order creation -> exchange submission.
func (e *Engine) executeIntent(ctx context.Context, intent *bus.OrderIntent, traceID string) error {
	lg := log.With().
		Str("trace_id", traceID).
		Str("intent_id", intent.IntentID).
		Str("strategy_id", intent.StrategyID).
		Str("exchange", intent.Exchange).
		Str("symbol", intent.Symbol).
		Str("side", intent.Side).
		Str("qty", intent.Qty.String()).
		Logger()

	// ---- Step 1: Idempotency check (fast path under read lock) ----
	e.mu.RLock()
	_, exists := e.orders[intent.IntentID]
	e.mu.RUnlock()
	if exists {
		lg.Warn().Msg("Duplicate intent_id detected, skipping")
		return nil
	}

	// ---- Step 2: Risk check — MANDATORY, no order reaches exchange without this ----
	decision := e.riskEngine.Check(*intent)
	if !decision.Allowed {
		lg.Warn().
			Strs("reasons", decision.ReasonCodes).
			Msg("Risk check DENIED order")

		// Publish risk decision event for audit trail.
		riskEvt := bus.RiskDecision{
			BaseEvent:   bus.NewBaseEvent("execution-engine", "1.0"),
			IntentID:    intent.IntentID,
			Decision:    "deny",
			ReasonCodes: decision.ReasonCodes,
		}
		riskEvt.TraceID = traceID
		_ = e.producer.PublishJSON(ctx, bus.Topics.RiskPretradeChecks(), intent.IntentID, riskEvt)

		return fmt.Errorf("risk denied intent %s: %v", intent.IntentID, decision.ReasonCodes)
	}

	lg.Info().Msg("Risk check ALLOWED")

	// Publish allow decision for audit.
	allowEvt := bus.RiskDecision{
		BaseEvent:   bus.NewBaseEvent("execution-engine", "1.0"),
		IntentID:    intent.IntentID,
		Decision:    "allow",
		ReasonCodes: nil,
	}
	allowEvt.TraceID = traceID
	_ = e.producer.PublishJSON(ctx, bus.Topics.RiskPretradeChecks(), intent.IntentID, allowEvt)

	// ---- Step 3: Create Order via the state machine (state = CREATED) ----
	order := NewOrder(*intent)

	// Insert into orders map under write lock (double-check for races).
	e.mu.Lock()
	if _, exists := e.orders[intent.IntentID]; exists {
		e.mu.Unlock()
		lg.Warn().Msg("Duplicate intent_id detected (race condition), skipping")
		return nil
	}
	e.orders[intent.IntentID] = order
	e.mu.Unlock()

	// ---- Step 4: Transition to SUBMITTED ----
	if err := order.Transition(EventSubmit, nil); err != nil {
		lg.Error().Err(err).Msg("Failed to transition to SUBMITTED")
		return err
	}
	lg.Info().Str("state", string(OrderSubmitted)).Msg("Order state transition")

	// ---- Step 5: Dry-run — first-class citizen ----
	if e.dryRun {
		lg.Info().
			Str("order_type", intent.OrderType).
			Str("limit_price", intent.LimitPrice.String()).
			Str("time_in_force", intent.TimeInForce).
			Msg("DRY RUN: Would submit order to exchange (no exchange call made)")

		// Simulate exchange acceptance so downstream logic can proceed.
		_ = order.Transition(EventAck, &AckData{OrderID: "DRYRUN-" + intent.IntentID})
		return nil
	}

	// ---- Step 6: Route to the correct adapter ----
	e.mu.RLock()
	adapter, ok := e.adapters[intent.Exchange]
	e.mu.RUnlock()
	if !ok {
		_ = order.Transition(EventReject, nil)
		lg.Error().Msg("No adapter registered for exchange")
		return fmt.Errorf("no adapter for exchange: %s", intent.Exchange)
	}

	// ---- Step 7: Submit to exchange ----
	ack, err := adapter.SubmitOrder(ctx, *intent)
	if err != nil {
		_ = order.Transition(EventReject, nil)
		lg.Error().Err(err).Msg("Exchange submission failed")
		return fmt.Errorf("submit order to %s: %w", intent.Exchange, err)
	}

	// ---- Step 8: Transition based on exchange acknowledgement ----
	if ack.Status == "accepted" {
		if err := order.Transition(EventAck, &AckData{OrderID: ack.OrderID}); err != nil {
			lg.Error().Err(err).Msg("Failed to transition to ACKED")
			return err
		}
		lg.Info().
			Str("exchange_oid", ack.OrderID).
			Str("state", string(OrderAcked)).
			Msg("Order state transition")
	} else {
		if err := order.Transition(EventReject, nil); err != nil {
			lg.Error().Err(err).Msg("Failed to transition to REJECTED")
			return err
		}
		lg.Warn().
			Str("reason", ack.Reason).
			Str("state", string(OrderRejected)).
			Msg("Order REJECTED by exchange")
	}

	// ---- Step 9: Publish execution event ----
	_ = e.producer.PublishJSON(ctx, bus.Topics.Orders(intent.Exchange), intent.IntentID, order)

	return nil
}

// ---------------------------------------------------------------------------
// HandleFill — processes an incoming fill from an exchange
// ---------------------------------------------------------------------------

// HandleFill processes a fill event: transitions the order state, updates the
// position manager, and publishes the fill event.
func (e *Engine) HandleFill(ctx context.Context, fill bus.Fill) error {
	traceID := fill.TraceID
	lg := log.With().
		Str("trace_id", traceID).
		Str("intent_id", fill.IntentID).
		Str("exchange", fill.Exchange).
		Str("symbol", fill.Symbol).
		Str("side", fill.Side).
		Str("fill_qty", fill.Qty.String()).
		Str("fill_price", fill.Price.String()).
		Logger()

	// Find the order this fill belongs to.
	e.mu.RLock()
	order, ok := e.orders[fill.IntentID]
	e.mu.RUnlock()
	if !ok {
		lg.Warn().Msg("Received fill for unknown order intent")
		return fmt.Errorf("unknown order for intent: %s", fill.IntentID)
	}

	// Build the FillRecord for the state machine.
	fr := &FillRecord{
		FillID:      fill.EventID,
		Qty:         fill.Qty,
		Price:       fill.Price,
		Fee:         fill.Fee,
		FeeCcy:      fill.FeeCurrency,
		Timestamp:   fill.Timestamp,
		SlippageBps: fill.SlippageBps,
	}

	// Determine whether this is a partial or complete fill.
	remaining := order.RemainingQty()
	event := EventPartialFill
	if fill.Qty.GreaterThanOrEqual(remaining) {
		event = EventFill
	}

	// Transition the order state machine.
	if err := order.Transition(event, fr); err != nil {
		lg.Error().Err(err).Str("event", string(event)).Msg("Failed to transition order on fill")
		return fmt.Errorf("fill transition for %s: %w", fill.IntentID, err)
	}

	lg.Info().
		Str("filled_qty", order.FilledQty.String()).
		Str("total_qty", order.Qty.String()).
		Str("state", string(order.GetState())).
		Msg("Fill applied to order")

	// Update position manager.
	if err := e.posMgr.ApplyFill(fill); err != nil {
		lg.Error().Err(err).Msg("Failed to apply fill to position manager")
		return err
	}

	// Publish fill event.
	_ = e.producer.PublishJSON(ctx, bus.Topics.Fills(fill.Exchange), fill.IntentID, fill)

	return nil
}

// ---------------------------------------------------------------------------
// CancelOrder
// ---------------------------------------------------------------------------

// CancelOrder requests cancellation of an active order.
func (e *Engine) CancelOrder(ctx context.Context, intentID string) error {
	lg := log.With().Str("intent_id", intentID).Logger()

	e.mu.RLock()
	order, ok := e.orders[intentID]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown order: %s", intentID)
	}

	// Transition to CANCEL_PENDING.
	if err := order.Transition(EventRequestCancel, nil); err != nil {
		lg.Error().Err(err).Msg("Cannot cancel order in current state")
		return fmt.Errorf("cannot cancel order %s: %w", intentID, err)
	}
	lg.Info().Str("state", string(OrderCancelPending)).Msg("Order state transition")

	// Dry-run: simulate cancellation.
	if e.dryRun {
		lg.Info().Msg("DRY RUN: Would cancel order on exchange (no exchange call made)")
		_ = order.Transition(EventCancelAck, nil)
		return nil
	}

	// Route to adapter using exchange-assigned OrderID.
	exchangeOID := order.OrderID
	e.mu.RLock()
	adapter, ok := e.adapters[order.Exchange]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no adapter for exchange: %s", order.Exchange)
	}

	if err := adapter.CancelOrder(ctx, exchangeOID); err != nil {
		lg.Error().Err(err).Msg("Exchange cancel failed")
		return fmt.Errorf("cancel order on %s: %w", order.Exchange, err)
	}

	if err := order.Transition(EventCancelAck, nil); err != nil {
		lg.Error().Err(err).Msg("Failed to transition to CANCELLED")
		return err
	}
	lg.Info().Str("state", string(OrderCancelled)).Msg("Order state transition")

	// Publish cancel event.
	_ = e.producer.PublishJSON(ctx, bus.Topics.Orders(order.Exchange), intentID, order)

	return nil
}

// ---------------------------------------------------------------------------
// Query helpers
// ---------------------------------------------------------------------------

// GetOrder returns an order by its intent ID.
func (e *Engine) GetOrder(intentID string) (*Order, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	o, ok := e.orders[intentID]
	return o, ok
}

// ActiveOrders returns all orders that are not in a terminal state.
func (e *Engine) ActiveOrders() []*Order {
	e.mu.RLock()
	defer e.mu.RUnlock()

	active := make([]*Order, 0, len(e.orders)/2+1)
	for _, o := range e.orders {
		if !o.IsTerminal() {
			active = append(active, o)
		}
	}
	return active
}
