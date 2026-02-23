package correlation

import (
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// ---------------------------------------------------------------------------
// Cross-Token Correlation Detector — v3.2 Module
// "Jeden deployer, pięć tokenów — to nie zbieg okoliczności"
// Detects rotation pumps, distraction launches, and serial deploys.
// ---------------------------------------------------------------------------

// CorrelationPattern classifies the type of cross-token correlation.
type CorrelationPattern string

const (
	PatternRotation    CorrelationPattern = "ROTATION"    // A pumps → dump → B pumps
	PatternDistraction CorrelationPattern = "DISTRACTION" // A is bait, B is real target
	PatternSerial      CorrelationPattern = "SERIAL"      // Same deployer launches every 2-4h
	PatternNone        CorrelationPattern = "NONE"
)

// RiskLevel of a cross-token correlation.
type RiskLevel string

const (
	RiskLow      RiskLevel = "LOW"
	RiskMedium   RiskLevel = "MEDIUM"
	RiskHigh     RiskLevel = "HIGH"
	RiskCritical RiskLevel = "CRITICAL"
)

// TokenStatus tracks the state of a token in a cluster.
type TokenStatus string

const (
	StatusAlive   TokenStatus = "ALIVE"
	StatusPumping TokenStatus = "PUMPING"
	StatusDumped  TokenStatus = "DUMPED"
	StatusRugged  TokenStatus = "RUGGED"
)

// TokenRef references a token in the cross-token state.
type TokenRef struct {
	Address    string      `json:"address"`
	LaunchedAt int64       `json:"launched_at"`
	Status     TokenStatus `json:"status"`
	PeakMcap   float64     `json:"peak_mcap"`
	CurrentMcap float64    `json:"current_mcap"`
}

// CrossTokenState tracks all tokens from the same deployer cluster.
type CrossTokenState struct {
	ClusterID     uint32             `json:"cluster_id"`
	ActiveTokens  []TokenRef         `json:"active_tokens"`
	TokenCount24h int                `json:"token_count_24h"`
	Pattern       CorrelationPattern `json:"pattern"`
	RiskLevel     RiskLevel          `json:"risk_level"`
	LastUpdated   int64              `json:"last_updated"`
}

// DetectorConfig configures the cross-token detector.
type DetectorConfig struct {
	RotationWindowH   int     `yaml:"rotation_window_h"`     // hours to track (default 6)
	SerialThreshold   int     `yaml:"serial_threshold"`      // >N tokens/24h = serial (default 3)
	RotationDropPct   float64 `yaml:"rotation_drop_pct"`     // mcap drop % to mark as DUMPED (default 50)
	MaxClustersTracked int    `yaml:"max_clusters_tracked"`  // max clusters in memory (default 5000)
}

// DefaultDetectorConfig returns production defaults.
func DefaultDetectorConfig() DetectorConfig {
	return DetectorConfig{
		RotationWindowH:    6,
		SerialThreshold:    3,
		RotationDropPct:    50,
		MaxClustersTracked: 5000,
	}
}

// Detector tracks cross-token correlations per deployer cluster.
type Detector struct {
	config DetectorConfig

	mu       sync.RWMutex
	clusters map[uint32]*CrossTokenState // clusterID → state
}

// NewDetector creates a new cross-token correlation detector.
func NewDetector(config DetectorConfig) *Detector {
	return &Detector{
		config:   config,
		clusters: make(map[uint32]*CrossTokenState),
	}
}

// RecordTokenLaunch registers a new token from a known cluster.
func (d *Detector) RecordTokenLaunch(clusterID uint32, tokenAddr string, mcap float64) {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	state, exists := d.clusters[clusterID]
	if !exists {
		if len(d.clusters) >= d.config.MaxClustersTracked {
			d.evictOldest()
		}
		state = &CrossTokenState{
			ClusterID:    clusterID,
			ActiveTokens: make([]TokenRef, 0, 5),
		}
		d.clusters[clusterID] = state
	}

	token := TokenRef{
		Address:     tokenAddr,
		LaunchedAt:  now.UnixNano(),
		Status:      StatusAlive,
		PeakMcap:    mcap,
		CurrentMcap: mcap,
	}

	state.ActiveTokens = append(state.ActiveTokens, token)
	state.LastUpdated = now.UnixNano()
	d.recalculate(state)
}

// UpdateTokenStatus updates the current mcap and status of a tracked token.
func (d *Detector) UpdateTokenStatus(clusterID uint32, tokenAddr string, currentMcap float64) {
	d.mu.Lock()
	defer d.mu.Unlock()

	state := d.clusters[clusterID]
	if state == nil {
		return
	}

	for i := range state.ActiveTokens {
		t := &state.ActiveTokens[i]
		if t.Address != tokenAddr {
			continue
		}

		t.CurrentMcap = currentMcap
		if currentMcap > t.PeakMcap {
			t.PeakMcap = currentMcap
		}

		// Detect status changes.
		if t.PeakMcap > 0 {
			dropPct := (t.PeakMcap - currentMcap) / t.PeakMcap * 100
			if dropPct >= d.config.RotationDropPct {
				t.Status = StatusDumped
			} else if currentMcap > t.PeakMcap*0.8 {
				t.Status = StatusPumping
			}
		}
		break
	}

	state.LastUpdated = time.Now().UnixNano()
	d.recalculate(state)
}

// MarkRugged marks a token as rugged in its cluster.
func (d *Detector) MarkRugged(clusterID uint32, tokenAddr string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	state := d.clusters[clusterID]
	if state == nil {
		return
	}

	for i := range state.ActiveTokens {
		if state.ActiveTokens[i].Address == tokenAddr {
			state.ActiveTokens[i].Status = StatusRugged
			break
		}
	}

	d.recalculate(state)
}

// recalculate updates pattern detection and risk level for a cluster.
// Must hold d.mu.
func (d *Detector) recalculate(state *CrossTokenState) {
	now := time.Now()
	windowCutoff := now.Add(-time.Duration(d.config.RotationWindowH) * time.Hour).UnixNano()
	cutoff24h := now.Add(-24 * time.Hour).UnixNano()

	// Count tokens in windows.
	var activeInWindow int
	var count24h int
	var dumpedCount, ruggedCount int
	var hasPumping, hasDumped bool

	for _, t := range state.ActiveTokens {
		if t.LaunchedAt >= cutoff24h {
			count24h++
		}
		if t.LaunchedAt >= windowCutoff {
			activeInWindow++
		}
		switch t.Status {
		case StatusDumped:
			dumpedCount++
			hasDumped = true
		case StatusRugged:
			ruggedCount++
		case StatusPumping:
			hasPumping = true
		}
	}

	state.TokenCount24h = count24h

	// Pattern detection (ordered by severity: lowest → highest, later overrides earlier).
	state.Pattern = PatternNone

	// DISTRACTION: 2+ tokens launched close together from same cluster (lowest severity).
	if activeInWindow >= 2 && !hasDumped && !hasPumping {
		state.Pattern = PatternDistraction
	}

	// ROTATION: one is pumping while another is dumped.
	if hasPumping && hasDumped && activeInWindow >= 2 {
		state.Pattern = PatternRotation
	}

	// SERIAL: >threshold tokens in 24h.
	if count24h >= d.config.SerialThreshold {
		state.Pattern = PatternSerial
	}

	// If all previous tokens are dead/rugged, that's SERIAL with highest risk.
	if ruggedCount >= 2 {
		state.Pattern = PatternSerial
	}

	// Risk level.
	state.RiskLevel = d.assessRisk(state, ruggedCount, dumpedCount, activeInWindow)

	if state.Pattern != PatternNone {
		log.Debug().
			Uint32("cluster", state.ClusterID).
			Str("pattern", string(state.Pattern)).
			Str("risk", string(state.RiskLevel)).
			Int("tokens_24h", count24h).
			Msg("correlation: pattern detected")
	}
}

// assessRisk determines the risk level based on cluster state.
func (d *Detector) assessRisk(state *CrossTokenState, ruggedCount, dumpedCount, activeInWindow int) RiskLevel {
	// All previous tokens rugged = CRITICAL.
	if ruggedCount >= 2 {
		return RiskCritical
	}

	// Rotation confirmed = HIGH.
	if state.Pattern == PatternRotation {
		return RiskHigh
	}

	// Serial launch pattern = HIGH.
	if state.TokenCount24h >= d.config.SerialThreshold+2 {
		return RiskCritical
	}
	if state.Pattern == PatternSerial {
		return RiskHigh
	}

	// Multiple tokens in window = MEDIUM.
	if activeInWindow >= 2 {
		return RiskMedium
	}

	return RiskLow
}

// GetClusterState returns the cross-token state for a cluster.
func (d *Detector) GetClusterState(clusterID uint32) *CrossTokenState {
	d.mu.RLock()
	defer d.mu.RUnlock()

	state := d.clusters[clusterID]
	if state == nil {
		return nil
	}
	// Return a copy.
	cp := *state
	cp.ActiveTokens = make([]TokenRef, len(state.ActiveTokens))
	copy(cp.ActiveTokens, state.ActiveTokens)
	return &cp
}

// ScoreImpact returns the ENTITY_SCORE adjustment for cross-token correlation.
func (state *CrossTokenState) ScoreImpact() float64 {
	if state == nil {
		return 0
	}

	switch {
	case state.RiskLevel == RiskCritical:
		return -100 // INSTANT_KILL equivalent
	case state.Pattern == PatternRotation:
		return -40
	case state.Pattern == PatternSerial && state.TokenCount24h >= 3:
		return -25
	case state.Pattern == PatternDistraction:
		return -10
	default:
		return 0
	}
}

// ShouldPanicSell returns true if cross-token state demands immediate exit.
func (state *CrossTokenState) ShouldPanicSell(heldTokenAddr string) bool {
	if state == nil {
		return false
	}

	// Rotation confirmed and our token is being dumped while another pumps.
	if state.Pattern == PatternRotation {
		for _, t := range state.ActiveTokens {
			if t.Address == heldTokenAddr && t.Status == StatusDumped {
				return true
			}
		}
	}

	// Critical risk from serial rugs.
	if state.RiskLevel == RiskCritical {
		return true
	}

	return false
}

// evictOldest removes the least recently updated cluster.
// Must hold d.mu.
func (d *Detector) evictOldest() {
	var oldestID uint32
	var oldestTime int64 = 1<<63 - 1

	for id, state := range d.clusters {
		if state.LastUpdated < oldestTime {
			oldestID = id
			oldestTime = state.LastUpdated
		}
	}
	delete(d.clusters, oldestID)
}

// Cleanup removes clusters with no activity in the given window.
func (d *Detector) Cleanup(maxAge time.Duration) int {
	cutoff := time.Now().Add(-maxAge).UnixNano()

	d.mu.Lock()
	defer d.mu.Unlock()

	removed := 0
	for id, state := range d.clusters {
		if state.LastUpdated < cutoff {
			delete(d.clusters, id)
			removed++
		}
	}

	if removed > 0 {
		log.Debug().Int("removed", removed).Msg("correlation: cleaned up stale clusters")
	}
	return removed
}

// Stats returns detector statistics.
type DetectorStats struct {
	TrackedClusters int            `json:"tracked_clusters"`
	Patterns        map[string]int `json:"patterns"`
}

func (d *Detector) Stats() DetectorStats {
	d.mu.RLock()
	defer d.mu.RUnlock()

	patterns := make(map[string]int)
	for _, state := range d.clusters {
		if state.Pattern != PatternNone {
			patterns[string(state.Pattern)]++
		}
	}

	return DetectorStats{
		TrackedClusters: len(d.clusters),
		Patterns:        patterns,
	}
}
