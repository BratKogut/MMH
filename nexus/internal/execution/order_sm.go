package execution

import (
	"fmt"
	"sync"
	"time"

	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// OrderState represents the current lifecycle state of an order.
type OrderState string

const (
	OrderCreated       OrderState = "CREATED"
	OrderSubmitted     OrderState = "SUBMITTED"
	OrderAcked         OrderState = "ACKED"
	OrderPartialFilled OrderState = "PARTIAL"
	OrderFilled        OrderState = "FILLED"
	OrderRejected      OrderState = "REJECTED"
	OrderCancelPending OrderState = "CANCEL_PENDING"
	OrderCancelled     OrderState = "CANCELLED"
	OrderCancelReject  OrderState = "CANCEL_REJECTED"
)

// OrderEvent represents an event that triggers a state transition.
type OrderEvent string

const (
	EventSubmit        OrderEvent = "SUBMIT"
	EventAck           OrderEvent = "ACK"
	EventReject        OrderEvent = "REJECT"
	EventPartialFill   OrderEvent = "PARTIAL_FILL"
	EventFill          OrderEvent = "FILL"
	EventRequestCancel OrderEvent = "REQUEST_CANCEL"
	EventCancelAck     OrderEvent = "CANCEL_ACK"
	EventCancelReject  OrderEvent = "CANCEL_REJECT"
)

// FillRecord captures a single fill execution against an order.
type FillRecord struct {
	FillID      string
	Qty         decimal.Decimal
	Price       decimal.Decimal
	Fee         decimal.Decimal
	FeeCcy      string
	Timestamp   time.Time
	SlippageBps float64
}

// Order tracks the full lifecycle of an exchange order through a deterministic
// state machine. All monetary values use shopspring/decimal. The struct is
// safe for concurrent access via its embedded mutex.
type Order struct {
	mu sync.Mutex

	IntentID     string
	OrderID      string // assigned by exchange on ACK
	StrategyID   string
	Exchange     string
	Symbol       string
	Side         string // buy|sell
	OrderType    string // market|limit
	Qty          decimal.Decimal
	FilledQty    decimal.Decimal
	AvgFillPrice decimal.Decimal
	LimitPrice   decimal.Decimal
	State        OrderState
	Fills        []FillRecord
	CreatedAt    time.Time
	UpdatedAt    time.Time
	SubmittedAt  time.Time
	AckedAt      time.Time
	CompletedAt  time.Time

	// stateBeforeCancel remembers whether we were ACKED or PARTIAL
	// so CANCEL_REJECT can restore the correct state.
	stateBeforeCancel OrderState
}

// transition defines an allowed state machine edge.
type transition struct {
	from  OrderState
	event OrderEvent
}

// transitions is the authoritative transition table. Every valid
// (currentState, event) pair maps to exactly one target state.
var transitions = map[transition]OrderState{
	{OrderCreated, EventSubmit}:          OrderSubmitted,
	{OrderSubmitted, EventAck}:           OrderAcked,
	{OrderSubmitted, EventReject}:        OrderRejected,
	{OrderAcked, EventPartialFill}:       OrderPartialFilled,
	{OrderAcked, EventFill}:              OrderFilled,
	{OrderPartialFilled, EventPartialFill}: OrderPartialFilled,
	{OrderPartialFilled, EventFill}:      OrderFilled,
	{OrderAcked, EventRequestCancel}:     OrderCancelPending,
	{OrderPartialFilled, EventRequestCancel}: OrderCancelPending,
	{OrderCancelPending, EventCancelAck}: OrderCancelled,
	{OrderCancelPending, EventCancelReject}: OrderCancelReject,
	// Race condition: fill arrives while cancel is in-flight.
	{OrderCancelPending, EventFill}: OrderFilled,
}

// NewOrder creates an Order in the CREATED state from an OrderIntent.
func NewOrder(intent bus.OrderIntent) *Order {
	now := time.Now()
	return &Order{
		IntentID:     intent.IntentID,
		StrategyID:   intent.StrategyID,
		Exchange:     intent.Exchange,
		Symbol:       intent.Symbol,
		Side:         intent.Side,
		OrderType:    intent.OrderType,
		Qty:          intent.Qty,
		LimitPrice:   intent.LimitPrice,
		FilledQty:    decimal.Zero,
		AvgFillPrice: decimal.Zero,
		State:        OrderCreated,
		Fills:        make([]FillRecord, 0),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

// AckData carries the exchange-assigned order ID on ACK.
type AckData struct {
	OrderID string
}

// Transition advances the order through the state machine.
//
// data is interpreted based on the event type:
//   - EventAck:         *AckData (sets exchange OrderID)
//   - EventPartialFill: *FillRecord
//   - EventFill:        *FillRecord
//   - EventCancelReject: nil (restores pre-cancel state)
//   - others:           nil
//
// Returns an error on invalid transitions or missing data.
func (o *Order) Transition(event OrderEvent, data interface{}) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	prevState := o.State
	key := transition{from: o.State, event: event}

	nextState, ok := transitions[key]
	if !ok {
		return fmt.Errorf("invalid transition: state=%s event=%s", o.State, event)
	}

	now := time.Now()

	// Pre-transition side effects based on event type.
	switch event {
	case EventSubmit:
		o.SubmittedAt = now

	case EventAck:
		if data != nil {
			if ack, ok := data.(*AckData); ok {
				o.OrderID = ack.OrderID
			}
		}
		o.AckedAt = now

	case EventPartialFill, EventFill:
		fr, ok := data.(*FillRecord)
		if !ok || fr == nil {
			return fmt.Errorf("event %s requires *FillRecord data, got %T", event, data)
		}
		if err := o.recordFill(fr); err != nil {
			return fmt.Errorf("recording fill: %w", err)
		}

	case EventRequestCancel:
		// Remember state so CANCEL_REJECT can restore it.
		o.stateBeforeCancel = o.State

	case EventCancelReject:
		// Restore the state we were in before the cancel request.
		if o.stateBeforeCancel != "" {
			nextState = o.stateBeforeCancel
		}
		// If stateBeforeCancel is empty for some reason, fall through
		// to the transition table value (CANCEL_REJECTED).
	}

	o.State = nextState
	o.UpdatedAt = now

	// Mark completion time on terminal states.
	if o.IsTerminalLocked() {
		o.CompletedAt = now
	}

	log.Info().
		Str("intent_id", o.IntentID).
		Str("order_id", o.OrderID).
		Str("symbol", o.Symbol).
		Str("prev_state", string(prevState)).
		Str("event", string(event)).
		Str("new_state", string(o.State)).
		Str("filled_qty", o.FilledQty.String()).
		Str("avg_price", o.AvgFillPrice.String()).
		Msg("order state transition")

	return nil
}

// recordFill adds a fill to the order and recalculates the running
// weighted average fill price. Must be called while holding o.mu.
func (o *Order) recordFill(fr *FillRecord) error {
	if fr.Qty.IsZero() || fr.Qty.IsNegative() {
		return fmt.Errorf("fill qty must be positive, got %s", fr.Qty.String())
	}

	newFilledQty := o.FilledQty.Add(fr.Qty)
	if newFilledQty.GreaterThan(o.Qty) {
		return fmt.Errorf("fill would exceed order qty: filled=%s + fill=%s > order=%s",
			o.FilledQty.String(), fr.Qty.String(), o.Qty.String())
	}

	// Weighted average: avg = (prev_avg * prev_qty + fill_price * fill_qty) / new_total
	totalCost := o.AvgFillPrice.Mul(o.FilledQty).Add(fr.Price.Mul(fr.Qty))
	o.AvgFillPrice = totalCost.Div(newFilledQty)
	o.FilledQty = newFilledQty

	o.Fills = append(o.Fills, *fr)
	return nil
}

// IsTerminal returns true if the order is in a terminal state
// (FILLED, REJECTED, or CANCELLED). Thread-safe.
func (o *Order) IsTerminal() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.IsTerminalLocked()
}

// IsTerminalLocked is the non-locking version for internal use.
// Caller must hold o.mu.
func (o *Order) IsTerminalLocked() bool {
	switch o.State {
	case OrderFilled, OrderRejected, OrderCancelled:
		return true
	default:
		return false
	}
}

// RemainingQty returns the unfilled quantity. Thread-safe.
func (o *Order) RemainingQty() decimal.Decimal {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.Qty.Sub(o.FilledQty)
}

// GetState returns the current state. Thread-safe.
func (o *Order) GetState() OrderState {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.State
}
