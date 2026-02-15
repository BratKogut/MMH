package clickhouse

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeTick creates a test tick with the given index for uniqueness.
func makeTick(i int) bus.Tick {
	return bus.Tick{
		BaseEvent: bus.NewBaseEvent("test-writer", "1.0.0"),
		Exchange:  "binance",
		Symbol:    "BTCUSDT",
		Bid:       decimal.NewFromFloat(50000.0 + float64(i)),
		Ask:       decimal.NewFromFloat(50001.0 + float64(i)),
		BidSize:   decimal.NewFromFloat(1.5),
		AskSize:   decimal.NewFromFloat(2.0),
	}
}

// makeTrade creates a test trade with the given index for uniqueness.
func makeTrade(i int) bus.Trade {
	return bus.Trade{
		BaseEvent: bus.NewBaseEvent("test-writer", "1.0.0"),
		Exchange:  "binance",
		Symbol:    "BTCUSDT",
		Price:     decimal.NewFromFloat(50000.5 + float64(i)),
		Qty:       decimal.NewFromFloat(0.1),
		Side:      "buy",
		TradeID:   bus.NewBaseEvent("test", "1.0").EventID,
	}
}

func TestBatchSizeTrigger_Ticks(t *testing.T) {
	const batchSize = 10

	var mu sync.Mutex
	var flushedRows [][]any

	w := NewBatchWriter(nil, "nexus", batchSize, time.Hour) // huge interval so timer won't fire
	w.SetFlushHook(func(_ context.Context, table string, rows [][]any) error {
		mu.Lock()
		flushedRows = append(flushedRows, rows...)
		mu.Unlock()
		assert.Equal(t, "nexus.md_ticks", table)
		return nil
	})

	ctx := context.Background()

	// Write exactly batchSize ticks. The last write should trigger a flush.
	for i := 0; i < batchSize; i++ {
		err := w.WriteTick(ctx, makeTick(i))
		require.NoError(t, err)
	}

	mu.Lock()
	count := len(flushedRows)
	mu.Unlock()

	assert.Equal(t, batchSize, count, "flush should have been triggered at batchSize")
}

func TestBatchSizeTrigger_Trades(t *testing.T) {
	const batchSize = 5

	var mu sync.Mutex
	var flushedRows [][]any

	w := NewBatchWriter(nil, "", batchSize, time.Hour)
	w.SetFlushHook(func(_ context.Context, table string, rows [][]any) error {
		mu.Lock()
		flushedRows = append(flushedRows, rows...)
		mu.Unlock()
		assert.Equal(t, "md_trades", table)
		return nil
	})

	ctx := context.Background()

	for i := 0; i < batchSize; i++ {
		err := w.WriteTrade(ctx, makeTrade(i))
		require.NoError(t, err)
	}

	mu.Lock()
	count := len(flushedRows)
	mu.Unlock()

	assert.Equal(t, batchSize, count, "flush should have been triggered at batchSize")
}

func TestBatchSizeTrigger_Mixed(t *testing.T) {
	const batchSize = 6

	var totalFlushed atomic.Int64

	w := NewBatchWriter(nil, "nexus", batchSize, time.Hour)
	w.SetFlushHook(func(_ context.Context, _ string, rows [][]any) error {
		totalFlushed.Add(int64(len(rows)))
		return nil
	})

	ctx := context.Background()

	// Write 3 ticks + 3 trades = 6 total, should trigger flush.
	for i := 0; i < 3; i++ {
		require.NoError(t, w.WriteTick(ctx, makeTick(i)))
	}
	for i := 0; i < 3; i++ {
		require.NoError(t, w.WriteTrade(ctx, makeTrade(i)))
	}

	assert.Equal(t, int64(6), totalFlushed.Load(), "flush should trigger when combined buffers reach batchSize")
}

