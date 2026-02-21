package market

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/nexus-trading/nexus/internal/adapters"
	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/nexus-trading/nexus/internal/config"
	"github.com/nexus-trading/nexus/internal/features"
	"github.com/nexus-trading/nexus/internal/quality"
	"github.com/rs/zerolog/log"
)

// Producer is the interface for publishing serialized messages to Kafka.
// Decoupled from the bus.Producer implementation so the service compiles
// independently of the real Kafka client.
type Producer interface {
	Produce(ctx context.Context, topic string, key []byte, value []byte) error
	Close()
}

// CHWriter is the interface for writing market data to ClickHouse.
// Decoupled so the service compiles while the real ClickHouse writer
// is built in parallel.
type CHWriter interface {
	WriteTick(ctx context.Context, tick bus.Tick) error
	WriteTrade(ctx context.Context, trade bus.Trade) error
	Flush(ctx context.Context) error
	Close() error
}

// Service is the main S1 orchestrator. It connects exchange adapters,
// streams market data, and fans events to Kafka, ClickHouse, the Feature
// Engine, and the Quality Monitor.
type Service struct {
	adapters map[string]adapters.ExchangeAdapter
	producer Producer
	chWriter CHWriter
	features *features.Engine
	quality  *quality.Monitor
	config   config.Config

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// NewService creates a new Market Data Service.
func NewService(
	cfg config.Config,
	producer Producer,
	chWriter CHWriter,
	featureEngine *features.Engine,
	qualityMonitor *quality.Monitor,
) *Service {
	return &Service{
		adapters: make(map[string]adapters.ExchangeAdapter),
		producer: producer,
		chWriter: chWriter,
		features: featureEngine,
		quality:  qualityMonitor,
		config:   cfg,
	}
}

// RegisterAdapter adds an exchange adapter to the service.
func (s *Service) RegisterAdapter(name string, adapter adapters.ExchangeAdapter) {
	s.adapters[name] = adapter
	log.Info().Str("adapter", name).Msg("Registered exchange adapter")
}

// Start connects all adapters and begins streaming market data.
// It blocks until the context is cancelled or all streams complete.
func (s *Service) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	log.Info().Int("adapter_count", len(s.adapters)).Msg("Starting Market Data Service")

	// Connect each adapter.
	for name, adapter := range s.adapters {
		if err := adapter.Connect(ctx); err != nil {
			log.Error().Err(err).Str("adapter", name).Msg("Failed to connect adapter")
			// Continue with other adapters; don't crash on one failure.
			continue
		}
		log.Info().Str("adapter", name).Msg("Adapter connected")
	}

	// Start the quality monitor background checker.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.quality.Start(ctx)
	}()

	// For each adapter, start streaming goroutines for all configured symbols.
	for name, adapter := range s.adapters {
		symbols := s.symbolsForAdapter(name)
		if len(symbols) == 0 {
			log.Warn().Str("adapter", name).Msg("No symbols configured, skipping")
			continue
		}

		for _, symbol := range symbols {
			s.startTradeStream(ctx, adapter, name, symbol)
			s.startTickStream(ctx, adapter, name, symbol)
		}
	}

	log.Info().Msg("Market Data Service started, all streams running")

	// Block until context is cancelled.
	<-ctx.Done()

	// Wait for all goroutines to finish.
	s.wg.Wait()
	log.Info().Msg("Market Data Service: all goroutines stopped")

	return nil
}

// Stop triggers graceful shutdown: cancels context, disconnects adapters,
// flushes writers, and closes connections.
func (s *Service) Stop() error {
	log.Info().Msg("Market Data Service stopping")

	// Cancel context to signal all goroutines.
	if s.cancel != nil {
		s.cancel()
	}

	// Wait for goroutines to drain.
	s.wg.Wait()

	// Disconnect all adapters.
	for name, adapter := range s.adapters {
		if err := adapter.Disconnect(); err != nil {
			log.Error().Err(err).Str("adapter", name).Msg("Error disconnecting adapter")
		} else {
			log.Info().Str("adapter", name).Msg("Adapter disconnected")
		}
	}

	// Flush ClickHouse writer.
	if s.chWriter != nil {
		if err := s.chWriter.Flush(context.Background()); err != nil {
			log.Error().Err(err).Msg("Error flushing ClickHouse writer")
		}
		if err := s.chWriter.Close(); err != nil {
			log.Error().Err(err).Msg("Error closing ClickHouse writer")
		}
	}

	// Close Kafka producer.
	if s.producer != nil {
		s.producer.Close()
	}

	log.Info().Msg("Market Data Service stopped")
	return nil
}

// startTradeStream starts a goroutine that reads trades from an adapter
// for a given symbol and fans them to Kafka, ClickHouse, Feature Engine,
// and Quality Monitor.
func (s *Service) startTradeStream(ctx context.Context, adapter adapters.ExchangeAdapter, exchange, symbol string) {
	tradeCh, err := adapter.StreamTrades(ctx, symbol)
	if err != nil {
		log.Error().Err(err).
			Str("exchange", exchange).
			Str("symbol", symbol).
			Msg("Failed to start trade stream")
		return
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		logger := log.With().
			Str("exchange", exchange).
			Str("symbol", symbol).
			Str("stream", "trades").
			Logger()

		logger.Info().Msg("Trade stream started")

		for {
			select {
			case <-ctx.Done():
				logger.Info().Msg("Trade stream stopping (context cancelled)")
				return
			case trade, ok := <-tradeCh:
				if !ok {
					logger.Warn().Msg("Trade channel closed")
					return
				}
				s.handleTrade(ctx, exchange, symbol, trade)
			}
		}
	}()
}

