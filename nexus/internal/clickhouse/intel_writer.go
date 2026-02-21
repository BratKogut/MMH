package clickhouse

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// IntelEventRow represents a single intel event row for ClickHouse insertion.
// Maps to the intel_events table from NEXUS_PLAN section 4.
type IntelEventRow struct {
	EventID          string    `json:"event_id"`
	Timestamp        time.Time `json:"ts"`
	Topic            string    `json:"topic"`
	Priority         string    `json:"priority"`
	TTLSeconds       uint32    `json:"ttl_seconds"`
	Confidence       float32   `json:"confidence"`
	ImpactHorizon    string    `json:"impact_horizon"`
	DirectionalBias  string    `json:"directional_bias"`
	AssetTags        []string  `json:"asset_tags"`
	ClaimsJSON       string    `json:"claims_json"`
	SourcesJSON      string    `json:"sources_json"`
	Provider         string    `json:"provider"`
	CostUSD          float64   `json:"cost_usd"`
	LatencyMs        uint32    `json:"latency_ms"`
	DownstreamTrades []string  `json:"downstream_trade_ids"`
}

// CostAttributionRow represents a daily cost rollup row.
// Maps to the cost_attribution_daily table from NEXUS_PLAN section 4.
type CostAttributionRow struct {
	Date            time.Time `json:"date"`
	StrategyID      string    `json:"strategy_id"`
	Exchange        string    `json:"exchange"`
	TradingFeesUSD  float64   `json:"trading_fees_usd"`
	SlippageCostUSD float64   `json:"slippage_cost_usd"`
	InfraCostUSD    float64   `json:"infra_cost_usd"`
	LLMCostUSD      float64   `json:"llm_cost_usd"`
	TotalCostUSD    float64   `json:"total_cost_usd"`
	GrossPnlUSD     float64   `json:"gross_pnl_usd"`
	NetPnlUSD       float64   `json:"net_pnl_usd"`
	TradeCount      uint32    `json:"trade_count"`
	IntelEventsUsed uint32    `json:"intel_events_used"`
}

// IntelWriter batches intel events and cost attribution rows for ClickHouse.
type IntelWriter struct {
	client        *Client
	dbPrefix      string
	batchSize     int
	flushInterval time.Duration

	mu       sync.Mutex
	intelBuf []IntelEventRow
	costBuf  []CostAttributionRow
	closed   bool

	flushCount atomic.Int64
	errorCount atomic.Int64

	cancel context.CancelFunc
	done   chan struct{}

	// flushHook replaces real writes during testing.
	flushHook func(ctx context.Context, table string, rows [][]any) error
}

// NewIntelWriter creates a new intel event batch writer.
func NewIntelWriter(client *Client, dbPrefix string, batchSize int, flushInterval time.Duration) *IntelWriter {
	if batchSize <= 0 {
		batchSize = 500
	}
	if flushInterval <= 0 {
		flushInterval = 10 * time.Second
	}
	return &IntelWriter{
		client:        client,
		dbPrefix:      dbPrefix,
		batchSize:     batchSize,
		flushInterval: flushInterval,
		intelBuf:      make([]IntelEventRow, 0, batchSize),
		costBuf:       make([]CostAttributionRow, 0, 64),
	}
}

func (w *IntelWriter) tableName(name string) string {
	if w.dbPrefix == "" {
		return name
	}
	return w.dbPrefix + "." + name
}

// WriteIntelEvent adds an intel event row to the buffer.
func (w *IntelWriter) WriteIntelEvent(ctx context.Context, row IntelEventRow) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return fmt.Errorf("intel writer is closed")
	}
	w.intelBuf = append(w.intelBuf, row)
	needsFlush := len(w.intelBuf) >= w.batchSize
	w.mu.Unlock()

	if needsFlush {
		return w.Flush(ctx)
	}
	return nil
}

