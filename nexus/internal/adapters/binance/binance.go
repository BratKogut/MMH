package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nexus-trading/nexus/internal/adapters"
	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// Adapter implements adapters.ExchangeAdapter for Binance.
type Adapter struct {
	wsURL     string
	restURL   string
	apiKey    string
	apiSecret string
	symbols   []string

	conn      *websocket.Conn
	mu        sync.RWMutex
	connected bool
	health    adapters.HealthStatus
}

// New creates a new Binance adapter.
func New(wsURL, restURL, apiKey, apiSecret string, symbols []string) *Adapter {
	return &Adapter{
		wsURL:     wsURL,
		restURL:   restURL,
		apiKey:    apiKey,
		apiSecret: apiSecret,
		symbols:   symbols,
		health: adapters.HealthStatus{
			State: "disconnected",
		},
	}
}

func (a *Adapter) Name() string { return "binance" }

func (a *Adapter) Connect(ctx context.Context) error {
	// Build combined stream URL for all symbols
	streams := make([]string, 0, len(a.symbols)*2)
	for _, s := range a.symbols {
		lower := strings.ToLower(s)
		streams = append(streams, lower+"@trade")
		streams = append(streams, lower+"@bookTicker")
	}

	wsURL := fmt.Sprintf("%s/%s", a.wsURL, strings.Join(streams, "/"))
	log.Info().Str("exchange", "binance").Str("url", wsURL).Msg("Connecting to Binance WS")

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("binance ws connect: %w", err)
	}

	a.mu.Lock()
	a.conn = conn
	a.connected = true
	a.health.Connected = true
	a.health.State = "healthy"
	a.health.LastHeartbeat = time.Now().Unix()
	a.mu.Unlock()

	log.Info().Str("exchange", "binance").Msg("Connected to Binance WS")
	return nil
}

func (a *Adapter) Disconnect() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.conn != nil {
		a.conn.Close()
		a.connected = false
		a.health.Connected = false
		a.health.State = "disconnected"
	}
	return nil
}

func (a *Adapter) StreamTrades(ctx context.Context, symbol string) (<-chan bus.Trade, error) {
	ch := make(chan bus.Trade, 1024)

	a.mu.RLock()
	conn := a.conn
	a.mu.RUnlock()

	if conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	go func() {
		defer close(ch)
		for {
			select {
			case <-ctx.Done():
				return
			default:
				_, msg, err := conn.ReadMessage()
				if err != nil {
					log.Error().Err(err).Str("exchange", "binance").Msg("WS read error")
					return
				}

				trade := a.parseTrade(msg, symbol)
				if trade != nil {
					select {
					case ch <- *trade:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()

	return ch, nil
}

func (a *Adapter) StreamOrderbook(ctx context.Context, symbol string) (<-chan bus.OrderbookUpdate, error) {
	ch := make(chan bus.OrderbookUpdate, 256)
	go func() {
		defer close(ch)
		<-ctx.Done()
	}()
	return ch, nil
}

func (a *Adapter) StreamTicks(ctx context.Context, symbol string) (<-chan bus.Tick, error) {
	ch := make(chan bus.Tick, 1024)
	go func() {
		defer close(ch)
		<-ctx.Done()
	}()
	return ch, nil
}

func (a *Adapter) SubmitOrder(ctx context.Context, intent bus.OrderIntent) (*adapters.OrderAck, error) {
	return &adapters.OrderAck{
		OrderID: "dry-run-" + intent.IntentID,
		Status:  "accepted",
	}, nil
}

func (a *Adapter) CancelOrder(ctx context.Context, orderID string) error { return nil }

func (a *Adapter) GetBalances(ctx context.Context) ([]adapters.Balance, error) {
	return []adapters.Balance{}, nil
}

func (a *Adapter) GetOpenOrders(ctx context.Context, symbol string) ([]adapters.Order, error) {
	return []adapters.Order{}, nil
}

func (a *Adapter) Health() adapters.HealthStatus {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.health
}

func (a *Adapter) parseTrade(msg []byte, symbol string) *bus.Trade {
	var raw map[string]interface{}
	if err := json.Unmarshal(msg, &raw); err != nil {
		return nil
	}

	eventType, _ := raw["e"].(string)
	if eventType != "trade" {
		return nil
	}

	price, _ := decimal.NewFromString(fmt.Sprintf("%v", raw["p"]))
	qty, _ := decimal.NewFromString(fmt.Sprintf("%v", raw["q"]))

	side := "buy"
	if isBuyerMaker, ok := raw["m"].(bool); ok && isBuyerMaker {
		side = "sell"
	}

	return &bus.Trade{
		BaseEvent: bus.NewBaseEvent("binance-adapter", "1.0.0"),
		Exchange:  "binance",
		Symbol:    symbol,
		Price:     price,
		Qty:       qty,
		Side:      side,
		TradeID:   fmt.Sprintf("%v", raw["t"]),
	}
}
