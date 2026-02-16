package features

import (
	"sync"

	"github.com/nexus-trading/nexus/internal/bus"
)

// spreadState holds per-symbol spread state.
type spreadState struct {
	bid          float64
	ask          float64
	mid          float64
	spreadAbs    float64 // ask - bid
	spreadBps    float64 // (ask - bid) / mid * 10000
	emaSpreadBps float64 // EMA-smoothed spread in bps
	initialized  bool    // have we seen at least one tick?
}

// Spread calculates bid-ask spread metrics from tick (quote) data.
//
// Metrics computed:
//   - QuotedSpread     = ask - bid
//   - QuotedSpreadBps  = (ask - bid) / mid * 10000
//   - MidPrice         = (bid + ask) / 2
//   - EMA(SpreadBps)   with configurable alpha
//
// Only responds to tick events; trades are ignored.
type Spread struct {
	alpha  float64 // EMA smoothing factor (0 < alpha <= 1)
	mu     sync.RWMutex
	states map[string]*spreadState
}

// NewSpread creates a Spread calculator with the given EMA alpha.
// Alpha controls how fast the EMA reacts: higher = more responsive.
// Typical value: 0.1 (slow smoothing).
func NewSpread(alpha float64) *Spread {
	if alpha <= 0 || alpha > 1 {
		alpha = 0.1
	}
	return &Spread{
		alpha:  alpha,
		states: make(map[string]*spreadState),
	}
}

func (sp *Spread) Name() string { return "spread_bps" }

func (sp *Spread) OnTrade(_ string, _ bus.Trade) {
	// Spread only responds to ticks.
}

func (sp *Spread) OnTick(symbol string, tick bus.Tick) {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	s := sp.getOrCreate(symbol)

	bid := tick.Bid.InexactFloat64()
	ask := tick.Ask.InexactFloat64()

	if bid <= 0 || ask <= 0 || ask < bid {
		return // Invalid tick data, skip.
	}

	s.bid = bid
	s.ask = ask
	s.mid = (bid + ask) / 2.0
	s.spreadAbs = ask - bid

	if s.mid > 0 {
		s.spreadBps = (s.spreadAbs / s.mid) * 10000.0
	}

	if !s.initialized {
		// First tick: seed the EMA.
		s.emaSpreadBps = s.spreadBps
		s.initialized = true
	} else {
		// EMA update: new = alpha * current + (1 - alpha) * old
		s.emaSpreadBps = sp.alpha*s.spreadBps + (1-sp.alpha)*s.emaSpreadBps
	}
}

// Value returns the current EMA-smoothed spread in basis points.
func (sp *Spread) Value(symbol string) float64 {
	sp.mu.RLock()
	defer sp.mu.RUnlock()

	s, ok := sp.states[symbol]
	if !ok || !s.initialized {
		return 0
	}
	return s.emaSpreadBps
}

func (sp *Spread) Ready(symbol string) bool {
	sp.mu.RLock()
	defer sp.mu.RUnlock()

	s, ok := sp.states[symbol]
	return ok && s.initialized
}

func (sp *Spread) Reset(symbol string) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	delete(sp.states, symbol)
}

// MidPrice returns the current mid price for a symbol.
func (sp *Spread) MidPrice(symbol string) float64 {
	sp.mu.RLock()
	defer sp.mu.RUnlock()

	s, ok := sp.states[symbol]
	if !ok || !s.initialized {
		return 0
	}
	return s.mid
}

// RawSpreadBps returns the most recent un-smoothed spread in bps.
func (sp *Spread) RawSpreadBps(symbol string) float64 {
	sp.mu.RLock()
	defer sp.mu.RUnlock()

	s, ok := sp.states[symbol]
	if !ok || !s.initialized {
		return 0
	}
	return s.spreadBps
}

func (sp *Spread) getOrCreate(symbol string) *spreadState {
	s, ok := sp.states[symbol]
	if !ok {
		s = &spreadState{}
		sp.states[symbol] = s
	}
	return s
}
