package narrative

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestPhase_String(t *testing.T) {
	assert.Equal(t, "UNKNOWN", PhaseUnknown.String())
	assert.Equal(t, "EMERGING", PhaseEmerging.String())
	assert.Equal(t, "GROWING", PhaseGrowing.String())
	assert.Equal(t, "PEAK", PhasePeak.String())
	assert.Equal(t, "DECLINING", PhaseDeclining.String())
	assert.Equal(t, "DEAD", PhaseDead.String())
}

func TestVelocity_String(t *testing.T) {
	assert.Equal(t, "STABLE", VelocityStable.String())
	assert.Equal(t, "ACCELERATING", VelocityAccelerating.String())
	assert.Equal(t, "DECELERATING", VelocityDecelerating.String())
}

func TestNewEngine(t *testing.T) {
	e := NewEngine(DefaultEngineConfig(), DefaultNarratives())
	assert.NotNil(t, e)
	assert.NotEmpty(t, e.keywordMap)
	assert.Equal(t, 7, len(e.keywords))
}

func TestClassifyToken(t *testing.T) {
	e := NewEngine(DefaultEngineConfig(), DefaultNarratives())

	tests := []struct {
		name     string
		symbol   string
		expected string
	}{
		{"AI Agent Token", "AGENT", "AI_AGENTS"},
		{"TrumpCoin", "TRUMP", "POLITICAL"},
		{"DogeMoon", "DOGE", "ANIMAL_MEME"},
		{"ElonGold", "ELON", "CELEBRITY"},
		{"YieldFarm", "YIELD", "DEFI_META"},
		{"GameFi", "GAME", "GAMING"},
		{"RWA Token", "RWA", "RWA"},
		{"RandomCoin", "XYZ", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := e.ClassifyToken(tt.name, tt.symbol)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestRecordToken(t *testing.T) {
	e := NewEngine(DefaultEngineConfig(), DefaultNarratives())

	e.RecordToken("addr1", "AI Bot", "BOT", 150.0)
	e.RecordToken("addr2", "Dog Coin", "DOG", 200.0)
	e.RecordToken("addr3", "Random", "RND", 50.0) // no narrative match

	e.mu.RLock()
	assert.Equal(t, 2, len(e.tokens)) // only 2 matched narratives
	e.mu.RUnlock()
}

func TestRecordToken_RingBuffer(t *testing.T) {
	e := NewEngine(DefaultEngineConfig(), DefaultNarratives())

	// Fill beyond 5000 limit.
	for i := 0; i < 5002; i++ {
		e.RecordToken("addr", "AI Token", "AI", float64(i))
	}

	e.mu.RLock()
	assert.LessOrEqual(t, len(e.tokens), 5000)
	e.mu.RUnlock()
}

func TestUpdatePerformance(t *testing.T) {
	e := NewEngine(DefaultEngineConfig(), DefaultNarratives())

	e.RecordToken("addr1", "AI Bot", "BOT", 100.0)
	e.UpdatePerformance("addr1", 250.0)

	e.mu.RLock()
	assert.Equal(t, 250.0, e.tokens[0].performance)
	e.mu.RUnlock()
}

func TestUpdatePerformance_NotFound(t *testing.T) {
	e := NewEngine(DefaultEngineConfig(), DefaultNarratives())
	// Should not panic.
	e.UpdatePerformance("nonexistent", 100.0)
}

func TestRefresh_CreatesNarrativeStates(t *testing.T) {
	e := NewEngine(DefaultEngineConfig(), DefaultNarratives())

	// Record tokens in AI_AGENTS narrative.
	for i := 0; i < 5; i++ {
		e.RecordToken("addr_ai_"+string(rune('a'+i)), "AI Bot", "BOT", 120.0)
	}

	e.Refresh()

	ns := e.GetNarrative("AI_AGENTS")
	assert.NotNil(t, ns)
	assert.Equal(t, 5, ns.TokenCount1h)
	assert.Equal(t, 5, ns.TokenCount24h)
	assert.Equal(t, 120.0, ns.AvgPerformance)
}

func TestRefresh_RemovesDeadNarratives(t *testing.T) {
	e := NewEngine(DefaultEngineConfig(), DefaultNarratives())

	// Add old tokens that are beyond 24h.
	e.mu.Lock()
	e.tokens = append(e.tokens, tokenEvent{
		tokenAddr:   "old_addr",
		narrative:   "AI_AGENTS",
		performance: 50,
		launchedAt:  time.Now().Add(-25 * time.Hour),
	})
	e.narratives["AI_AGENTS"] = &NarrativeState{Name: "AI_AGENTS"}
	e.mu.Unlock()

	e.Refresh()

	// Should be removed since count24h=0.
	ns := e.GetNarrative("AI_AGENTS")
	assert.Nil(t, ns)
}

func TestDetectPhase_Emerging(t *testing.T) {
	cfg := DefaultEngineConfig()
	e := NewEngine(cfg, DefaultNarratives())

	// EMERGING: small count but high performance.
	for i := 0; i < 3; i++ {
		e.RecordToken("addr"+string(rune('a'+i)), "AI Bot", "BOT", 150.0)
	}

	e.Refresh()

	ns := e.GetNarrative("AI_AGENTS")
	assert.NotNil(t, ns)
	assert.Equal(t, PhaseEmerging, ns.Phase)
}

func TestDetectPhase_Peak(t *testing.T) {
	cfg := DefaultEngineConfig()
	e := NewEngine(cfg, DefaultNarratives())

	// PEAK: >=15 tokens in 1h.
	for i := 0; i < 16; i++ {
		e.RecordToken("addr"+string(rune(i+65)), "AI Bot", "BOT", 50.0)
	}

	e.Refresh()

	ns := e.GetNarrative("AI_AGENTS")
	assert.NotNil(t, ns)
	assert.Equal(t, PhasePeak, ns.Phase)
}

func TestGetTokenNarrative(t *testing.T) {
	e := NewEngine(DefaultEngineConfig(), DefaultNarratives())

	e.RecordToken("addr1", "AI Bot", "BOT", 100.0)
	e.Refresh()

	ns := e.GetTokenNarrative("AI Bot", "BOT")
	assert.NotNil(t, ns)
	assert.Equal(t, "AI_AGENTS", ns.Name)

	// No match.
	ns2 := e.GetTokenNarrative("Random", "RND")
	assert.Nil(t, ns2)
}

func TestGetActive(t *testing.T) {
	e := NewEngine(DefaultEngineConfig(), DefaultNarratives())

	e.RecordToken("addr1", "AI Bot", "BOT", 100.0)
	e.RecordToken("addr2", "Dog Coin", "DOG", 200.0)
	e.Refresh()

	active := e.GetActive()
	assert.Equal(t, 2, len(active))
}

func TestScoreImpact(t *testing.T) {
	tests := []struct {
		phase    Phase
		expected float64
	}{
		{PhaseEmerging, 15},
		{PhaseGrowing, 10},
		{PhasePeak, 0},
		{PhaseDeclining, -15},
		{PhaseDead, -25},
		{PhaseUnknown, 0},
	}

	for _, tt := range tests {
		ns := &NarrativeState{Phase: tt.phase}
		assert.Equal(t, tt.expected, ScoreImpact(ns), "phase=%s", tt.phase)
	}

	// Nil should return 0.
	assert.Equal(t, 0.0, ScoreImpact(nil))
}

func TestStats(t *testing.T) {
	e := NewEngine(DefaultEngineConfig(), DefaultNarratives())

	e.RecordToken("addr1", "AI Bot", "BOT", 100.0)
	e.Refresh()

	stats := e.Stats()
	assert.Equal(t, 1, stats.ActiveNarratives)
	assert.Equal(t, 1, stats.TotalTokens)
	assert.NotEmpty(t, stats.Phases)
}

func TestDefaultEngineConfig(t *testing.T) {
	cfg := DefaultEngineConfig()
	assert.Equal(t, 2, cfg.EmergingMinTokens1h)
	assert.Equal(t, 5, cfg.GrowingMinTokens1h)
	assert.Equal(t, 15, cfg.PeakMinTokens1h)
	assert.Equal(t, 5, cfg.DeadMaxTokens24h)
	assert.Equal(t, 100.0, cfg.EmergingMinPerf)
	assert.Equal(t, 50.0, cfg.GrowingMinPerf)
	assert.Equal(t, 50, cfg.MaxNarratives)
}

func TestDefaultNarratives(t *testing.T) {
	narrs := DefaultNarratives()
	assert.Equal(t, 7, len(narrs))
	assert.Equal(t, "AI_AGENTS", narrs[0].Name)
}
