package intel

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// ---------------------------------------------------------------------------
// Trigger Engine -- decides WHEN to query the Brain
// ---------------------------------------------------------------------------

// TriggerHandler is called when the engine fires a trigger.
// Typically wired to Brain.ProcessTrigger.
type TriggerHandler func(ctx context.Context, trigger Trigger) (*IntelEvent, error)

// TriggerEngineConfig configures the trigger engine.
type TriggerEngineConfig struct {
	// Volatility spike detection.
	VolSpikeThreshold float64 // multiplier above rolling mean (e.g. 2.0 = 2x normal vol)
	VolWindowSize     int     // number of samples for rolling volatility mean

	// Gap detection.
	GapThresholdPct float64 // price gap as percent (e.g. 2.0 = 2%)

	// Correlation break detection.
	CorrBreakThreshold float64 // absolute change in rolling correlation (e.g. 0.3)
	CorrWindowSize     int     // number of samples for rolling correlation

	// Schedule check interval.
	ScheduleCheckIntervalMs int

	// Cascade: max depth of cascading triggers (prevent infinite loops).
	MaxCascadeDepth int
}

// DefaultTriggerEngineConfig returns sensible defaults.
func DefaultTriggerEngineConfig() TriggerEngineConfig {
	return TriggerEngineConfig{
		VolSpikeThreshold:       2.0,
		VolWindowSize:           60,
		GapThresholdPct:         2.0,
		CorrBreakThreshold:      0.3,
		CorrWindowSize:          30,
		ScheduleCheckIntervalMs: 60_000,
		MaxCascadeDepth:         3,
	}
}

// ---------------------------------------------------------------------------
// Market anomaly detectors
// ---------------------------------------------------------------------------

// symbolState tracks rolling statistics for a single symbol.
type symbolState struct {
	// Rolling volatility samples.
	volSamples []float64
	volIdx     int
	volFull    bool

	// Last price for gap detection.
	lastPrice float64
	hasPrice  bool

	// Timestamp of last trigger per type (cooldown).
	lastTrigger map[TriggerType]time.Time
}

// ScheduledEvent represents a calendar event that triggers intel queries.
type ScheduledEvent struct {
	Name      string    // e.g. "FOMC Decision", "CPI Release"
	Topic     Topic     // which intel topic to query
	Time      time.Time // when to fire
	AssetTags []string  // affected assets
	Priority  Priority
	Prompt    string // what to ask the LLM
	Fired     bool   // whether this event has been triggered
}

// ---------------------------------------------------------------------------
// TriggerEngine
// ---------------------------------------------------------------------------

// TriggerEngine monitors market data and fires triggers to the Brain when
// anomalies are detected or scheduled events occur.
type TriggerEngine struct {
	mu      sync.RWMutex
	config  TriggerEngineConfig
	handler TriggerHandler

	// Per-symbol state for anomaly detection.
	symbols map[string]*symbolState

	// Scheduled events (macro calendar).
	schedule []*ScheduledEvent

	// Cooldown: minimum interval between triggers of the same type per symbol.
	cooldownMs int64

	// Stats.
	triggersEmitted int64
	anomaliesFound  int64

	stopCh  chan struct{}
	stopped sync.Once
}

// NewTriggerEngine creates a new TriggerEngine.
func NewTriggerEngine(config TriggerEngineConfig, handler TriggerHandler) *TriggerEngine {
	return &TriggerEngine{
		config:     config,
		handler:    handler,
		symbols:    make(map[string]*symbolState),
		schedule:   make([]*ScheduledEvent, 0),
		cooldownMs: 60_000, // 1 minute cooldown per trigger type per symbol
		stopCh:     make(chan struct{}),
	}
}

// AddScheduledEvent registers a calendar event for future triggering.
func (e *TriggerEngine) AddScheduledEvent(event ScheduledEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.schedule = append(e.schedule, &event)
	log.Info().
		Str("name", event.Name).
		Time("time", event.Time).
		Msg("intel: scheduled event registered")
}

