package solana

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// ---------------------------------------------------------------------------
// Dynamic Priority Fees — p75 from recent slots
// Architecture v3.1: normal=p75, high demand=2x p75, ceiling=0.05 SOL
// ---------------------------------------------------------------------------

const (
	// MaxPriorityFeeLamports is the hard ceiling (0.05 SOL = 50,000,000 lamports).
	MaxPriorityFeeLamports = 50_000_000

	// DefaultPriorityFeeLamports is the fallback when no data is available.
	DefaultPriorityFeeLamports = 10_000 // 0.00001 SOL

	// FeeRefreshInterval is how often we refresh priority fee estimates.
	FeeRefreshInterval = 15 * time.Second
)

// CongestionLevel describes current network congestion.
type CongestionLevel int

const (
	CongestionNormal CongestionLevel = iota
	CongestionHigh                   // "perfect storm" - competitive bidding
)

// PriorityFeeEstimator dynamically estimates priority fees from recent slots.
type PriorityFeeEstimator struct {
	rpc *LiveRPCClient

	mu        sync.RWMutex
	feeP50    uint64
	feeP75    uint64
	feeP90    uint64
	lastFetch time.Time
	samples   int

	stopCh chan struct{}
}

// NewPriorityFeeEstimator creates a new estimator that polls recent fees.
func NewPriorityFeeEstimator(rpc *LiveRPCClient) *PriorityFeeEstimator {
	return &PriorityFeeEstimator{
		rpc:    rpc,
		stopCh: make(chan struct{}),
	}
}

// Start begins periodic fee estimation. Call Stop() to terminate.
func (e *PriorityFeeEstimator) Start(ctx context.Context) {
	// Initial fetch.
	e.refresh(ctx)

	ticker := time.NewTicker(FeeRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.refresh(ctx)
		}
	}
}

// Stop terminates the estimator.
func (e *PriorityFeeEstimator) Stop() {
	select {
	case <-e.stopCh:
	default:
		close(e.stopCh)
	}
}

// EstimateFee returns the recommended priority fee in lamports.
func (e *PriorityFeeEstimator) EstimateFee(congestion CongestionLevel) uint64 {
	e.mu.RLock()
	p75 := e.feeP75
	e.mu.RUnlock()

	if p75 == 0 {
		return DefaultPriorityFeeLamports
	}

	var fee uint64
	switch congestion {
	case CongestionHigh:
		fee = p75 * 2 // 2x p75 for competitive scenarios
	default:
		fee = p75
	}

	// Hard ceiling.
	if fee > MaxPriorityFeeLamports {
		fee = MaxPriorityFeeLamports
	}

	return fee
}

// FeeStats returns current fee estimation stats.
type FeeStats struct {
	P50Lamports uint64    `json:"p50_lamports"`
	P75Lamports uint64    `json:"p75_lamports"`
	P90Lamports uint64    `json:"p90_lamports"`
	Samples     int       `json:"samples"`
	LastFetch   time.Time `json:"last_fetch"`
}

func (e *PriorityFeeEstimator) Stats() FeeStats {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return FeeStats{
		P50Lamports: e.feeP50,
		P75Lamports: e.feeP75,
		P90Lamports: e.feeP90,
		Samples:     e.samples,
		LastFetch:   e.lastFetch,
	}
}

// refresh calls getRecentPrioritizationFees and computes percentiles.
func (e *PriorityFeeEstimator) refresh(ctx context.Context) {
	fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	result, err := e.rpc.call(fetchCtx, "getRecentPrioritizationFees", nil)
	if err != nil {
		log.Debug().Err(err).Msg("priority_fees: failed to fetch recent fees")
		return
	}

	var fees []struct {
		Slot              uint64 `json:"slot"`
		PrioritizationFee uint64 `json:"prioritizationFee"`
	}
	if err := json.Unmarshal(result, &fees); err != nil {
		log.Debug().Err(err).Msg("priority_fees: failed to parse fees")
		return
	}

	if len(fees) == 0 {
		return
	}

	// Extract non-zero fee values.
	values := make([]uint64, 0, len(fees))
	for _, f := range fees {
		if f.PrioritizationFee > 0 {
			values = append(values, f.PrioritizationFee)
		}
	}

	if len(values) == 0 {
		return
	}

	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })

	e.mu.Lock()
	e.feeP50 = percentile(values, 50)
	e.feeP75 = percentile(values, 75)
	e.feeP90 = percentile(values, 90)
	e.samples = len(values)
	e.lastFetch = time.Now()
	e.mu.Unlock()

	log.Debug().
		Uint64("p50", e.feeP50).
		Uint64("p75", e.feeP75).
		Uint64("p90", e.feeP90).
		Int("samples", len(values)).
		Msg("priority_fees: updated estimates")
}

// percentile computes the p-th percentile of sorted values.
func percentile(sorted []uint64, p int) uint64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := len(sorted) * p / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// GetRecentPriorityFee is a convenience method on LiveRPCClient.
func (c *LiveRPCClient) GetRecentPriorityFee(ctx context.Context) (uint64, error) {
	result, err := c.call(ctx, "getRecentPrioritizationFees", nil)
	if err != nil {
		return DefaultPriorityFeeLamports, fmt.Errorf("rpc: getRecentPrioritizationFees: %w", err)
	}

	var fees []struct {
		Slot              uint64 `json:"slot"`
		PrioritizationFee uint64 `json:"prioritizationFee"`
	}
	if err := json.Unmarshal(result, &fees); err != nil {
		return DefaultPriorityFeeLamports, err
	}

	if len(fees) == 0 {
		return DefaultPriorityFeeLamports, nil
	}

	// Quick p75 without full sort: collect non-zero, sort, return p75.
	values := make([]uint64, 0, len(fees))
	for _, f := range fees {
		if f.PrioritizationFee > 0 {
			values = append(values, f.PrioritizationFee)
		}
	}
	if len(values) == 0 {
		return DefaultPriorityFeeLamports, nil
	}

	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return percentile(values, 75), nil
}
