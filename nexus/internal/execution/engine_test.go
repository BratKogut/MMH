package execution

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/nexus-trading/nexus/internal/adapters"
	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/nexus-trading/nexus/internal/risk"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Mock exchange adapter
// ---------------------------------------------------------------------------

type mockAdapter struct {
	mu        sync.Mutex
	name      string
	submitted []bus.OrderIntent
	cancelled []string
	ackStatus string // "accepted" | "rejected"
	ackReason string
	submitErr error
	cancelErr error
}

func newMockAdapter(name, ackStatus string) *mockAdapter {
	return &mockAdapter{
		name:      name,
		ackStatus: ackStatus,
		submitted: make([]bus.OrderIntent, 0),
		cancelled: make([]string, 0),
	}
}

func (m *mockAdapter) Name() string                    { return m.name }
func (m *mockAdapter) Connect(_ context.Context) error { return nil }
func (m *mockAdapter) Disconnect() error               { return nil }
func (m *mockAdapter) StreamTrades(_ context.Context, _ string) (<-chan bus.Trade, error) {
	return nil, nil
}
func (m *mockAdapter) StreamOrderbook(_ context.Context, _ string) (<-chan bus.OrderbookUpdate, error) {
	return nil, nil
}
func (m *mockAdapter) StreamTicks(_ context.Context, _ string) (<-chan bus.Tick, error) {
	return nil, nil
}
func (m *mockAdapter) SubmitOrder(_ context.Context, intent bus.OrderIntent) (*adapters.OrderAck, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.submitErr != nil {
		return nil, m.submitErr
	}
	m.submitted = append(m.submitted, intent)
	return &adapters.OrderAck{
		OrderID:   "exch-" + intent.IntentID,
		Status:    m.ackStatus,
		Reason:    m.ackReason,
		Timestamp: 1700000000000,
	}, nil
}
func (m *mockAdapter) CancelOrder(_ context.Context, orderID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancelErr != nil {
		return m.cancelErr
	}
	m.cancelled = append(m.cancelled, orderID)
	return nil
}
func (m *mockAdapter) GetBalances(_ context.Context) ([]adapters.Balance, error) {
	return nil, nil
}
func (m *mockAdapter) GetOpenOrders(_ context.Context, _ string) ([]adapters.Order, error) {
	return nil, nil
}
func (m *mockAdapter) Health() adapters.HealthStatus {
	return adapters.HealthStatus{Connected: true, State: "healthy"}
}

func (m *mockAdapter) submittedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.submitted)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// permissiveRiskConfig returns a risk config that allows most orders.
func permissiveRiskConfig() risk.Config {
	return risk.Config{
		MaxDailyLossUSD:           1_000_000,
		MaxTotalExposureUSD:       10_000_000,
		MaxPositionPerSymbol:      1_000,
		MaxNotionalPerOrder:       5_000_000,
		CooldownAfterLosses:       100,
		CooldownMinutes:           0,
		FeedLagThresholdMs:        5000,
		OrderRejectSpikeThreshold: 100,
	}
}

func makeSignalWithIntent(strategyID, exchange, symbol, side string, qty, limitPrice float64) bus.StrategySignal {
	intent := bus.OrderIntent{
		BaseEvent:   bus.NewBaseEvent("test", "1.0"),
		IntentID:    "intent-" + symbol + "-001",
		StrategyID:  strategyID,
		Exchange:    exchange,
		Symbol:      symbol,
		Side:        side,
		OrderType:   "limit",
		Qty:         decimal.NewFromFloat(qty),
		LimitPrice:  decimal.NewFromFloat(limitPrice),
		TimeInForce: "GTC",
	}
	signal := bus.StrategySignal{
		BaseEvent:    bus.NewBaseEvent("test", "1.0"),
		StrategyID:   strategyID,
		Symbol:       symbol,
		SignalType:   "order_intent",
		Confidence:   0.85,
		OrdersIntent: []bus.OrderIntent{intent},
	}
	return signal
}

