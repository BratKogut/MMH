package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/nexus-trading/nexus/internal/adapters/jupiter"
	"github.com/nexus-trading/nexus/internal/config"
	"github.com/nexus-trading/nexus/internal/graph"
	"github.com/nexus-trading/nexus/internal/scanner"
	"github.com/nexus-trading/nexus/internal/sniper"
	"github.com/nexus-trading/nexus/internal/solana"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

func main() {
	// 1. Parse flags.
	configPath := flag.String("config", "config/config.yaml", "Path to configuration file")
	stubMode := flag.Bool("stub", false, "Use stub RPC (no real Solana connection)")
	flag.Parse()

	// 2. Load configuration.
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: failed to load config from %s: %v\n", *configPath, err)
		os.Exit(1)
	}

	// 3. Setup logging.
	setupLogging(cfg.General)

	log.Info().Msg("=============================================")
	log.Info().Msg("NEXUS Memecoin Hunter - Starting")
	log.Info().Msg("DETECT -> ANALYZE -> SNIPE -> PROFIT")
	log.Info().Msg("SAFETY > PROFIT > SPEED")
	log.Info().Msg("=============================================")

	dryRun := cfg.Hunter.DryRun || cfg.General.DryRun
	log.Info().
		Str("instance_id", cfg.General.InstanceID).
		Bool("dry_run", dryRun).
		Bool("stub_mode", *stubMode).
		Strs("dexes", cfg.Hunter.MonitorDEXes).
		Float64("max_buy_sol", cfg.Hunter.MaxBuySOL).
		Float64("take_profit_x", cfg.Hunter.TakeProfitMultiplier).
		Float64("stop_loss_pct", cfg.Hunter.StopLossPct).
		Int("max_positions", cfg.Hunter.MaxPositions).
		Int("min_safety", cfg.Hunter.MinSafetyScore).
		Float64("min_liquidity", cfg.Hunter.MinLiquidityUSD).
		Msg("Configuration loaded")

	// 4. Create Solana RPC client.
	var rpc solana.RPCClient
	if *stubMode {
		rpc = solana.NewStubRPCClient()
		log.Info().Msg("Solana RPC: STUB mode")
	} else {
		// TODO: Create live RPC client when Solana SDK is integrated.
		rpc = solana.NewStubRPCClient()
		log.Info().
			Str("endpoint", cfg.Solana.RPCEndpoint).
			Msg("Solana RPC: STUB (live RPC integration pending)")
	}

	// 5. Create Jupiter adapter.
	jupConfig := jupiter.Config{
		RPCEndpoint:    cfg.Solana.RPCEndpoint,
		WSEndpoint:     cfg.Solana.WSEndpoint,
		WalletKey:      cfg.Solana.PrivateKey,
		DefaultSlipBps: cfg.Hunter.SlippageBps,
		PriorityFee:    cfg.Hunter.PriorityFee,
		UseJito:        cfg.Hunter.UseJito,
		JitoTipSOL:     cfg.Hunter.JitoTipSOL,
	}
	wallet := solana.Pubkey("HUNTER-WALLET") // placeholder until real wallet
	jupAdapter := jupiter.New(jupConfig, rpc, wallet)

	if err := jupAdapter.Connect(context.Background()); err != nil {
		log.Warn().Err(err).Msg("Jupiter adapter connection failed (continuing in degraded mode)")
	}

	// 6. Create Token Analyzer.
	analyzerConfig := scanner.AnalyzerConfig{
		MinSafetyScore:         cfg.Hunter.MinSafetyScore,
		MaxTop10HolderPct:      50.0,
		MaxSingleHolderPct:     15.0,
		RequireMintRenounced:   false,
		RequireFreezeRenounced: true,
		RequireLPSafe:          false,
		MinLiquidityUSD:        cfg.Hunter.MinLiquidityUSD,
		TopHoldersToCheck:      10,
	}
	analyzer := scanner.NewAnalyzer(analyzerConfig, rpc)

	// 6b. Create Entity Graph Engine.
	graphConfig := graph.DefaultConfig()
	entityGraph := graph.NewEngine(graphConfig)
	log.Info().
		Int("max_nodes", graphConfig.MaxNodes).
		Int("label_depth", graphConfig.LabelPropDepth).
		Msg("Entity Graph Engine initialized")

	// 6c. Create Sell Simulator.
	sellSimulator := scanner.NewSellSimulator(rpc)
	log.Info().Msg("Sell Simulator initialized")

	// 6d. Create 5D Scorer.
	scoringConfig := scanner.DefaultScoringConfig()
	scorer := scanner.NewScorer(scoringConfig)
	log.Info().
		Float64("safety_w", scoringConfig.Weights.Safety).
		Float64("entity_w", scoringConfig.Weights.Entity).
		Float64("social_w", scoringConfig.Weights.Social).
		Float64("onchain_w", scoringConfig.Weights.OnChain).
		Float64("timing_w", scoringConfig.Weights.Timing).
		Msg("5D Scorer initialized")

	// 7. Create Sniper Engine.
	sniperConfig := sniper.Config{
		MaxBuySOL:            cfg.Hunter.MaxBuySOL,
		SlippageBps:          cfg.Hunter.SlippageBps,
		TakeProfitMultiplier: cfg.Hunter.TakeProfitMultiplier,
		StopLossPct:          cfg.Hunter.StopLossPct,
		TrailingStopEnabled:  cfg.Hunter.TrailingStopEnabled,
		TrailingStopPct:      cfg.Hunter.TrailingStopPct,
		MaxPositions:         cfg.Hunter.MaxPositions,
		MaxDailyLossSOL:      cfg.Hunter.MaxDailyLossSOL,
		MaxDailySpendSOL:     cfg.Hunter.MaxDailySpendSOL,
		PriceCheckIntervalMs: 3000,
		MinSafetyScore:       cfg.Hunter.MinSafetyScore,
		AutoSellAfterMinutes: cfg.Hunter.AutoSellAfterMinutes,
		DryRun:               dryRun,
	}
	sniperEngine := sniper.NewEngine(sniperConfig, jupAdapter, rpc)

	// Configure multi-level TP exit engine.
	exitConfig := sniper.DefaultExitConfig()
	exitConfig.StopLossPct = cfg.Hunter.StopLossPct
	exitConfig.TrailingStopEnabled = cfg.Hunter.TrailingStopEnabled
	exitConfig.TrailingStopPct = cfg.Hunter.TrailingStopPct
	sniperEngine.SetExitConfig(exitConfig)

	// Wire position callbacks.
	sniperEngine.SetOnPositionOpen(func(pos *sniper.Position) {
		log.Info().
			Str("pos_id", pos.ID).
			Str("mint", string(pos.TokenMint)).
			Str("dex", pos.DEX).
			Str("cost_sol", pos.CostSOL.String()).
			Int("safety", pos.SafetyScore).
			Msg("[POSITION OPENED]")
	})
	sniperEngine.SetOnPositionClose(func(pos *sniper.Position) {
		log.Info().
			Str("pos_id", pos.ID).
			Str("mint", string(pos.TokenMint)).
			Str("reason", pos.CloseReason).
			Float64("pnl_pct", pos.PnLPct).
			Msg("[POSITION CLOSED]")
	})

	// 8. Create Token Scanner.
	scannerConfig := scanner.ScannerConfig{
		MinLiquidityUSD:    cfg.Hunter.MinLiquidityUSD,
		MaxTokenAgeMinutes: cfg.Hunter.MaxTokenAgeMinutes,
		MonitorDEXes:       cfg.Hunter.MonitorDEXes,
		PollIntervalMs:     2000,
		MaxTrackedPools:    500,
		MinQuoteReserveSOL: 1.0,
	}

	// Scanner -> Analyzer -> 5D Scorer -> Sniper pipeline.
	tokenScanner := scanner.NewScanner(scannerConfig, rpc, func(ctx context.Context, discovery scanner.PoolDiscovery) {
		log.Info().
			Str("pool", string(discovery.Pool.PoolAddress)).
			Str("dex", discovery.Pool.DEX).
			Str("liquidity", discovery.Pool.LiquidityUSD.String()).
			Int64("latency_ms", discovery.LatencyMs).
			Msg("[NEW POOL] Analyzing...")

		// Stage 1: Safety analysis.
		analysis := analyzer.Analyze(ctx, discovery)

		log.Info().
			Str("mint", string(analysis.Mint)).
			Int("safety_score", analysis.SafetyScore).
			Str("verdict", string(analysis.Verdict)).
			Msg("[ANALYSIS] Safety result")

		// Stage 2: Pre-buy sell simulation.
		preBuySim := sellSimulator.SimulatePreBuy(ctx,
			decimal.NewFromFloat(cfg.Hunter.MaxBuySOL), discovery.Pool)

		log.Info().
			Str("mint", string(analysis.Mint)).
			Bool("can_sell", preBuySim.CanSell).
			Float64("tax_pct", preBuySim.EstimatedTaxPct).
			Msg("[SELLSIM] Pre-buy result")

		// Stage 3: Entity graph query.
		var entityReport *graph.EntityReport
		if discovery.Pool.TokenMint != "" {
			report := entityGraph.QueryDeployer(string(discovery.Pool.TokenMint))
			entityReport = &report
			log.Debug().
				Str("mint", string(analysis.Mint)).
				Float64("risk", report.RiskScore).
				Int("hops_rugger", report.HopsToRugger).
				Msg("[ENTITY] Deployer report")
		}

		// Stage 4: 5-Dimensional scoring.
		scoringInput := scanner.ScoringInput{
			Analysis:     analysis,
			EntityReport: entityReport,
			SellSim:      &preBuySim,
		}
		tokenScore := scorer.Score(scoringInput)

		log.Info().
			Str("mint", string(analysis.Mint)).
			Float64("total", tokenScore.Total).
			Float64("safety", tokenScore.Safety).
			Float64("entity", tokenScore.Entity).
			Float64("social", tokenScore.Social).
			Float64("onchain", tokenScore.OnChain).
			Float64("timing", tokenScore.Timing).
			Str("recommendation", tokenScore.Recommendation).
			Str("correlation", tokenScore.CorrelationType).
			Bool("instant_kill", tokenScore.InstantKill).
			Msg("[5D SCORE] Result")

		// Stage 5: Apply 5D score to sniper decision.
		if tokenScore.InstantKill {
			log.Warn().
				Str("mint", string(analysis.Mint)).
				Str("kill_reason", tokenScore.KillReason).
				Msg("[SKIP] Instant kill")
			return
		}

		if tokenScore.Recommendation == "SKIP" || tokenScore.Recommendation == "WAIT" {
			log.Info().
				Str("mint", string(analysis.Mint)).
				Str("recommendation", tokenScore.Recommendation).
				Msg("[SKIP] Score too low")
			return
		}

		// Feed to sniper for buy decision.
		sniperEngine.OnDiscovery(ctx, analysis)
	})

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

	// Start scanner.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := tokenScanner.Start(ctx); err != nil {
			log.Error().Err(err).Msg("Scanner error")
		}
	}()

	// Start HTTP health/stats endpoint.
	wg.Add(1)
	go func() {
		defer wg.Done()
		mux := http.NewServeMux()

		mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"status":"ok","dry_run":%t}`, dryRun)
		})

		mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
			sniperStats := sniperEngine.Stats()
			scannerStats := tokenScanner.Stats()
			jupStats := jupAdapter.Stats()
			graphStats := entityGraph.Stats()

			combined := map[string]any{
				"sniper":  sniperStats,
				"scanner": scannerStats,
				"jupiter": jupStats,
				"graph":   graphStats,
				"dry_run": dryRun,
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(combined)
		})

		mux.HandleFunc("/positions", func(w http.ResponseWriter, _ *http.Request) {
			positions := sniperEngine.Positions()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(positions)
		})

		mux.HandleFunc("/positions/open", func(w http.ResponseWriter, _ *http.Request) {
			positions := sniperEngine.OpenPositions()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(positions)
		})

		port := cfg.Metrics.PrometheusPort + 2 // hunter on port +2
		addr := fmt.Sprintf(":%d", port)
		server := &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}

		log.Info().Str("addr", addr).Msg("Hunter HTTP server started")

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

	// Periodic stats logging.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ss := sniperEngine.Stats()
				sc := tokenScanner.Stats()
				log.Info().
					Int64("pools_scanned", sc.PoolsScanned).
					Int64("pools_accepted", sc.PoolsAccepted).
					Int64("snipes", ss.TotalSnipes).
					Int("open_pos", ss.OpenPositions).
					Int64("wins", ss.WinCount).
					Int64("losses", ss.LossCount).
					Float64("win_rate", ss.WinRate).
					Str("daily_spent", ss.DailySpentSOL).
					Msg("[STATS]")
			}
		}
	}()

	log.Info().Msg("NEXUS Memecoin Hunter - Running")
	log.Info().Msg("Monitoring for new memecoin pools...")

	// 11. Block until shutdown.
	<-ctx.Done()

	// 12. Graceful shutdown.
	log.Info().Msg("Shutting down Hunter...")

	// Force close all open positions.
	sniperEngine.ForceClose(context.Background())

	jupAdapter.Disconnect()
	wg.Wait()

	// Final stats.
	finalStats := sniperEngine.Stats()
	log.Info().
		Int64("total_snipes", finalStats.TotalSnipes).
		Int64("total_sells", finalStats.TotalSells).
		Int64("wins", finalStats.WinCount).
		Int64("losses", finalStats.LossCount).
		Float64("win_rate", finalStats.WinRate).
		Str("daily_spent", finalStats.DailySpentSOL).
		Msg("NEXUS Memecoin Hunter - Final Statistics")

	log.Info().Msg("NEXUS Memecoin Hunter - Shutdown complete")
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
			With().Timestamp().Str("service", "nexus-hunter").
			Str("instance", general.InstanceID).Logger()
	} else {
		log.Logger = zerolog.New(os.Stdout).
			With().Timestamp().Str("service", "nexus-hunter").
			Str("instance", general.InstanceID).Logger()
	}
}
