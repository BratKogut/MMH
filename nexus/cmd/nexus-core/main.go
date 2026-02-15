package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/nexus-trading/nexus/internal/adapters/binance"
	"github.com/nexus-trading/nexus/internal/adapters/kraken"
	"github.com/nexus-trading/nexus/internal/bus"
	chpkg "github.com/nexus-trading/nexus/internal/clickhouse"
	"github.com/nexus-trading/nexus/internal/config"
	"github.com/nexus-trading/nexus/internal/features"
	"github.com/nexus-trading/nexus/internal/market"
	"github.com/nexus-trading/nexus/internal/quality"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	// 1. Parse flags.
	configPath := flag.String("config", "config/config.yaml", "Path to configuration file")
	stubMode := flag.Bool("stub", false, "Use stub producer/writer (no Kafka/ClickHouse required)")
	flag.Parse()

	// 2. Load configuration.
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: failed to load config from %s: %v\n", *configPath, err)
		os.Exit(1)
	}

	// 3. Setup structured logging.
	setupLogging(cfg.General)

	log.Info().Msg("========================================")
	log.Info().Msg("NEXUS Trading Platform - Starting")
	log.Info().Msg("SAFETY > PROFIT > SPEED")
	log.Info().Msg("========================================")

	log.Info().
		Str("instance_id", cfg.General.InstanceID).
		Str("environment", cfg.General.Environment).
		Bool("dry_run", cfg.General.DryRun).
		Bool("stub_mode", *stubMode).
		Strs("exchanges", cfg.Exchanges.Enabled).
		Strs("kafka_brokers", cfg.Kafka.Brokers).
		Str("clickhouse_dsn", cfg.ClickHouse.DSN).
		Msg("Configuration loaded")

	// 4. Create Kafka producer.
	var producer market.Producer
	if *stubMode {
		producer = bus.NewStubProducer()
		log.Info().Msg("Kafka producer: STUB mode")
	} else {
		p, err := bus.NewProducer(cfg.Kafka.Brokers, cfg.General.InstanceID)
		if err != nil {
			log.Warn().Err(err).Msg("Failed to create Kafka producer, falling back to stub")
			producer = bus.NewStubProducer()
		} else {
			producer = p
			log.Info().Strs("brokers", cfg.Kafka.Brokers).Msg("Kafka producer: LIVE (franz-go)")
		}
	}

	// 5. Create ClickHouse writer.
	var chWriter market.CHWriter
	var chBatchWriter *chpkg.BatchWriter
	if *stubMode {
		chWriter = &stubCHWriter{}
		log.Info().Msg("ClickHouse writer: STUB mode")
	} else {
		chClient, err := chpkg.NewClient(cfg.ClickHouse.DSN)
		if err != nil {
			log.Warn().Err(err).Msg("Failed to connect to ClickHouse, falling back to stub")
			chWriter = &stubCHWriter{}
		} else {
			if err := chClient.Ping(context.Background()); err != nil {
				log.Warn().Err(err).Msg("ClickHouse ping failed, falling back to stub")
				chClient.Close()
				chWriter = &stubCHWriter{}
			} else {
				chBatchWriter = chpkg.NewBatchWriter(chClient, 1000, 5*time.Second)
				chWriter = chBatchWriter
				log.Info().Str("dsn", cfg.ClickHouse.DSN).Msg("ClickHouse writer: LIVE")
			}
		}
	}

	// 6. Create Feature Engine for all configured symbols.
	allSymbols := collectAllSymbols(cfg)
	featureEngine := features.NewEngine(allSymbols)
	log.Info().Strs("symbols", allSymbols).Msg("Feature engine created")

	// 7. Create Quality Monitor.
	qualityMonitor := quality.NewMonitor(cfg.Risk.FeedLagThresholdMs)
	log.Info().Int("lag_threshold_ms", cfg.Risk.FeedLagThresholdMs).Msg("Quality monitor created")

	// 8. Create Market Data Service.
	svc := market.NewService(*cfg, producer, chWriter, featureEngine, qualityMonitor)

	// 9. Register exchange adapters.
	registerAdapters(svc, cfg)

	// 10. Setup context with cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 11. Handle shutdown signals.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Warn().Str("signal", sig.String()).Msg("Shutdown signal received")
		cancel()
	}()

	// 12. Start background services.
	var wg sync.WaitGroup

	// Quality monitor alert logger.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for alert := range qualityMonitor.Alerts() {
			switch alert.Level {
			case "critical":
				log.Error().
					Str("exchange", alert.Exchange).
					Str("symbol", alert.Symbol).
					Msg(alert.Message)
			default:
				log.Warn().
					Str("exchange", alert.Exchange).
					Str("symbol", alert.Symbol).
					Msg(alert.Message)
			}
		}
	}()

	// ClickHouse batch writer (if live).
	if chBatchWriter != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			chBatchWriter.Start(ctx)
		}()
	}

	// 13. Start Market Data Service (blocks until ctx cancelled).
	log.Info().Msg("Starting Market Data Service")
	if err := svc.Start(ctx); err != nil {
		log.Error().Err(err).Msg("Market Data Service exited with error")
	}

	// 14. Graceful shutdown.
	if err := svc.Stop(); err != nil {
		log.Error().Err(err).Msg("Error during shutdown")
	}

	wg.Wait()
	log.Info().Msg("NEXUS Trading Platform - Shutdown complete")
}

