package honeypot

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewTracker(t *testing.T) {
	tr := NewTracker(DefaultEvolutionConfig())
	assert.NotNil(t, tr)
	assert.Equal(t, 0, tr.PatternCount())
}

func TestDefaultEvolutionConfig(t *testing.T) {
	cfg := DefaultEvolutionConfig()
	assert.Equal(t, 0.3, cfg.MinConfidence)
	assert.Equal(t, 5, cfg.MinHitsForHighConf)
	assert.Equal(t, 2000, cfg.MaxPatterns)
	assert.Equal(t, 8, cfg.PatternMinBytes)
	assert.True(t, cfg.RetroactiveScan)
}

func TestRecordRug_ExtractsNewPattern(t *testing.T) {
	tr := NewTracker(DefaultEvolutionConfig())

	sample := RugSample{
		TokenAddress: "token1",
		Chain:        "solana",
		DeployerAddr: "deployer1",
		ContractData: []byte("this is fake contract data that is long enough for pattern extraction"),
		RugType:      "honeypot",
		LossAmount:   100.0,
		DetectedBy:   "CSM",
	}

	sig := tr.RecordRug(sample)
	assert.NotNil(t, sig)
	assert.True(t, sig.AutoDetected)
	assert.Equal(t, "solana", sig.Chain)
	assert.Equal(t, 1, sig.HitCount)
	assert.Equal(t, 0.3, sig.Confidence)
	assert.Equal(t, 1, tr.PatternCount())

	stats := tr.Stats()
	assert.Equal(t, int64(1), stats.TotalSamples)
	assert.Equal(t, int64(1), stats.AutoPatterns)
}

func TestRecordRug_MatchesExistingPattern(t *testing.T) {
	tr := NewTracker(DefaultEvolutionConfig())

	contractData := []byte("this is the same contract data for both rugs — must be long enough")

	// First rug creates pattern.
	sig1 := tr.RecordRug(RugSample{
		TokenAddress: "token1",
		Chain:        "solana",
		ContractData: contractData,
		RugType:      "honeypot",
	})
	assert.NotNil(t, sig1)
	assert.Equal(t, 1, sig1.HitCount)

	// Second rug with same contract data matches.
	sig2 := tr.RecordRug(RugSample{
		TokenAddress: "token2",
		Chain:        "solana",
		ContractData: contractData,
		RugType:      "honeypot",
	})
	assert.NotNil(t, sig2)
	assert.Equal(t, sig1.PatternID, sig2.PatternID)
	assert.Equal(t, 2, sig2.HitCount)
	assert.True(t, sig2.Confidence > 0.3)

	// Still only 1 pattern.
	assert.Equal(t, 1, tr.PatternCount())
}

func TestRecordRug_TooSmallContractData(t *testing.T) {
	tr := NewTracker(DefaultEvolutionConfig())

	sig := tr.RecordRug(RugSample{
		TokenAddress: "token1",
		Chain:        "solana",
		ContractData: []byte("short"),
		RugType:      "honeypot",
	})
	assert.Nil(t, sig)
	assert.Equal(t, 0, tr.PatternCount())
}

func TestRecordRug_OnNewPatternCallback(t *testing.T) {
	tr := NewTracker(DefaultEvolutionConfig())

	var callbackSig *Signature
	tr.SetOnNewPattern(func(sig *Signature) {
		callbackSig = sig
	})

	tr.RecordRug(RugSample{
		TokenAddress: "token1",
		Chain:        "solana",
		ContractData: []byte("long enough contract data for pattern extraction to work"),
		RugType:      "honeypot",
	})

	assert.NotNil(t, callbackSig)
	assert.True(t, callbackSig.AutoDetected)
}

func TestCheckContract(t *testing.T) {
	tr := NewTracker(DefaultEvolutionConfig())

	contractData := []byte("specific contract bytecode that represents a honeypot pattern here")

	// No patterns yet.
	assert.Nil(t, tr.CheckContract(contractData))

	// Record a rug to create a pattern.
	tr.RecordRug(RugSample{
		TokenAddress: "token1",
		Chain:        "solana",
		ContractData: contractData,
		RugType:      "honeypot",
	})

	// Now should match.
	sig := tr.CheckContract(contractData)
	assert.NotNil(t, sig)
}

