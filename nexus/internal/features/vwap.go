package features

import (
	"sync"
	"time"

	"github.com/nexus-trading/nexus/internal/bus"
)

// vwapEntry stores a single trade's contribution to VWAP.
type vwapEntry struct {
	priceQty float64 // price * qty
	qty      float64
	ts       time.Time
}

// vwapState holds per-symbol VWAP state with a circular buffer.
type vwapState struct {
	buf    []vwapEntry
	head   int  // next write position
	count  int  // number of valid entries (up to cap)
	full   bool // whether the buffer has wrapped around
	sumPQ  float64
	sumQ   float64
}

// VWAP calculates Volume-Weighted Average Price over a rolling time window.
//
// Formula: VWAP = Sum(price_i * qty_i) / Sum(qty_i)
//
// Uses a circular buffer of fixed capacity. Entries older than the configured
// window duration are evicted on each new trade. Internal math uses float64
// for performance; decimal conversion happens only at the bus.Trade boundary.
type VWAP struct {
	window   time.Duration
	capacity int
	mu       sync.RWMutex
	states   map[string]*vwapState
}

const defaultVWAPCapacity = 10000

// NewVWAP creates a VWAP calculator with the given rolling window duration.
func NewVWAP(window time.Duration) *VWAP {
	return &VWAP{
		window:   window,
		capacity: defaultVWAPCapacity,
		states:   make(map[string]*vwapState),
	}
}

func (v *VWAP) Name() string { return "vwap" }

func (v *VWAP) OnTrade(symbol string, trade bus.Trade) {
	v.mu.Lock()
	defer v.mu.Unlock()

	s := v.getOrCreate(symbol)

	price := trade.Price.InexactFloat64()
	qty := trade.Qty.InexactFloat64()
	pq := price * qty

	// Evict expired entries before adding.
	v.evictExpired(s, trade.Timestamp)

	entry := vwapEntry{
		priceQty: pq,
		qty:      qty,
		ts:       trade.Timestamp,
	}

	if s.count < v.capacity {
		// Buffer not yet full: append.
		s.buf[s.head] = entry
		s.head = (s.head + 1) % v.capacity
		s.count++
	} else {
		// Buffer full: overwrite oldest (at head position since it wraps).
		oldest := s.buf[s.head]
		s.sumPQ -= oldest.priceQty
		s.sumQ -= oldest.qty
		s.buf[s.head] = entry
		s.head = (s.head + 1) % v.capacity
		s.full = true
	}

	s.sumPQ += pq
	s.sumQ += qty
}

func (v *VWAP) OnTick(_ string, _ bus.Tick) {
	// VWAP only responds to trades.
}

func (v *VWAP) Value(symbol string) float64 {
	v.mu.RLock()
	defer v.mu.RUnlock()

	s, ok := v.states[symbol]
	if !ok || s.count == 0 || s.sumQ == 0 {
		return 0
	}
	return s.sumPQ / s.sumQ
}

func (v *VWAP) Ready(symbol string) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()

	s, ok := v.states[symbol]
	return ok && s.count > 0 && s.sumQ > 0
}

func (v *VWAP) Reset(symbol string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.states, symbol)
}

// getOrCreate returns existing state or creates a new one. Caller must hold mu.
func (v *VWAP) getOrCreate(symbol string) *vwapState {
	s, ok := v.states[symbol]
	if !ok {
		s = &vwapState{
			buf: make([]vwapEntry, v.capacity),
		}
		v.states[symbol] = s
	}
	return s
}

// evictExpired removes entries older than the window. Caller must hold mu.
// We scan from the tail (oldest) to head, removing expired entries.
func (v *VWAP) evictExpired(s *vwapState, now time.Time) {
	cutoff := now.Add(-v.window)

	// The oldest entry is at position (head - count + capacity) % capacity.
	for s.count > 0 {
		tailIdx := (s.head - s.count + v.capacity) % v.capacity
		if s.buf[tailIdx].ts.Before(cutoff) {
			s.sumPQ -= s.buf[tailIdx].priceQty
			s.sumQ -= s.buf[tailIdx].qty
			s.count--
		} else {
			break // All remaining entries are within the window.
		}
	}

	// Guard against floating-point drift going negative.
	if s.sumPQ < 0 {
		s.sumPQ = 0
	}
	if s.sumQ < 0 {
		s.sumQ = 0
	}
}