// --- Stub implementations for development ---

type stubCHWriter struct{}

func (w *stubCHWriter) WriteTick(_ context.Context, tick bus.Tick) error {
	log.Debug().Str("exchange", tick.Exchange).Str("symbol", tick.Symbol).Msg("stub: tick")
	return nil
}
func (w *stubCHWriter) WriteTrade(_ context.Context, trade bus.Trade) error {
	log.Debug().Str("exchange", trade.Exchange).Str("symbol", trade.Symbol).Msg("stub: trade")
	return nil
}
func (w *stubCHWriter) Flush(_ context.Context) error { return nil }
func (w *stubCHWriter) Close() error                  { return nil }

// --- Helper functions ---

func setupLogging(general config.GeneralConfig) {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMicro
	level, err := zerolog.ParseLevel(general.LogLevel)
	if err != nil {
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)

	if general.LogFormat == "text" {
		log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout}).
			With().Timestamp().Str("service", "nexus-core").Str("instance", general.InstanceID).Logger()
	} else {
		log.Logger = zerolog.New(os.Stdout).
			With().Timestamp().Str("service", "nexus-core").Str("instance", general.InstanceID).Logger()
	}
}

func collectAllSymbols(cfg *config.Config) []string {
	seen := make(map[string]struct{})
	var symbols []string
	for _, sym := range cfg.Exchanges.Kraken.Symbols {
		if _, ok := seen[sym]; !ok {
			seen[sym] = struct{}{}
			symbols = append(symbols, sym)
		}
	}
	for _, sym := range cfg.Exchanges.Binance.Symbols {
		if _, ok := seen[sym]; !ok {
			seen[sym] = struct{}{}
			symbols = append(symbols, sym)
		}
	}
	return symbols
}

func registerAdapters(svc *market.Service, cfg *config.Config) {
	for _, name := range cfg.Exchanges.Enabled {
		switch name {
		case "kraken":
			adapter := kraken.New(
				cfg.Exchanges.Kraken.WSURL, cfg.Exchanges.Kraken.RESTURL,
				cfg.Exchanges.Kraken.APIKey, cfg.Exchanges.Kraken.APISecret,
				cfg.Exchanges.Kraken.Symbols,
			)
			svc.RegisterAdapter("kraken", adapter)
			log.Info().Str("adapter", "kraken").Strs("symbols", cfg.Exchanges.Kraken.Symbols).Msg("Registered")
		case "binance":
			adapter := binance.New(
				cfg.Exchanges.Binance.WSURL, cfg.Exchanges.Binance.RESTURL,
				cfg.Exchanges.Binance.APIKey, cfg.Exchanges.Binance.APISecret,
				cfg.Exchanges.Binance.Symbols,
			)
			svc.RegisterAdapter("binance", adapter)
			log.Info().Str("adapter", "binance").Strs("symbols", cfg.Exchanges.Binance.Symbols).Msg("Registered")
		default:
			log.Warn().Str("exchange", name).Msg("Unknown exchange, skipping")
		}
	}
}
