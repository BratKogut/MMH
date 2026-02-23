package copytrade

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNewTracker(t *testing.T) {
	tr := NewTracker(DefaultIntelConfig())
	assert.NotNil(t, tr)
	stats := tr.Stats()
	assert.Equal(t, 0, stats.TrackedWallets)
}

func TestDefaultIntelConfig(t *testing.T) {
	cfg := DefaultIntelConfig()
	assert.Equal(t, 1000, cfg.MaxTrackedWallets)
	assert.Equal(t, 5, cfg.SignalWindowMin)
	assert.Equal(t, 10.0, cfg.MinWhaleAmountSOL)
	assert.Equal(t, 0.3, cfg.MinConfidence)
}

func TestAddWallet(t *testing.T) {
	tr := NewTracker(DefaultIntelConfig())
	tr.AddWallet(TrackedWallet{
		Address: "whale1",
		Tier:    TierWhale,
		Label:   "Big Whale",
	})

	stats := tr.Stats()
	assert.Equal(t, 1, stats.TrackedWallets)
	assert.Equal(t, 1, stats.TierBreakdown["WHALE"])
}

func TestAddWallet_MaxCapacity(t *testing.T) {
	cfg := DefaultIntelConfig()
	cfg.MaxTrackedWallets = 2
	tr := NewTracker(cfg)

	tr.AddWallet(TrackedWallet{Address: "w1", Tier: TierWhale})
	tr.AddWallet(TrackedWallet{Address: "w2", Tier: TierKOL})
	tr.AddWallet(TrackedWallet{Address: "w3", Tier: TierSmartMoney}) // over capacity

	assert.Equal(t, 2, tr.Stats().TrackedWallets)
}

func TestRemoveWallet(t *testing.T) {
	tr := NewTracker(DefaultIntelConfig())
	tr.AddWallet(TrackedWallet{Address: "w1", Tier: TierWhale})
	tr.RemoveWallet("w1")
	assert.Equal(t, 0, tr.Stats().TrackedWallets)
}

func TestRecordTrade_UntrackedWallet(t *testing.T) {
	tr := NewTracker(DefaultIntelConfig())
	sig := tr.RecordTrade(TradeEvent{
		WalletAddr: "unknown",
		TokenMint:  "token1",
		AmountSOL:  5.0,
	})
	assert.Nil(t, sig)
}

func TestRecordTrade_TrackedWallet(t *testing.T) {
	tr := NewTracker(DefaultIntelConfig())
	tr.AddWallet(TrackedWallet{
		Address: "whale1",
		Tier:    TierWhale,
		WinRate: 0.8,
	})

	sig := tr.RecordTrade(TradeEvent{
		WalletAddr: "whale1",
		TokenMint:  "token1",
		AmountSOL:  50.0,
	})

	assert.NotNil(t, sig)
	assert.Equal(t, "token1", sig.TokenMint)
	assert.Equal(t, TierWhale, sig.Tier)
	assert.True(t, sig.Confidence >= 0.3)
	assert.Equal(t, 50.0, sig.TotalAmountSOL)
}

func TestRecordTrade_MultiWallet_Accumulate(t *testing.T) {
	tr := NewTracker(DefaultIntelConfig())
	tr.AddWallet(TrackedWallet{Address: "w1", Tier: TierWhale})
	tr.AddWallet(TrackedWallet{Address: "w2", Tier: TierSmartMoney})

	tr.RecordTrade(TradeEvent{WalletAddr: "w1", TokenMint: "token1", AmountSOL: 10})
	sig := tr.RecordTrade(TradeEvent{WalletAddr: "w2", TokenMint: "token1", AmountSOL: 20})

	assert.NotNil(t, sig)
	assert.Equal(t, SignalAccum, sig.Signal) // upgraded from BUY to ACCUMULATE
	assert.Equal(t, 2, sig.WalletCount)
	assert.Equal(t, 30.0, sig.TotalAmountSOL)
	assert.Equal(t, TierSmartMoney, sig.Tier) // highest tier
}

func TestRecordTrade_Sell(t *testing.T) {
	tr := NewTracker(DefaultIntelConfig())
	tr.AddWallet(TrackedWallet{Address: "w1", Tier: TierWhale})

	sig := tr.RecordTrade(TradeEvent{
		WalletAddr: "w1",
		TokenMint:  "token1",
		Signal:     SignalSell,
		AmountSOL:  -10,
	})

	assert.NotNil(t, sig)
	assert.Equal(t, SignalSell, sig.Signal)
}