// startTickStream starts a goroutine that reads ticks from an adapter
// for a given symbol and fans them to Kafka, ClickHouse, Feature Engine,
// and Quality Monitor.
func (s *Service) startTickStream(ctx context.Context, adapter adapters.ExchangeAdapter, exchange, symbol string) {
	tickCh, err := adapter.StreamTicks(ctx, symbol)
	if err != nil {
		log.Error().Err(err).
			Str("exchange", exchange).
			Str("symbol", symbol).
			Msg("Failed to start tick stream")
		return
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		logger := log.With().
			Str("exchange", exchange).
			Str("symbol", symbol).
			Str("stream", "ticks").
			Logger()

		logger.Info().Msg("Tick stream started")

		for {
			select {
			case <-ctx.Done():
				logger.Info().Msg("Tick stream stopping (context cancelled)")
				return
			case tick, ok := <-tickCh:
				if !ok {
					logger.Warn().Msg("Tick channel closed")
					return
				}
				s.handleTick(ctx, exchange, symbol, tick)
			}
		}
	}()
}

// handleTrade processes a single trade event through the full pipeline:
// Kafka publish, ClickHouse write, Feature Engine, Quality Monitor.
func (s *Service) handleTrade(ctx context.Context, exchange, symbol string, trade bus.Trade) {
	// 1. Report to Quality Monitor.
	s.quality.RecordTrade(exchange, symbol, trade.Timestamp)

	// 2. Publish to Kafka: md.trades.<exchange>.<symbol>
	topic := bus.Topics.Trades(exchange, symbol)
	if err := s.publishJSON(ctx, topic, trade.TradeID, trade); err != nil {
		log.Error().Err(err).
			Str("topic", topic).
			Str("exchange", exchange).
			Str("symbol", symbol).
			Msg("Failed to publish trade to Kafka")
		// Log and continue; never crash on a single bad event.
	}

	// 3. Write to ClickHouse.
	if s.chWriter != nil {
		if err := s.chWriter.WriteTrade(ctx, trade); err != nil {
			log.Error().Err(err).
				Str("exchange", exchange).
				Str("symbol", symbol).
				Msg("Failed to write trade to ClickHouse")
		}
	}

	// 4. Feed to Feature Engine and publish computed features.
	if s.features != nil {
		featureResults := s.features.ProcessTrade(trade)
		s.publishFeatures(ctx, symbol, featureResults)
	}
}

// handleTick processes a single tick event through the full pipeline:
// Kafka publish, ClickHouse write, Feature Engine, Quality Monitor.
func (s *Service) handleTick(ctx context.Context, exchange, symbol string, tick bus.Tick) {
	// 1. Report to Quality Monitor.
	s.quality.RecordTick(exchange, symbol, tick.Timestamp)

	// 2. Publish to Kafka: md.ticks.<exchange>.<symbol>
	topic := bus.Topics.Ticks(exchange, symbol)
	key := fmt.Sprintf("%s.%s", exchange, symbol)
	if err := s.publishJSON(ctx, topic, key, tick); err != nil {
		log.Error().Err(err).
			Str("topic", topic).
			Str("exchange", exchange).
			Str("symbol", symbol).
			Msg("Failed to publish tick to Kafka")
	}

	// 3. Write to ClickHouse.
	if s.chWriter != nil {
		if err := s.chWriter.WriteTick(ctx, tick); err != nil {
			log.Error().Err(err).
				Str("exchange", exchange).
				Str("symbol", symbol).
				Msg("Failed to write tick to ClickHouse")
		}
	}

	// 4. Feed to Feature Engine and publish computed features.
	if s.features != nil {
		featureResults := s.features.ProcessTick(tick)
		s.publishFeatures(ctx, symbol, featureResults)
	}
}

// publishJSON serializes value as JSON and publishes to Kafka via the Producer.
func (s *Service) publishJSON(ctx context.Context, topic, key string, value interface{}) error {
	if s.producer == nil {
		return nil
	}

	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal JSON for topic %s: %w", topic, err)
	}

	return s.producer.Produce(ctx, topic, []byte(key), data)
}

// publishFeatures publishes computed feature results to Kafka.
func (s *Service) publishFeatures(ctx context.Context, symbol string, results []features.FeatureResult) {
	if s.producer == nil || len(results) == 0 {
		return
	}

	topic := bus.Topics.Features(symbol)
	data, err := json.Marshal(results)
	if err != nil {
		log.Error().Err(err).Str("symbol", symbol).Msg("Failed to marshal feature results")
		return
	}

	if err := s.producer.Produce(ctx, topic, []byte(symbol), data); err != nil {
		log.Error().Err(err).
			Str("topic", topic).
			Str("symbol", symbol).
			Msg("Failed to publish features to Kafka")
	}
}

// symbolsForAdapter returns the configured symbols for a given adapter name.
func (s *Service) symbolsForAdapter(name string) []string {
	switch name {
	case "kraken":
		return s.config.Exchanges.Kraken.Symbols
	case "binance":
		return s.config.Exchanges.Binance.Symbols
	default:
		log.Warn().Str("adapter", name).Msg("Unknown adapter, no symbols configured")
		return nil
	}
}
