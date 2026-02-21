package execution

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nexus-trading/nexus/internal/adapters"
	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// PaperBroker simulates exchange order execution for paper trading.
// It implements the order submission and cancellation portion of the
// ExchangeAdapter interface. Market orders fill immediately with
// configurable slippage; limit orders are placed into a pending map
// for manual or price-triggered fills.
//
// Thread-safe: all shared state is guarded by mu.
type PaperBroker struct {
	mu            sync.Mutex
	orders        map[string]*Order // intentID -> Order
	pendingOrders map[string]*Order // intentID -> Order (limit orders waiting for fill)
	nextOrderID   atomic.Int64
	fills         []bus.Fill
	slippageBps   float64       // simulated slippage in basis points
	fillDelay     time.Duration // simulated latency before fill (for market orders)

	// seenIntents tracks submitted intent IDs for idempotency.
	seenIntents map[string]struct{}
}

// Compile-time interface subset check: PaperBroker satisfies the order
// submission and cancellation methods. It does not implement streaming
// methods since paper trading does not require real market data feeds.
var _ interface {
	SubmitOrder(ctx context.Context, intent bus.OrderIntent) (*adapters.OrderAck, error)
	CancelOrder(ctx context.Context, orderID string) error
} = (*PaperBroker)(nil)

// NewPaperBroker creates a PaperBroker with the given slippage and fill delay.
//
// slippageBps: basis points of slippage applied to market fills.
//
//	e.g., 5.0 means 0.05% slippage.
//
// fillDelay: artificial latency before market orders are filled.
//
//	Set to 0 for instant fills in tests.
func NewPaperBroker(slippageBps float64, fillDelay time.Duration) *PaperBroker {
	pb := &PaperBroker{
		orders:        make(map[string]*Order),
		pendingOrders: make(map[string]*Order),
		fills:         make([]bus.Fill, 0),
		slippageBps:   slippageBps,
		fillDelay:     fillDelay,
		seenIntents:   make(map[string]struct{}),
	}
	pb.nextOrderID.Store(1)
	log.Info().
		Float64("slippage_bps", slippageBps).
		Dur("fill_delay", fillDelay).
		Msg("paper broker initialized")
	return pb
}

// SubmitOrder accepts an OrderIntent and simulates exchange behaviour.
//
// Market orders: filled immediately (after optional delay) at the intent's
// limit price +/- slippage. If no limit price is set, uses a default
// reference price (the caller is expected to set ExpectedPrice on the
// intent or LimitPrice as a reference).
//
// Limit orders: placed into the pending map; must be filled via
// SimulateFill or a price-crossing check.
//
// Duplicate intent IDs are rejected (idempotency guard).
func (pb *PaperBroker) SubmitOrder(ctx context.Context, intent bus.OrderIntent) (*adapters.OrderAck, error) {
	pb.mu.Lock()
	defer pb.mu.Unlock()

	// Idempotency: reject duplicate intent IDs.
	if _, seen := pb.seenIntents[intent.IntentID]; seen {
		log.Warn().
			Str("intent_id", intent.IntentID).
			Msg("paper broker: duplicate intent rejected")
		return &adapters.OrderAck{
			OrderID:   "",
			Status:    "rejected",
			Reason:    "duplicate_intent_id",
			Timestamp: time.Now().UnixMicro(),
		}, fmt.Errorf("duplicate intent_id: %s", intent.IntentID)
	}
	pb.seenIntents[intent.IntentID] = struct{}{}

	// Create the order and advance to SUBMITTED.
	order := NewOrder(intent)
	if err := order.Transition(EventSubmit, nil); err != nil {
		return nil, fmt.Errorf("paper broker submit transition: %w", err)
	}

	// Assign a synthetic exchange order ID.
	exchangeOrderID := fmt.Sprintf("PAPER-%d", pb.nextOrderID.Add(1)-1)

	if err := order.Transition(EventAck, &AckData{OrderID: exchangeOrderID}); err != nil {
		return nil, fmt.Errorf("paper broker ack transition: %w", err)
	}

	pb.orders[intent.IntentID] = order

	ack := &adapters.OrderAck{
		OrderID:   exchangeOrderID,
		Status:    "accepted",
		Timestamp: time.Now().UnixMicro(),
	}

	log.Info().
		Str("intent_id", intent.IntentID).
		Str("order_id", exchangeOrderID).
		Str("type", intent.OrderType).
		Str("side", intent.Side).
		Str("symbol", intent.Symbol).
		Str("qty", intent.Qty.String()).
		Msg("paper broker: order accepted")

	switch intent.OrderType {
	case "market":
		// Market orders fill immediately (synchronously for simplicity;
		// fillDelay is applied as a sleep if non-zero).
		if pb.fillDelay > 0 {
			// Release lock during sleep so other goroutines aren't blocked.
			pb.mu.Unlock()
			select {
			case <-time.After(pb.fillDelay):
			case <-ctx.Done():
				pb.mu.Lock()
				return ack, ctx.Err()
			}
			pb.mu.Lock()
		}
		pb.executeMarketFill(order, intent)

	case "limit":
		// Limit orders wait in the pending map.
		pb.pendingOrders[intent.IntentID] = order
		log.Info().
			Str("intent_id", intent.IntentID).
			Str("limit_price", intent.LimitPrice.String()).
			Msg("paper broker: limit order pending")

	default:
		log.Warn().
			Str("order_type", intent.OrderType).
			Msg("paper broker: unknown order type, treating as limit")
		pb.pendingOrders[intent.IntentID] = order
	}

	return ack, nil
}

