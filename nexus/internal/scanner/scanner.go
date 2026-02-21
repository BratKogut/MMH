package scanner

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nexus-trading/nexus/internal/solana"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Token Scanner — detects new memecoin pools and emits discovery events
// ---------------------------------------------------------------------------

// ScannerConfig configures the token scanner.
type ScannerConfig struct {
	// Minimum liquidity in USD to consider a pool.
	MinLiquidityUSD float64 `yaml:"min_liquidity_usd"`

	// Maximum token age (from pool creation) to consider.
	MaxTokenAgeMinutes int `yaml:"max_token_age_minutes"`

	// DEXes to monitor.
	MonitorDEXes []string `yaml:"monitor_dexes"` // raydium|pumpfun|orca|meteora

	// Poll interval for non-websocket fallback.
	PollIntervalMs int `yaml:"poll_interval_ms"`

	// Maximum pools to track concurrently.
	MaxTrackedPools int `yaml:"max_tracked_pools"`

	// Minimum SOL in quote reserve to avoid dust pools.
	MinQuoteReserveSOL float64 `yaml:"min_quote_reserve_sol"`
}

// DefaultScannerConfig returns sensible defaults for memecoin hunting.
func DefaultScannerConfig() ScannerConfig {
	return ScannerConfig{
		MinLiquidityUSD:    500,   // At least $500 liquidity
		MaxTokenAgeMinutes: 30,    // Only tokens < 30 min old
		MonitorDEXes:       []string{"raydium", "pumpfun"},
		PollIntervalMs:     2000,  // 2s poll fallback
		MaxTrackedPools:    500,
		MinQuoteReserveSOL: 1.0,   // At least 1 SOL in pool
	}
}

// PoolDiscovery is emitted when a new pool is detected that passes initial filters.
type PoolDiscovery struct {
	Pool        solana.PoolInfo  `json:"pool"`
	Token       *solana.TokenInfo `json:"token,omitempty"`
	DiscoveredAt time.Time       `json:"discovered_at"`
	LatencyMs   int64            `json:"latency_ms"` // time from pool creation to discovery
	Source      string           `json:"source"`      // websocket|poll
}

// OnDiscovery is the callback type for new pool discoveries.
type OnDiscovery func(ctx context.Context, discovery PoolDiscovery)

// Scanner monitors Solana DEXes for new memecoin pools.
type Scanner struct {
	config  ScannerConfig
	rpc     solana.RPCClient
	onDisc  OnDiscovery

	mu          sync.RWMutex
	seenPools   map[solana.Pubkey]time.Time // pool address -> first seen
	running     atomic.Bool

	// Stats.
	poolsScanned   atomic.Int64
	poolsAccepted  atomic.Int64
	poolsRejected  atomic.Int64
}

// NewScanner creates a new token scanner.
func NewScanner(config ScannerConfig, rpc solana.RPCClient, onDiscovery OnDiscovery) *Scanner {
	return &Scanner{
		config:    config,
		rpc:       rpc,
		onDisc:    onDiscovery,
		seenPools: make(map[solana.Pubkey]time.Time),
	}
}

// Start begins monitoring for new pools. Blocks until ctx is cancelled.
func (s *Scanner) Start(ctx context.Context) error {
	if s.running.Load() {
		return fmt.Errorf("scanner already running")
	}
	s.running.Store(true)
	defer s.running.Store(false)

	log.Info().
		Strs("dexes", s.config.MonitorDEXes).
		Float64("min_liquidity_usd", s.config.MinLiquidityUSD).
		Int("max_age_min", s.config.MaxTokenAgeMinutes).
		Msg("scanner: starting pool monitor")

	var wg sync.WaitGroup

	// Try websocket subscription for each DEX.
	for _, dex := range s.config.MonitorDEXes {
		dex := dex
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.monitorDEX(ctx, dex)
		}()
	}

	// Periodic cleanup of old tracked pools.
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.cleanupLoop(ctx)
	}()

	wg.Wait()
	log.Info().Msg("scanner: stopped")
	return nil
}

// monitorDEX monitors a single DEX for new pools.
func (s *Scanner) monitorDEX(ctx context.Context, dex string) {
	// Try websocket first.
	poolCh, err := s.rpc.SubscribeNewPools(ctx, dex)
	if err != nil {
		log.Warn().Err(err).Str("dex", dex).Msg("scanner: websocket unavailable, falling back to polling")
		s.pollDEX(ctx, dex)
		return
	}

	log.Info().Str("dex", dex).Msg("scanner: websocket subscription active")

	for {
		select {
		case <-ctx.Done():
			return
		case pool, ok := <-poolCh:
			if !ok {
				log.Warn().Str("dex", dex).Msg("scanner: websocket channel closed, switching to poll")
				s.pollDEX(ctx, dex)
				return
			}
			s.handlePool(ctx, pool, "websocket")
		}
	}
}

