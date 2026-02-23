package narrative

import (
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// ---------------------------------------------------------------------------
// Narrative Momentum Engine — v3.2 Module
// "Nie kupuj tokenu — kupuj narrację w odpowiednim momencie"
// Tracks narrative lifecycle: EMERGING → GROWING → PEAK → DECLINING → DEAD
// ---------------------------------------------------------------------------

// Phase represents the lifecycle stage of a narrative.
type Phase int

const (
	PhaseUnknown   Phase = iota
	PhaseEmerging        // New narrative, <10 tokens, high performance
	PhaseGrowing         // Accelerating, sweet spot for entry
	PhasePeak            // Crowded, highest token count
	PhaseDeclining       // Performance dropping
	PhaseDead            // <5 tokens/24h, negative avg performance
)

func (p Phase) String() string {
	switch p {
	case PhaseEmerging:
		return "EMERGING"
	case PhaseGrowing:
		return "GROWING"
	case PhasePeak:
		return "PEAK"
	case PhaseDeclining:
		return "DECLINING"
	case PhaseDead:
		return "DEAD"
	default:
		return "UNKNOWN"
	}
}

// Velocity indicates the rate of change.
type Velocity int

const (
	VelocityStable       Velocity = iota
	VelocityAccelerating
	VelocityDecelerating
)

func (v Velocity) String() string {
	switch v {
	case VelocityAccelerating:
		return "ACCELERATING"
	case VelocityDecelerating:
		return "DECELERATING"
	default:
		return "STABLE"
	}
}

// NarrativeState tracks the current state of a narrative.
type NarrativeState struct {
	Name           string  `json:"name"`
	Phase          Phase   `json:"phase"`
	TokenCount1h   int     `json:"token_count_1h"`
	TokenCount6h   int     `json:"token_count_6h"`
	TokenCount24h  int     `json:"token_count_24h"`
	AvgPerformance float64 `json:"avg_performance"` // avg return % of tokens
	BestPerformer  string  `json:"best_performer"`  // token address
	Velocity       Velocity `json:"velocity"`
	DetectedAt     int64   `json:"detected_at"`
	LastUpdated    int64   `json:"last_updated"`
}

// NarrativeKeywords defines keyword groups per narrative.
type NarrativeKeywords struct {
	Name     string   `yaml:"name"`
	Keywords []string `yaml:"keywords"`
}

// DefaultNarratives returns the built-in narrative keyword database.
func DefaultNarratives() []NarrativeKeywords {
	return []NarrativeKeywords{
		{Name: "AI_AGENTS", Keywords: []string{"ai", "agent", "gpt", "neural", "llm", "openai", "chatgpt", "claude"}},
		{Name: "POLITICAL", Keywords: []string{"trump", "biden", "maga", "vote", "election", "president", "political"}},
		{Name: "ANIMAL_MEME", Keywords: []string{"dog", "cat", "pepe", "frog", "shib", "doge", "floki", "inu", "penguin"}},
		{Name: "CELEBRITY", Keywords: []string{"elon", "drake", "kanye", "celebrity", "influencer", "famous"}},
		{Name: "DEFI_META", Keywords: []string{"yield", "farm", "stake", "restake", "liquid", "lsd", "lst"}},
		{Name: "GAMING", Keywords: []string{"game", "play", "nft", "metaverse", "virtual", "pixel"}},
		{Name: "RWA", Keywords: []string{"rwa", "real world", "asset", "tokenized", "treasury"}},
	}
}

// tokenEvent records a token launch/update within a narrative.
type tokenEvent struct {
	tokenAddr   string
	narrative   string
	performance float64 // % return since launch
	launchedAt  time.Time
}

// EngineConfig configures the narrative engine.
type EngineConfig struct {
	EmergingMinTokens1h int     `yaml:"emerging_min_tokens_1h"` // min tokens/1h for EMERGING
	GrowingMinTokens1h  int     `yaml:"growing_min_tokens_1h"`
	PeakMinTokens1h     int     `yaml:"peak_min_tokens_1h"`
	DeadMaxTokens24h    int     `yaml:"dead_max_tokens_24h"`
	EmergingMinPerf     float64 `yaml:"emerging_min_perf"`      // min avg performance % for EMERGING
	GrowingMinPerf      float64 `yaml:"growing_min_perf"`
	MaxNarratives       int     `yaml:"max_narratives"`         // max tracked narratives
}

// DefaultEngineConfig returns production defaults per v3.2 spec.
func DefaultEngineConfig() EngineConfig {
	return EngineConfig{
		EmergingMinTokens1h: 2,
		GrowingMinTokens1h:  5,
		PeakMinTokens1h:     15,
		DeadMaxTokens24h:    5,
		EmergingMinPerf:     100, // +100% avg
		GrowingMinPerf:      50,
		MaxNarratives:       50,
	}
}

// Engine tracks narrative momentum across all active narratives.
type Engine struct {
	config     EngineConfig
	keywords   []NarrativeKeywords
	keywordMap map[string]string // lowered keyword → narrative name

	mu         sync.RWMutex
	narratives map[string]*NarrativeState // name → state
	tokens     []tokenEvent              // ring buffer of recent tokens
}

// NewEngine creates a new Narrative Momentum Engine.
func NewEngine(config EngineConfig, keywords []NarrativeKeywords) *Engine {
	km := make(map[string]string)
	for _, nk := range keywords {
		for _, kw := range nk.Keywords {
			km[strings.ToLower(kw)] = nk.Name
		}
	}

	return &Engine{
		config:     config,
		keywords:   keywords,
		keywordMap: km,
		narratives: make(map[string]*NarrativeState, config.MaxNarratives),
		tokens:     make([]tokenEvent, 0, 1000),
	}
}

// ClassifyToken detects which narrative(s) a token belongs to based on name/symbol.
func (e *Engine) ClassifyToken(name, symbol string) string {
	combined := strings.ToLower(name + " " + symbol)
	for kw, narrative := range e.keywordMap {
		if strings.Contains(combined, kw) {
			return narrative
		}
	}
	return ""
}

// RecordToken registers a new token launch for narrative tracking.
func (e *Engine) RecordToken(tokenAddr, name, symbol string, performance float64) {
	narrative := e.ClassifyToken(name, symbol)
	if narrative == "" {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	event := tokenEvent{
		tokenAddr:   tokenAddr,
		narrative:   narrative,
		performance: performance,
		launchedAt:  time.Now(),
	}

	// Ring buffer for tokens.
	if len(e.tokens) >= 5000 {
		e.tokens = e.tokens[1:]
	}
	e.tokens = append(e.tokens, event)
}

// UpdatePerformance updates the performance of a previously recorded token.
func (e *Engine) UpdatePerformance(tokenAddr string, performance float64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for i := len(e.tokens) - 1; i >= 0; i-- {
		if e.tokens[i].tokenAddr == tokenAddr {
			e.tokens[i].performance = performance
			break
		}
	}
}

// Refresh recalculates all narrative states. Call periodically (every 5-30 min).
func (e *Engine) Refresh() {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()
	cutoff1h := now.Add(-1 * time.Hour)
	cutoff6h := now.Add(-6 * time.Hour)
	cutoff24h := now.Add(-24 * time.Hour)

	// Aggregate per narrative.
	type stats struct {
		count1h, count6h, count24h int
		perfSum                    float64
		perfCount                  int
		bestPerf                   float64
		bestAddr                   string
		prevCount1h                int // for velocity
	}
	agg := make(map[string]*stats)

	for _, t := range e.tokens {
		if t.launchedAt.Before(cutoff24h) {
			continue
		}

		s, ok := agg[t.narrative]
		if !ok {
			s = &stats{}
			agg[t.narrative] = s
		}

		s.count24h++
		if t.launchedAt.After(cutoff6h) {
			s.count6h++
		}
		if t.launchedAt.After(cutoff1h) {
			s.count1h++
		}
		s.perfSum += t.performance
		s.perfCount++
		if t.performance > s.bestPerf {
			s.bestPerf = t.performance
			s.bestAddr = t.tokenAddr
		}
	}

	// Update/create narrative states.
	for name, s := range agg {
		ns, exists := e.narratives[name]
		if !exists {
			ns = &NarrativeState{
				Name:       name,
				DetectedAt: now.UnixNano(),
			}
			e.narratives[name] = ns
		}

		// Velocity: compare current vs previous 1h count.
		prevCount := ns.TokenCount1h
		ns.TokenCount1h = s.count1h
		ns.TokenCount6h = s.count6h
		ns.TokenCount24h = s.count24h
		ns.LastUpdated = now.UnixNano()

		if s.perfCount > 0 {
			ns.AvgPerformance = s.perfSum / float64(s.perfCount)
		}
		ns.BestPerformer = s.bestAddr

		// Velocity detection.
		if s.count1h > prevCount+2 {
			ns.Velocity = VelocityAccelerating
		} else if s.count1h < prevCount-2 {
			ns.Velocity = VelocityDecelerating
		} else {
			ns.Velocity = VelocityStable
		}

		// Phase detection per spec.
		ns.Phase = e.detectPhase(ns)
	}

	// Clean up dead narratives with no recent activity.
	for name, ns := range e.narratives {
		if ns.TokenCount24h == 0 {
			delete(e.narratives, name)
		}
	}

	log.Debug().Int("narratives", len(e.narratives)).Msg("narrative: refreshed")
}

// detectPhase classifies the narrative lifecycle phase.
func (e *Engine) detectPhase(ns *NarrativeState) Phase {
	cfg := e.config

	// DEAD: very few tokens and negative performance.
	if ns.TokenCount24h <= cfg.DeadMaxTokens24h && ns.AvgPerformance < 0 {
		return PhaseDead
	}

	// EMERGING: small token count but high performance.
	if ns.TokenCount1h >= cfg.EmergingMinTokens1h &&
		ns.TokenCount6h < cfg.GrowingMinTokens1h*3 &&
		ns.AvgPerformance >= cfg.EmergingMinPerf {
		return PhaseEmerging
	}

	// PEAK: very high token count or decelerating from high.
	if ns.TokenCount1h >= cfg.PeakMinTokens1h ||
		(ns.TokenCount1h >= cfg.GrowingMinTokens1h*2 && ns.Velocity == VelocityDecelerating) {
		return PhasePeak
	}

	// GROWING: medium token count, accelerating, decent performance.
	if ns.TokenCount1h >= cfg.GrowingMinTokens1h &&
		ns.Velocity == VelocityAccelerating &&
		ns.AvgPerformance >= cfg.GrowingMinPerf {
		return PhaseGrowing
	}

	// DECLINING: token count declining, low performance.
	if ns.Velocity == VelocityDecelerating && ns.AvgPerformance < cfg.GrowingMinPerf {
		return PhaseDeclining
	}

	return PhaseUnknown
}

// GetNarrative returns the state for a specific narrative.
func (e *Engine) GetNarrative(name string) *NarrativeState {
	e.mu.RLock()
	defer e.mu.RUnlock()
	ns := e.narratives[name]
	if ns == nil {
		return nil
	}
	copy := *ns
	return &copy
}

// GetTokenNarrative returns the narrative state for a token by name/symbol.
func (e *Engine) GetTokenNarrative(name, symbol string) *NarrativeState {
	narr := e.ClassifyToken(name, symbol)
	if narr == "" {
		return nil
	}
	return e.GetNarrative(narr)
}

// GetActive returns all active narrative states.
func (e *Engine) GetActive() []NarrativeState {
	e.mu.RLock()
	defer e.mu.RUnlock()

	result := make([]NarrativeState, 0, len(e.narratives))
	for _, ns := range e.narratives {
		result = append(result, *ns)
	}
	return result
}

// ScoreImpact returns the TIMING_SCORE adjustment for a token's narrative.
func ScoreImpact(ns *NarrativeState) float64 {
	if ns == nil {
		return 0
	}
	switch ns.Phase {
	case PhaseEmerging:
		return 15
	case PhaseGrowing:
		return 10
	case PhasePeak:
		return 0  // crowded
	case PhaseDeclining:
		return -15
	case PhaseDead:
		return -25
	default:
		return 0
	}
}

// Stats returns engine statistics.
type EngineStats struct {
	ActiveNarratives int              `json:"active_narratives"`
	TotalTokens      int              `json:"total_tokens"`
	Phases           map[string]int   `json:"phases"`
}

func (e *Engine) Stats() EngineStats {
	e.mu.RLock()
	defer e.mu.RUnlock()

	phases := make(map[string]int)
	for _, ns := range e.narratives {
		phases[ns.Phase.String()]++
	}

	return EngineStats{
		ActiveNarratives: len(e.narratives),
		TotalTokens:      len(e.tokens),
		Phases:           phases,
	}
}