// ---------------------------------------------------------------------------
// TestEngine_ExecuteHappyPath
// Signal -> risk check ALLOW -> submit to adapter -> ACKED.
// ---------------------------------------------------------------------------
func TestEngine_ExecuteHappyPath(t *testing.T) {
	riskEng := risk.New(permissiveRiskConfig())
	producer := bus.NewStubProducer()
	posMgr := NewPositionManager()
	engine := NewEngine(riskEng, producer, posMgr, false)

	adapter := newMockAdapter("kraken", "accepted")
	engine.RegisterAdapter("kraken", adapter)

	signal := makeSignalWithIntent("strat-1", "kraken", "BTC/USD", "buy", 0.5, 50000.0)
	intentID := signal.OrdersIntent[0].IntentID

	err := engine.Execute(context.Background(), signal)
	require.NoError(t, err)

	// Adapter received exactly one order.
	assert.Equal(t, 1, adapter.submittedCount(), "adapter should receive 1 order")

	// Order tracked and in ACKED state.
	order, ok := engine.GetOrder(intentID)
	require.True(t, ok, "order should be tracked")
	assert.Equal(t, OrderAcked, order.GetState())
	assert.Equal(t, "exch-"+intentID, order.OrderID)

	// Active orders includes this order.
	active := engine.ActiveOrders()
	assert.Len(t, active, 1)

	// Producer should have published events (risk allow + order).
	assert.True(t, len(producer.Messages) >= 2,
		"expected at least 2 published messages (risk decision + order), got %d", len(producer.Messages))
}

// ---------------------------------------------------------------------------
// TestEngine_RiskDeny
// Risk engine kills -> signal is denied, no adapter call.
// ---------------------------------------------------------------------------
func TestEngine_RiskDeny(t *testing.T) {
	riskEng := risk.New(permissiveRiskConfig())
	riskEng.Kill() // activate kill switch -> all orders denied

	producer := bus.NewStubProducer()
	posMgr := NewPositionManager()
	engine := NewEngine(riskEng, producer, posMgr, false)

	adapter := newMockAdapter("kraken", "accepted")
	engine.RegisterAdapter("kraken", adapter)

	signal := makeSignalWithIntent("strat-1", "kraken", "BTC/USD", "buy", 1.0, 50000.0)

	err := engine.Execute(context.Background(), signal)
	require.Error(t, err, "should return error when risk denies")
	assert.Contains(t, err.Error(), "risk denied")

	// No orders submitted to adapter.
	assert.Equal(t, 0, adapter.submittedCount(), "adapter should receive 0 orders")

	// Risk denial event published.
	var foundDeny bool
	for _, msg := range producer.Messages {
		if msg.Topic == bus.Topics.RiskPretradeChecks() {
			foundDeny = true
			break
		}
	}
	assert.True(t, foundDeny, "risk deny event should be published")
}

// ---------------------------------------------------------------------------
// TestEngine_DryRun
// dryRun=true -> no adapter call, order still reaches ACKED (simulated).
// ---------------------------------------------------------------------------
func TestEngine_DryRun(t *testing.T) {
	riskEng := risk.New(permissiveRiskConfig())
	producer := bus.NewStubProducer()
	posMgr := NewPositionManager()
	engine := NewEngine(riskEng, producer, posMgr, true) // dryRun = true

	adapter := newMockAdapter("kraken", "accepted")
	engine.RegisterAdapter("kraken", adapter)

	signal := makeSignalWithIntent("strat-1", "kraken", "BTC/USD", "buy", 1.0, 50000.0)
	intentID := signal.OrdersIntent[0].IntentID

	err := engine.Execute(context.Background(), signal)
	require.NoError(t, err)

	// Adapter should NOT have been called.
	assert.Equal(t, 0, adapter.submittedCount(), "dry-run: adapter should not be called")

	// Order should exist and be in ACKED state (simulated).
	order, ok := engine.GetOrder(intentID)
	require.True(t, ok)
	assert.Equal(t, OrderAcked, order.GetState(), "dry-run: order should be ACKED (simulated)")
	assert.Contains(t, order.OrderID, "DRYRUN-", "dry-run: order ID should contain DRYRUN prefix")
}

