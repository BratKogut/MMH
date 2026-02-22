package scanner

import (
	"testing"
	"time"

	"github.com/nexus-trading/nexus/internal/graph"
	"github.com/nexus-trading/nexus/internal/solana"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
)

func TestSanitizer_PassesCleanToken(t *testing.T) {
	graphEngine := graph.NewEngine(graph.DefaultConfig())
	poolState := NewPoolStateManager(DefaultPoolStateConfig(), nil)

	s := NewSanitizer(DefaultSanitizerConfig(), graphEngine, poolState)

	discovery := PoolDiscovery{
		Pool: solana.PoolInfo{
			PoolAddress:  "pool1",
			DEX:          "raydium",
			TokenMint:    "CleanToken111",
			QuoteMint:    solana.SOLMint,
			LiquidityUSD: decimal.NewFromInt(10000),
			QuoteReserve: decimal.NewFromInt(50),
			CreatedAt:    time.Now(),
		},
		Token: &solana.TokenInfo{
			Mint:     "CleanToken111",
			Name:     "GoodToken",
			Symbol:   "GOOD",
			Decimals: 9,
		},
	}

	result := s.Check(discovery)
	assert.True(t, result.Passed)
	assert.Empty(t, result.Reason)
	assert.True(t, result.LatencyUs >= 0)
}

func TestSanitizer_DropsBlacklisted(t *testing.T) {
	s := NewSanitizer(DefaultSanitizerConfig(), nil, nil)
	s.AddBlacklist([]string{"ScamToken999"})

	discovery := PoolDiscovery{
		Pool: solana.PoolInfo{
			TokenMint:    "ScamToken999",
			LiquidityUSD: decimal.NewFromInt(10000),
			QuoteReserve: decimal.NewFromInt(50),
			QuoteMint:    solana.SOLMint,
		},
	}

	result := s.Check(discovery)
	assert.False(t, result.Passed)
	assert.Equal(t, "blacklist", result.Filter)
}

func TestSanitizer_DropsHoneypotNames(t *testing.T) {
	s := NewSanitizer(DefaultSanitizerConfig(), nil, nil)

	tests := []struct {
		name   string
		symbol string
		drop   bool
	}{
		{"SafeMoonV3", "SAFEMOON", true},
		{"ElonCumRocket", "ECR", true},
		{"test token", "TEST", true},
		{"free airdrop", "FREE", true},
		{"LegitMemeCoin", "MEME", false},
		{"CoolDogToken", "DOG", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			discovery := PoolDiscovery{
				Pool: solana.PoolInfo{
					LiquidityUSD: decimal.NewFromInt(10000),
					QuoteReserve: decimal.NewFromInt(50),
					QuoteMint:    solana.SOLMint,
				},
				Token: &solana.TokenInfo{
					Name:   tt.name,
					Symbol: tt.symbol,
				},
			}
			result := s.Check(discovery)
			if tt.drop {
				assert.False(t, result.Passed, "expected drop for %s", tt.name)
				assert.Equal(t, "name_pattern", result.Filter)
			} else {
				assert.True(t, result.Passed, "expected pass for %s", tt.name)
			}
		})
	}
}

func TestSanitizer_DropsImpersonation(t *testing.T) {
	s := NewSanitizer(DefaultSanitizerConfig(), nil, nil)

	discovery := PoolDiscovery{
		Pool: solana.PoolInfo{
			LiquidityUSD: decimal.NewFromInt(10000),
			QuoteReserve: decimal.NewFromInt(50),
			QuoteMint:    solana.SOLMint,
		},
		Token: &solana.TokenInfo{
			Name:   "Solana",
			Symbol: "SOL",
		},
	}

	result := s.Check(discovery)
	assert.False(t, result.Passed)
	assert.Equal(t, "name_pattern", result.Filter)
	assert.Contains(t, result.Reason, "impersonates")
}

