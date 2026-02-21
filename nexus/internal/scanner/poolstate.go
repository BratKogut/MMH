package scanner

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nexus-trading/nexus/internal/solana"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Pool State Manager — in-memory real-time pool tracking
// Plan v3.1: PoolState map, real-time updates, liquidity tracking
// ---------------------------------------------------------------------------

// PoolState tracks the live state of a monitored pool.
type PoolState struct {
	Pool            solana.PoolInfo `json:"pool"`
	LastUpdate      time.Time       `json:"last_update"`
	InitialLiqUSD   decimal.Decimal `json:"initial_liq_usd"`
	CurrentLiqUSD   decimal.Decimal `json:"current_liq_usd"`
	LiqChangesPct   float64         `json:"liq_changes_pct"` // % change from initial
	PriceAtDetect   decimal.Decimal `json:"price_at_detect"`
	CurrentPrice    decimal.Decimal `json:"current_price"`
	PriceChangePct  float64         `json:"price_change_pct"`
	BuyCount        int             `json:"buy_count"`
	SellCount       int             `json:"sell_count"`
	LargestBuySOL   decimal.Decimal `json:"largest_buy_sol"`
	LargestSellSOL  decimal.Decimal `json:"largest_sell_sol"`
	HolderCount     int             `json:"holder_count"`
	IsActive        bool            `json:"is_active"` // still being monitored
	DetectedAt      time.Time       `json:"detected_at"`
}

// PoolStateConfig configures the pool state manager.
type PoolStateConfig struct {
	MaxTrackedPools    int `yaml:"max_tracked_pools"`    // max pools in memory
	RefreshIntervalMs  int `yaml:"refresh_interval_ms"`  // how often to refresh pool state
	EvictAfterMinutes  int `yaml:"evict_after_minutes"`  // remove pools older than this
	PriceAlertPct      float64 `yaml:"price_alert_pct"`  // alert on price changes > this %
}

// DefaultPoolStateConfig returns production defaults.
func DefaultPoolStateConfig() PoolStateConfig {
	return PoolStateConfig{
		MaxTrackedPools:   1000,
		RefreshIntervalMs: 5000,  // 5s refresh
		EvictAfterMinutes: 120,   // keep 2h of history
		PriceAlertPct:     10.0,  // alert on >10% moves
	}
}

// PoolStateManager manages real-time pool state tracking.
type PoolStateManager struct {
	config PoolStateConfig
	rpc    solana.RPCClient

	mu     sync.RWMutex
	pools  map[solana.Pubkey]*PoolState

	// Callbacks.
	onLiquidityChange func(pool *PoolState, changesPct float64)
	onPriceChange     func(pool *PoolState, changesPct float64)

	// Stats.
	tracked   atomic.Int64
	refreshes atomic.Int64
	evictions atomic.Int64
}

// NewPoolStateManager creates a new pool state manager.
func NewPoolStateManager(config PoolStateConfig, rpc solana.RPCClient) *PoolStateManager {
	return &PoolStateManager{
		config: config,
		rpc:    rpc,
		pools:  make(map[solana.Pubkey]*PoolState, config.MaxTrackedPools),
	}
}

// SetOnLiquidityChange sets callback for significant liquidity changes.
func (m *PoolStateManager) SetOnLiquidityChange(fn func(pool *PoolState, changesPct float64)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onLiquidityChange = fn
}

// SetOnPriceChange sets callback for significant price changes.
func (m *PoolStateManager) SetOnPriceChange(fn func(pool *PoolState, changesPct float64)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onPriceChange = fn
}

// TrackPool starts tracking a new pool.
func (m *PoolStateManager) TrackPool(pool solana.PoolInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.pools[pool.PoolAddress]; exists {
		return // already tracked
	}

	if len(m.pools) >= m.config.MaxTrackedPools {
		m.evictOldest()
	}

	state := &PoolState{
		Pool:          pool,
		LastUpdate:    time.Now(),
		InitialLiqUSD: pool.LiquidityUSD,
		CurrentLiqUSD: pool.LiquidityUSD,
		PriceAtDetect: pool.PriceUSD,
		CurrentPrice:  pool.PriceUSD,
		IsActive:      true,
		DetectedAt:    time.Now(),
	}

	m.pools[pool.PoolAddress] = state
	m.tracked.Add(1)

	log.Debug().
		Str("pool", string(pool.PoolAddress)).
		Str("dex", pool.DEX).
		Str("liquidity", pool.LiquidityUSD.String()).
		Msg("poolstate: tracking new pool")
}

// GetState returns the current state of a tracked pool.
func (m *PoolStateManager) GetState(poolAddress solana.Pubkey) *PoolState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.pools[poolAddress]
}

// GetActiveStates returns all actively tracked pool states.
func (m *PoolStateManager) GetActiveStates() []*PoolState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var active []*PoolState
	for _, state := range m.pools {
		if state.IsActive {
			active = append(active, state)
		}
	}
	return active
}

// RecordTrade records a buy or sell event on a pool.
func (m *PoolStateManager) RecordTrade(poolAddress solana.Pubkey, isBuy bool, amountSOL decimal.Decimal) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state := m.pools[poolAddress]
	if state == nil {
		return
	}

	if isBuy {
		state.BuyCount++
		if amountSOL.GreaterThan(state.LargestBuySOL) {
			state.LargestBuySOL = amountSOL
		}
	} else {
		state.SellCount++
		if amountSOL.GreaterThan(state.LargestSellSOL) {
			state.LargestSellSOL = amountSOL
		}
	}
}

