package clickhouse

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/rs/zerolog/log"
)

// BatchWriter batches market data rows and flushes to ClickHouse periodically
// or when the batch is full. It maintains separate internal buffers for ticks
// and trades, flushing each to the appropriate ClickHouse table.
//
// The writer is safe for concurrent use from multiple goroutines.
type BatchWriter struct {
	client        *Client
	table         string // database/table namespace prefix (e.g. "nexus")
	batchSize     int
	flushInterval time.Duration

	mu       sync.Mutex
	tickBuf  []bus.Tick
	tradeBuf []bus.Trade
	closed   bool

	flushCount atomic.Int64
	errorCount atomic.Int64

	cancel context.CancelFunc
	done   chan struct{}

	// flushHook, if set, replaces real ClickHouse writes during flush.
	// Used for testing the batch logic without infrastructure.
	// The string parameter is the table name, the interface slice is the rows.
	flushHook func(ctx context.Context, table string, rows [][]any) error
}

// NewBatchWriter creates a batch writer that flushes on size or interval.
// The table parameter specifies the database namespace (e.g. "nexus"). If empty,
// tables are referenced without a database prefix. Default batch size is 1000
// rows and default flush interval is 5 seconds.
func NewBatchWriter(client *Client, table string, batchSize int, flushInterval time.Duration) *BatchWriter {
	if batchSize <= 0 {
		batchSize = 1000
	}
	if flushInterval <= 0 {
		flushInterval = 5 * time.Second
	}

	return &BatchWriter{
		client:        client,
		table:         table,
		batchSize:     batchSize,
		flushInterval: flushInterval,
		tickBuf:       make([]bus.Tick, 0, batchSize),
		tradeBuf:      make([]bus.Trade, 0, batchSize),
	}
}

// tableName returns the fully qualified table name with optional database prefix.
func (w *BatchWriter) tableName(name string) string {
	if w.table == "" {
		return name
	}
	return w.table + "." + name
}

// WriteTick adds a tick to the batch buffer. If the buffer reaches batchSize,
// an automatic flush is triggered.
func (w *BatchWriter) WriteTick(ctx context.Context, tick bus.Tick) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return fmt.Errorf("writer is closed")
	}
	w.tickBuf = append(w.tickBuf, tick)
	needsFlush := len(w.tickBuf)+len(w.tradeBuf) >= w.batchSize
	w.mu.Unlock()

	if needsFlush {
		return w.Flush(ctx)
	}
	return nil
}

// WriteTrade adds a trade to the batch buffer. If the buffer reaches batchSize,
// an automatic flush is triggered.
func (w *BatchWriter) WriteTrade(ctx context.Context, trade bus.Trade) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return fmt.Errorf("writer is closed")
	}
	w.tradeBuf = append(w.tradeBuf, trade)
	needsFlush := len(w.tickBuf)+len(w.tradeBuf) >= w.batchSize
	w.mu.Unlock()

	if needsFlush {
		return w.Flush(ctx)
	}
	return nil
}

// Start begins the background flush goroutine. The goroutine runs until ctx
// is cancelled, at which point it performs a final flush and exits.
func (w *BatchWriter) Start(ctx context.Context) {
	bgCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	w.done = make(chan struct{})

	go func() {
		defer close(w.done)
		ticker := time.NewTicker(w.flushInterval)
		defer ticker.Stop()

		log.Info().
			Str("table", w.table).
			Int("batch_size", w.batchSize).
			Dur("flush_interval", w.flushInterval).
			Msg("clickhouse batch writer started")

		for {
			select {
			case <-bgCtx.Done():
				// Final flush on shutdown using a fresh context.
				if err := w.Flush(context.Background()); err != nil {
					log.Error().Err(err).Msg("final flush error on shutdown")
				}
				return
			case <-ticker.C:
				if err := w.Flush(bgCtx); err != nil {
					log.Error().Err(err).Msg("periodic flush error")
				}
			}
		}
	}()
}