func TestRecordTrade_OnSignalCallback(t *testing.T) {
	tr := NewTracker(DefaultIntelConfig())
	tr.AddWallet(TrackedWallet{Address: "w1", Tier: TierSmartMoney, WinRate: 0.9})

	var received *CopySignal
	tr.SetOnSignal(func(sig CopySignal) {
		received = &sig
	})

	tr.RecordTrade(TradeEvent{WalletAddr: "w1", TokenMint: "token1", AmountSOL: 50})

	assert.NotNil(t, received)
	assert.Equal(t, "token1", received.TokenMint)
}

func TestGetSignal(t *testing.T) {
	tr := NewTracker(DefaultIntelConfig())
	tr.AddWallet(TrackedWallet{Address: "w1", Tier: TierWhale})
	tr.RecordTrade(TradeEvent{WalletAddr: "w1", TokenMint: "token1", AmountSOL: 10})

	sig := tr.GetSignal("token1")
	assert.NotNil(t, sig)
	assert.Equal(t, "token1", sig.TokenMint)

	// Non-existent.
	assert.Nil(t, tr.GetSignal("nonexistent"))
}

func TestGetWallets(t *testing.T) {
	tr := NewTracker(DefaultIntelConfig())
	tr.AddWallet(TrackedWallet{Address: "w1", Tier: TierWhale})
	tr.AddWallet(TrackedWallet{Address: "w2", Tier: TierKOL})

	wallets := tr.GetWallets()
	assert.Equal(t, 2, len(wallets))
}

func TestUpdateWalletPerformance(t *testing.T) {
	tr := NewTracker(DefaultIntelConfig())
	tr.AddWallet(TrackedWallet{Address: "w1", Tier: TierWhale, WinRate: 0.5, TradeCount: 10})

	tr.UpdateWalletPerformance("w1", true, 150)

	tr.mu.RLock()
	w := tr.wallets["w1"]
	assert.True(t, w.WinRate > 0.5)
	assert.True(t, w.AvgROI > 0)
	tr.mu.RUnlock()
}

func TestScoreImpact(t *testing.T) {
	tests := []struct {
		name     string
		sig      *CopySignal
		positive bool
	}{
		{"nil", nil, false},
		{"first_move_smart", &CopySignal{Signal: SignalFirstMove, Tier: TierSmartMoney, Confidence: 0.8}, true},
		{"buy_whale", &CopySignal{Signal: SignalBuy, Tier: TierWhale, Confidence: 0.6}, true},
		{"sell", &CopySignal{Signal: SignalSell, Tier: TierWhale, Confidence: 0.8}, false},
		{"dump", &CopySignal{Signal: SignalDump, Tier: TierInsider, Confidence: 0.9}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			impact := ScoreImpact(tt.sig)
			if tt.positive {
				assert.True(t, impact > 0, "expected positive impact, got %f", impact)
			} else {
				assert.True(t, impact <= 0, "expected non-positive impact, got %f", impact)
			}
		})
	}
}

func TestCleanup(t *testing.T) {
	tr := NewTracker(DefaultIntelConfig())
	tr.AddWallet(TrackedWallet{Address: "w1", Tier: TierWhale})

	// Add a signal and manually age it.
	tr.RecordTrade(TradeEvent{WalletAddr: "w1", TokenMint: "token1", AmountSOL: 10})

	tr.mu.Lock()
	tr.signals["token1"].Timestamp = time.Now().Add(-2 * time.Hour)
	tr.mu.Unlock()

	removed := tr.Cleanup(1 * time.Hour)
	assert.Equal(t, 1, removed)
	assert.Nil(t, tr.GetSignal("token1"))
}

func TestWalletTier_String(t *testing.T) {
	assert.Equal(t, "WHALE", TierWhale.String())
	assert.Equal(t, "SMART_MONEY", TierSmartMoney.String())
}

func TestSignalType_String(t *testing.T) {
	assert.Equal(t, "BUY", SignalBuy.String())
	assert.Equal(t, "FIRST_MOVE", SignalFirstMove.String())
}

func TestTierRank(t *testing.T) {
	assert.True(t, tierRank(TierSmartMoney) > tierRank(TierWhale))
	assert.True(t, tierRank(TierWhale) > tierRank(TierKOL))
	assert.True(t, tierRank(TierKOL) > tierRank(TierInsider))
	assert.True(t, tierRank(TierInsider) > tierRank(TierFresh))
}
