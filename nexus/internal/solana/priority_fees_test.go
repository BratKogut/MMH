package solana

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPercentile(t *testing.T) {
	values := []uint64{100, 200, 300, 400, 500, 600, 700, 800, 900, 1000}

	assert.Equal(t, uint64(600), percentile(values, 50))
	assert.Equal(t, uint64(800), percentile(values, 75))
	assert.Equal(t, uint64(1000), percentile(values, 90))
	assert.Equal(t, uint64(0), percentile(nil, 50))
	assert.Equal(t, uint64(100), percentile([]uint64{100}, 50))
}

func TestPriorityFeeEstimator_EstimateFee(t *testing.T) {
	// Create estimator without RPC (we'll set values directly).
	e := &PriorityFeeEstimator{
		stopCh: make(chan struct{}),
	}

	// No data: should return default.
	fee := e.EstimateFee(CongestionNormal)
	assert.Equal(t, uint64(DefaultPriorityFeeLamports), fee)

	// Set known values.
	e.mu.Lock()
	e.feeP75 = 50000
	e.mu.Unlock()

	// Normal congestion: p75.
	fee = e.EstimateFee(CongestionNormal)
	assert.Equal(t, uint64(50000), fee)

	// High congestion: 2x p75.
	fee = e.EstimateFee(CongestionHigh)
	assert.Equal(t, uint64(100000), fee)

	// Test ceiling enforcement.
	e.mu.Lock()
	e.feeP75 = MaxPriorityFeeLamports // very high
	e.mu.Unlock()

	fee = e.EstimateFee(CongestionHigh) // 2x would exceed ceiling
	assert.Equal(t, uint64(MaxPriorityFeeLamports), fee)
}

func TestPriorityFeeEstimator_Stats(t *testing.T) {
	e := &PriorityFeeEstimator{
		stopCh: make(chan struct{}),
	}

	e.mu.Lock()
	e.feeP50 = 100
	e.feeP75 = 200
	e.feeP90 = 300
	e.samples = 20
	e.mu.Unlock()

	stats := e.Stats()
	assert.Equal(t, uint64(100), stats.P50Lamports)
	assert.Equal(t, uint64(200), stats.P75Lamports)
	assert.Equal(t, uint64(300), stats.P90Lamports)
	assert.Equal(t, 20, stats.Samples)
}