func TestSanitizer_DropsFreezeActive(t *testing.T) {
	s := NewSanitizer(DefaultSanitizerConfig(), nil, nil)

	discovery := PoolDiscovery{
		Pool: solana.PoolInfo{
			LiquidityUSD: decimal.NewFromInt(10000),
			QuoteReserve: decimal.NewFromInt(50),
			QuoteMint:    solana.SOLMint,
		},
		Token: &solana.TokenInfo{
			Name:            "SomeToken",
			Symbol:          "SOME",
			FreezeAuthority: "SomeFreezeAuth",
		},
	}

	result := s.Check(discovery)
	assert.False(t, result.Passed)
	assert.Equal(t, "authority", result.Filter)
}

func TestSanitizer_DropsLowLiquidity(t *testing.T) {
	s := NewSanitizer(DefaultSanitizerConfig(), nil, nil)

	discovery := PoolDiscovery{
		Pool: solana.PoolInfo{
			LiquidityUSD: decimal.NewFromInt(100), // Below 500 min
			QuoteReserve: decimal.NewFromInt(50),
			QuoteMint:    solana.SOLMint,
		},
		Token: &solana.TokenInfo{
			Name:   "LowLiqToken",
			Symbol: "LOW",
		},
	}

	result := s.Check(discovery)
	assert.False(t, result.Passed)
	assert.Equal(t, "liquidity", result.Filter)
}

func TestSanitizer_DropsEntityGraphRugger(t *testing.T) {
	graphEngine := graph.NewEngine(graph.DefaultConfig())

	// Set up: funder1 is a serial rugger, deployer1 was funded by funder1.
	now := time.Now().Unix()
	graphEngine.AddDeploy("funder1", now)
	graphEngine.MarkRug("funder1")
	graphEngine.MarkRug("funder1") // 2x → SerialRugger label
	// Transfer from funder1 → deployer1 creates a 1-hop link.
	graphEngine.AddTransfer("funder1", "deployer1", 5.0, now, "transfer")

	s := NewSanitizer(DefaultSanitizerConfig(), graphEngine, nil)

	// Query for deployer1 — BFS will find funder1 (SerialRugger) at depth 1.
	discovery := PoolDiscovery{
		Pool: solana.PoolInfo{
			TokenMint:    solana.Pubkey("deployer1"),
			LiquidityUSD: decimal.NewFromInt(10000),
			QuoteReserve: decimal.NewFromInt(50),
			QuoteMint:    solana.SOLMint,
		},
		Token: &solana.TokenInfo{
			Name:   "RugToken",
			Symbol: "RUG",
		},
	}

	result := s.Check(discovery)
	assert.False(t, result.Passed)
	assert.Equal(t, "entity_graph", result.Filter)
}

func TestSanitizer_Stats(t *testing.T) {
	s := NewSanitizer(DefaultSanitizerConfig(), nil, nil)

	// Pass one.
	s.Check(PoolDiscovery{
		Pool: solana.PoolInfo{
			LiquidityUSD: decimal.NewFromInt(10000),
			QuoteReserve: decimal.NewFromInt(50),
			QuoteMint:    solana.SOLMint,
		},
		Token: &solana.TokenInfo{Name: "Good", Symbol: "G"},
	})

	// Drop one.
	s.AddBlacklist([]string{"bad"})
	s.Check(PoolDiscovery{
		Pool: solana.PoolInfo{
			TokenMint:    "bad",
			LiquidityUSD: decimal.NewFromInt(10000),
			QuoteMint:    solana.SOLMint,
		},
	})

	stats := s.Stats()
	assert.Equal(t, int64(2), stats.TotalChecked)
	assert.Equal(t, int64(1), stats.TotalPassed)
	assert.Equal(t, int64(1), stats.TotalDropped)
	assert.Equal(t, 50.0, stats.PassRate)
	assert.Equal(t, int64(1), stats.FilterCounts["blacklist"])
}

func TestSanitizer_CleanupDeployerHistory(t *testing.T) {
	s := NewSanitizer(DefaultSanitizerConfig(), nil, nil)

	// Add old entry.
	s.deployerMu.Lock()
	s.deployerHist["old_deployer"] = &deployerEntry{
		count:     3,
		firstSeen: time.Now().Add(-48 * time.Hour),
	}
	s.deployerMu.Unlock()

	s.CleanupDeployerHistory()

	s.deployerMu.RLock()
	_, exists := s.deployerHist["old_deployer"]
	s.deployerMu.RUnlock()
	assert.False(t, exists)
}