// Run starts the background refresh loop. Blocks until ctx is cancelled.
func (m *PoolStateManager) Run(ctx context.Context) {
	interval := time.Duration(m.config.RefreshIntervalMs) * time.Millisecond
	if interval == 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	evictInterval := time.Duration(m.config.EvictAfterMinutes) * time.Minute / 10
	if evictInterval < time.Minute {
		evictInterval = time.Minute
	}
	evictTicker := time.NewTicker(evictInterval)
	defer evictTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.refreshAll(ctx)
		case <-evictTicker.C:
			m.evictExpired()
		}
	}
}

// refreshAll updates state for all tracked pools.
func (m *PoolStateManager) refreshAll(ctx context.Context) {
	m.mu.RLock()
	var addresses []solana.Pubkey
	for addr, state := range m.pools {
		if state.IsActive {
			addresses = append(addresses, addr)
		}
	}
	m.mu.RUnlock()

	for _, addr := range addresses {
		select {
		case <-ctx.Done():
			return
		default:
		}

		pool, err := m.rpc.GetPoolInfo(ctx, addr)
		if err != nil {
			continue
		}

		m.updateState(addr, pool)
		m.refreshes.Add(1)
	}
}

func (m *PoolStateManager) updateState(addr solana.Pubkey, pool *solana.PoolInfo) {
	m.mu.Lock()
	state := m.pools[addr]
	if state == nil {
		m.mu.Unlock()
		return
	}

	prevLiq := state.CurrentLiqUSD
	prevPrice := state.CurrentPrice

	state.Pool = *pool
	state.CurrentLiqUSD = pool.LiquidityUSD
	state.CurrentPrice = pool.PriceUSD
	state.LastUpdate = time.Now()

	// Calculate changes.
	if state.InitialLiqUSD.IsPositive() {
		liqChange := pool.LiquidityUSD.Sub(state.InitialLiqUSD).
			Div(state.InitialLiqUSD).Mul(decimal.NewFromInt(100))
		state.LiqChangesPct, _ = liqChange.Float64()
	}

	if state.PriceAtDetect.IsPositive() {
		priceChange := pool.PriceUSD.Sub(state.PriceAtDetect).
			Div(state.PriceAtDetect).Mul(decimal.NewFromInt(100))
		state.PriceChangePct, _ = priceChange.Float64()
	}

	liqCb := m.onLiquidityChange
	priceCb := m.onPriceChange
	m.mu.Unlock()

	// Trigger callbacks for significant changes.
	if liqCb != nil && prevLiq.IsPositive() {
		liqDelta := pool.LiquidityUSD.Sub(prevLiq).Div(prevLiq).Mul(decimal.NewFromInt(100))
		deltaVal, _ := liqDelta.Float64()
		if deltaVal > m.config.PriceAlertPct || deltaVal < -m.config.PriceAlertPct {
			liqCb(state, deltaVal)
		}
	}

	if priceCb != nil && prevPrice.IsPositive() {
		priceDelta := pool.PriceUSD.Sub(prevPrice).Div(prevPrice).Mul(decimal.NewFromInt(100))
		deltaVal, _ := priceDelta.Float64()
		if deltaVal > m.config.PriceAlertPct || deltaVal < -m.config.PriceAlertPct {
			priceCb(state, deltaVal)
		}
	}
}

// evictOldest removes the oldest pool from tracking.
func (m *PoolStateManager) evictOldest() {
	var oldestAddr solana.Pubkey
	var oldestTime time.Time

	for addr, state := range m.pools {
		if oldestTime.IsZero() || state.DetectedAt.Before(oldestTime) {
			oldestAddr = addr
			oldestTime = state.DetectedAt
		}
	}

	if oldestAddr != "" {
		delete(m.pools, oldestAddr)
		m.evictions.Add(1)
	}
}

// evictExpired removes pools older than the eviction threshold.
func (m *PoolStateManager) evictExpired() {
	cutoff := time.Now().Add(-time.Duration(m.config.EvictAfterMinutes) * time.Minute)

	m.mu.Lock()
	defer m.mu.Unlock()

	evicted := 0
	for addr, state := range m.pools {
		if state.DetectedAt.Before(cutoff) {
			state.IsActive = false
			delete(m.pools, addr)
			evicted++
		}
	}

	if evicted > 0 {
		m.evictions.Add(int64(evicted))
		log.Debug().Int("evicted", evicted).Msg("poolstate: evicted expired pools")
	}
}

// PoolStateStats returns manager statistics.
type PoolStateStats struct {
	TrackedPools int   `json:"tracked_pools"`
	ActivePools  int   `json:"active_pools"`
	Refreshes    int64 `json:"refreshes"`
	Evictions    int64 `json:"evictions"`
}

func (m *PoolStateManager) Stats() PoolStateStats {
	m.mu.RLock()
	active := 0
	total := len(m.pools)
	for _, s := range m.pools {
		if s.IsActive {
			active++
		}
	}
	m.mu.RUnlock()

	return PoolStateStats{
		TrackedPools: total,
		ActivePools:  active,
		Refreshes:    m.refreshes.Load(),
		Evictions:    m.evictions.Load(),
	}
}