func TestCheckContract_TooSmall(t *testing.T) {
	tr := NewTracker(DefaultEvolutionConfig())
	assert.Nil(t, tr.CheckContract([]byte("ab")))
}

func TestRetroactiveScan(t *testing.T) {
	tr := NewTracker(DefaultEvolutionConfig())

	contractData := []byte("known honeypot pattern data that will be matched during retroactive scanning")

	var retroHits []string
	tr.SetOnRetroactiveHit(func(tokenAddr string, sig *Signature) {
		retroHits = append(retroHits, tokenAddr)
	})

	// Create pattern.
	tr.RecordRug(RugSample{
		TokenAddress: "token1",
		Chain:        "solana",
		ContractData: contractData,
		RugType:      "honeypot",
	})

	// Scan with matching and non-matching positions.
	matches := tr.RetroactiveScan([]RetroPosition{
		{TokenAddress: "pos1", ContractData: contractData},       // match
		{TokenAddress: "pos2", ContractData: []byte("different contract data that is also long enough for comparison")}, // no match
		{TokenAddress: "pos3", ContractData: []byte("x")},       // too small
	})

	assert.Equal(t, 1, len(matches))
	assert.Equal(t, "pos1", matches[0].TokenAddress)
	assert.Equal(t, 1, len(retroHits))

	stats := tr.Stats()
	assert.Equal(t, int64(1), stats.RetroCatches)
}

func TestAddPattern(t *testing.T) {
	tr := NewTracker(DefaultEvolutionConfig())

	sig := Signature{
		PatternID:   "HP-solana-manual1",
		Chain:       "solana",
		SigType:     "instruction",
		Pattern:     []byte("deadbeef"),
		Description: "Manual pattern",
		Confidence:  0.8,
		HitCount:    10,
	}

	tr.AddPattern(sig)
	assert.Equal(t, 1, tr.PatternCount())

	patterns := tr.GetPatterns()
	assert.Equal(t, 1, len(patterns))
	assert.Equal(t, "HP-solana-manual1", patterns[0].PatternID)
	assert.True(t, patterns[0].FirstSeen > 0) // auto-set
}

func TestGetPatterns(t *testing.T) {
	tr := NewTracker(DefaultEvolutionConfig())

	tr.AddPattern(Signature{PatternID: "p1", Chain: "solana"})
	tr.AddPattern(Signature{PatternID: "p2", Chain: "solana"})

	patterns := tr.GetPatterns()
	assert.Equal(t, 2, len(patterns))
}

func TestComputeConfidence(t *testing.T) {
	tr := NewTracker(DefaultEvolutionConfig())

	// Hit count 1 → MinConfidence = 0.3
	assert.Equal(t, 0.3, tr.computeConfidence(1))

	// Hit count >= MinHitsForHighConf (5) → 0.9
	assert.Equal(t, 0.9, tr.computeConfidence(5))
	assert.Equal(t, 0.9, tr.computeConfidence(10))

	// Intermediate: linear interpolation.
	c := tr.computeConfidence(3)
	assert.True(t, c > 0.3 && c < 0.9, "expected between 0.3 and 0.9, got %f", c)
}

func TestMaxPatternsCapacity(t *testing.T) {
	cfg := DefaultEvolutionConfig()
	cfg.MaxPatterns = 2
	tr := NewTracker(cfg)

	tr.AddPattern(Signature{PatternID: "p1"})
	tr.AddPattern(Signature{PatternID: "p2"})

	// At capacity, extractPattern should return nil.
	sig := tr.RecordRug(RugSample{
		TokenAddress: "token_extra",
		Chain:        "solana",
		ContractData: []byte("some totally new contract data that doesn't match anything"),
		RugType:      "honeypot",
	})
	assert.Nil(t, sig)
	assert.Equal(t, 2, tr.PatternCount())
}

func TestSampleRingBuffer(t *testing.T) {
	tr := NewTracker(DefaultEvolutionConfig())

	// Record 501 rugs to test ring buffer.
	for i := 0; i < 501; i++ {
		tr.RecordRug(RugSample{
			TokenAddress: "token",
			Chain:        "solana",
			ContractData: []byte("x"), // too small for pattern
			RugType:      "honeypot",
		})
	}

	tr.mu.RLock()
	assert.Equal(t, 500, len(tr.samples))
	tr.mu.RUnlock()
}
