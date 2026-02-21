package features

import (
	"sync"
	"time"

	"github.com/nexus-trading/nexus/internal/bus"
)

// momentumState holds per-symbol momentum state.
type momentumState struct {
	prices   []float64   // circular buffer of sampled prices
	times    []time.Time // timestamps corresponding to prices
	head     int         // next write position
	count    int         // valid entries in buffer
	lastSamp time.Time   // timestamp of the last sampled price
}

// Momentum calculates price Rate of Change (ROC) over a lookback window.
//
// Formula: ROC = (price_now - price_N_ago) / price_N_ago
//
// It samples trade prices at a fixed interval (e.g., every 1 second) and
// compares the latest price to the oldest price in the buffer.
//
// The lookback window = sampleInterval * lookbackPeriods.
// E.g., 1s interval * 60 periods = 60s lookback.
//
// This is a simple but effective feature for regime detection.
type Momentum struct {
	sampleInterval  time.Duration
	lookbackPeriods int // number of sample points to keep
	mu              sync.RWMutex
	states          map[string]*momentumState
}

// NewMomentum creates a Momentum calculator.
// sampleInterval: minimum time between price samples (e.g., 1s).
// lookbackPeriods: number of samples to keep (e.g., 60 for 60s lookback at 1s interval).
func NewMomentum(sampleInterval time.Duration, lookbackPeriods int) *Momentum {
	if lookbackPeriods < 2 {
		lookbackPeriods = 2
	}
	if sampleInterval <= 0 {
		sampleInterval = time.Second
	}
	return &Momentum{
		sampleInterval:  sampleInterval,
		lookbackPeriods: lookbackPeriods,
		states:          make(map[string]*momentumState),
	}
}

func (m *Momentum) Name() string { return "momentum" }

func (m *Momentum) OnTrade(symbol string, trade bus.Trade) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s := m.getOrCreate(symbol)
	price := trade.Price.InexactFloat64()
	ts := trade.Timestamp

	if price <= 0 {
		return
	}

	// Rate-limit: only sample at the configured interval.
	if !s.lastSamp.IsZero() && ts.Sub(s.lastSamp) < m.sampleInterval {
		// Update the most recent sample in-place if within the same interval.
		// This ensures we always have the latest price for the current period.
		if s.count > 0 {
			latestIdx := (s.head - 1 + m.lookbackPeriods) % m.lookbackPeriods
			s.prices[latestIdx] = price
			s.times[latestIdx] = ts
		}
		return
	}

	// New sample period: write to the buffer.
	s.prices[s.head] = price
	s.times[s.head] = ts
	s.head = (s.head + 1) % m.lookbackPeriods
	if s.count < m.lookbackPeriods {
		s.count++
	}
	s.lastSamp = ts
}

func (m *Momentum) OnTick(_ string, _ bus.Tick) {
	// Momentum only uses trade prices.
}

// Value returns the price ROC for a symbol.
// Returns 0 if not enough data (fewer than 2 samples).
func (m *Momentum) Value(symbol string) float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s, ok := m.states[symbol]
	if !ok || s.count < 2 {
		return 0
	}

	// Latest price is at (head - 1).
	latestIdx := (s.head - 1 + m.lookbackPeriods) % m.lookbackPeriods
	// Oldest price is at (head - count).
	oldestIdx := (s.head - s.count + m.lookbackPeriods) % m.lookbackPeriods

	latest := s.prices[latestIdx]
	oldest := s.prices[oldestIdx]

	if oldest == 0 {
		return 0
	}

	return (latest - oldest) / oldest
}

func (m *Momentum) Ready(symbol string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s, ok := m.states[symbol]
	return ok && s.count >= 2
}

func (m *Momentum) Reset(symbol string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.states, symbol)
}

func (m *Momentum) getOrCreate(symbol string) *momentumState {
	s, ok := m.states[symbol]
	if !ok {
		s = &momentumState{
			prices: make([]float64, m.lookbackPeriods),
			times:  make([]time.Time, m.lookbackPeriods),
		}
		m.states[symbol] = s
	}
	return s
}
