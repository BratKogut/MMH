package risk

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/rs/zerolog/log"
)

// Engine is the risk management engine.
// SAFETY > PROFIT > SPEED
//
// Hardcoded minimums (not configurable, not disableable):
// - max_daily_loss: ALWAYS active
// - max_total_exposure: ALWAYS active
// - kill_switch: ALWAYS responsive (in-process, not through Kafka)
type Engine struct {
	config Config

	// State
	dailyPnL      float64
	totalExposure float64
	positionCount map[string]int // symbol -> count
	mu            sync.RWMutex

	// Kill switch - atomic for lock-free check
	killed atomic.Bool
	frozen atomic.Bool

	// Metrics
	allowed atomic.Int64
	denied  atomic.Int64
	freezes atomic.Int64
}

// Config holds risk engine configuration.
type Config struct {
	MaxDailyLossUSD           float64
	MaxTotalExposureUSD       float64
	MaxPositionPerSymbol      float64
	MaxNotionalPerOrder       float64
	CooldownAfterLosses       int
	CooldownMinutes           int
	FeedLagThresholdMs        int
	OrderRejectSpikeThreshold int
}

// Decision represents a risk decision.
type Decision struct {
	IntentID    string   `json:"intent_id"`
	Allowed     bool     `json:"allowed"`
	ReasonCodes []string `json:"reason_codes"`
	Timestamp   int64    `json:"ts"`
}

// New creates a new risk engine.
func New(cfg Config) *Engine {
	return &Engine{
		config:        cfg,
		positionCount: make(map[string]int),
	}
}

// Check evaluates a trade intent against all risk policies.
// Returns a Decision with allow/deny and reason codes.
func (e *Engine) Check(intent bus.OrderIntent) Decision {
	d := Decision{
		IntentID:  intent.IntentID,
		Allowed:   true,
		Timestamp: time.Now().UnixMicro(),
	}

	// Kill switch check - ALWAYS first, atomic, no lock needed
	if e.killed.Load() {
		d.Allowed = false
		d.ReasonCodes = append(d.ReasonCodes, "KILL_SWITCH_ACTIVE")
		return d
	}

	if e.frozen.Load() {
		d.Allowed = false
		d.ReasonCodes = append(d.ReasonCodes, "SYSTEM_FROZEN")
		return d
	}

	e.mu.RLock()
	defer e.mu.RUnlock()

	// Daily loss limit (HARDCODED - NOT DISABLEABLE)
	if e.dailyPnL < -e.config.MaxDailyLossUSD {
		d.Allowed = false
		d.ReasonCodes = append(d.ReasonCodes,
			fmt.Sprintf("DAILY_LOSS_EXCEEDED:pnl=%.2f,limit=%.2f", e.dailyPnL, -e.config.MaxDailyLossUSD))
	}

	// Total exposure limit (HARDCODED - NOT DISABLEABLE)
	notional := intent.Qty.InexactFloat64() * intent.LimitPrice.InexactFloat64()
	if e.totalExposure+notional > e.config.MaxTotalExposureUSD {
		d.Allowed = false
		d.ReasonCodes = append(d.ReasonCodes,
			fmt.Sprintf("EXPOSURE_EXCEEDED:current=%.2f,order=%.2f,limit=%.2f",
				e.totalExposure, notional, e.config.MaxTotalExposureUSD))
	}

	// Max notional per order
	if notional > e.config.MaxNotionalPerOrder {
		d.Allowed = false
		d.ReasonCodes = append(d.ReasonCodes,
			fmt.Sprintf("ORDER_TOO_LARGE:notional=%.2f,limit=%.2f", notional, e.config.MaxNotionalPerOrder))
	}

	// Max position per symbol
	if e.config.MaxPositionPerSymbol > 0 {
		currentPos := float64(e.positionCount[intent.Symbol])
		if currentPos+intent.Qty.InexactFloat64() > e.config.MaxPositionPerSymbol {
			d.Allowed = false
			d.ReasonCodes = append(d.ReasonCodes,
				fmt.Sprintf("POSITION_LIMIT:%s:current=%.2f,limit=%.2f",
					intent.Symbol, currentPos, e.config.MaxPositionPerSymbol))
		}
	}

	if d.Allowed {
		e.allowed.Add(1)
		log.Debug().Str("intent_id", intent.IntentID).Msg("Risk check: ALLOW")
	} else {
		e.denied.Add(1)
		log.Warn().Str("intent_id", intent.IntentID).Strs("reasons", d.ReasonCodes).Msg("Risk check: DENY")
	}

	return d
}

// Kill activates the kill switch. Immediate, in-process, no Kafka dependency.
func (e *Engine) Kill() {
	e.killed.Store(true)
	log.Error().Msg("KILL SWITCH ACTIVATED - All trading stopped")
}

// Freeze freezes the system (can be resumed, unlike kill).
func (e *Engine) Freeze(reason string) {
	e.frozen.Store(true)
	e.freezes.Add(1)
	log.Warn().Str("reason", reason).Msg("SYSTEM FROZEN")
}

// Resume unfreezes the system.
func (e *Engine) Resume() {
	if e.killed.Load() {
		log.Warn().Msg("Cannot resume: kill switch is active (requires restart)")
		return
	}
	e.frozen.Store(false)
	log.Info().Msg("System resumed")
}

// UpdatePnL updates the daily PnL.
func (e *Engine) UpdatePnL(pnl float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dailyPnL = pnl

	// Auto-freeze on daily loss breach
	if e.dailyPnL < -e.config.MaxDailyLossUSD {
		e.frozen.Store(true)
		log.Error().Float64("pnl", pnl).Float64("limit", -e.config.MaxDailyLossUSD).
			Msg("AUTO-FREEZE: Daily loss limit breached")
	}
}

// UpdateExposure updates total exposure.
func (e *Engine) UpdateExposure(exposure float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.totalExposure = exposure
}

// IsActive returns true if the system is not killed or frozen.
func (e *Engine) IsActive() bool {
	return !e.killed.Load() && !e.frozen.Load()
}

// Metrics returns risk engine metrics.
func (e *Engine) Metrics() map[string]interface{} {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return map[string]interface{}{
		"daily_pnl":      e.dailyPnL,
		"total_exposure":  e.totalExposure,
		"killed":          e.killed.Load(),
		"frozen":          e.frozen.Load(),
		"allowed_total":   e.allowed.Load(),
		"denied_total":    e.denied.Load(),
		"freezes_total":   e.freezes.Load(),
	}
}