// Flush writes all buffered data to ClickHouse. It atomically drains both
// tick and trade buffers, then writes each to the respective table. If a
// flushHook is set (for testing), it is called instead of real writes.
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
			log.Error().Err(err).Int("count", len(ticks)).Msg("failed to flush ticks")
			w.errorCount.Add(1)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	if len(trades) > 0 {
		if err := w.flushTrades(ctx, trades); err != nil {
			log.Error().Err(err).Int("count", len(trades)).Msg("failed to flush trades")
			w.errorCount.Add(1)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	w.flushCount.Add(1)
	log.Debug().
		Int("ticks", len(ticks)).
		Int("trades", len(trades)).
		Int64("total_flushes", w.flushCount.Load()).
		Msg("clickhouse batch flushed")

	return firstErr
}

// flushTicks writes tick data to the ClickHouse md_ticks table.
func (w *BatchWriter) flushTicks(ctx context.Context, ticks []bus.Tick) error {
	// If a flush hook is set, convert ticks to generic rows and call it.
	if w.flushHook != nil {
		rows := make([][]any, len(ticks))
		for i, t := range ticks {
			rows[i] = []any{
				t.Exchange, t.Symbol, t.Timestamp,
				t.Bid.InexactFloat64(), t.Ask.InexactFloat64(),
				t.BidSize.InexactFloat64(), t.AskSize.InexactFloat64(),
				t.EventID,
			}
		}
		return w.flushHook(ctx, w.tableName("md_ticks"), rows)
	}

	query := fmt.Sprintf("INSERT INTO %s (exchange, symbol, ts, bid, ask, bid_size, ask_size, event_id)",
		w.tableName("md_ticks"))

	batch, err := w.client.Conn().PrepareBatch(ctx, query)
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

// flushTrades writes trade data to the ClickHouse md_trades table.
func (w *BatchWriter) flushTrades(ctx context.Context, trades []bus.Trade) error {
	// If a flush hook is set, convert trades to generic rows and call it.
	if w.flushHook != nil {
		rows := make([][]any, len(trades))
		for i, t := range trades {
			rows[i] = []any{
				t.Exchange, t.Symbol, t.Timestamp,
				t.Price.InexactFloat64(), t.Qty.InexactFloat64(),
				sideToEnum(t.Side), t.TradeID, t.EventID,
			}
		}
		return w.flushHook(ctx, w.tableName("md_trades"), rows)
	}

	query := fmt.Sprintf("INSERT INTO %s (exchange, symbol, ts, price, qty, side, trade_id, event_id)",
		w.tableName("md_trades"))

	batch, err := w.client.Conn().PrepareBatch(ctx, query)
	if err != nil {
		return fmt.Errorf("prepare trade batch: %w", err)
	}

	for _, t := range trades {
		if err := batch.Append(
			t.Exchange,
			t.Symbol,
			t.Timestamp,
			t.Price.InexactFloat64(),
			t.Qty.InexactFloat64(),
			sideToEnum(t.Side),
			t.TradeID,
			t.EventID,
		); err != nil {
			return fmt.Errorf("append trade: %w", err)
		}
	}

	return batch.Send()
}

// sideToEnum normalizes the side string for the ClickHouse Enum8 column.
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

// Close stops the background flush goroutine (if running), performs a final
// flush, and marks the writer as closed.
func (w *BatchWriter) Close() error {
	// Stop the background goroutine if Start was called.
	if w.cancel != nil {
		w.cancel()
		<-w.done
	}

	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()

	// Final flush to capture anything written after the last periodic flush.
	if err := w.Flush(context.Background()); err != nil {
		log.Error().Err(err).Msg("final flush on close failed")
		return err
	}

	log.Info().
		Int64("total_flushes", w.flushCount.Load()).
		Int64("errors", w.errorCount.Load()).
		Msg("clickhouse batch writer closed")

	return nil
}

// Stats returns writer statistics: total flushes, error count, and pending
// row counts for each buffer.
func (w *BatchWriter) Stats() (flushCount, errorCount int64, pendingTicks, pendingTrades int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushCount.Load(), w.errorCount.Load(), len(w.tickBuf), len(w.tradeBuf)
}

// SetFlushHook sets a hook function that replaces real ClickHouse writes.
// This is intended for testing only. The hook receives the table name and
// the rows that would be written.
func (w *BatchWriter) SetFlushHook(hook func(ctx context.Context, table string, rows [][]any) error) {
	w.flushHook = hook
}
