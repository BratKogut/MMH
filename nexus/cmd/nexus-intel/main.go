package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	chpkg "github.com/nexus-trading/nexus/internal/clickhouse"
	"github.com/nexus-trading/nexus/internal/config"
	"github.com/nexus-trading/nexus/internal/intel"

	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/nexus-trading/nexus/internal/observability"
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

	log.Info().Msg("============================================")
	log.Info().Msg("NEXUS Intel Pipeline - Starting")
	log.Info().Msg("LLM NIE HANDLUJE. LLM = extraction, classification, synthesis.")
	log.Info().Msg("SELEKTYWNA INTELIGENCJA: budget, TTL, measurable ROI.")
	log.Info().Msg("============================================")

	log.Info().
		Str("instance_id", cfg.General.InstanceID).
		Str("environment", cfg.General.Environment).
		Bool("stub_mode", *stubMode).
		Bool("intel_enabled", cfg.Intel.Enabled).
		Float64("daily_budget_usd", cfg.Intel.DailyBudgetUSD).
		Int("max_events_per_hour", cfg.Intel.MaxEventsPerHour).
		Msg("Configuration loaded")

	// 4. Create Kafka producer.
	var producer bus.Producer
	if *stubMode {
		producer = bus.NewStubProducer()
		log.Info().Msg("Kafka producer: STUB mode")
	} else {
		p, pErr := bus.NewProducer(cfg.Kafka.Brokers,
			bus.WithInstanceID(cfg.General.InstanceID+"-intel"))
		if pErr != nil {
			log.Warn().Err(pErr).Msg("Failed to create Kafka producer, falling back to stub")
			producer = bus.NewStubProducer()
		} else {
			producer = p
			log.Info().Strs("brokers", cfg.Kafka.Brokers).Msg("Kafka producer: LIVE")
		}
	}

	// 5. Create ClickHouse intel writer (optional).
	var intelWriter *chpkg.IntelWriter
	if !*stubMode && cfg.ClickHouse.DSN != "" {
		chClient, chErr := chpkg.NewClient(cfg.ClickHouse.DSN)
		if chErr != nil {
			log.Warn().Err(chErr).Msg("ClickHouse unavailable, intel events will not be persisted")
		} else {
			if pingErr := chClient.Ping(context.Background()); pingErr != nil {
				log.Warn().Err(pingErr).Msg("ClickHouse ping failed, intel events will not be persisted")
				chClient.Close()
			} else {
				intelWriter = chpkg.NewIntelWriter(chClient, cfg.ClickHouse.Database, 500, 10*time.Second)
				log.Info().Str("database", cfg.ClickHouse.Database).Msg("ClickHouse intel writer: LIVE")
			}
		}
	}

	// 6. Build Intel Service.
	maxEventsPerHour := cfg.Intel.MaxEventsPerHour
	if maxEventsPerHour == 0 {
		maxEventsPerHour = 60
	}

	svcConfig := intel.IntelServiceConfig{
		BrainConfig: intel.BrainConfig{
			MaxEventsPerHourNormal:   maxEventsPerHour,
			MaxEventsPerHourVolatile: maxEventsPerHour * 2,
			CircuitBreakerErrorPct:   0.50,
			CircuitBreakerWindowMs:   60_000,
			CircuitBreakerCooldownMs: 120_000,
		},
		TriggerEngineConfig: intel.DefaultTriggerEngineConfig(),
		ROILinkWindowMs:     600_000, // 10 minutes
		CHDBPrefix:          cfg.ClickHouse.Database,
	}

	intelService := intel.NewIntelService(svcConfig, producer, intelWriter)

	// 7. Register LLM providers.
	// In stub mode or when no real providers are configured, use a stub.
	if *stubMode || cfg.Intel.InternalLLMAddress == "" {
		stubResp := intel.QueryResponse{
			Text:       "Stub analysis: market conditions nominal.",
			Confidence: 0.5,
			TokensUsed: 64,
			CostUSD:    0.001,
			Claims: []intel.Claim{
				{Statement: "Market conditions appear stable", Confidence: 0.5, Verifiable: false},
			},
			Sources: []intel.Source{
				{Name: "stub", Type: "internal"},
			},
		}
		intelService.RegisterProvider(intel.NewStubProvider("primary", []intel.QueryResponse{stubResp}))
		log.Info().Msg("LLM provider: STUB (no real LLM configured)")
	}
	// TODO: Register real providers when configured:
	//   if cfg.Intel.InternalLLMAddress != "" { ... }
	//   if cfg.Intel.GrokEnabled { ... }
	//   if cfg.Intel.ClaudeEnabled { ... }

	// 8. Setup observability.
	metrics := observability.NexusMetrics()
	healthMonitor := observability.NewHealthMonitor(15 * time.Second)

	// Register health checks.
	healthMonitor.Register("intel_brain", func(ctx context.Context) observability.ComponentHealth {
		stats := intelService.Brain().Stats()
		status := observability.StatusHealthy
		msg := fmt.Sprintf("queries=%d, events=%d, cost=$%.4f",
			stats.TotalQueries, stats.TotalEvents, stats.TotalCostUSD)

		// Check if all providers have tripped circuit breakers.
		allBroken := true
		for _, ps := range stats.ProviderStats {
			if !ps.CircuitOpen {
				allBroken = false
				break
			}
		}
		if allBroken && len(stats.ProviderStats) > 0 {
			status = observability.StatusUnhealthy
			msg = "all providers have tripped circuit breakers"
		}

		return observability.ComponentHealth{
			Status:  status,
			Message: msg,
		}
	})

	healthMonitor.Register("intel_trigger_engine", func(_ context.Context) observability.ComponentHealth {
		stats := intelService.TriggerEngine().Stats()
		return observability.ComponentHealth{
			Status: observability.StatusHealthy,
			Message: fmt.Sprintf("triggers=%d, anomalies=%d, symbols=%d",
				stats.TriggersEmitted, stats.AnomaliesFound, stats.SymbolsTracked),
		}
	})

	healthMonitor.Register("intel_roi", func(_ context.Context) observability.ComponentHealth {
		roi := intelService.ROITracker().ComputeROI()
		return observability.ComponentHealth{
			Status: observability.StatusHealthy,
			Message: fmt.Sprintf("links=%d, cost=$%.4f, pnl=$%.2f, roi=%.2f%%",
				roi.TotalLinks, roi.TotalIntelCostUSD, roi.TotalLinkedPnL, roi.NetROI*100),
		}
	})

	_ = metrics // registered for future Prometheus exposition

	// 9. Setup context.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Warn().Str("signal", sig.String()).Msg("Shutdown signal received")
		cancel()
	}()

	// 10. Start services.
	var wg sync.WaitGroup

	// Start Intel Service (trigger scheduler, ROI cleanup).
	intelService.Start(ctx)

	// Start ClickHouse intel writer if available.
	if intelWriter != nil {
		intelWriter.Start(ctx)
	}

	// Start health monitor.
	wg.Add(1)
	go func() {
		defer wg.Done()
		healthMonitor.Start(ctx)
	}()

	// Health alert logger.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case alert := <-healthMonitor.Alerts():
				switch alert.Level {
				case "critical":
					log.Error().Str("component", alert.Component).Msg(alert.Message)
				case "warn":
					log.Warn().Str("component", alert.Component).Msg(alert.Message)
				default:
					log.Info().Str("component", alert.Component).Msg(alert.Message)
				}
			}
		}
	}()

	// Start Kafka consumer for intel events (if not in stub mode).
	if !*stubMode {
		wg.Add(1)
		go func() {
			defer wg.Done()
			consumer, cErr := bus.NewConsumer(
				cfg.Kafka.Brokers,
				cfg.General.InstanceID+"-intel-consumer",
				[]string{bus.Topics.IntelEvents()},
			)
			if cErr != nil {
				log.Warn().Err(cErr).Msg("Failed to create intel consumer, skipping")
				return
			}
			defer consumer.Close()

			log.Info().Msg("Intel event consumer started")
			if consumeErr := consumer.Consume(ctx, func(cCtx context.Context, msg bus.Message) error {
				return intelService.HandleIntelMessage(cCtx, msg)
			}); consumeErr != nil && ctx.Err() == nil {
				log.Error().Err(consumeErr).Msg("Intel consumer error")
			}
		}()
	}

	// Start simple HTTP health endpoint.
	wg.Add(1)
	go func() {
		defer wg.Done()
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			health := healthMonitor.Check(r.Context())
			w.Header().Set("Content-Type", "application/json")
			if health.Status == observability.StatusUnhealthy {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			fmt.Fprintf(w, `{"status":"%s","uptime_s":%.0f}`, health.Status, health.Uptime.Seconds())
		})
		mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
			stats := intelService.Stats()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"events_produced":%d,"events_consumed":%d,"brain_queries":%d,"brain_cost_usd":%.4f,"triggers":%d,"roi_links":%d}`,
				stats.EventsProduced, stats.EventsConsumed,
				stats.BrainStats.TotalQueries, stats.BrainStats.TotalCostUSD,
				stats.TriggerStats.TriggersEmitted, stats.ROISummary.TotalLinks)
		})

		port := cfg.Metrics.PrometheusPort + 1 // intel runs on next port
		addr := fmt.Sprintf(":%d", port)
		server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

		log.Info().Str("addr", addr).Msg("Intel HTTP health/stats server started")

		go func() {
			<-ctx.Done()
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			server.Shutdown(shutdownCtx)
		}()

		if srvErr := server.ListenAndServe(); srvErr != nil && srvErr != http.ErrServerClosed {
			log.Error().Err(srvErr).Msg("HTTP server error")
		}
	}()

	log.Info().Msg("NEXUS Intel Pipeline - Running")

	// 11. Block until shutdown.
	<-ctx.Done()

	// 12. Graceful shutdown.
	log.Info().Msg("Shutting down Intel Pipeline...")

	intelService.Stop()
	healthMonitor.Stop()

	if intelWriter != nil {
		if closeErr := intelWriter.Close(); closeErr != nil {
			log.Error().Err(closeErr).Msg("Intel writer close error")
		}
	}

	wg.Wait()

	// Log final stats.
	stats := intelService.Stats()
	log.Info().
		Int64("events_produced", stats.EventsProduced).
		Int64("events_consumed", stats.EventsConsumed).
		Int64("brain_queries", stats.BrainStats.TotalQueries).
		Float64("total_cost_usd", stats.BrainStats.TotalCostUSD).
		Int64("triggers_fired", stats.TriggerStats.TriggersEmitted).
		Int("roi_links", stats.ROISummary.TotalLinks).
		Float64("net_roi", stats.ROISummary.NetROI).
		Msg("NEXUS Intel Pipeline - Final statistics")

	log.Info().Msg("NEXUS Intel Pipeline - Shutdown complete")
}

func setupLogging(general config.GeneralConfig) {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMicro
	level, err := zerolog.ParseLevel(general.LogLevel)
	if err != nil {
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)

	if general.LogFormat == "text" {
		log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout}).
			With().Timestamp().Str("service", "nexus-intel").
			Str("instance", general.InstanceID).Logger()
	} else {
		log.Logger = zerolog.New(os.Stdout).
			With().Timestamp().Str("service", "nexus-intel").
			Str("instance", general.InstanceID).Logger()
	}
}
