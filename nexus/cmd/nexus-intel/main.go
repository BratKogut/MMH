package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMicro
	log.Logger = zerolog.New(os.Stdout).With().
		Timestamp().
		Str("service", "nexus-intel").
		Logger()

	log.Info().Msg("NEXUS Intel Pipeline - Starting")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		log.Warn().Str("signal", sig.String()).Msg("Shutdown signal received")
		cancel()
	}()

	// TODO: Initialize Intel Pipeline
	// 1. Query Brain (orchestrator)
	// 2. Worker pool connection (gRPC to Python workers)
	// 3. Kafka consumer/producer
	// 4. Budget manager
	// 5. Circuit breakers

	log.Info().Msg("Intel Pipeline initialized (stub)")

	<-ctx.Done()
	log.Info().Msg("NEXUS Intel Pipeline - Shutdown complete")
}