func TestFlushIntervalTrigger(t *testing.T) {
	var totalFlushed atomic.Int64

	w := NewBatchWriter(nil, "nexus", 1000, 50*time.Millisecond)
	w.SetFlushHook(func(_ context.Context, _ string, rows [][]any) error {
		totalFlushed.Add(int64(len(rows)))
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())

	// Write fewer rows than batchSize.
	for i := 0; i < 5; i++ {
		require.NoError(t, w.WriteTick(ctx, makeTick(i)))
	}

	// Start the background flush goroutine.
	w.Start(ctx)

	// Wait for the flush interval to fire.
	time.Sleep(200 * time.Millisecond)

	cancel()
	// Close waits for the background goroutine and does a final flush.
	err := w.Close()
	require.NoError(t, err)

	assert.Equal(t, int64(5), totalFlushed.Load(),
		"periodic flush should have written all 5 rows")
}

func TestFlushEmpty(t *testing.T) {
	hookCalled := false

	w := NewBatchWriter(nil, "nexus", 100, time.Hour)
	w.SetFlushHook(func(_ context.Context, _ string, _ [][]any) error {
		hookCalled = true
		return nil
	})

	// Flushing with no data should not call the hook.
	err := w.Flush(context.Background())
	require.NoError(t, err)
	assert.False(t, hookCalled, "flush hook should not be called when buffers are empty")
}

func TestConcurrentWrites(t *testing.T) {
	const (
		numGoroutines = 10
		writesPerGo   = 100
		batchSize     = 50
	)

	var totalFlushed atomic.Int64

	w := NewBatchWriter(nil, "nexus", batchSize, time.Hour) // no timer flush
	w.SetFlushHook(func(_ context.Context, _ string, rows [][]any) error {
		totalFlushed.Add(int64(len(rows)))
		return nil
	})

	ctx := context.Background()

	// Launch concurrent writers.
	var wg sync.WaitGroup
	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gID int) {
			defer wg.Done()
			for i := 0; i < writesPerGo; i++ {
				if gID%2 == 0 {
					_ = w.WriteTick(ctx, makeTick(i))
				} else {
					_ = w.WriteTrade(ctx, makeTrade(i))
				}
			}
		}(g)
	}
	wg.Wait()

	// Flush any remaining buffered rows.
	err := w.Flush(ctx)
	require.NoError(t, err)

	expected := int64(numGoroutines * writesPerGo)
	assert.Equal(t, expected, totalFlushed.Load(),
		"all rows from concurrent writers must be flushed")
}

func TestWriterClosedRejectsWrites(t *testing.T) {
	w := NewBatchWriter(nil, "nexus", 100, time.Hour)
	w.SetFlushHook(func(_ context.Context, _ string, _ [][]any) error { return nil })

	err := w.Close()
	require.NoError(t, err)

	err = w.WriteTick(context.Background(), makeTick(0))
	assert.Error(t, err, "writing to a closed writer should return an error")

	err = w.WriteTrade(context.Background(), makeTrade(0))
	assert.Error(t, err, "writing to a closed writer should return an error")
}

func TestBatchNotFlushedBelowThreshold(t *testing.T) {
	hookCalled := false

	w := NewBatchWriter(nil, "nexus", 100, time.Hour)
	w.SetFlushHook(func(_ context.Context, _ string, _ [][]any) error {
		hookCalled = true
		return nil
	})

	ctx := context.Background()

	// Write fewer rows than batchSize - should NOT trigger auto-flush.
	for i := 0; i < 50; i++ {
		require.NoError(t, w.WriteTick(ctx, makeTick(i)))
	}

	assert.False(t, hookCalled, "auto-flush should not fire below batchSize")

	// Verify the rows are buffered.
	_, _, pending, _ := w.Stats()
	assert.Equal(t, 50, pending, "50 ticks should be buffered")
}

func TestTableNamePrefix(t *testing.T) {
	var capturedTable string

	w := NewBatchWriter(nil, "mydb", 1, time.Hour)
	w.SetFlushHook(func(_ context.Context, table string, _ [][]any) error {
		capturedTable = table
		return nil
	})

	ctx := context.Background()

	// Writing 1 tick should trigger a flush (batchSize=1).
	require.NoError(t, w.WriteTick(ctx, makeTick(0)))

	assert.Equal(t, "mydb.md_ticks", capturedTable,
		"table name should include the database prefix")
}

func TestTableNameNoPrefix(t *testing.T) {
	var capturedTable string

	w := NewBatchWriter(nil, "", 1, time.Hour)
	w.SetFlushHook(func(_ context.Context, table string, _ [][]any) error {
		capturedTable = table
		return nil
	})

	ctx := context.Background()

	require.NoError(t, w.WriteTrade(ctx, makeTrade(0)))

	assert.Equal(t, "md_trades", capturedTable,
		"table name should not have a prefix when table is empty")
}
