package execution

import (
	"context"
	"testing"
	"time"

	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func marketIntent(id string, side string, qty float64, refPrice float64) bus.OrderIntent {
	return bus.OrderIntent{
		BaseEvent:   bus.NewBaseEvent("test", "1.0"),
		IntentID:    id,
		StrategyID:  "strat-test",
		Exchange:    "paper",
		Symbol:      "BTC/USDT",
		Side:        side,
		OrderType:   "market",
		Qty:         decimal.NewFromFloat(qty),
		LimitPrice:  decimal.NewFromFloat(refPrice),
		TimeInForce: "GTC",
	}
}

func limitIntent(id string, side string, qty float64, limitPrice float64) bus.OrderIntent {
	return bus.OrderIntent{
		BaseEvent:   bus.NewBaseEvent("test", "1.0"),
		IntentID:    id,
		StrategyID:  "strat-test",
		Exchange:    "paper",
		Symbol:      "ETH/USDT",
		Side:        side,
		OrderType:   "limit",
		Qty:         decimal.NewFromFloat(qty),
		LimitPrice:  decimal.NewFromFloat(limitPrice),
		TimeInForce: "GTC",
	}
}

func TestPaperBroker_MarketOrderFills(t *testing.T) {
	pb := NewPaperBroker(0, 0) // zero slippage, zero delay
	ctx := context.Background()

	intent := marketIntent("mkt-001", "buy", 2.0, 50000)
	ack, err := pb.SubmitOrder(ctx, intent)
	require.NoError(t, err)
	assert.Equal(t, "accepted", ack.Status)
	assert.NotEmpty(t, ack.OrderID)

	// Order should be filled immediately.
	order := pb.GetOrder("mkt-001")
	require.NotNil(t, order)
	assert.Equal(t, OrderFilled, order.GetState())
	assert.True(t, order.FilledQty.Equal(decimal.NewFromFloat(2.0)))

	// Fill should be recorded.
	fills := pb.GetFills()
	require.Len(t, fills, 1)
	assert.Equal(t, "mkt-001", fills[0].IntentID)
	assert.True(t, fills[0].Qty.Equal(decimal.NewFromFloat(2.0)))
	// No slippage -> fill at exact reference price.
	assert.True(t, fills[0].Price.Equal(decimal.NewFromFloat(50000)))

	// No pending orders.
	assert.Empty(t, pb.GetPendingOrders())
}

func TestPaperBroker_Slippage(t *testing.T) {
	// 10 bps slippage = 0.1%
	pb := NewPaperBroker(10, 0)
	ctx := context.Background()

	// Buy: price goes UP by slippage.
	buyIntent := marketIntent("slip-buy", "buy", 1.0, 50000)
	_, err := pb.SubmitOrder(ctx, buyIntent)
	require.NoError(t, err)

	buyFills := pb.GetFills()
	require.Len(t, buyFills, 1)
	// Expected: 50000 * (1 + 10/10000) = 50000 * 1.001 = 50050
	expectedBuyPrice := decimal.NewFromFloat(50000).Mul(
		decimal.NewFromInt(1).Add(decimal.NewFromFloat(10).Div(decimal.NewFromInt(10000))),
	)
	assert.True(t, buyFills[0].Price.Equal(expectedBuyPrice),
		"buy fill price: expected=%s got=%s", expectedBuyPrice.String(), buyFills[0].Price.String())

	// Sell: price goes DOWN by slippage.
	sellIntent := marketIntent("slip-sell", "sell", 1.0, 50000)
	_, err = pb.SubmitOrder(ctx, sellIntent)
	require.NoError(t, err)

	allFills := pb.GetFills()
	require.Len(t, allFills, 2)
	sellFill := allFills[1]
	// Expected: 50000 * (1 - 10/10000) = 50000 * 0.999 = 49950
	expectedSellPrice := decimal.NewFromFloat(50000).Mul(
		decimal.NewFromInt(1).Sub(decimal.NewFromFloat(10).Div(decimal.NewFromInt(10000))),
	)
	assert.True(t, sellFill.Price.Equal(expectedSellPrice),
		"sell fill price: expected=%s got=%s", expectedSellPrice.String(), sellFill.Price.String())
}

func TestPaperBroker_LimitOrder(t *testing.T) {
	pb := NewPaperBroker(0, 0)
	ctx := context.Background()

	intent := limitIntent("lmt-001", "buy", 5.0, 3000)
	ack, err := pb.SubmitOrder(ctx, intent)
	require.NoError(t, err)
	assert.Equal(t, "accepted", ack.Status)

	// Order should be pending, NOT filled.
	order := pb.GetOrder("lmt-001")
	require.NotNil(t, order)
	assert.Equal(t, OrderAcked, order.GetState())
	assert.True(t, order.FilledQty.IsZero())

	// Should appear in pending orders.
	pending := pb.GetPendingOrders()
	require.Len(t, pending, 1)
	assert.Equal(t, "lmt-001", pending[0].IntentID)

	// No fills yet.
	assert.Empty(t, pb.GetFills())

	// Now simulate a fill.
	err = pb.SimulateFill("lmt-001", decimal.NewFromFloat(2990))
	require.NoError(t, err)

	assert.Equal(t, OrderFilled, order.GetState())
	assert.True(t, order.FilledQty.Equal(decimal.NewFromFloat(5.0)))
	assert.Empty(t, pb.GetPendingOrders())

	fills := pb.GetFills()
	require.Len(t, fills, 1)
	assert.True(t, fills[0].Price.Equal(decimal.NewFromFloat(2990)))
}

func TestPaperBroker_MultipleOrders(t *testing.T) {
	pb := NewPaperBroker(5, 0) // 5 bps slippage
	ctx := context.Background()

	// Submit 3 market orders and 2 limit orders.
	intents := []bus.OrderIntent{
		marketIntent("m1", "buy", 1.0, 50000),
		marketIntent("m2", "sell", 0.5, 50000),
		marketIntent("m3", "buy", 2.0, 50000),
		limitIntent("l1", "buy", 1.0, 49000),
		limitIntent("l2", "sell", 3.0, 51000),
	}

	for _, intent := range intents {
		ack, err := pb.SubmitOrder(ctx, intent)
		require.NoError(t, err)
		assert.Equal(t, "accepted", ack.Status)
	}

	// 3 market orders should have filled.
	fills := pb.GetFills()
	assert.Len(t, fills, 3)

	// 2 limit orders should be pending.
	pending := pb.GetPendingOrders()
	assert.Len(t, pending, 2)

	// All 5 orders should be tracked.
	for _, id := range []string{"m1", "m2", "m3", "l1", "l2"} {
		order := pb.GetOrder(id)
		require.NotNil(t, order, "order %s should exist", id)
	}

	// Market orders should be in terminal state.
	for _, id := range []string{"m1", "m2", "m3"} {
		order := pb.GetOrder(id)
		assert.True(t, order.IsTerminal(), "market order %s should be terminal", id)
	}

	// Limit orders should NOT be terminal.
	for _, id := range []string{"l1", "l2"} {
		order := pb.GetOrder(id)
		assert.False(t, order.IsTerminal(), "limit order %s should not be terminal", id)
	}
}

func TestPaperBroker_DuplicateIntentRejected(t *testing.T) {
	pb := NewPaperBroker(0, 0)
	ctx := context.Background()

	intent := marketIntent("dup-001", "buy", 1.0, 50000)

	// First submission succeeds.
	ack, err := pb.SubmitOrder(ctx, intent)
	require.NoError(t, err)
	assert.Equal(t, "accepted", ack.Status)

	// Second submission with same intent_id must be rejected.
	ack2, err := pb.SubmitOrder(ctx, intent)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")
	assert.Equal(t, "rejected", ack2.Status)

	// Only one fill should exist.
	assert.Len(t, pb.GetFills(), 1)
}

func TestPaperBroker_SimulateFillNotFound(t *testing.T) {
	pb := NewPaperBroker(0, 0)

	err := pb.SimulateFill("nonexistent", decimal.NewFromFloat(100))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no pending order")
}

func TestPaperBroker_CancelOrder(t *testing.T) {
	pb := NewPaperBroker(0, 0)
	ctx := context.Background()

	intent := limitIntent("cancel-001", "buy", 1.0, 45000)
	ack, err := pb.SubmitOrder(ctx, intent)
	require.NoError(t, err)

	// Cancel by exchange order ID.
	err = pb.CancelOrder(ctx, ack.OrderID)
	require.NoError(t, err)

	order := pb.GetOrder("cancel-001")
	require.NotNil(t, order)
	assert.Equal(t, OrderCancelled, order.GetState())
	assert.True(t, order.IsTerminal())
	assert.Empty(t, pb.GetPendingOrders())
}

func TestPaperBroker_CancelNotFound(t *testing.T) {
	pb := NewPaperBroker(0, 0)
	ctx := context.Background()

	err := pb.CancelOrder(ctx, "PAPER-nonexistent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestPaperBroker_FillDelay(t *testing.T) {
	// Verify that fill delay actually causes a delay.
	delay := 50 * time.Millisecond
	pb := NewPaperBroker(0, delay)
	ctx := context.Background()

	intent := marketIntent("delay-001", "buy", 1.0, 50000)
	start := time.Now()
	_, err := pb.SubmitOrder(ctx, intent)
	elapsed := time.Since(start)
	require.NoError(t, err)

	assert.GreaterOrEqual(t, elapsed.Milliseconds(), delay.Milliseconds()-5,
		"fill should take at least the configured delay")

	order := pb.GetOrder("delay-001")
	require.NotNil(t, order)
	assert.Equal(t, OrderFilled, order.GetState())
}

func TestPaperBroker_Name(t *testing.T) {
	pb := NewPaperBroker(0, 0)
	assert.Equal(t, "paper", pb.Name())
}

func TestPaperBroker_ZeroSlippage(t *testing.T) {
	pb := NewPaperBroker(0, 0)
	ctx := context.Background()

	intent := marketIntent("zero-slip", "buy", 1.0, 42000)
	_, err := pb.SubmitOrder(ctx, intent)
	require.NoError(t, err)

	fills := pb.GetFills()
	require.Len(t, fills, 1)
	assert.True(t, fills[0].Price.Equal(decimal.NewFromFloat(42000)),
		"zero slippage: fill price should equal reference price")
}