// pollDEX polls a DEX for new pools at regular intervals.
func (s *Scanner) pollDEX(ctx context.Context, dex string) {
	interval := time.Duration(s.config.PollIntervalMs) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pools, err := s.rpc.GetRecentPools(ctx, dex, s.config.MaxTokenAgeMinutes)
			if err != nil {
				log.Error().Err(err).Str("dex", dex).Msg("scanner: poll failed")
				continue
			}
			for _, pool := range pools {
				s.handlePool(ctx, pool, "poll")
			}
		}
	}
}

// handlePool processes a discovered pool through filters.
func (s *Scanner) handlePool(ctx context.Context, pool solana.PoolInfo, source string) {
	s.poolsScanned.Add(1)

	// Dedup: skip if already seen.
	s.mu.RLock()
	_, seen := s.seenPools[pool.PoolAddress]
	s.mu.RUnlock()
	if seen {
		return
	}

	// Mark as seen.
	s.mu.Lock()
	if len(s.seenPools) >= s.config.MaxTrackedPools {
		// Evict oldest entry.
		var oldestKey solana.Pubkey
		var oldestTime time.Time
		for k, v := range s.seenPools {
			if oldestTime.IsZero() || v.Before(oldestTime) {
				oldestKey = k
				oldestTime = v
			}
		}
		delete(s.seenPools, oldestKey)
	}
	s.seenPools[pool.PoolAddress] = time.Now()
	s.mu.Unlock()

	// Filter 1: Minimum liquidity.
	if pool.LiquidityUSD.LessThan(decimal.NewFromFloat(s.config.MinLiquidityUSD)) {
		s.poolsRejected.Add(1)
		log.Debug().
			Str("pool", string(pool.PoolAddress)).
			Str("liquidity", pool.LiquidityUSD.String()).
			Msg("scanner: rejected - low liquidity")
		return
	}

	// Filter 2: Minimum quote reserve.
	minQuote := decimal.NewFromFloat(s.config.MinQuoteReserveSOL)
	if pool.QuoteMint == solana.SOLMint && pool.QuoteReserve.LessThan(minQuote) {
		s.poolsRejected.Add(1)
		log.Debug().
			Str("pool", string(pool.PoolAddress)).
			Str("quote_reserve", pool.QuoteReserve.String()).
			Msg("scanner: rejected - low SOL reserve")
		return
	}

	// Filter 3: Token age.
	age := time.Since(pool.CreatedAt)
	maxAge := time.Duration(s.config.MaxTokenAgeMinutes) * time.Minute
	if age > maxAge {
		s.poolsRejected.Add(1)
		return
	}

	// Try to fetch token info for enrichment.
	var tokenInfo *solana.TokenInfo
	if info, err := s.rpc.GetTokenInfo(ctx, pool.TokenMint); err == nil {
		tokenInfo = info
	}

	latencyMs := time.Since(pool.CreatedAt).Milliseconds()

	discovery := PoolDiscovery{
		Pool:         pool,
		Token:        tokenInfo,
		DiscoveredAt: time.Now(),
		LatencyMs:    latencyMs,
		Source:       source,
	}

	s.poolsAccepted.Add(1)

	log.Info().
		Str("pool", string(pool.PoolAddress)).
		Str("dex", pool.DEX).
		Str("token", string(pool.TokenMint)).
		Str("liquidity", pool.LiquidityUSD.String()).
		Int64("latency_ms", latencyMs).
		Str("source", source).
		Msg("scanner: NEW POOL DISCOVERED")

	if s.onDisc != nil {
		s.onDisc(ctx, discovery)
	}
}

// cleanupLoop removes old tracked pools periodically.
func (s *Scanner) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			maxAge := time.Duration(s.config.MaxTokenAgeMinutes*2) * time.Minute
			cutoff := time.Now().Add(-maxAge)

			s.mu.Lock()
			for k, v := range s.seenPools {
				if v.Before(cutoff) {
					delete(s.seenPools, k)
				}
			}
			s.mu.Unlock()
		}
	}
}

// Stats returns scanner statistics.
type ScannerStats struct {
	PoolsScanned  int64 `json:"pools_scanned"`
	PoolsAccepted int64 `json:"pools_accepted"`
	PoolsRejected int64 `json:"pools_rejected"`
	TrackedPools  int   `json:"tracked_pools"`
}

func (s *Scanner) Stats() ScannerStats {
	s.mu.RLock()
	tracked := len(s.seenPools)
	s.mu.RUnlock()

	return ScannerStats{
		PoolsScanned:  s.poolsScanned.Load(),
		PoolsAccepted: s.poolsAccepted.Load(),
		PoolsRejected: s.poolsRejected.Load(),
		TrackedPools:  tracked,
	}
}