// ---------------------------------------------------------------------------
// TestEngine_DuplicateIntent
// Same intent ID twice -> second call is a no-op.
// ---------------------------------------------------------------------------
func TestEngine_DuplicateIntent(t *testing.T) {
	riskEng := risk.New(permissiveRiskConfig())
	producer := bus.NewStubProducer()
	posMgr := NewPositionManager()
	engine := NewEngine(riskEng, producer, posMgr, false)

	adapter := newMockAdapter("kraken", "accepted")
	engine.RegisterAdapter("kraken", adapter)

	signal := makeSignalWithIntent("strat-1", "kraken", "BTC/USD", "buy", 1.0, 50000.0)

	// First execution.
	err := engine.Execute(context.Background(), signal)
	require.NoError(t, err)
	assert.Equal(t, 1, adapter.submittedCount())

	// Second execution with the same signal (same intent_id).
	err = engine.Execute(context.Background(), signal)
	require.NoError(t, err) // no error -- just skipped

	// Still only 1 submission.
	assert.Equal(t, 1, adapter.submittedCount(),
		"duplicate intent should not trigger second submission")
}

// ---------------------------------------------------------------------------
// TestEngine_HandleFill
// Fill arrives -> order transitions, position updated.
// ---------------------------------------------------------------------------
func TestEngine_HandleFill(t *testing.T) {
	riskEng := risk.New(permissiveRiskConfig())
	producer := bus.NewStubProducer()
	posMgr := NewPositionManager()
	engine := NewEngine(riskEng, producer, posMgr, false)

	adapter := newMockAdapter("kraken", "accepted")
	engine.RegisterAdapter("kraken", adapter)

	signal := makeSignalWithIntent("strat-1", "kraken", "BTC/USD", "buy", 2.0, 50000.0)
	intentID := signal.OrdersIntent[0].IntentID

	err := engine.Execute(context.Background(), signal)
	require.NoError(t, err)

	// Verify order is ACKED.
	order, ok := engine.GetOrder(intentID)
	require.True(t, ok)
	assert.Equal(t, OrderAcked, order.GetState())

	// --- Partial fill: 1 of 2 ---
	partialFill := bus.Fill{
		BaseEvent:  bus.NewBaseEvent("test", "1.0"),
		Exchange:   "kraken",
		Symbol:     "BTC/USD",
		OrderID:    order.OrderID,
		IntentID:   intentID,
		StrategyID: "strat-1",
		Side:       "buy",
		Qty:        decimal.NewFromFloat(1.0),
		Price:      decimal.NewFromFloat(50100.0),
		Fee:        decimal.NewFromFloat(10.0),
	}

	err = engine.HandleFill(context.Background(), partialFill)
	require.NoError(t, err)

	order, _ = engine.GetOrder(intentID)
	assert.Equal(t, OrderPartialFilled, order.GetState())
	assert.True(t, order.FilledQty.Equal(decimal.NewFromFloat(1.0)),
		"filled qty should be 1, got %s", order.FilledQty)

	// Position should reflect the partial fill.
	pos := posMgr.GetPosition("strat-1", "kraken", "BTC/USD")
	require.NotNil(t, pos)
	assert.Equal(t, "long", pos.Side)
	assert.True(t, pos.Qty.Equal(decimal.NewFromFloat(1.0)))

	// --- Final fill: remaining 1 of 2 ---
	finalFill := bus.Fill{
		BaseEvent:  bus.NewBaseEvent("test", "1.0"),
		Exchange:   "kraken",
		Symbol:     "BTC/USD",
		OrderID:    order.OrderID,
		IntentID:   intentID,
		StrategyID: "strat-1",
		Side:       "buy",
		Qty:        decimal.NewFromFloat(1.0),
		Price:      decimal.NewFromFloat(50200.0),
		Fee:        decimal.NewFromFloat(10.0),
	}

	err = engine.HandleFill(context.Background(), finalFill)
	require.NoError(t, err)

	order, _ = engine.GetOrder(intentID)
	assert.Equal(t, OrderFilled, order.GetState())
	assert.True(t, order.FilledQty.Equal(decimal.NewFromFloat(2.0)),
		"filled qty should be 2, got %s", order.FilledQty)

	// Position fully reflects both fills.
	pos = posMgr.GetPosition("strat-1", "kraken", "BTC/USD")
	require.NotNil(t, pos)
	assert.True(t, pos.Qty.Equal(decimal.NewFromFloat(2.0)))
	// avg entry = (50100*1 + 50200*1) / 2 = 50150
	assert.True(t, pos.AvgEntry.Equal(decimal.NewFromFloat(50150.0)),
		"avg entry should be 50150, got %s", pos.AvgEntry)

	// Filled order should NOT be in ActiveOrders.
	active := engine.ActiveOrders()
	assert.Len(t, active, 0, "fully filled order should not be active")

	// Fill events should have been published.
	var fillCount int
	for _, msg := range producer.Messages {
		if msg.Topic == bus.Topics.Fills("kraken") {
			fillCount++
		}
	}
	assert.Equal(t, 2, fillCount, "should publish 2 fill events")
}

