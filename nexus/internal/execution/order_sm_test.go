package execution

import (
	"testing"

	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helper to create a standard test intent.
func testIntent() bus.OrderIntent {
	return bus.OrderIntent{
		BaseEvent:   bus.NewBaseEvent("test", "1.0"),
		IntentID:    "intent-001",
		StrategyID:  "strat-alpha",
		Exchange:    "paper",
		Symbol:      "BTC/USDT",
		Side:        "buy",
		OrderType:   "market",
		Qty:         decimal.NewFromFloat(1.5),
		LimitPrice:  decimal.NewFromFloat(50000),
		TimeInForce: "GTC",
	}
}

func TestOrder_HappyPath(t *testing.T) {
	// Full lifecycle: CREATED -> SUBMITTED -> ACKED -> PARTIAL -> FILLED
	o := NewOrder(testIntent())
	assert.Equal(t, OrderCreated, o.State)
	assert.Equal(t, "intent-001", o.IntentID)
	assert.Equal(t, "BTC/USDT", o.Symbol)

	// CREATED -> SUBMITTED
	err := o.Transition(EventSubmit, nil)
	require.NoError(t, err)
	assert.Equal(t, OrderSubmitted, o.State)
	assert.False(t, o.SubmittedAt.IsZero())

	// SUBMITTED -> ACKED
	err = o.Transition(EventAck, &AckData{OrderID: "exch-12345"})
	require.NoError(t, err)
	assert.Equal(t, OrderAcked, o.State)
	assert.Equal(t, "exch-12345", o.OrderID)
	assert.False(t, o.AckedAt.IsZero())

	// ACKED -> PARTIAL (fill 0.5 @ 49500)
	err = o.Transition(EventPartialFill, &FillRecord{
		FillID: "fill-1",
		Qty:    decimal.NewFromFloat(0.5),
		Price:  decimal.NewFromFloat(49500),
		Fee:    decimal.NewFromFloat(2.50),
		FeeCcy: "USDT",
	})
	require.NoError(t, err)
	assert.Equal(t, OrderPartialFilled, o.State)
	assert.True(t, o.FilledQty.Equal(decimal.NewFromFloat(0.5)))
	assert.True(t, o.AvgFillPrice.Equal(decimal.NewFromFloat(49500)))

	// PARTIAL -> FILLED (fill remaining 1.0 @ 50000)
	err = o.Transition(EventFill, &FillRecord{
		FillID: "fill-2",
		Qty:    decimal.NewFromFloat(1.0),
		Price:  decimal.NewFromFloat(50000),
		Fee:    decimal.NewFromFloat(5.00),
		FeeCcy: "USDT",
	})
	require.NoError(t, err)
	assert.Equal(t, OrderFilled, o.State)
	assert.True(t, o.FilledQty.Equal(decimal.NewFromFloat(1.5)))
	assert.True(t, o.IsTerminal())
	assert.False(t, o.CompletedAt.IsZero())
	assert.Equal(t, 2, len(o.Fills))
}

func TestOrder_Rejection(t *testing.T) {
	// CREATED -> SUBMITTED -> REJECTED
	o := NewOrder(testIntent())

	err := o.Transition(EventSubmit, nil)
	require.NoError(t, err)

	err = o.Transition(EventReject, nil)
	require.NoError(t, err)
	assert.Equal(t, OrderRejected, o.State)
	assert.True(t, o.IsTerminal())
	assert.False(t, o.CompletedAt.IsZero())
}

func TestOrder_Cancel(t *testing.T) {
	// ACKED -> CANCEL_PENDING -> CANCELLED
	o := NewOrder(testIntent())
	require.NoError(t, o.Transition(EventSubmit, nil))
	require.NoError(t, o.Transition(EventAck, &AckData{OrderID: "exch-999"}))

	err := o.Transition(EventRequestCancel, nil)
	require.NoError(t, err)
	assert.Equal(t, OrderCancelPending, o.State)

	err = o.Transition(EventCancelAck, nil)
	require.NoError(t, err)
	assert.Equal(t, OrderCancelled, o.State)
	assert.True(t, o.IsTerminal())
}

func TestOrder_CancelFromPartial(t *testing.T) {
	// PARTIAL -> CANCEL_PENDING -> CANCELLED
	o := NewOrder(testIntent())
	require.NoError(t, o.Transition(EventSubmit, nil))
	require.NoError(t, o.Transition(EventAck, &AckData{OrderID: "exch-100"}))
	require.NoError(t, o.Transition(EventPartialFill, &FillRecord{
		FillID: "f1",
		Qty:    decimal.NewFromFloat(0.3),
		Price:  decimal.NewFromFloat(50000),
		Fee:    decimal.NewFromFloat(1.0),
		FeeCcy: "USDT",
	}))
	assert.Equal(t, OrderPartialFilled, o.State)

	require.NoError(t, o.Transition(EventRequestCancel, nil))
	assert.Equal(t, OrderCancelPending, o.State)

	require.NoError(t, o.Transition(EventCancelAck, nil))
	assert.Equal(t, OrderCancelled, o.State)
	assert.True(t, o.IsTerminal())
	// Filled qty should still reflect the partial fill.
	assert.True(t, o.FilledQty.Equal(decimal.NewFromFloat(0.3)))
}

func TestOrder_CancelReject(t *testing.T) {
	// ACKED -> CANCEL_PENDING -> CANCEL_REJECTED (restores to ACKED)
	o := NewOrder(testIntent())
	require.NoError(t, o.Transition(EventSubmit, nil))
	require.NoError(t, o.Transition(EventAck, &AckData{OrderID: "exch-200"}))

	require.NoError(t, o.Transition(EventRequestCancel, nil))
	assert.Equal(t, OrderCancelPending, o.State)

	err := o.Transition(EventCancelReject, nil)
	require.NoError(t, err)
	// Should restore to the state before cancel was requested.
	assert.Equal(t, OrderAcked, o.State)
	assert.False(t, o.IsTerminal())
}

func TestOrder_CancelRejectFromPartial(t *testing.T) {
	// PARTIAL -> CANCEL_PENDING -> CANCEL_REJECTED (restores to PARTIAL)
	o := NewOrder(testIntent())
	require.NoError(t, o.Transition(EventSubmit, nil))
	require.NoError(t, o.Transition(EventAck, &AckData{OrderID: "exch-300"}))
	require.NoError(t, o.Transition(EventPartialFill, &FillRecord{
		FillID: "f1",
		Qty:    decimal.NewFromFloat(0.5),
		Price:  decimal.NewFromFloat(49000),
		Fee:    decimal.NewFromFloat(1.0),
		FeeCcy: "USDT",
	}))

	require.NoError(t, o.Transition(EventRequestCancel, nil))
	assert.Equal(t, OrderCancelPending, o.State)

	err := o.Transition(EventCancelReject, nil)
	require.NoError(t, err)
	assert.Equal(t, OrderPartialFilled, o.State)
}

func TestOrder_CancelRace(t *testing.T) {
	// CANCEL_PENDING -> FILL (exchange filled before cancel was processed)
	o := NewOrder(testIntent())
	require.NoError(t, o.Transition(EventSubmit, nil))
	require.NoError(t, o.Transition(EventAck, &AckData{OrderID: "exch-400"}))
	require.NoError(t, o.Transition(EventRequestCancel, nil))
	assert.Equal(t, OrderCancelPending, o.State)

	err := o.Transition(EventFill, &FillRecord{
		FillID: "fill-race",
		Qty:    decimal.NewFromFloat(1.5),
		Price:  decimal.NewFromFloat(50100),
		Fee:    decimal.NewFromFloat(5.0),
		FeeCcy: "USDT",
	})
	require.NoError(t, err)
	assert.Equal(t, OrderFilled, o.State)
	assert.True(t, o.IsTerminal())
	assert.True(t, o.FilledQty.Equal(decimal.NewFromFloat(1.5)))
}

func TestOrder_InvalidTransition(t *testing.T) {
	o := NewOrder(testIntent())

	// CREATED -> FILL is not allowed
	err := o.Transition(EventFill, &FillRecord{
		FillID: "bad",
		Qty:    decimal.NewFromFloat(1.0),
		Price:  decimal.NewFromFloat(50000),
		Fee:    decimal.Zero,
		FeeCcy: "USDT",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid transition")
	assert.Equal(t, OrderCreated, o.State, "state must not change on invalid transition")

	// CREATED -> ACK is not allowed
	err = o.Transition(EventAck, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid transition")

	// FILLED -> anything should fail
	o2 := NewOrder(testIntent())
	require.NoError(t, o2.Transition(EventSubmit, nil))
	require.NoError(t, o2.Transition(EventAck, &AckData{OrderID: "exch-500"}))
	require.NoError(t, o2.Transition(EventFill, &FillRecord{
		FillID: "f1",
		Qty:    decimal.NewFromFloat(1.5),
		Price:  decimal.NewFromFloat(50000),
		Fee:    decimal.Zero,
		FeeCcy: "USDT",
	}))
	assert.Equal(t, OrderFilled, o2.State)

	err = o2.Transition(EventSubmit, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid transition")
}

func TestOrder_AvgFillPrice(t *testing.T) {
	// 3 partial fills with different prices; verify weighted average.
	o := NewOrder(testIntent())
	require.NoError(t, o.Transition(EventSubmit, nil))
	require.NoError(t, o.Transition(EventAck, &AckData{OrderID: "exch-600"}))

	// Fill 1: 0.3 @ 49000
	require.NoError(t, o.Transition(EventPartialFill, &FillRecord{
		FillID: "f1",
		Qty:    decimal.NewFromFloat(0.3),
		Price:  decimal.NewFromFloat(49000),
		Fee:    decimal.NewFromFloat(1.0),
		FeeCcy: "USDT",
	}))

	// Fill 2: 0.7 @ 50000
	require.NoError(t, o.Transition(EventPartialFill, &FillRecord{
		FillID: "f2",
		Qty:    decimal.NewFromFloat(0.7),
		Price:  decimal.NewFromFloat(50000),
		Fee:    decimal.NewFromFloat(2.0),
		FeeCcy: "USDT",
	}))

	// Fill 3 (final): 0.5 @ 51000
	require.NoError(t, o.Transition(EventFill, &FillRecord{
		FillID: "f3",
		Qty:    decimal.NewFromFloat(0.5),
		Price:  decimal.NewFromFloat(51000),
		Fee:    decimal.NewFromFloat(3.0),
		FeeCcy: "USDT",
	}))

	assert.Equal(t, OrderFilled, o.State)
	assert.True(t, o.FilledQty.Equal(decimal.NewFromFloat(1.5)))

	// Expected average: (0.3*49000 + 0.7*50000 + 0.5*51000) / 1.5
	// = (14700 + 35000 + 25500) / 1.5 = 75200 / 1.5 = 50133.3333...
	expected := decimal.NewFromFloat(75200).Div(decimal.NewFromFloat(1.5))
	assert.True(t, o.AvgFillPrice.Equal(expected),
		"expected avg=%s got=%s", expected.String(), o.AvgFillPrice.String())
	assert.Equal(t, 3, len(o.Fills))
}

func TestOrder_FillExceedsQty(t *testing.T) {
	// Overfill must be rejected.
	o := NewOrder(testIntent())
	require.NoError(t, o.Transition(EventSubmit, nil))
	require.NoError(t, o.Transition(EventAck, &AckData{OrderID: "exch-700"}))

	err := o.Transition(EventFill, &FillRecord{
		FillID: "overflow",
		Qty:    decimal.NewFromFloat(2.0), // order is for 1.5
		Price:  decimal.NewFromFloat(50000),
		Fee:    decimal.Zero,
		FeeCcy: "USDT",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceed order qty")
	// State must not change on fill error.
	assert.Equal(t, OrderAcked, o.State)
}

func TestOrder_FillMissingData(t *testing.T) {
	o := NewOrder(testIntent())
	require.NoError(t, o.Transition(EventSubmit, nil))
	require.NoError(t, o.Transition(EventAck, &AckData{OrderID: "exch-800"}))

	// Passing nil data for a fill event must error.
	err := o.Transition(EventFill, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires *FillRecord")
	assert.Equal(t, OrderAcked, o.State)
}

func TestOrder_Idempotent(t *testing.T) {
	// Submitting the same event twice: second must fail.
	o := NewOrder(testIntent())
	require.NoError(t, o.Transition(EventSubmit, nil))
	assert.Equal(t, OrderSubmitted, o.State)

	// Second SUBMIT from SUBMITTED is not a valid transition.
	err := o.Transition(EventSubmit, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid transition")
	assert.Equal(t, OrderSubmitted, o.State, "state must remain SUBMITTED")
}

func TestOrder_IsTerminal(t *testing.T) {
	tests := []struct {
		name     string
		state    OrderState
		terminal bool
	}{
		{"CREATED", OrderCreated, false},
		{"SUBMITTED", OrderSubmitted, false},
		{"ACKED", OrderAcked, false},
		{"PARTIAL", OrderPartialFilled, false},
		{"CANCEL_PENDING", OrderCancelPending, false},
		{"CANCEL_REJECTED", OrderCancelReject, false},
		{"FILLED", OrderFilled, true},
		{"REJECTED", OrderRejected, true},
		{"CANCELLED", OrderCancelled, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &Order{State: tt.state}
			assert.Equal(t, tt.terminal, o.IsTerminal())
		})
	}
}

func TestOrder_RemainingQty(t *testing.T) {
	o := NewOrder(testIntent())
	assert.True(t, o.RemainingQty().Equal(decimal.NewFromFloat(1.5)))

	require.NoError(t, o.Transition(EventSubmit, nil))
	require.NoError(t, o.Transition(EventAck, &AckData{OrderID: "exch-900"}))
	require.NoError(t, o.Transition(EventPartialFill, &FillRecord{
		FillID: "f1",
		Qty:    decimal.NewFromFloat(0.6),
		Price:  decimal.NewFromFloat(50000),
		Fee:    decimal.Zero,
		FeeCcy: "USDT",
	}))

	assert.True(t, o.RemainingQty().Equal(decimal.NewFromFloat(0.9)))
}

func TestOrder_ZeroQtyFill(t *testing.T) {
	o := NewOrder(testIntent())
	require.NoError(t, o.Transition(EventSubmit, nil))
	require.NoError(t, o.Transition(EventAck, &AckData{OrderID: "exch-1000"}))

	err := o.Transition(EventPartialFill, &FillRecord{
		FillID: "bad-fill",
		Qty:    decimal.Zero,
		Price:  decimal.NewFromFloat(50000),
		Fee:    decimal.Zero,
		FeeCcy: "USDT",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "positive")
	assert.Equal(t, OrderAcked, o.State)
}

func TestNewOrder_Fields(t *testing.T) {
	intent := testIntent()
	o := NewOrder(intent)

	assert.Equal(t, intent.IntentID, o.IntentID)
	assert.Equal(t, intent.StrategyID, o.StrategyID)
	assert.Equal(t, intent.Exchange, o.Exchange)
	assert.Equal(t, intent.Symbol, o.Symbol)
	assert.Equal(t, intent.Side, o.Side)
	assert.Equal(t, intent.OrderType, o.OrderType)
	assert.True(t, o.Qty.Equal(intent.Qty))
	assert.True(t, o.LimitPrice.Equal(intent.LimitPrice))
	assert.True(t, o.FilledQty.IsZero())
	assert.True(t, o.AvgFillPrice.IsZero())
	assert.Equal(t, OrderCreated, o.State)
	assert.Empty(t, o.Fills)
	assert.False(t, o.CreatedAt.IsZero())
}
