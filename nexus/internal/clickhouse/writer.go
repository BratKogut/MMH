package clickhouse

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/rs/zerolog/log"
)

// BatchWriter batches market data rows and flushes to ClickHouse periodically
// or when the batch is full.
type BatchWriter struct {
	client        *Client
	batchSize     int
	flushInterval time.Duration

	mu          sync.Mutex
	tickBuf     []bus.Tick
	tradeBuf    []bus.Trade
	closed      bool
	flushCount  int64
	errorCount  int64
}

// NewBatchWriter creates a batch writer that flushes on size or interval.
func NewBatchWriter(client *Client, batchSize int, flushInterval time.Duration) *BatchWriter {
	if batchSize <= 0 {
		batchSize = 1000
	}
	if flushInterval <= 0 {
		flushInterval = 5 * time.Second
	}

	return &BatchWriter{
		client:        client,
		batchSize:     batchSize,
		flushInterval: flushInterval,
		tickBuf:       make([]bus.Tick, 0, batchSize),
		tradeBuf:      make([]bus.Trade, 0, batchSize),
	}
}

// WriteTick adds a tick to the batch buffer.
func (w *BatchWriter) WriteTick(_ context.Context, tick bus.Tick) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return fmt.Errorf("writer is closed")
	}

	w.tickBuf = append(w.tickBuf, tick)
	return nil
}

// WriteTrade adds a trade to the batch buffer.
func (w *BatchWriter) WriteTrade(_ context.Context, trade bus.Trade) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return fmt.Errorf("writer is closed")
	}

	w.tradeBuf = append(w.tradeBuf, trade)
	return nil
}

// Start begins the background flush loop. Blocks until context is cancelled.
func (w *BatchWriter) Start(ctx context.Context) {
	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()

	log.Info().
		Int("batch_size", w.batchSize).
		Dur("flush_interval", w.flushInterval).
		Msg("ClickHouse batch writer started")

	for {
		select {
		case <-ctx.Done():
			// Final flush on shutdown.
			if err := w.Flush(context.Background()); err != nil {
				log.Error().Err(err).Msg("Final flush error on shutdown")
			}
			return
		case <-ticker.C:
			if err := w.Flush(ctx); err != nil {
				log.Error().Err(err).Msg("Periodic flush error")
			}
		}
	}
}

// Flush writes all buffered data to ClickHouse.
func (w *BatchWriter) Flush(ctx context.Context) error {
	w.mu.Lock()
	ticks := w.tickBuf
	trades := w.tradeBuf
	w.tickBuf = make([]bus.Tick, 0, w.batchSize)
	w.tradeBuf = make([]bus.Trade, 0, w.batchSize)
	w.mu.Unlock()

	if len(ticks) == 0 && len(trades) == 0 {
		return nil
	}

	var firstErr error

	if len(ticks) > 0 {
		if err := w.flushTicks(ctx, ticks); err != nil {
			log.Error().Err(err).Int("count", len(ticks)).Msg("Failed to flush ticks")
			w.errorCount++
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	if len(trades) > 0 {
		if err := w.flushTrades(ctx, trades); err != nil {
			log.Error().Err(err).Int("count", len(trades)).Msg("Failed to flush trades")
			w.errorCount++
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	w.flushCount++
	log.Debug().
		Int("ticks", len(ticks)).
		Int("trades", len(trades)).
		Int64("total_flushes", w.flushCount).
		Msg("ClickHouse batch flushed")

	return firstErr
}

// flushTicks writes tick data to ClickHouse md_ticks table.
func (w *BatchWriter) flushTicks(ctx context.Context, ticks []bus.Tick) error {
	batch, err := w.client.Conn().PrepareBatch(ctx,
		"INSERT INTO md_ticks (exchange, symbol, ts, bid, ask, bid_size, ask_size, event_id)")
	if err != nil {
		return fmt.Errorf("prepare tick batch: %w", err)
	}

	for _, t := range ticks {
		if err := batch.Append(
			t.Exchange,
			t.Symbol,
			t.Timestamp,
			t.Bid.InexactFloat64(),
			t.Ask.InexactFloat64(),
			t.BidSize.InexactFloat64(),
			t.AskSize.InexactFloat64(),
			t.EventID,
		); err != nil {
			return fmt.Errorf("append tick: %w", err)
		}
	}

	return batch.Send()
}

// flushTrades writes trade data to ClickHouse md_trades table.
func (w *BatchWriter) flushTrades(ctx context.Context, trades []bus.Trade) error {
	batch, err := w.client.Conn().PrepareBatch(ctx,
		"INSERT INTO md_trades (exchange, symbol, ts, price, qty, side, trade_id, event_id)")
	if err != nil {
		return fmt.Errorf("prepare trade batch: %w", err)
	}

	for _, t := range trades {
		side := sideToEnum(t.Side)
		if err := batch.Append(
			t.Exchange,
			t.Symbol,
			t.Timestamp,
			t.Price.InexactFloat64(),
			t.Qty.InexactFloat64(),
			side,
			t.TradeID,
			t.EventID,
		); err != nil {
			return fmt.Errorf("append trade: %w", err)
		}
	}

	return batch.Send()
}

// sideToEnum converts side string to ClickHouse Enum8 value.
func sideToEnum(side string) string {
	switch side {
	case "buy":
		return "buy"
	case "sell":
		return "sell"
	default:
		return "unknown"
	}
}

// Close marks the writer as closed.
func (w *BatchWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	log.Info().
		Int64("total_flushes", w.flushCount).
		Int64("errors", w.errorCount).
		Msg("ClickHouse batch writer closed")
	return nil
}

// Stats returns writer statistics.
func (w *BatchWriter) Stats() (flushCount, errorCount int64, pendingTicks, pendingTrades int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushCount, w.errorCount, len(w.tickBuf), len(w.tradeBuf)
}