// ---------------------------------------------------------------------------
// TestEngine_CancelOrder
// Order in ACKED state -> cancel -> CANCELLED.
// ---------------------------------------------------------------------------
func TestEngine_CancelOrder(t *testing.T) {
	riskEng := risk.New(permissiveRiskConfig())
	producer := bus.NewStubProducer()
	posMgr := NewPositionManager()
	engine := NewEngine(riskEng, producer, posMgr, false)

	adapter := newMockAdapter("kraken", "accepted")
	engine.RegisterAdapter("kraken", adapter)

	signal := makeSignalWithIntent("strat-1", "kraken", "BTC/USD", "buy", 1.0, 50000.0)
	intentID := signal.OrdersIntent[0].IntentID

	err := engine.Execute(context.Background(), signal)
	require.NoError(t, err)

	err = engine.CancelOrder(context.Background(), intentID)
	require.NoError(t, err)

	order, ok := engine.GetOrder(intentID)
	require.True(t, ok)
	assert.Equal(t, OrderCancelled, order.GetState())
	assert.True(t, order.IsTerminal())

	// Adapter should have received the cancel.
	adapter.mu.Lock()
	assert.Len(t, adapter.cancelled, 1)
	adapter.mu.Unlock()
}

// ---------------------------------------------------------------------------
// TestEngine_ConcurrentExecute
// Multiple goroutines calling Execute with distinct intents.
// ---------------------------------------------------------------------------
func TestEngine_ConcurrentExecute(t *testing.T) {
	riskEng := risk.New(permissiveRiskConfig())
	producer := bus.NewStubProducer()
	posMgr := NewPositionManager()
	engine := NewEngine(riskEng, producer, posMgr, false)

	adapter := newMockAdapter("kraken", "accepted")
	engine.RegisterAdapter("kraken", adapter)

	const numGoroutines = 50
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			intent := bus.OrderIntent{
				BaseEvent:   bus.NewBaseEvent("test", "1.0"),
				IntentID:    fmt.Sprintf("concurrent-intent-%d", idx),
				StrategyID:  "strat-1",
				Exchange:    "kraken",
				Symbol:      "BTC/USD",
				Side:        "buy",
				OrderType:   "limit",
				Qty:         decimal.NewFromFloat(0.01),
				LimitPrice:  decimal.NewFromFloat(50000.0),
				TimeInForce: "GTC",
			}
			signal := bus.StrategySignal{
				BaseEvent:    bus.NewBaseEvent("test", "1.0"),
				StrategyID:   "strat-1",
				Symbol:       "BTC/USD",
				SignalType:   "order_intent",
				OrdersIntent: []bus.OrderIntent{intent},
			}
			_ = engine.Execute(context.Background(), signal)
		}(i)
	}

	wg.Wait()

	// All intents should be tracked.
	assert.Equal(t, numGoroutines, adapter.submittedCount(),
		"all %d intents should be submitted", numGoroutines)
}

// ---------------------------------------------------------------------------
// TestEngine_ExchangeReject
// Exchange rejects the order -> order state is REJECTED.
// ---------------------------------------------------------------------------
func TestEngine_ExchangeReject(t *testing.T) {
	riskEng := risk.New(permissiveRiskConfig())
	producer := bus.NewStubProducer()
	posMgr := NewPositionManager()
	engine := NewEngine(riskEng, producer, posMgr, false)

	adapter := newMockAdapter("kraken", "rejected")
	adapter.ackReason = "insufficient_funds"
	engine.RegisterAdapter("kraken", adapter)

	signal := makeSignalWithIntent("strat-1", "kraken", "BTC/USD", "buy", 1.0, 50000.0)
	intentID := signal.OrdersIntent[0].IntentID

	err := engine.Execute(context.Background(), signal)
	require.NoError(t, err) // no Go error -- the rejection is in the order state

	order, ok := engine.GetOrder(intentID)
	require.True(t, ok)
	assert.Equal(t, OrderRejected, order.GetState())
	assert.True(t, order.IsTerminal())

	// Rejected orders are not active.
	active := engine.ActiveOrders()
	assert.Len(t, active, 0)
}