// executeMarketFill fills a market order at the reference price +/- slippage.
// Must be called while holding pb.mu.
func (pb *PaperBroker) executeMarketFill(order *Order, intent bus.OrderIntent) {
	refPrice := intent.LimitPrice
	if refPrice.IsZero() {
		// Fallback: use a sentinel. In production the caller always sets
		// a reference price; this guards against zero-price fills.
		log.Warn().Str("intent_id", intent.IntentID).
			Msg("paper broker: no reference price, using 1.0")
		refPrice = decimal.NewFromInt(1)
	}

	fillPrice := pb.applySlippage(refPrice, intent.Side)
	fillID := uuid.New().String()[:16]

	fr := &FillRecord{
		FillID:      fillID,
		Qty:         intent.Qty,
		Price:       fillPrice,
		Fee:         decimal.Zero, // paper trading: no fees
		FeeCcy:      "USD",
		Timestamp:   time.Now(),
		SlippageBps: pb.slippageBps,
	}

	if err := order.Transition(EventFill, fr); err != nil {
		log.Error().Err(err).
			Str("intent_id", intent.IntentID).
			Msg("paper broker: market fill transition failed")
		return
	}

	// Record the fill event for consumers.
	fill := bus.Fill{
		BaseEvent:     bus.NewBaseEvent("paper-broker", "1.0"),
		Exchange:      intent.Exchange,
		Symbol:        intent.Symbol,
		OrderID:       order.OrderID,
		IntentID:      intent.IntentID,
		StrategyID:    intent.StrategyID,
		Side:          intent.Side,
		Qty:           intent.Qty,
		Price:         fillPrice,
		Fee:           decimal.Zero,
		FeeCurrency:   "USD",
		ExpectedPrice: refPrice,
		SlippageBps:   pb.slippageBps,
	}
	pb.fills = append(pb.fills, fill)

	log.Info().
		Str("intent_id", intent.IntentID).
		Str("fill_id", fillID).
		Str("fill_price", fillPrice.String()).
		Str("qty", intent.Qty.String()).
		Float64("slippage_bps", pb.slippageBps).
		Msg("paper broker: market order filled")
}

