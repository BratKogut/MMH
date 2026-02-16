package features

import (
	"sync"
	"time"

	"github.com/nexus-trading/nexus/internal/bus"
)

// imbalanceEntry stores a single trade's volume contribution for imbalance.
type imbalanceEntry struct {
	qty    float64
	isBuy  bool
	ts     time.Time
}

// imbalanceState holds per-symbol trade flow imbalance state.
type imbalanceState struct {
	buf    []imbalanceEntry
	head   int
	count  int
	buyVol float64
	selVol float64
}

// Imbalance calculates trade flow imbalance (VPIN-like) over a rolling window.
//
// Formula: Imbalance = (buyVolume - sellVolume) / (buyVolume + sellVolume)
//
// Range: [-1, +1]
//   - +1 = all buys (aggressive buying pressure)
//   -  0 = balanced flow
//   - -1 = all sells (aggressive selling pressure)
//
// Trades are classified by their Side field ("buy" or "sell").
// Trades with side "unknown" are excluded from the calculation.
type Imbalance struct {
	window   time.Duration
	capacity int
	mu       sync.RWMutex
	states   map[string]*imbalanceState
}

const defaultImbalanceCapacity = 10000

// NewImbalance creates an Imbalance calculator with the given rolling window.
func NewImbalance(window time.Duration) *Imbalance {
	return &Imbalance{
		window:   window,
		capacity: defaultImbalanceCapacity,
		states:   make(map[string]*imbalanceState),
	}
}

func (im *Imbalance) Name() string { return "trade_imbalance" }

func (im *Imbalance) OnTrade(symbol string, trade bus.Trade) {
	im.mu.Lock()
	defer im.mu.Unlock()

	// Only count trades with known side.
	isBuy := trade.Side == "buy"
	isSell := trade.Side == "sell"
	if !isBuy && !isSell {
		return
	}

	s := im.getOrCreate(symbol)
	qty := trade.Qty.InexactFloat64()

	// Evict expired entries before adding.
	im.evictExpired(s, trade.Timestamp)

	entry := imbalanceEntry{
		qty:   qty,
		isBuy: isBuy,
		ts:    trade.Timestamp,
	}

	if s.count < im.capacity {
		s.buf[s.head] = entry
		s.head = (s.head + 1) % im.capacity
		s.count++
	} else {
		// Overwrite oldest.
		oldest := s.buf[s.head]
		if oldest.isBuy {
			s.buyVol -= oldest.qty
		} else {
			s.selVol -= oldest.qty
		}
		s.buf[s.head] = entry
		s.head = (s.head + 1) % im.capacity
	}

	if isBuy {
		s.buyVol += qty
	} else {
		s.selVol += qty
	}
}

func (im *Imbalance) OnTick(_ string, _ bus.Tick) {
	// Imbalance only uses trade data.
}

// Value returns the current trade flow imbalance for a symbol.
// Returns 0 if no trades have been recorded.
func (im *Imbalance) Value(symbol string) float64 {
	im.mu.RLock()
	defer im.mu.RUnlock()

	s, ok := im.states[symbol]
	if !ok || s.count == 0 {
		return 0
	}

	total := s.buyVol + s.selVol
	if total == 0 {
		return 0
	}

	return (s.buyVol - s.selVol) / total
}

func (im *Imbalance) Ready(symbol string) bool {
	im.mu.RLock()
	defer im.mu.RUnlock()

	s, ok := im.states[symbol]
	return ok && s.count > 0
}

func (im *Imbalance) Reset(symbol string) {
	im.mu.Lock()
	defer im.mu.Unlock()
	delete(im.states, symbol)
}

func (im *Imbalance) getOrCreate(symbol string) *imbalanceState {
	s, ok := im.states[symbol]
	if !ok {
		s = &imbalanceState{
			buf: make([]imbalanceEntry, im.capacity),
		}
		im.states[symbol] = s
	}
	return s
}

// evictExpired removes entries older than the window. Caller must hold mu.
func (im *Imbalance) evictExpired(s *imbalanceState, now time.Time) {
	cutoff := now.Add(-im.window)

	for s.count > 0 {
		tailIdx := (s.head - s.count + im.capacity) % im.capacity
		if s.buf[tailIdx].ts.Before(cutoff) {
			if s.buf[tailIdx].isBuy {
				s.buyVol -= s.buf[tailIdx].qty
			} else {
				s.selVol -= s.buf[tailIdx].qty
			}
			s.count--
		} else {
			break
		}
	}

	// Guard against floating-point drift.
	if s.buyVol < 0 {
		s.buyVol = 0
	}
	if s.selVol < 0 {
		s.selVol = 0
	}
}
