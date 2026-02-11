package adapters

import (
	"context"

	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/shopspring/decimal"
)

// ExchangeAdapter is the unified interface for all exchanges (CEX and DEX).
// A strategy NEVER knows whether it's trading on a CEX or DEX.
type ExchangeAdapter interface {
	// Name returns the exchange identifier (e.g., "kraken", "binance", "jupiter")
	Name() string

	// Connect establishes connection to the exchange.
	Connect(ctx context.Context) error

	// Disconnect closes all connections.
	Disconnect() error

	// StreamTrades returns a channel of real-time trades.
	StreamTrades(ctx context.Context, symbol string) (<-chan bus.Trade, error)

	// StreamOrderbook returns a channel of orderbook updates.
	StreamOrderbook(ctx context.Context, symbol string) (<-chan bus.OrderbookUpdate, error)

	// StreamTicks returns a channel of tick/BBO updates.
	StreamTicks(ctx context.Context, symbol string) (<-chan bus.Tick, error)

	// SubmitOrder submits an order to the exchange.
	SubmitOrder(ctx context.Context, intent bus.OrderIntent) (*OrderAck, error)

	// CancelOrder cancels an existing order.
	CancelOrder(ctx context.Context, orderID string) error

	// GetBalances returns current account balances.
	GetBalances(ctx context.Context) ([]Balance, error)

	// GetOpenOrders returns currently open orders.
	GetOpenOrders(ctx context.Context, symbol string) ([]Order, error)

	// Health returns the adapter health status.
	Health() HealthStatus
}

// OrderAck is the acknowledgement from the exchange after order submission.
type OrderAck struct {
	OrderID   string `json:"order_id"`
	Status    string `json:"status"` // accepted|rejected
	Reason    string `json:"reason,omitempty"`
	Timestamp int64  `json:"timestamp"`
}

// Balance represents an account balance for a single asset.
type Balance struct {
	Asset  string          `json:"asset"`
	Free   decimal.Decimal `json:"free"`
	Locked decimal.Decimal `json:"locked"`
	Total  decimal.Decimal `json:"total"`
}

// Order represents an open order.
type Order struct {
	OrderID   string          `json:"order_id"`
	Symbol    string          `json:"symbol"`
	Side      string          `json:"side"`
	OrderType string          `json:"order_type"`
	Qty       decimal.Decimal `json:"qty"`
	FilledQty decimal.Decimal `json:"filled_qty"`
	Price     decimal.Decimal `json:"price"`
	Status    string          `json:"status"` // open|partial|filled|cancelled
	CreatedAt int64           `json:"created_at"`
}

// HealthStatus represents the health of an exchange connection.
type HealthStatus struct {
	Connected     bool    `json:"connected"`
	LastHeartbeat int64   `json:"last_heartbeat"`
	FeedLagMs     float64 `json:"feed_lag_ms"`
	ErrorRate     float64 `json:"error_rate"` // errors per minute
	State         string  `json:"state"`      // healthy|degraded|disconnected
}