// WriteCostAttribution adds a cost attribution row to the buffer.
func (w *IntelWriter) WriteCostAttribution(ctx context.Context, row CostAttributionRow) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return fmt.Errorf("intel writer is closed")
	}
	w.costBuf = append(w.costBuf, row)
	w.mu.Unlock()
	return nil
}

// Start begins the background flush loop.
func (w *IntelWriter) Start(ctx context.Context) {
	bgCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	w.done = make(chan struct{})

	go func() {
		defer close(w.done)
		ticker := time.NewTicker(w.flushInterval)
		defer ticker.Stop()

		log.Info().
			Str("prefix", w.dbPrefix).
			Int("batch_size", w.batchSize).
			Dur("flush_interval", w.flushInterval).
			Msg("intel writer started")

		for {
			select {
			case <-bgCtx.Done():
				if err := w.Flush(context.Background()); err != nil {
					log.Error().Err(err).Msg("intel writer: final flush error")
				}
				return
			case <-ticker.C:
				if err := w.Flush(bgCtx); err != nil {
					log.Error().Err(err).Msg("intel writer: periodic flush error")
				}
			}
		}
	}()
}

// Flush writes all buffered rows to ClickHouse.
func (w *IntelWriter) Flush(ctx context.Context) error {
	w.mu.Lock()
	intelRows := w.intelBuf
	costRows := w.costBuf
	w.intelBuf = make([]IntelEventRow, 0, w.batchSize)
	w.costBuf = make([]CostAttributionRow, 0, 64)
	w.mu.Unlock()

	if len(intelRows) == 0 && len(costRows) == 0 {
		return nil
	}

	var firstErr error

	if len(intelRows) > 0 {
		if err := w.flushIntelEvents(ctx, intelRows); err != nil {
			log.Error().Err(err).Int("count", len(intelRows)).Msg("intel writer: flush intel events failed")
			w.errorCount.Add(1)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	if len(costRows) > 0 {
		if err := w.flushCostAttribution(ctx, costRows); err != nil {
			log.Error().Err(err).Int("count", len(costRows)).Msg("intel writer: flush cost attribution failed")
			w.errorCount.Add(1)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	w.flushCount.Add(1)
	log.Debug().
		Int("intel_events", len(intelRows)).
		Int("cost_rows", len(costRows)).
		Msg("intel writer flushed")

	return firstErr
}

func (w *IntelWriter) flushIntelEvents(ctx context.Context, rows []IntelEventRow) error {
	if w.flushHook != nil {
		generic := make([][]any, len(rows))
		for i, r := range rows {
			generic[i] = []any{
				r.EventID, r.Timestamp, r.Topic, r.Priority,
				r.TTLSeconds, r.Confidence, r.ImpactHorizon,
				r.DirectionalBias, r.AssetTags, r.ClaimsJSON,
				r.SourcesJSON, r.Provider, r.CostUSD,
				r.LatencyMs, r.DownstreamTrades,
			}
		}
		return w.flushHook(ctx, w.tableName("intel_events"), generic)
	}

	query := fmt.Sprintf(
		"INSERT INTO %s (event_id, ts, topic, priority, ttl_seconds, confidence, "+
			"impact_horizon, directional_bias, asset_tags, claims_json, sources_json, "+
			"provider, cost_usd, latency_ms, downstream_trade_ids)",
		w.tableName("intel_events"))

	batch, err := w.client.Conn().PrepareBatch(ctx, query)
	if err != nil {
		return fmt.Errorf("prepare intel batch: %w", err)
	}

	for _, r := range rows {
		if err := batch.Append(
			r.EventID, r.Timestamp, r.Topic, r.Priority,
			r.TTLSeconds, r.Confidence, r.ImpactHorizon,
			r.DirectionalBias, r.AssetTags, r.ClaimsJSON,
			r.SourcesJSON, r.Provider, r.CostUSD,
			r.LatencyMs, r.DownstreamTrades,
		); err != nil {
			return fmt.Errorf("append intel row: %w", err)
		}
	}

	return batch.Send()
}

func (w *IntelWriter) flushCostAttribution(ctx context.Context, rows []CostAttributionRow) error {
	if w.flushHook != nil {
		generic := make([][]any, len(rows))
		for i, r := range rows {
			generic[i] = []any{
				r.Date, r.StrategyID, r.Exchange,
				r.TradingFeesUSD, r.SlippageCostUSD, r.InfraCostUSD,
				r.LLMCostUSD, r.TotalCostUSD, r.GrossPnlUSD,
				r.NetPnlUSD, r.TradeCount, r.IntelEventsUsed,
			}
		}
		return w.flushHook(ctx, w.tableName("cost_attribution_daily"), generic)
	}

	query := fmt.Sprintf(
		"INSERT INTO %s (date, strategy_id, exchange, trading_fees_usd, "+
			"slippage_cost_usd, infra_cost_usd, llm_cost_usd, total_cost_usd, "+
			"gross_pnl_usd, net_pnl_usd, trade_count, intel_events_used)",
		w.tableName("cost_attribution_daily"))

	batch, err := w.client.Conn().PrepareBatch(ctx, query)
	if err != nil {
		return fmt.Errorf("prepare cost batch: %w", err)
	}

	for _, r := range rows {
		if err := batch.Append(
			r.Date, r.StrategyID, r.Exchange,
			r.TradingFeesUSD, r.SlippageCostUSD, r.InfraCostUSD,
			r.LLMCostUSD, r.TotalCostUSD, r.GrossPnlUSD,
			r.NetPnlUSD, r.TradeCount, r.IntelEventsUsed,
		); err != nil {
			return fmt.Errorf("append cost row: %w", err)
		}
	}

	return batch.Send()
}

// Close stops the background writer and performs a final flush.
func (w *IntelWriter) Close() error {
	if w.cancel != nil {
		w.cancel()
		<-w.done
	}
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()

	if err := w.Flush(context.Background()); err != nil {
		log.Error().Err(err).Msg("intel writer: final flush on close failed")
		return err
	}

	log.Info().
		Int64("flushes", w.flushCount.Load()).
		Int64("errors", w.errorCount.Load()).
		Msg("intel writer closed")
	return nil
}

// Stats returns writer statistics.
func (w *IntelWriter) Stats() (flushCount, errorCount int64, pendingIntel, pendingCost int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushCount.Load(), w.errorCount.Load(), len(w.intelBuf), len(w.costBuf)
}

// SetFlushHook sets a test hook. Intended for testing only.
func (w *IntelWriter) SetFlushHook(hook func(ctx context.Context, table string, rows [][]any) error) {
	w.flushHook = hook
}

// IntelEventToRow converts intel package types to a ClickHouse row.
// This is a helper to avoid tight coupling between the intel and clickhouse packages.
func IntelEventToRow(eventID string, ts time.Time, topic, priority string,
	ttl int, confidence float64, horizon, bias string,
	assetTags []string, claims, sources any,
	provider string, costUSD float64, latencyMs int,
) IntelEventRow {
	claimsJSON, _ := json.Marshal(claims)
	sourcesJSON, _ := json.Marshal(sources)

	return IntelEventRow{
		EventID:          eventID,
		Timestamp:        ts,
		Topic:            topic,
		Priority:         priority,
		TTLSeconds:       uint32(ttl),
		Confidence:       float32(confidence),
		ImpactHorizon:    horizon,
		DirectionalBias:  bias,
		AssetTags:        assetTags,
		ClaimsJSON:       string(claimsJSON),
		SourcesJSON:      string(sourcesJSON),
		Provider:         provider,
		CostUSD:          costUSD,
		LatencyMs:        uint32(latencyMs),
		DownstreamTrades: []string{},
	}
}
