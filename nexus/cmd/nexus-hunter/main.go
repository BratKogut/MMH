package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nexus-trading/nexus/internal/adapters/jupiter"
	"github.com/nexus-trading/nexus/internal/config"
	"github.com/nexus-trading/nexus/internal/copytrade"
	"github.com/nexus-trading/nexus/internal/correlation"
	"github.com/nexus-trading/nexus/internal/graph"
	"github.com/nexus-trading/nexus/internal/honeypot"
	"github.com/nexus-trading/nexus/internal/liquidity"
	"github.com/nexus-trading/nexus/internal/narrative"
	"github.com/nexus-trading/nexus/internal/scanner"
	"github.com/nexus-trading/nexus/internal/sniper"
	"github.com/nexus-trading/nexus/internal/solana"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// controlState tracks the system's operational state for the Control Plane.
type controlState struct {
	paused   atomic.Bool // soft pause: stop new entries, manage existing
	killed   atomic.Bool // hard kill: close all, halt
}

func main() {
	// 1. Parse flags.
	configPath := flag.String("config", "config/config.yaml", "Path to configuration file")
	stubMode := flag.Bool("stub", false, "Use stub RPC (no real Solana connection)")
	snapshotDir := flag.String("snapshot-dir", "data/snapshots", "Directory for graph snapshots")
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
	log.Info().Msg("NEXUS Memecoin Hunter v3.2 - Starting")
	log.Info().Msg("DETECT -> SANITIZE -> ANALYZE -> SNIPE -> PROFIT")
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

	// 3b. Validate configuration.
	if err := cfg.Validate(); err != nil {
		log.Fatal().Err(err).Msg("Configuration validation failed")
	}

	// 4. Create Solana RPC client.
	var rpc solana.RPCClient
	var liveRPC *solana.LiveRPCClient
	if *stubMode {
		rpc = solana.NewStubRPCClient()
		log.Info().Msg("Solana RPC: STUB mode")
	} else {
		rpcConfig := solana.RPCConfig{
			Endpoint:     cfg.Solana.RPCEndpoint,
			WSEndpoint:   cfg.Solana.WSEndpoint,
			Timeout:      10 * time.Second,
			MaxRetries:   3,
			RateLimitRPS: cfg.Solana.RateLimitRPS,
			PrivateKey:   cfg.Solana.PrivateKey,
		}
		liveRPC = solana.NewLiveRPCClient(rpcConfig)
		rpc = liveRPC
		defer liveRPC.Close()

		// Verify RPC connectivity.
		healthCtx, healthCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := rpc.Health(healthCtx); err != nil {
			log.Warn().Err(err).Str("endpoint", cfg.Solana.RPCEndpoint).
				Msg("Solana RPC health check failed (continuing, may be rate-limited)")
		} else {
			log.Info().Str("endpoint", cfg.Solana.RPCEndpoint).Msg("Solana RPC: LIVE - connected")
		}
		healthCancel()
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
	walletPub := cfg.Solana.WalletPubkey
	if walletPub == "" && dryRun {
		walletPub = "DRY-RUN-WALLET"
	}
	wallet := solana.Pubkey(walletPub)
	jupAdapter := jupiter.New(jupConfig, rpc, wallet)

	if err := jupAdapter.Connect(context.Background()); err != nil {
		log.Warn().Err(err).Msg("Jupiter adapter connection failed (continuing in degraded mode)")
	}

	// 6a. Create Entity Graph Engine + load snapshot.
	graphConfig := graph.DefaultConfig()
	entityGraph := graph.NewEngine(graphConfig)

	snapshotPath := filepath.Join(*snapshotDir, "entity_graph.gob")
	if err := entityGraph.LoadSnapshot(snapshotPath); err != nil {
		log.Warn().Err(err).Msg("Failed to load graph snapshot, starting fresh")
	}
	log.Info().
		Int("max_nodes", graphConfig.MaxNodes).
		Int("label_depth", graphConfig.LabelPropDepth).
		Int("cex_wallets", graph.CEXWalletCount()).
		Msg("Entity Graph Engine initialized")

	// 6b. Create Pool State Manager.
	poolState := scanner.NewPoolStateManager(scanner.DefaultPoolStateConfig(), nil)

	// 6c. Create L0 Sanitizer.
	sanitizerConfig := scanner.DefaultSanitizerConfig()
	sanitizerConfig.MinLiquidityUSD = cfg.Hunter.MinLiquidityUSD
	sanitizerConfig.MinQuoteReserveSOL = 1.0
	sanitizer := scanner.NewSanitizer(sanitizerConfig, entityGraph, poolState)
	log.Info().Msg("L0 Sanitizer initialized")

	// 6d. Create Token Analyzer.
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

	// 6e. Create Sell Simulator.
	sellSimulator := scanner.NewSellSimulator(rpc)
	log.Info().Msg("Sell Simulator initialized")

	// 6f. Create 5D Scorer.
	scoringConfig := scanner.DefaultScoringConfig()
	scorer := scanner.NewScorer(scoringConfig)
	log.Info().
		Float64("safety_w", scoringConfig.Weights.Safety).
		Float64("entity_w", scoringConfig.Weights.Entity).
		Float64("social_w", scoringConfig.Weights.Social).
		Float64("onchain_w", scoringConfig.Weights.OnChain).
		Float64("timing_w", scoringConfig.Weights.Timing).
		Msg("5D Scorer initialized")

	// 6g. Create v3.2 Advanced Analysis Modules.
	flowAnalyzer := liquidity.NewFlowAnalyzer(liquidity.DefaultFlowAnalyzerConfig())
	log.Info().Msg("v3.2 Liquidity Flow Analyzer initialized")

	narrativeEngine := narrative.NewEngine(narrative.DefaultEngineConfig(), narrative.DefaultNarratives())
	log.Info().Msg("v3.2 Narrative Momentum Engine initialized")

	honeypotTracker := honeypot.NewTracker(honeypot.DefaultEvolutionConfig())
	log.Info().Msg("v3.2 Honeypot Evolution Tracker initialized")

	correlationDetector := correlation.NewDetector(correlation.DefaultDetectorConfig())
	log.Info().Msg("v3.2 Cross-Token Correlation Detector initialized")

	copytradeTracker := copytrade.NewTracker(copytrade.DefaultIntelConfig())
	log.Info().Msg("v3.2 Copy-Trade Intelligence Tracker initialized")

	adaptiveWeights := scanner.NewAdaptiveWeightEngine(
		scanner.DefaultAdaptiveWeightConfig(), scoringConfig.Weights)
	log.Info().Msg("v3.2 Adaptive Scoring Weights Engine initialized")

	// 6h. Create Dynamic Priority Fee Estimator (non-stub only).
	var feeEstimator *solana.PriorityFeeEstimator
	if liveRPC != nil {
		feeEstimator = solana.NewPriorityFeeEstimator(liveRPC)
		log.Info().Msg("Dynamic Priority Fee Estimator initialized")
	}

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

		// Feed outcome to adaptive weight engine.
		adaptiveWeights.RecordOutcome(scanner.TradeOutcome{
			PnLPct: pos.PnLPct,
			IsWin:  pos.PnLPct > 0,
			IsRug:  pos.CloseReason == "RUG" || pos.CloseReason == "PANIC_SELL",
		})
	})

	// 8. Control state.
	ctrl := &controlState{}

	// 8b. Create Token Scanner with full pipeline.
	scannerConfig := scanner.ScannerConfig{
		MinLiquidityUSD:    cfg.Hunter.MinLiquidityUSD,
		MaxTokenAgeMinutes: cfg.Hunter.MaxTokenAgeMinutes,
		MonitorDEXes:       cfg.Hunter.MonitorDEXes,
		PollIntervalMs:     2000,
		MaxTrackedPools:    500,
		MinQuoteReserveSOL: 1.0,
	}

	// Pipeline: Scanner -> L0 Sanitizer -> Analyzer -> SellSim -> EntityGraph -> 5D Scorer -> Sniper
	tokenScanner := scanner.NewScanner(scannerConfig, rpc, func(ctx context.Context, discovery scanner.PoolDiscovery) {
		// Check control state.
		if ctrl.killed.Load() || ctrl.paused.Load() {
			return
		}

		log.Info().
			Str("pool", string(discovery.Pool.PoolAddress)).
			Str("dex", discovery.Pool.DEX).
			Str("liquidity", discovery.Pool.LiquidityUSD.String()).
			Int64("latency_ms", discovery.LatencyMs).
			Msg("[NEW POOL] Processing through pipeline...")

		// ── Stage 0: L0 Sanitizer (<10ms, ZERO external calls) ──
		sanitizerResult := sanitizer.Check(discovery)
		if !sanitizerResult.Passed {
			log.Debug().
				Str("pool", string(discovery.Pool.PoolAddress)).
				Str("reason", sanitizerResult.Reason).
				Str("filter", sanitizerResult.Filter).
				Int64("latency_us", sanitizerResult.LatencyUs).
				Msg("[L0 DROP]")
			return
		}
		log.Debug().
			Str("pool", string(discovery.Pool.PoolAddress)).
			Int64("latency_us", sanitizerResult.LatencyUs).
			Msg("[L0 PASS]")

		// ── Stage 1: Safety analysis ──
		analysis := analyzer.Analyze(ctx, discovery)

		log.Info().
			Str("mint", string(analysis.Mint)).
			Int("safety_score", analysis.SafetyScore).
			Str("verdict", string(analysis.Verdict)).
			Msg("[ANALYSIS] Safety result")

		// ── Stage 2: Pre-buy sell simulation ──
		preBuySim := sellSimulator.SimulatePreBuy(ctx,
			decimal.NewFromFloat(cfg.Hunter.MaxBuySOL), discovery.Pool)

		log.Info().
			Str("mint", string(analysis.Mint)).
			Bool("can_sell", preBuySim.CanSell).
			Float64("tax_pct", preBuySim.EstimatedTaxPct).
			Msg("[SELLSIM] Pre-buy result")

		// ── Stage 3: Entity graph query ──
		var entityReport *graph.EntityReport
		if discovery.Pool.TokenMint != "" {
			report := entityGraph.QueryDeployer(string(discovery.Pool.TokenMint))
			entityReport = &report
			log.Debug().
				Str("mint", string(analysis.Mint)).
				Float64("risk", report.RiskScore).
				Int("hops_rugger", report.HopsToRugger).
				Int("cluster_size", report.ClusterSize).
				Msg("[ENTITY] Deployer report")
		}

		// ── Stage 3b: v3.2 Advanced Analysis ──
		// Liquidity flow.
		flowAnalyzer.RecordLiquidityChange(string(analysis.Mint),
			func() float64 { f, _ := discovery.Pool.LiquidityUSD.Float64(); return f }(),
			true, liquidity.QualityMedium)
		flowResult := flowAnalyzer.Analyze(string(analysis.Mint))

		// Narrative classification.
		tokenName := ""
		if analysis.Token != nil {
			tokenName = analysis.Token.Name
		}
		narrativeEngine.RecordToken(string(analysis.Mint), tokenName, string(analysis.Mint), 0)
		narrativeState := narrativeEngine.GetTokenNarrative(tokenName, string(analysis.Mint))

		// Cross-token correlation (use entity graph cluster ID if available).
		var crossTokenState *correlation.CrossTokenState
		if entityReport != nil && entityReport.SeedFunder != "" {
			// Use hash of seed funder as cluster ID.
			clusterID := uint32(0)
			for _, b := range entityReport.SeedFunder {
				clusterID = clusterID*31 + uint32(b)
			}
			mcap, _ := discovery.Pool.LiquidityUSD.Mul(decimal.NewFromInt(2)).Float64()
			correlationDetector.RecordTokenLaunch(clusterID, string(analysis.Mint), mcap)
			crossTokenState = correlationDetector.GetClusterState(clusterID)
		}

		// Honeypot evolution check.
		var honeypotMatch *honeypot.Signature
		contractData := []byte(discovery.Pool.PoolAddress)
		if sig := honeypotTracker.CheckContract(contractData); sig != nil {
			honeypotMatch = sig
		}

		// Copy-trade signal lookup.
		copySignal := copytradeTracker.GetSignal(string(analysis.Mint))

		log.Debug().
			Str("mint", string(analysis.Mint)).
			Str("flow_pattern", string(flowResult.Pattern)).
			Bool("rug_precursor", flowResult.IsRugPrecursor()).
			Bool("copy_signal", copySignal != nil).
			Msg("[v3.2] Advanced analysis")

		// ── Stage 4: 5-Dimensional scoring ──
		scoringInput := scanner.ScoringInput{
			Analysis:        analysis,
			EntityReport:    entityReport,
			SellSim:         &preBuySim,
			LiquidityFlow:   &flowResult,
			NarrativeState:  narrativeState,
			CrossTokenState: crossTokenState,
			HoneypotMatch:   honeypotMatch,
			CopyTradeSignal: copySignal,
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

		// ── Stage 5: Decision ──
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

		// Re-check control state before execution.
		if ctrl.killed.Load() || ctrl.paused.Load() {
			log.Info().Str("mint", string(analysis.Mint)).Msg("[SKIP] System paused/killed")
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

	// Start graph snapshot loop.
	graphStopCh := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		entityGraph.SnapshotLoop(snapshotPath, 5*time.Minute, graphStopCh)
	}()

	// Start priority fee estimator (non-stub only).
	if feeEstimator != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			feeEstimator.Start(ctx)
		}()
	}

	// Start scanner.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := tokenScanner.Start(ctx); err != nil {
			log.Error().Err(err).Msg("Scanner error")
		}
	}()

	// Start HTTP health/stats/control endpoint.
	wg.Add(1)
	go func() {
		defer wg.Done()
		mux := http.NewServeMux()

		// ── Health ──
		mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"status":  "ok",
				"dry_run": dryRun,
				"paused":  ctrl.paused.Load(),
				"killed":  ctrl.killed.Load(),
			})
		})

		// ── Stats ──
		mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
			combined := map[string]any{
				"sniper":           sniperEngine.Stats(),
				"scanner":          tokenScanner.Stats(),
				"jupiter":          jupAdapter.Stats(),
				"graph":            entityGraph.Stats(),
				"sanitizer":        sanitizer.Stats(),
				"liquidity":        flowAnalyzer.Stats(),
				"narrative":        narrativeEngine.Stats(),
				"honeypot":         honeypotTracker.Stats(),
				"correlation":      correlationDetector.Stats(),
				"copytrade":        copytradeTracker.Stats(),
				"adaptive_weights": adaptiveWeights.Stats(),
				"dry_run":          dryRun,
				"paused":           ctrl.paused.Load(),
				"killed":           ctrl.killed.Load(),
			}
			if feeEstimator != nil {
				combined["priority_fees"] = feeEstimator.Stats()
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(combined)
		})

		// ── Positions ──
		mux.HandleFunc("/positions", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(sniperEngine.Positions())
		})
		mux.HandleFunc("/positions/open", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(sniperEngine.OpenPositions())
		})

		// ── Control Plane ──
		mux.HandleFunc("/control/pause", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			ctrl.paused.Store(true)
			log.Warn().Msg("[CONTROL] System PAUSED - no new entries")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"status":"paused"}`)
		})

		mux.HandleFunc("/control/resume", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			ctrl.paused.Store(false)
			ctrl.killed.Store(false)
			log.Info().Msg("[CONTROL] System RESUMED")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"status":"running"}`)
		})

		mux.HandleFunc("/control/kill", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			ctrl.killed.Store(true)
			ctrl.paused.Store(true)
			log.Error().Msg("[CONTROL] KILL SWITCH - closing all positions")

			// Force close all positions.
			go func() {
				killCtx, killCancel := context.WithTimeout(context.Background(), 30*time.Second)
				sniperEngine.ForceClose(killCtx)
				killCancel()
			}()

			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"status":"killed","action":"force_close_all"}`)
		})

		mux.HandleFunc("/control/status", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"paused":         ctrl.paused.Load(),
				"killed":         ctrl.killed.Load(),
				"dry_run":        dryRun,
				"open_positions": len(sniperEngine.OpenPositions()),
				"instance_id":    cfg.General.InstanceID,
				"graph_snapshot": graph.GetSnapshotInfo(snapshotPath),
			})
		})

		port := cfg.Metrics.PrometheusPort + 2 // hunter on port +2
		addr := fmt.Sprintf(":%d", port)
		server := &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}

		log.Info().Str("addr", addr).Msg("Hunter HTTP server started (health + stats + control)")

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
				sanStats := sanitizer.Stats()
				logEvt := log.Info().
					Int64("pools_scanned", sc.PoolsScanned).
					Int64("pools_accepted", sc.PoolsAccepted).
					Int64("l0_checked", sanStats.TotalChecked).
					Int64("l0_passed", sanStats.TotalPassed).
					Float64("l0_pass_rate", sanStats.PassRate).
					Int64("snipes", ss.TotalSnipes).
					Int("open_pos", ss.OpenPositions).
					Int64("wins", ss.WinCount).
					Int64("losses", ss.LossCount).
					Float64("win_rate", ss.WinRate).
					Str("daily_spent", ss.DailySpentSOL).
					Bool("paused", ctrl.paused.Load())
				if feeEstimator != nil {
					feeStats := feeEstimator.Stats()
					logEvt = logEvt.Uint64("fee_p75", feeStats.P75Lamports)
				}
				logEvt.Msg("[STATS]")
			}
		}
	}()

	// Periodic sanitizer cleanup.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sanitizer.CleanupDeployerHistory()
			}
		}
	}()

	// Periodic v3.2 module maintenance.
	wg.Add(1)
	go func() {
		defer wg.Done()
		narrativeTicker := time.NewTicker(5 * time.Minute)
		cleanupTicker := time.NewTicker(15 * time.Minute)
		adaptiveTicker := time.NewTicker(30 * time.Minute)
		defer narrativeTicker.Stop()
		defer cleanupTicker.Stop()
		defer adaptiveTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-narrativeTicker.C:
				narrativeEngine.Refresh()
			case <-cleanupTicker.C:
				flowAnalyzer.Cleanup(1 * time.Hour)
				correlationDetector.Cleanup(6 * time.Hour)
				copytradeTracker.Cleanup(30 * time.Minute)
			case <-adaptiveTicker.C:
				adaptiveWeights.Recalculate()
				scorer.SetWeights(adaptiveWeights.GetWeights())
				log.Debug().Msg("[v3.2] Adaptive weights recalculated and applied")
			}
		}
	}()

	log.Info().Msg("NEXUS Memecoin Hunter v3.2 - Running")
	log.Info().Msg("Pipeline: Pool -> L0 Sanitizer -> Analyzer -> SellSim -> Entity Graph -> v3.2 Analysis -> 5D Scorer -> Sniper")
	log.Info().Msg("v3.2 Modules: Liquidity Flow + Narrative Momentum + Honeypot Evolution + Cross-Token Correlation + Copy-Trade Intel + Adaptive Weights")
	log.Info().Msg("Monitoring for new memecoin pools...")

	// 11. Block until shutdown.
	<-ctx.Done()

	// 12. Graceful shutdown.
	log.Info().Msg("Shutting down Hunter...")

	// Stop graph snapshot loop (triggers final save).
	close(graphStopCh)

	// Stop fee estimator.
	if feeEstimator != nil {
		feeEstimator.Stop()
	}

	// Force close all open positions with timeout.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	sniperEngine.ForceClose(shutdownCtx)
	shutdownCancel()

	jupAdapter.Disconnect()
	wg.Wait()

	// Final stats.
	finalStats := sniperEngine.Stats()
	sanStats := sanitizer.Stats()
	log.Info().
		Int64("total_snipes", finalStats.TotalSnipes).
		Int64("total_sells", finalStats.TotalSells).
		Int64("wins", finalStats.WinCount).
		Int64("losses", finalStats.LossCount).
		Float64("win_rate", finalStats.WinRate).
		Str("daily_spent", finalStats.DailySpentSOL).
		Int64("l0_checked", sanStats.TotalChecked).
		Int64("l0_passed", sanStats.TotalPassed).
		Float64("l0_pass_rate", sanStats.PassRate).
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