// OnVolatility receives a new volatility observation for a symbol.
// If the value is a spike relative to the rolling window, a trigger is emitted.
func (e *TriggerEngine) OnVolatility(ctx context.Context, symbol string, vol float64) {
	e.mu.Lock()
	state := e.getOrCreateSymbolLocked(symbol)

	// Add to rolling window.
	if len(state.volSamples) < e.config.VolWindowSize {
		state.volSamples = append(state.volSamples, vol)
	} else {
		state.volSamples[state.volIdx] = vol
	}
	state.volIdx = (state.volIdx + 1) % e.config.VolWindowSize

	if len(state.volSamples) == e.config.VolWindowSize {
		state.volFull = true
	}

	// Need at least half a window of data before detecting spikes.
	minSamples := e.config.VolWindowSize / 2
	if len(state.volSamples) < minSamples {
		e.mu.Unlock()
		return
	}

	// Calculate rolling mean (exclude the current sample to avoid self-influence).
	mean := rollingMean(state.volSamples, state.volIdx, len(state.volSamples))

	// Check for spike.
	if mean > 0 && vol > mean*e.config.VolSpikeThreshold {
		if e.canTriggerLocked(state, TriggerMarketAnomaly) {
			state.lastTrigger[TriggerMarketAnomaly] = time.Now()
			e.anomaliesFound++
			e.mu.Unlock()

			e.fireTrigger(ctx, Trigger{
				Type:      TriggerMarketAnomaly,
				Topic:     TopicVolatilitySpike,
				AssetTags: []string{symbol},
				Prompt: fmt.Sprintf(
					"Volatility spike detected for %s: current=%.4f, rolling_mean=%.4f (%.1fx). "+
						"What is driving this? Extract key claims about the cause and expected impact horizon.",
					symbol, vol, mean, vol/mean),
				Priority: PriorityP0,
				Source:   "trigger_engine:vol_spike",
			})
			return
		}
	}
	e.mu.Unlock()
}

// OnPrice receives a new price observation for a symbol.
// Detects gaps (sudden price jumps beyond threshold).
func (e *TriggerEngine) OnPrice(ctx context.Context, symbol string, price float64) {
	e.mu.Lock()
	state := e.getOrCreateSymbolLocked(symbol)

	if !state.hasPrice {
		state.lastPrice = price
		state.hasPrice = true
		e.mu.Unlock()
		return
	}

	prevPrice := state.lastPrice
	state.lastPrice = price

	if prevPrice == 0 {
		e.mu.Unlock()
		return
	}

	gapPct := math.Abs(price-prevPrice) / prevPrice * 100

	if gapPct >= e.config.GapThresholdPct {
		if e.canTriggerLocked(state, TriggerMarketAnomaly) {
			state.lastTrigger[TriggerMarketAnomaly] = time.Now()
			e.anomaliesFound++
			direction := "up"
			if price < prevPrice {
				direction = "down"
			}
			e.mu.Unlock()

			e.fireTrigger(ctx, Trigger{
				Type:      TriggerMarketAnomaly,
				Topic:     TopicVolatilitySpike,
				AssetTags: []string{symbol},
				Prompt: fmt.Sprintf(
					"Price gap detected for %s: %.2f -> %.2f (%.2f%% %s). "+
						"Identify the cause: exchange outage, large trade, news event, or technical movement.",
					symbol, prevPrice, price, gapPct, direction),
				Priority: PriorityP1,
				Source:   "trigger_engine:price_gap",
			})
			return
		}
	}
	e.mu.Unlock()
}

// OnCascade handles a cascading trigger: a previous intel event generates a
// follow-up query for deeper analysis.
func (e *TriggerEngine) OnCascade(ctx context.Context, parentEvent *IntelEvent, prompt string, depth int) {
	if depth >= e.config.MaxCascadeDepth {
		log.Warn().
			Str("parent_event_id", parentEvent.EventID).
			Int("depth", depth).
			Msg("intel: cascade depth exceeded, stopping")
		return
	}

	e.fireTrigger(ctx, Trigger{
		Type:      TriggerCascade,
		Topic:     parentEvent.Topic,
		AssetTags: parentEvent.AssetTags,
		Prompt:    prompt,
		Priority:  parentEvent.Priority,
		Source:    fmt.Sprintf("cascade:%s:depth=%d", parentEvent.EventID, depth),
	})
}

