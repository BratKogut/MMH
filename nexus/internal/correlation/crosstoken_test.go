package correlation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNewDetector(t *testing.T) {
	d := NewDetector(DefaultDetectorConfig())
	assert.NotNil(t, d)
	stats := d.Stats()
	assert.Equal(t, 0, stats.TrackedClusters)
}

func TestDefaultDetectorConfig(t *testing.T) {
	cfg := DefaultDetectorConfig()
	assert.Equal(t, 6, cfg.RotationWindowH)
	assert.Equal(t, 3, cfg.SerialThreshold)
	assert.Equal(t, 50.0, cfg.RotationDropPct)
	assert.Equal(t, 5000, cfg.MaxClustersTracked)
}

func TestRecordTokenLaunch(t *testing.T) {
	d := NewDetector(DefaultDetectorConfig())

	d.RecordTokenLaunch(1, "token1", 1000)

	state := d.GetClusterState(1)
	assert.NotNil(t, state)
	assert.Equal(t, uint32(1), state.ClusterID)
	assert.Equal(t, 1, len(state.ActiveTokens))
	assert.Equal(t, "token1", state.ActiveTokens[0].Address)
	assert.Equal(t, StatusAlive, state.ActiveTokens[0].Status)
}

func TestRecordTokenLaunch_MultipleTokens(t *testing.T) {
	d := NewDetector(DefaultDetectorConfig())

	d.RecordTokenLaunch(1, "token1", 1000)
	d.RecordTokenLaunch(1, "token2", 2000)
	d.RecordTokenLaunch(1, "token3", 3000)

	state := d.GetClusterState(1)
	assert.Equal(t, 3, len(state.ActiveTokens))
	assert.Equal(t, 3, state.TokenCount24h)
}

func TestUpdateTokenStatus_PeakTracking(t *testing.T) {
	d := NewDetector(DefaultDetectorConfig())

	d.RecordTokenLaunch(1, "token1", 1000)

	// Price goes up.
	d.UpdateTokenStatus(1, "token1", 2000)

	state := d.GetClusterState(1)
	assert.Equal(t, 2000.0, state.ActiveTokens[0].PeakMcap)
	assert.Equal(t, 2000.0, state.ActiveTokens[0].CurrentMcap)
	assert.Equal(t, StatusPumping, state.ActiveTokens[0].Status)
}

func TestUpdateTokenStatus_Dumped(t *testing.T) {
	d := NewDetector(DefaultDetectorConfig())

	d.RecordTokenLaunch(1, "token1", 1000)
	d.UpdateTokenStatus(1, "token1", 2000) // peak = 2000
	d.UpdateTokenStatus(1, "token1", 500)  // drop 75% from peak → DUMPED

	state := d.GetClusterState(1)
	assert.Equal(t, StatusDumped, state.ActiveTokens[0].Status)
	assert.Equal(t, 2000.0, state.ActiveTokens[0].PeakMcap)
	assert.Equal(t, 500.0, state.ActiveTokens[0].CurrentMcap)
}

func TestUpdateTokenStatus_NonexistentCluster(t *testing.T) {
	d := NewDetector(DefaultDetectorConfig())
	// Should not panic.
	d.UpdateTokenStatus(999, "token1", 1000)
}

func TestMarkRugged(t *testing.T) {
	d := NewDetector(DefaultDetectorConfig())

	d.RecordTokenLaunch(1, "token1", 1000)
	d.MarkRugged(1, "token1")

	state := d.GetClusterState(1)
	assert.Equal(t, StatusRugged, state.ActiveTokens[0].Status)
}

func TestMarkRugged_NonexistentCluster(t *testing.T) {
	d := NewDetector(DefaultDetectorConfig())
	d.MarkRugged(999, "token1") // should not panic
}

func TestPatternDetection_Serial(t *testing.T) {
	cfg := DefaultDetectorConfig()
	cfg.SerialThreshold = 3
	d := NewDetector(cfg)

	// 3+ tokens in 24h → SERIAL pattern.
	d.RecordTokenLaunch(1, "token1", 1000)
	d.RecordTokenLaunch(1, "token2", 1000)
	d.RecordTokenLaunch(1, "token3", 1000)

	state := d.GetClusterState(1)
	assert.Equal(t, PatternSerial, state.Pattern)
	assert.Equal(t, RiskHigh, state.RiskLevel)
}

func TestPatternDetection_Rotation(t *testing.T) {
	cfg := DefaultDetectorConfig()
	d := NewDetector(cfg)

	// Token 1 pumps then dumps.
	d.RecordTokenLaunch(1, "token1", 1000)
	d.UpdateTokenStatus(1, "token1", 5000) // peak
	d.UpdateTokenStatus(1, "token1", 1000) // 80% drop → DUMPED

	// Token 2 launches and pumps.
	d.RecordTokenLaunch(1, "token2", 2000)
	d.UpdateTokenStatus(1, "token2", 5000) // PUMPING

	state := d.GetClusterState(1)
	assert.Equal(t, PatternRotation, state.Pattern)
	assert.Equal(t, RiskHigh, state.RiskLevel)
}

func TestPatternDetection_Distraction(t *testing.T) {
	cfg := DefaultDetectorConfig()
	cfg.SerialThreshold = 5 // raise so 2 tokens doesn't trigger SERIAL
	d := NewDetector(cfg)

	// 2+ tokens launched close together, none pumping or dumped.
	d.RecordTokenLaunch(1, "token1", 1000)
	d.RecordTokenLaunch(1, "token2", 1000)

	state := d.GetClusterState(1)
	assert.Equal(t, PatternDistraction, state.Pattern)
	assert.Equal(t, RiskMedium, state.RiskLevel)
}

