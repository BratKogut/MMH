package honeypot

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// ---------------------------------------------------------------------------
// Honeypot Evolution Tracker — v3.2 Module
// "Każda strata uczy system — nigdy nie przegraj na tym samym"
// Learns new honeypot patterns from rugs, hot-reloads pattern DB.
// ---------------------------------------------------------------------------

// Signature represents a learned honeypot pattern.
type Signature struct {
	PatternID    string  `json:"pattern_id"`
	Chain        string  `json:"chain"`          // "solana", "evm"
	SigType      string  `json:"signature_type"` // "bytecode", "instruction", "behavior"
	Pattern      []byte  `json:"pattern"`
	Description  string  `json:"description"`
	Confidence   float64 `json:"confidence"` // 0-1, based on hit count
	HitCount     int     `json:"hit_count"`
	FirstSeen    int64   `json:"first_seen"`
	LastSeen     int64   `json:"last_seen"`
	SampleTxHash string  `json:"sample_tx_hash"`
	AutoDetected bool    `json:"auto_detected"`
}

// RugSample records details of a detected rug for pattern extraction.
type RugSample struct {
	TokenAddress  string  `json:"token_address"`
	Chain         string  `json:"chain"`
	DeployerAddr  string  `json:"deployer_address"`
	ContractData  []byte  `json:"contract_data"`  // full bytecode/account data
	RugType       string  `json:"rug_type"`       // honeypot, liquidity_pull, tax_increase, ownership_exploit
	LossAmount    float64 `json:"loss_amount"`
	DetectedBy    string  `json:"detected_by"`    // CSM, manual, auto_loss
	Timestamp     int64   `json:"timestamp"`
	MatchedPattern string `json:"matched_pattern"` // pattern_id if matched existing
}

// EvolutionConfig configures the honeypot evolution tracker.
type EvolutionConfig struct {
	MinConfidence      float64 `yaml:"min_confidence"`       // min confidence to add pattern (default 0.3)
	MinHitsForHighConf int     `yaml:"min_hits_for_high"`    // hits needed for confidence 0.9 (default 5)
	MaxPatterns        int     `yaml:"max_patterns"`         // max patterns in DB (default 2000)
	PatternMinBytes    int     `yaml:"pattern_min_bytes"`    // min pattern size (default 8)
	RetroactiveScan    bool    `yaml:"retroactive_scan"`     // scan open positions on new pattern
}

// DefaultEvolutionConfig returns production defaults.
func DefaultEvolutionConfig() EvolutionConfig {
	return EvolutionConfig{
		MinConfidence:      0.3,
		MinHitsForHighConf: 5,
		MaxPatterns:        2000,
		PatternMinBytes:    8,
		RetroactiveScan:    true,
	}
}

// Tracker learns and manages honeypot patterns.
type Tracker struct {
	config EvolutionConfig

	mu         sync.RWMutex
	signatures map[string]*Signature // patternID → Signature
	samples    []RugSample           // recent rug samples

	// Callbacks.
	onNewPattern   func(sig *Signature)
	onRetroHit     func(tokenAddr string, sig *Signature)

	// Stats.
	totalSamples     atomic.Int64
	autoPatterns     atomic.Int64
	retroCatches     atomic.Int64
}

// NewTracker creates a new Honeypot Evolution Tracker.
func NewTracker(config EvolutionConfig) *Tracker {
	return &Tracker{
		config:     config,
		signatures: make(map[string]*Signature, 500),
		samples:    make([]RugSample, 0, 100),
	}
}

// SetOnNewPattern sets callback for when a new pattern is discovered.
func (t *Tracker) SetOnNewPattern(fn func(sig *Signature)) {
	t.mu.Lock()
	t.onNewPattern = fn
	t.mu.Unlock()
}

// SetOnRetroactiveHit sets callback for retroactive scan matches.
func (t *Tracker) SetOnRetroactiveHit(fn func(tokenAddr string, sig *Signature)) {
	t.mu.Lock()
	t.onRetroHit = fn
	t.mu.Unlock()
}

// RecordRug processes a rug event and attempts to extract/match a pattern.
func (t *Tracker) RecordRug(sample RugSample) *Signature {
	t.totalSamples.Add(1)
	sample.Timestamp = time.Now().UnixNano()

	t.mu.Lock()

	// Store sample (ring buffer).
	if len(t.samples) >= 500 {
		t.samples = t.samples[1:]
	}
	t.samples = append(t.samples, sample)

	// Try to match against existing patterns.
	if len(sample.ContractData) >= t.config.PatternMinBytes {
		for _, sig := range t.signatures {
			if t.matchPattern(sample.ContractData, sig.Pattern) {
				sig.HitCount++
				sig.LastSeen = sample.Timestamp
				sig.Confidence = t.computeConfidence(sig.HitCount)
				sample.MatchedPattern = sig.PatternID
				t.mu.Unlock()

				log.Info().
					Str("pattern", sig.PatternID).
					Int("hits", sig.HitCount).
					Float64("confidence", sig.Confidence).
					Msg("honeypot_evolution: existing pattern matched")

				return sig
			}
		}
	}

	// No match — attempt to extract a new pattern.
	newSig := t.extractPattern(sample)
	if newSig != nil {
		t.signatures[newSig.PatternID] = newSig
		t.autoPatterns.Add(1)
		cb := t.onNewPattern
		t.mu.Unlock()

		log.Info().
			Str("pattern", newSig.PatternID).
			Str("description", newSig.Description).
			Msg("honeypot_evolution: NEW pattern extracted")

		if cb != nil {
			cb(newSig)
		}
		return newSig
	}

	t.mu.Unlock()
	return nil
}

