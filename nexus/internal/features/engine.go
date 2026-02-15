package features

import (
	"sync"
	"time"

	"github.com/nexus-trading/nexus/internal/bus"
)

// FeatureResult represents a single computed feature value.
type FeatureResult struct {
	Symbol    string  `json:"symbol"`
	Name      string  `json:"name"`
	Value     float64 `json:"value"`
	Timestamp int64   `json:"ts"`
}

// Calculator is the interface for all feature calculators.
// Each calculator is responsible for one feature (e.g., VWAP, spread).
// Implementations must be safe for concurrent read access via Value()
// while OnTrade/OnTick are called from a single writer goroutine.
type Calculator interface {
	// Name returns the unique identifier for this feature.
	Name() string

	// OnTrade processes a trade event. Called for every trade.
	OnTrade(symbol string, trade bus.Trade)

	// OnTick processes a tick (quote) event. Called for every tick.
	OnTick(symbol string, tick bus.Tick)

	// Value returns the current feature value for a symbol.
	// Returns 0 if insufficient data (cold start).
	Value(symbol string) float64

	// Ready returns true if the calculator has enough data to produce
	// a meaningful value for the given symbol.
	Ready(symbol string) bool

	// Reset clears all state for a symbol.
	Reset(symbol string)
}

// Engine orchestrates all feature calculators. It receives raw market data
// events, fans them out to registered calculators, and collects results.
type Engine struct {
	calculators []Calculator
	calcByName  map[string]Calculator
	symbols     []string
	mu          sync.RWMutex
}

// NewEngine creates a new feature engine with all built-in calculators
// registered for the given symbols.
func NewEngine(symbols []string) *Engine {
	e := &Engine{
		symbols:    symbols,
		calcByName: make(map[string]Calculator),
	}

	// Register all built-in calculators with sensible defaults.
	calcs := []Calculator{
		NewVWAP(5 * time.Minute),
		NewSpread(0.1),
		NewVolatility(100),
		NewImbalance(60 * time.Second),
		NewMomentum(time.Second, 60),
	}

	for _, c := range calcs {
		e.calculators = append(e.calculators, c)
		e.calcByName[c.Name()] = c
	}

	return e
}

// RegisterCalculator adds a custom calculator to the engine.
func (e *Engine) RegisterCalculator(c Calculator) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calculators = append(e.calculators, c)
	e.calcByName[c.Name()] = c
}

// ProcessTrade fans a trade event to all calculators and collects results.
func (e *Engine) ProcessTrade(trade bus.Trade) []FeatureResult {
	e.mu.RLock()
	defer e.mu.RUnlock()

	ts := trade.Timestamp.UnixMilli()
	results := make([]FeatureResult, 0, len(e.calculators))

	for _, c := range e.calculators {
		c.OnTrade(trade.Symbol, trade)
		results = append(results, FeatureResult{
			Symbol:    trade.Symbol,
			Name:      c.Name(),
			Value:     c.Value(trade.Symbol),
			Timestamp: ts,
		})
	}

	return results
}

// ProcessTick fans a tick event to all calculators and collects results.
func (e *Engine) ProcessTick(tick bus.Tick) []FeatureResult {
	e.mu.RLock()
	defer e.mu.RUnlock()

	ts := tick.Timestamp.UnixMilli()
	results := make([]FeatureResult, 0, len(e.calculators))

	for _, c := range e.calculators {
		c.OnTick(tick.Symbol, tick)
		results = append(results, FeatureResult{
			Symbol:    tick.Symbol,
			Name:      c.Name(),
			Value:     c.Value(tick.Symbol),
			Timestamp: ts,
		})
	}

	return results
}

// Snapshot returns all current feature values for a symbol.
func (e *Engine) Snapshot(symbol string) map[string]float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()

	snap := make(map[string]float64, len(e.calculators))
	for _, c := range e.calculators {
		snap[c.Name()] = c.Value(symbol)
	}
	return snap
}

// ResetSymbol clears all calculator state for a symbol.
func (e *Engine) ResetSymbol(symbol string) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	for _, c := range e.calculators {
		c.Reset(symbol)
	}
}