// OnStrategyRequest handles a direct strategy request for intel.
func (e *TriggerEngine) OnStrategyRequest(ctx context.Context, strategyID string, topic Topic, assetTags []string, prompt string) {
	e.fireTrigger(ctx, Trigger{
		Type:      TriggerStrategyReq,
		Topic:     topic,
		AssetTags: assetTags,
		Prompt:    prompt,
		Priority:  PriorityP2,
		Source:    fmt.Sprintf("strategy:%s", strategyID),
	})
}

// StartScheduler begins the background loop that fires scheduled events.
// It blocks until ctx is cancelled or Stop is called.
func (e *TriggerEngine) StartScheduler(ctx context.Context) {
	interval := time.Duration(e.config.ScheduleCheckIntervalMs) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-e.stopCh:
			return
		case now := <-ticker.C:
			e.checkSchedule(ctx, now)
		}
	}
}

// Stop signals the scheduler to stop.
func (e *TriggerEngine) Stop() {
	e.stopped.Do(func() {
		close(e.stopCh)
	})
}

// Stats returns trigger engine statistics.
func (e *TriggerEngine) Stats() TriggerEngineStats {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return TriggerEngineStats{
		TriggersEmitted: e.triggersEmitted,
		AnomaliesFound:  e.anomaliesFound,
		SymbolsTracked:  len(e.symbols),
		ScheduledEvents: len(e.schedule),
	}
}

// TriggerEngineStats reports aggregate statistics.
type TriggerEngineStats struct {
	TriggersEmitted int64 `json:"triggers_emitted"`
	AnomaliesFound  int64 `json:"anomalies_found"`
	SymbolsTracked  int   `json:"symbols_tracked"`
	ScheduledEvents int   `json:"scheduled_events"`
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func (e *TriggerEngine) getOrCreateSymbolLocked(symbol string) *symbolState {
	state, ok := e.symbols[symbol]
	if !ok {
		state = &symbolState{
			volSamples:  make([]float64, 0, e.config.VolWindowSize),
			lastTrigger: make(map[TriggerType]time.Time),
		}
		e.symbols[symbol] = state
	}
	return state
}

// canTriggerLocked checks cooldown for a trigger type on a symbol.
func (e *TriggerEngine) canTriggerLocked(state *symbolState, triggerType TriggerType) bool {
	lastTime, ok := state.lastTrigger[triggerType]
	if !ok {
		return true
	}
	return time.Since(lastTime).Milliseconds() >= e.cooldownMs
}

// fireTrigger sends a trigger to the handler.
func (e *TriggerEngine) fireTrigger(ctx context.Context, trigger Trigger) {
	e.mu.Lock()
	e.triggersEmitted++
	e.mu.Unlock()

	log.Info().
		Str("type", string(trigger.Type)).
		Str("topic", string(trigger.Topic)).
		Strs("assets", trigger.AssetTags).
		Str("source", trigger.Source).
		Msg("intel: trigger fired")

	if e.handler != nil {
		if _, err := e.handler(ctx, trigger); err != nil {
			log.Error().Err(err).
				Str("type", string(trigger.Type)).
				Str("source", trigger.Source).
				Msg("intel: trigger handler failed")
		}
	}
}

// checkSchedule fires any scheduled events whose time has come.
func (e *TriggerEngine) checkSchedule(ctx context.Context, now time.Time) {
	e.mu.Lock()
	var toFire []*ScheduledEvent
	for _, evt := range e.schedule {
		if !evt.Fired && !now.Before(evt.Time) {
			evt.Fired = true
			toFire = append(toFire, evt)
		}
	}
	e.mu.Unlock()

	for _, evt := range toFire {
		log.Info().
			Str("event", evt.Name).
			Time("scheduled_time", evt.Time).
			Msg("intel: scheduled event firing")

		e.fireTrigger(ctx, Trigger{
			Type:      TriggerSchedule,
			Topic:     evt.Topic,
			AssetTags: evt.AssetTags,
			Prompt:    evt.Prompt,
			Priority:  evt.Priority,
			Source:    fmt.Sprintf("schedule:%s", evt.Name),
		})
	}
}

// rollingMean computes the mean of the circular buffer, optionally excluding
// the most recent sample (at idx-1).
func rollingMean(samples []float64, _ int, n int) float64 {
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		sum += samples[i]
	}
	return sum / float64(n)
}