func TestPatternDetection_SerialRugger(t *testing.T) {
	d := NewDetector(DefaultDetectorConfig())

	d.RecordTokenLaunch(1, "token1", 1000)
	d.RecordTokenLaunch(1, "token2", 1000)
	d.MarkRugged(1, "token1")
	d.MarkRugged(1, "token2")

	state := d.GetClusterState(1)
	assert.Equal(t, PatternSerial, state.Pattern)
	assert.Equal(t, RiskCritical, state.RiskLevel)
}

func TestGetClusterState_ReturnsNilForUnknown(t *testing.T) {
	d := NewDetector(DefaultDetectorConfig())
	assert.Nil(t, d.GetClusterState(999))
}

func TestGetClusterState_ReturnsCopy(t *testing.T) {
	d := NewDetector(DefaultDetectorConfig())
	d.RecordTokenLaunch(1, "token1", 1000)

	state1 := d.GetClusterState(1)
	state2 := d.GetClusterState(1)

	// Modifying one shouldn't affect the other.
	state1.ActiveTokens[0].Address = "modified"
	assert.Equal(t, "token1", state2.ActiveTokens[0].Address)
}

func TestScoreImpact(t *testing.T) {
	tests := []struct {
		name     string
		state    *CrossTokenState
		expected float64
	}{
		{"nil", nil, 0},
		{"critical", &CrossTokenState{RiskLevel: RiskCritical}, -100},
		{"rotation", &CrossTokenState{Pattern: PatternRotation, RiskLevel: RiskHigh}, -40},
		{"serial_3", &CrossTokenState{Pattern: PatternSerial, TokenCount24h: 3, RiskLevel: RiskHigh}, -25},
		{"distraction", &CrossTokenState{Pattern: PatternDistraction, RiskLevel: RiskMedium}, -10},
		{"none", &CrossTokenState{Pattern: PatternNone, RiskLevel: RiskLow}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.state.ScoreImpact())
		})
	}
}

func TestShouldPanicSell(t *testing.T) {
	// Nil state.
	var nilState *CrossTokenState
	assert.False(t, nilState.ShouldPanicSell("token1"))

	// Critical risk → panic sell.
	state := &CrossTokenState{
		RiskLevel: RiskCritical,
	}
	assert.True(t, state.ShouldPanicSell("token1"))

	// Rotation and our token is dumped.
	state2 := &CrossTokenState{
		Pattern: PatternRotation,
		ActiveTokens: []TokenRef{
			{Address: "token1", Status: StatusDumped},
			{Address: "token2", Status: StatusPumping},
		},
	}
	assert.True(t, state2.ShouldPanicSell("token1"))
	assert.False(t, state2.ShouldPanicSell("token2"))

	// Normal state.
	state3 := &CrossTokenState{
		Pattern:   PatternNone,
		RiskLevel: RiskLow,
	}
	assert.False(t, state3.ShouldPanicSell("token1"))
}

func TestEvictOldest(t *testing.T) {
	cfg := DefaultDetectorConfig()
	cfg.MaxClustersTracked = 2
	d := NewDetector(cfg)

	d.RecordTokenLaunch(1, "token1", 1000)
	time.Sleep(1 * time.Millisecond) // ensure different timestamps
	d.RecordTokenLaunch(2, "token2", 2000)
	time.Sleep(1 * time.Millisecond)

	// Third should evict oldest (cluster 1).
	d.RecordTokenLaunch(3, "token3", 3000)

	stats := d.Stats()
	assert.Equal(t, 2, stats.TrackedClusters)
	assert.Nil(t, d.GetClusterState(1)) // evicted
	assert.NotNil(t, d.GetClusterState(2))
	assert.NotNil(t, d.GetClusterState(3))
}

func TestCleanup(t *testing.T) {
	d := NewDetector(DefaultDetectorConfig())

	// Add a cluster with old timestamp.
	d.mu.Lock()
	d.clusters[1] = &CrossTokenState{
		ClusterID:   1,
		LastUpdated: time.Now().Add(-2 * time.Hour).UnixNano(),
	}
	d.clusters[2] = &CrossTokenState{
		ClusterID:   2,
		LastUpdated: time.Now().UnixNano(),
	}
	d.mu.Unlock()

	removed := d.Cleanup(1 * time.Hour)
	assert.Equal(t, 1, removed)

	stats := d.Stats()
	assert.Equal(t, 1, stats.TrackedClusters)
}

func TestStats(t *testing.T) {
	d := NewDetector(DefaultDetectorConfig())

	d.RecordTokenLaunch(1, "token1", 1000)
	d.RecordTokenLaunch(1, "token2", 1000)
	d.RecordTokenLaunch(1, "token3", 1000) // triggers SERIAL

	stats := d.Stats()
	assert.Equal(t, 1, stats.TrackedClusters)
	assert.Equal(t, 1, stats.Patterns["SERIAL"])
}

func TestAssessRisk_CriticalFromSerialLaunches(t *testing.T) {
	cfg := DefaultDetectorConfig()
	cfg.SerialThreshold = 3
	d := NewDetector(cfg)

	// threshold+2 = 5 tokens → CRITICAL
	for i := 0; i < 5; i++ {
		d.RecordTokenLaunch(1, "token"+string(rune('a'+i)), 1000)
	}

	state := d.GetClusterState(1)
	assert.Equal(t, RiskCritical, state.RiskLevel)
}
