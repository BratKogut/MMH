package kraken

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nexus-trading/nexus/internal/adapters"
	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// Adapter implements adapters.ExchangeAdapter for Kraken.
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
	startTime time.Time
}

// New creates a new Kraken adapter.
func New(wsURL, restURL, apiKey, apiSecret string, symbols []string) *Adapter {
	return &Adapter{
		wsURL:     wsURL,
		restURL:   restURL,
		apiKey:    apiKey,
		apiSecret: apiSecret,
		symbols:   symbols,
		startTime: time.Now(),
		health: adapters.HealthStatus{
			State: "disconnected",
		},
	}
}

func (a *Adapter) Name() string { return "kraken" }

func (a *Adapter) Connect(ctx context.Context) error {
	log.Info().Str("exchange", "kraken").Str("ws_url", a.wsURL).Msg("Connecting to Kraken WS v2")

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.DialContext(ctx, a.wsURL, nil)
	if err != nil {
		return fmt.Errorf("kraken ws connect: %w", err)
	}

	a.mu.Lock()
	a.conn = conn
	a.connected = true
	a.health.Connected = true
	a.health.State = "healthy"
	a.health.LastHeartbeat = time.Now().Unix()
	a.mu.Unlock()

	log.Info().Str("exchange", "kraken").Msg("Connected to Kraken WS v2")
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
	log.Info().Str("exchange", "kraken").Msg("Disconnected from Kraken")
	return nil
}

func (a *Adapter) StreamTrades(ctx context.Context, symbol string) (<-chan bus.Trade, error) {
	ch := make(chan bus.Trade, 1024)

	// Subscribe to trades
	subMsg := map[string]interface{}{
		"method": "subscribe",
		"params": map[string]interface{}{
			"channel": "trade",
			"symbol":  []string{symbol},
		},
	}

	a.mu.RLock()
	conn := a.conn
	a.mu.RUnlock()

	if conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	if err := conn.WriteJSON(subMsg); err != nil {
		return nil, fmt.Errorf("subscribe trades: %w", err)
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
					log.Error().Err(err).Str("exchange", "kraken").Msg("WS read error")
					return
				}

				trades := a.parseTrades(msg, symbol)
				for _, t := range trades {
					select {
					case ch <- t:
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

	subMsg := map[string]interface{}{
		"method": "subscribe",
		"params": map[string]interface{}{
			"channel": "book",
			"symbol":  []string{symbol},
			"depth":   25,
		},
	}

	a.mu.RLock()
	conn := a.conn
	a.mu.RUnlock()

	if conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	if err := conn.WriteJSON(subMsg); err != nil {
		return nil, fmt.Errorf("subscribe orderbook: %w", err)
	}

	go func() {
		defer close(ch)
		// TODO: Parse orderbook messages from Kraken WS v2
		<-ctx.Done()
	}()

	return ch, nil
}

func (a *Adapter) StreamTicks(ctx context.Context, symbol string) (<-chan bus.Tick, error) {
	ch := make(chan bus.Tick, 1024)

	subMsg := map[string]interface{}{
		"method": "subscribe",
		"params": map[string]interface{}{
			"channel": "ticker",
			"symbol":  []string{symbol},
		},
	}

	a.mu.RLock()
	conn := a.conn
	a.mu.RUnlock()

	if conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	if err := conn.WriteJSON(subMsg); err != nil {
		return nil, fmt.Errorf("subscribe ticks: %w", err)
	}

	go func() {
		defer close(ch)
		// TODO: Parse ticker messages from Kraken WS v2
		<-ctx.Done()
	}()

	return ch, nil
}

func (a *Adapter) SubmitOrder(ctx context.Context, intent bus.OrderIntent) (*adapters.OrderAck, error) {
	// TODO: Implement Kraken REST order submission
	return &adapters.OrderAck{
		OrderID: "dry-run-" + intent.IntentID,
		Status:  "accepted",
	}, nil
}

func (a *Adapter) CancelOrder(ctx context.Context, orderID string) error {
	// TODO: Implement Kraken order cancellation
	return nil
}

func (a *Adapter) GetBalances(ctx context.Context) ([]adapters.Balance, error) {
	// TODO: Implement Kraken balance query
	return []adapters.Balance{}, nil
}

func (a *Adapter) GetOpenOrders(ctx context.Context, symbol string) ([]adapters.Order, error) {
	// TODO: Implement Kraken open orders query
	return []adapters.Order{}, nil
}

func (a *Adapter) Health() adapters.HealthStatus {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.health
}

func (a *Adapter) parseTrades(msg []byte, symbol string) []bus.Trade {
	// Parse Kraken WS v2 trade message format
	var raw map[string]interface{}
	if err := json.Unmarshal(msg, &raw); err != nil {
		return nil
	}

	channel, _ := raw["channel"].(string)
	if channel != "trade" {
		return nil
	}

	data, ok := raw["data"].([]interface{})
	if !ok {
		return nil
	}

	var trades []bus.Trade
	for _, item := range data {
		tradeMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		price, _ := decimal.NewFromString(fmt.Sprintf("%v", tradeMap["price"]))
		qty, _ := decimal.NewFromString(fmt.Sprintf("%v", tradeMap["qty"]))
		side := fmt.Sprintf("%v", tradeMap["side"])
		tradeID := fmt.Sprintf("%v", tradeMap["ord_type"])

		trades = append(trades, bus.Trade{
			BaseEvent: bus.NewBaseEvent("kraken-adapter", "1.0.0"),
			Exchange:  "kraken",
			Symbol:    symbol,
			Price:     price,
			Qty:       qty,
			Side:      side,
			TradeID:   tradeID,
		})
	}

	return trades
}
