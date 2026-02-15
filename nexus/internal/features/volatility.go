package features

import (
	"math"
	"sync"

	"github.com/nexus-trading/nexus/internal/bus"
)

// Annualization factor assuming 1-minute sampling for crypto markets.
// sqrt(365 days * 24 hours * 60 minutes)
var annualizationFactor = math.Sqrt(365.0 * 24.0 * 60.0)

// volState holds per-symbol volatility state.
type volState struct {
	prices []float64 // circular buffer of trade prices
	head   int       // next write position
	count  int       // number of valid entries (up to capacity)
}

// Volatility calculates realized volatility from trade prices using
// log-returns and standard deviation.
//
// Method:
//  1. Maintain a rolling window of the last N trade prices.
//  2. Compute log-returns: r_i = ln(price_i / price_{i-1})
//  3. vol = std(returns) * sqrt(annualization_factor)
//
// The annualization factor assumes 1-minute sampling intervals,
// appropriate for crypto markets: sqrt(365 * 24 * 60).
//
// Cold start: returns 0 until at least 2 prices are in the buffer.
type Volatility struct {
	capacity int // max number of prices to keep
	mu       sync.RWMutex
	states   map[string]*volState
}

// NewVolatility creates a Volatility calculator.
// capacity is the number of trade prices to keep in the rolling window.
func NewVolatility(capacity int) *Volatility {
	if capacity < 2 {
		capacity = 2
	}
	return &Volatility{
		capacity: capacity,
		states:   make(map[string]*volState),
	}
}

func (v *Volatility) Name() string { return "volatility" }

func (v *Volatility) OnTrade(symbol string, trade bus.Trade) {
	v.mu.Lock()
	defer v.mu.Unlock()

	s := v.getOrCreate(symbol)
	price := trade.Price.InexactFloat64()

	if price <= 0 {
		return // skip invalid prices
	}

	s.prices[s.head] = price
	s.head = (s.head + 1) % v.capacity
	if s.count < v.capacity {
		s.count++
	}
}

func (v *Volatility) OnTick(_ string, _ bus.Tick) {
	// Volatility only uses trade prices.
}

// Value returns the annualized realized volatility for a symbol.
// Returns 0 if fewer than 2 prices are available.
func (v *Volatility) Value(symbol string) float64 {
	v.mu.RLock()
	defer v.mu.RUnlock()

	s, ok := v.states[symbol]
	if !ok || s.count < 2 {
		return 0
	}

	return v.computeVol(s)
}

func (v *Volatility) Ready(symbol string) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()

	s, ok := v.states[symbol]
	return ok && s.count >= 2
}

func (v *Volatility) Reset(symbol string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.states, symbol)
}

func (v *Volatility) getOrCreate(symbol string) *volState {
	s, ok := v.states[symbol]
	if !ok {
		s = &volState{
			prices: make([]float64, v.capacity),
		}
		v.states[symbol] = s
	}
	return s
}

// computeVol calculates realized volatility from the price buffer.
// Caller must hold at least a read lock, and s.count must be >= 2.
func (v *Volatility) computeVol(s *volState) float64 {
	n := s.count
	nReturns := n - 1

	if nReturns < 1 {
		return 0
	}

	// Iterate through the circular buffer in chronological order.
	// The oldest entry is at (head - count + capacity) % capacity.
	startIdx := (s.head - n + v.capacity) % v.capacity

	// Compute log returns and their mean in a single pass.
	sumReturns := 0.0
	returns := make([]float64, 0, nReturns)

	prevPrice := s.prices[startIdx]
	for i := 1; i < n; i++ {
		idx := (startIdx + i) % v.capacity
		curPrice := s.prices[idx]

		if prevPrice <= 0 || curPrice <= 0 {
			prevPrice = curPrice
			continue
		}

		r := math.Log(curPrice / prevPrice)
		returns = append(returns, r)
		sumReturns += r
		prevPrice = curPrice
	}

	if len(returns) < 1 {
		return 0
	}

	mean := sumReturns / float64(len(returns))

	// Compute variance (using sample variance with Bessel's correction if n > 1).
	sumSqDev := 0.0
	for _, r := range returns {
		d := r - mean
		sumSqDev += d * d
	}

	var variance float64
	if len(returns) > 1 {
		variance = sumSqDev / float64(len(returns)-1) // Bessel's correction
	} else {
		variance = sumSqDev // Only one return, no correction possible.
	}

	stddev := math.Sqrt(variance)

	// Annualize: multiply by sqrt(periods per year).
	return stddev * annualizationFactor
}