// applySlippage adjusts a price by the configured slippage.
// Buys pay more (price goes up); sells receive less (price goes down).
func (pb *PaperBroker) applySlippage(price decimal.Decimal, side string) decimal.Decimal {
	if pb.slippageBps == 0 {
		return price
	}

	slippageFactor := decimal.NewFromFloat(pb.slippageBps).Div(decimal.NewFromInt(10000))

	switch side {
	case "buy":
		// Buyer pays more.
		return price.Mul(decimal.NewFromInt(1).Add(slippageFactor))
	case "sell":
		// Seller receives less.
		return price.Mul(decimal.NewFromInt(1).Sub(slippageFactor))
	default:
		return price
	}
}

// SimulateFill manually triggers a fill for a pending limit order.
// Useful for testing and for price-crossing logic.
func (pb *PaperBroker) SimulateFill(intentID string, price decimal.Decimal) error {
	pb.mu.Lock()
	defer pb.mu.Unlock()

	order, ok := pb.pendingOrders[intentID]
	if !ok {
		return fmt.Errorf("no pending order for intent_id=%s", intentID)
	}

	fillID := uuid.New().String()[:16]
	remainingQty := order.Qty.Sub(order.FilledQty)

	fr := &FillRecord{
		FillID:    fillID,
		Qty:       remainingQty,
		Price:     price,
		Fee:       decimal.Zero,
		FeeCcy:    "USD",
		Timestamp: time.Now(),
	}

	if err := order.Transition(EventFill, fr); err != nil {
		return fmt.Errorf("simulate fill transition: %w", err)
	}

	// Remove from pending.
	delete(pb.pendingOrders, intentID)

	// Record the fill event.
	fill := bus.Fill{
		BaseEvent:     bus.NewBaseEvent("paper-broker", "1.0"),
		Exchange:      order.Exchange,
		Symbol:        order.Symbol,
		OrderID:       order.OrderID,
		IntentID:      order.IntentID,
		StrategyID:    order.StrategyID,
		Side:          order.Side,
		Qty:           remainingQty,
		Price:         price,
		Fee:           decimal.Zero,
		FeeCurrency:   "USD",
		ExpectedPrice: order.LimitPrice,
	}
	pb.fills = append(pb.fills, fill)

	log.Info().
		Str("intent_id", intentID).
		Str("fill_id", fillID).
		Str("price", price.String()).
		Str("qty", remainingQty.String()).
		Msg("paper broker: simulated fill executed")

	return nil
}

// CancelOrder cancels a pending order by exchange order ID.
func (pb *PaperBroker) CancelOrder(_ context.Context, orderID string) error {
	pb.mu.Lock()
	defer pb.mu.Unlock()

	// Find order by exchange order ID.
	for intentID, order := range pb.pendingOrders {
		if order.OrderID == orderID {
			if err := order.Transition(EventRequestCancel, nil); err != nil {
				return fmt.Errorf("cancel request transition: %w", err)
			}
			if err := order.Transition(EventCancelAck, nil); err != nil {
				return fmt.Errorf("cancel ack transition: %w", err)
			}
			delete(pb.pendingOrders, intentID)

			log.Info().
				Str("order_id", orderID).
				Str("intent_id", intentID).
				Msg("paper broker: order cancelled")
			return nil
		}
	}

	return fmt.Errorf("order not found: %s", orderID)
}

// GetPendingOrders returns a snapshot of all pending (unfilled limit) orders.
func (pb *PaperBroker) GetPendingOrders() []*Order {
	pb.mu.Lock()
	defer pb.mu.Unlock()

	result := make([]*Order, 0, len(pb.pendingOrders))
	for _, o := range pb.pendingOrders {
		result = append(result, o)
	}
	return result
}

// GetFills returns a snapshot of all recorded fills.
func (pb *PaperBroker) GetFills() []bus.Fill {
	pb.mu.Lock()
	defer pb.mu.Unlock()

	out := make([]bus.Fill, len(pb.fills))
	copy(out, pb.fills)
	return out
}

// GetOrder returns the order for a given intent ID, or nil if not found.
func (pb *PaperBroker) GetOrder(intentID string) *Order {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	return pb.orders[intentID]
}

// Name returns the adapter name for interface compatibility.
func (pb *PaperBroker) Name() string {
	return "paper"
}
