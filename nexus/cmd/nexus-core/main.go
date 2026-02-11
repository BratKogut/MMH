package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/nexus-trading/nexus/internal/config"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	// Setup structured logging
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMicro
	log.Logger = zerolog.New(os.Stdout).With().
		Timestamp().
		Str("service", "nexus-core").
		Logger()

	log.Info().Msg("========================================")
	log.Info().Msg("NEXUS Trading Platform - Starting")
	log.Info().Msg("SAFETY > PROFIT > SPEED")
	log.Info().Msg("========================================")

	// Load configuration
	cfg, err := config.Load("config/config.yaml")
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to load configuration")
	}

	log.Info().
		Str("instance_id", cfg.General.InstanceID).
		Str("environment", cfg.General.Environment).
		Bool("dry_run", cfg.General.DryRun).
		Strs("exchanges", cfg.Exchanges.Enabled).
		Msg("Configuration loaded")

	// Setup context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		log.Warn().Str("signal", sig.String()).Msg("Shutdown signal received")
		cancel()
	}()

	// TODO: Initialize and start components in order:
	// 1. Kafka bus (RedPanda)
	// 2. ClickHouse connection
	// 3. Exchange adapters
	// 4. Feature Engine
	// 5. Regime Detector
	// 6. Strategy Runtime
	// 7. Risk Engine
	// 8. Execution Engine
	// 9. Audit trail
	// 10. Metrics server

	log.Info().Msg("All components initialized (stub)")

	// Wait for shutdown
	<-ctx.Done()
	log.Info().Msg("NEXUS Trading Platform - Shutdown complete")
	fmt.Println("Goodbye.")
}