// extractPattern attempts to extract a signature from contract data.
// Must hold t.mu.
func (t *Tracker) extractPattern(sample RugSample) *Signature {
	if len(sample.ContractData) < t.config.PatternMinBytes {
		return nil
	}

	// Extract a fingerprint from the contract data.
	// Strategy: take a hash of meaningful sections as the pattern.
	hash := sha256.Sum256(sample.ContractData)
	patternBytes := hash[:16] // 16-byte fingerprint

	// Check for duplicate fingerprint.
	patternID := fmt.Sprintf("HP-%s-%s", sample.Chain, hex.EncodeToString(patternBytes[:4]))
	if _, exists := t.signatures[patternID]; exists {
		return nil
	}

	if len(t.signatures) >= t.config.MaxPatterns {
		return nil // at capacity
	}

	return &Signature{
		PatternID:    patternID,
		Chain:        sample.Chain,
		SigType:      "bytecode_hash",
		Pattern:      patternBytes,
		Description:  fmt.Sprintf("Auto-extracted from %s (%s)", sample.RugType, sample.TokenAddress),
		Confidence:   t.config.MinConfidence,
		HitCount:     1,
		FirstSeen:    sample.Timestamp,
		LastSeen:     sample.Timestamp,
		SampleTxHash: sample.TokenAddress,
		AutoDetected: true,
	}
}

// matchPattern checks if contractData contains the signature pattern.
func (t *Tracker) matchPattern(contractData, pattern []byte) bool {
	if len(pattern) == 0 || len(contractData) < len(pattern) {
		return false
	}

	// For hash-based signatures, hash the data and compare prefix.
	hash := sha256.Sum256(contractData)
	if len(pattern) > len(hash) {
		return false
	}
	for i := 0; i < len(pattern); i++ {
		if hash[i] != pattern[i] {
			return false
		}
	}
	return true
}

// computeConfidence returns confidence based on hit count.
func (t *Tracker) computeConfidence(hitCount int) float64 {
	if hitCount >= t.config.MinHitsForHighConf {
		return 0.9
	}
	// Linear from MinConfidence to 0.9.
	return t.config.MinConfidence + float64(hitCount-1)*
		(0.9-t.config.MinConfidence)/float64(t.config.MinHitsForHighConf-1)
}

// CheckContract checks if contract data matches any known honeypot pattern.
func (t *Tracker) CheckContract(contractData []byte) *Signature {
	if len(contractData) < t.config.PatternMinBytes {
		return nil
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	for _, sig := range t.signatures {
		if t.matchPattern(contractData, sig.Pattern) {
			return sig
		}
	}
	return nil
}

// RetroactiveScan scans a list of open positions against all known patterns.
// Returns tokens that match new patterns.
func (t *Tracker) RetroactiveScan(positions []RetroPosition) []RetroMatch {
	t.mu.RLock()
	sigs := make([]*Signature, 0, len(t.signatures))
	for _, sig := range t.signatures {
		sigs = append(sigs, sig)
	}
	retroCb := t.onRetroHit
	t.mu.RUnlock()

	var matches []RetroMatch
	for _, pos := range positions {
		if len(pos.ContractData) < t.config.PatternMinBytes {
			continue
		}
		for _, sig := range sigs {
			if t.matchPattern(pos.ContractData, sig.Pattern) {
				match := RetroMatch{
					TokenAddress: pos.TokenAddress,
					PatternID:    sig.PatternID,
					Confidence:   sig.Confidence,
				}
				matches = append(matches, match)
				t.retroCatches.Add(1)

				if retroCb != nil {
					retroCb(pos.TokenAddress, sig)
				}
				break // one match per position is enough
			}
		}
	}

	if len(matches) > 0 {
		log.Warn().
			Int("matches", len(matches)).
			Msg("honeypot_evolution: retroactive scan found matches!")
	}
	return matches
}

// RetroPosition represents an open position for retroactive scanning.
type RetroPosition struct {
	TokenAddress string
	ContractData []byte
}

// RetroMatch indicates a retroactive pattern match.
type RetroMatch struct {
	TokenAddress string  `json:"token_address"`
	PatternID    string  `json:"pattern_id"`
	Confidence   float64 `json:"confidence"`
}

// GetPatterns returns all known patterns.
func (t *Tracker) GetPatterns() []Signature {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make([]Signature, 0, len(t.signatures))
	for _, sig := range t.signatures {
		result = append(result, *sig)
	}
	return result
}

// AddPattern manually adds a pattern to the tracker.
func (t *Tracker) AddPattern(sig Signature) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if sig.FirstSeen == 0 {
		sig.FirstSeen = time.Now().UnixNano()
	}
	t.signatures[sig.PatternID] = &sig
}

// PatternCount returns the number of known patterns.
func (t *Tracker) PatternCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.signatures)
}

// Stats returns tracker statistics.
type TrackerStats struct {
	TotalPatterns    int   `json:"total_patterns"`
	AutoPatterns     int64 `json:"auto_patterns"`
	TotalSamples     int64 `json:"total_samples"`
	RetroCatches     int64 `json:"retro_catches"`
}

func (t *Tracker) Stats() TrackerStats {
	return TrackerStats{
		TotalPatterns: t.PatternCount(),
		AutoPatterns:  t.autoPatterns.Load(),
		TotalSamples:  t.totalSamples.Load(),
		RetroCatches:  t.retroCatches.Load(),
	}
}
