package copytrade

import (
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// ---------------------------------------------------------------------------
// Copy-Trade Intelligence Module
// v3.2: Track whale/smart-money wallets, detect early buys, generate signals.
// ---------------------------------------------------------------------------

// WalletTier classifies tracked wallets.
type WalletTier string

const (
	TierWhale      WalletTier = "WHALE"       // large capital, >100 SOL trades
	TierSmartMoney WalletTier = "SMART_MONEY" // historically profitable
	TierKOL        WalletTier = "KOL"         // Key Opinion Leader
	TierInsider    WalletTier = "INSIDER"      // known early buyer
	TierFresh      WalletTier = "FRESH"        // newly funded, suspicious
)

func (t WalletTier) String() string { return string(t) }

// SignalType represents the type of copy-trade signal.
type SignalType string

const (
	SignalBuy       SignalType = "BUY"
	SignalSell      SignalType = "SELL"
	SignalAccum     SignalType = "ACCUMULATE"     // multiple buys over time
	SignalDump      SignalType = "DUMP"           // rapid sell after pump
	SignalFirstMove SignalType = "FIRST_MOVE"     // very early entry
)

func (s SignalType) String() string { return string(s) }

// TrackedWallet is a wallet we're monitoring.
type TrackedWallet struct {
	Address    string     `json:"address"`
	Tier       WalletTier `json:"tier"`
	Label      string     `json:"label"`       // human-readable label (e.g., "whale_0x1a")
	WinRate    float64    `json:"win_rate"`     // historical win rate (0-1)
	AvgROI     float64    `json:"avg_roi"`      // average ROI per trade
	TradeCount int        `json:"trade_count"`  // total tracked trades
	AddedAt    time.Time  `json:"added_at"`
}

// TradeEvent records a tracked wallet's trade.
type TradeEvent struct {
	WalletAddr string     `json:"wallet_addr"`
	TokenMint  string     `json:"token_mint"`
	Signal     SignalType `json:"signal"`
	AmountSOL  float64    `json:"amount_sol"`
	PriceUSD   float64    `json:"price_usd"`
	Timestamp  time.Time  `json:"timestamp"`
}

// CopySignal is the output signal for the scoring engine.
type CopySignal struct {
	TokenMint     string     `json:"token_mint"`
	Signal        SignalType `json:"signal"`
	Tier          WalletTier `json:"tier"`
	WalletCount   int        `json:"wallet_count"`    // how many tracked wallets made this move
	TotalAmountSOL float64   `json:"total_amount_sol"`
	Confidence    float64    `json:"confidence"`       // 0-1
	Timestamp     time.Time  `json:"timestamp"`
}

// IntelConfig configures the Copy-Trade Intelligence module.
type IntelConfig struct {
	MaxTrackedWallets   int     `yaml:"max_tracked_wallets"`
	SignalWindowMin     int     `yaml:"signal_window_min"`     // aggregate signals within this window (default 5min)
	MinWhaleAmountSOL   float64 `yaml:"min_whale_amount_sol"`  // minimum SOL for whale classification
	MinConfidence       float64 `yaml:"min_confidence"`        // minimum confidence to emit signal
	MaxTradeHistorySize int     `yaml:"max_trade_history_size"`
}

// DefaultIntelConfig returns defaults.
func DefaultIntelConfig() IntelConfig {
	return IntelConfig{
		MaxTrackedWallets:   1000,
		SignalWindowMin:     5,
		MinWhaleAmountSOL:   10,
		MinConfidence:       0.3,
		MaxTradeHistorySize: 10000,
	}
}

// Tracker monitors tracked wallets and generates copy-trade signals.
type Tracker struct {
	config  IntelConfig
	mu      sync.RWMutex
	wallets map[string]*TrackedWallet     // address -> wallet
	trades  []TradeEvent                  // ring buffer
	signals map[string]*CopySignal        // tokenMint -> aggregated signal

	onSignal func(sig CopySignal) // callback when new signal is generated
}

// NewTracker creates a new Copy-Trade Intelligence tracker.
func NewTracker(config IntelConfig) *Tracker {
	return &Tracker{
		config:  config,
		wallets: make(map[string]*TrackedWallet),
		signals: make(map[string]*CopySignal),
	}
}

// SetOnSignal sets the callback for new copy-trade signals.
func (t *Tracker) SetOnSignal(fn func(sig CopySignal)) {
	t.mu.Lock()
	t.onSignal = fn
	t.mu.Unlock()
}

// AddWallet registers a wallet for tracking.
func (t *Tracker) AddWallet(wallet TrackedWallet) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.wallets) >= t.config.MaxTrackedWallets {
		return
	}

	wallet.AddedAt = time.Now()
	t.wallets[wallet.Address] = &wallet

	log.Debug().
		Str("address", wallet.Address).
		Str("tier", string(wallet.Tier)).
		Str("label", wallet.Label).
		Msg("copytrade: wallet added")
}

// RemoveWallet removes a tracked wallet.
func (t *Tracker) RemoveWallet(address string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.wallets, address)
}

// RecordTrade records a trade from a tracked wallet and generates signals.
func (t *Tracker) RecordTrade(event TradeEvent) *CopySignal {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Only process tracked wallets.
	wallet, ok := t.wallets[event.WalletAddr]
	if !ok {
		return nil
	}

	// Store in ring buffer.
	if len(t.trades) >= t.config.MaxTradeHistorySize {
		t.trades = t.trades[1:]
	}
	event.Timestamp = time.Now()
	t.trades = append(t.trades, event)

	// Update wallet stats.
	wallet.TradeCount++

	// Auto-classify signal type.
	if event.Signal == "" {
		if event.AmountSOL > 0 {
			event.Signal = SignalBuy
		} else {
			event.Signal = SignalSell
		}
	}

	// Classify as FIRST_MOVE if token is very new (within signal window).
	if event.Signal == SignalBuy {
		recentBuys := 0
		cutoff := time.Now().Add(-time.Duration(t.config.SignalWindowMin) * time.Minute)
		for i := len(t.trades) - 1; i >= 0; i-- {
			trade := t.trades[i]
			if trade.Timestamp.Before(cutoff) {
				break
			}
			if trade.TokenMint == event.TokenMint && trade.Signal == SignalBuy {
				recentBuys++
			}
		}
		if recentBuys <= 1 {
			event.Signal = SignalFirstMove
		}
	}

	// Aggregate signal per token.
	sig, exists := t.signals[event.TokenMint]
	if !exists || time.Since(sig.Timestamp) > time.Duration(t.config.SignalWindowMin)*time.Minute {
		sig = &CopySignal{
			TokenMint:  event.TokenMint,
			Signal:     event.Signal,
			Tier:       wallet.Tier,
			Timestamp:  time.Now(),
		}
		t.signals[event.TokenMint] = sig
	}

	if event.AmountSOL > 0 {
		sig.TotalAmountSOL += event.AmountSOL
	}
	sig.WalletCount++

	// Compute confidence based on wallet tier and count.
	sig.Confidence = t.computeConfidence(sig, wallet)

	// Upgrade signal type if multiple wallets buying.
	if sig.WalletCount >= 2 && (sig.Signal == SignalBuy || sig.Signal == SignalFirstMove) {
		sig.Signal = SignalAccum
	}

	// Upgrade tier to highest.
	if tierRank(wallet.Tier) > tierRank(sig.Tier) {
		sig.Tier = wallet.Tier
	}

	// Emit callback if above threshold.
	if sig.Confidence >= t.config.MinConfidence && t.onSignal != nil {
		t.onSignal(*sig)
	}

	log.Debug().
		Str("wallet", event.WalletAddr).
		Str("token", event.TokenMint).
		Str("signal", string(event.Signal)).
		Str("tier", string(wallet.Tier)).
		Float64("amount_sol", event.AmountSOL).
		Float64("confidence", sig.Confidence).
		Msg("copytrade: trade recorded")

	return sig
}

// GetSignal returns the current aggregated signal for a token.
func (t *Tracker) GetSignal(tokenMint string) *CopySignal {
	t.mu.RLock()
	defer t.mu.RUnlock()

	sig, ok := t.signals[tokenMint]
	if !ok {
		return nil
	}

	// Stale check.
	if time.Since(sig.Timestamp) > time.Duration(t.config.SignalWindowMin)*time.Minute*2 {
		return nil
	}

	copy := *sig
	return &copy
}

// GetWallets returns all tracked wallets.
func (t *Tracker) GetWallets() []TrackedWallet {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make([]TrackedWallet, 0, len(t.wallets))
	for _, w := range t.wallets {
		result = append(result, *w)
	}
	return result
}

// UpdateWalletPerformance updates a wallet's win rate and ROI.
func (t *Tracker) UpdateWalletPerformance(address string, won bool, roi float64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	w, ok := t.wallets[address]
	if !ok {
		return
	}

	total := float64(w.TradeCount)
	if total == 0 {
		total = 1
	}

	// Running average.
	if won {
		w.WinRate = (w.WinRate*total + 1) / (total + 1)
	} else {
		w.WinRate = (w.WinRate * total) / (total + 1)
	}
	w.AvgROI = (w.AvgROI*total + roi) / (total + 1)
}

// ScoreImpact returns the score adjustment for a copy-trade signal.
func ScoreImpact(sig *CopySignal) float64 {
	if sig == nil {
		return 0
	}

	base := 0.0
	switch sig.Signal {
	case SignalFirstMove:
		base = 20
	case SignalAccum:
		base = 15
	case SignalBuy:
		base = 10
	case SignalSell:
		base = -15
	case SignalDump:
		base = -30
	}

	// Scale by tier.
	tierMultiplier := 1.0
	switch sig.Tier {
	case TierSmartMoney:
		tierMultiplier = 1.5
	case TierWhale:
		tierMultiplier = 1.3
	case TierKOL:
		tierMultiplier = 1.2
	case TierInsider:
		tierMultiplier = 1.1
	case TierFresh:
		tierMultiplier = 0.7
	}

	return base * tierMultiplier * sig.Confidence
}

// Cleanup removes stale signals.
func (t *Tracker) Cleanup(maxAge time.Duration) int {
	t.mu.Lock()
	defer t.mu.Unlock()

	removed := 0
	cutoff := time.Now().Add(-maxAge)
	for k, sig := range t.signals {
		if sig.Timestamp.Before(cutoff) {
			delete(t.signals, k)
			removed++
		}
	}

	if removed > 0 {
		log.Debug().Int("removed", removed).Msg("copytrade: cleaned up stale signals")
	}
	return removed
}

// Stats returns tracker statistics.
type TrackerStats struct {
	TrackedWallets int            `json:"tracked_wallets"`
	ActiveSignals  int            `json:"active_signals"`
	TotalTrades    int            `json:"total_trades"`
	TierBreakdown  map[string]int `json:"tier_breakdown"`
}

func (t *Tracker) Stats() TrackerStats {
	t.mu.RLock()
	defer t.mu.RUnlock()

	tiers := make(map[string]int)
	for _, w := range t.wallets {
		tiers[string(w.Tier)]++
	}

	return TrackerStats{
		TrackedWallets: len(t.wallets),
		ActiveSignals:  len(t.signals),
		TotalTrades:    len(t.trades),
		TierBreakdown:  tiers,
	}
}

// computeConfidence calculates signal confidence.
func (t *Tracker) computeConfidence(sig *CopySignal, wallet *TrackedWallet) float64 {
	base := 0.3

	// Wallet tier bonus.
	switch wallet.Tier {
	case TierSmartMoney:
		base += 0.3
	case TierWhale:
		base += 0.2
	case TierKOL:
		base += 0.15
	case TierInsider:
		base += 0.1
	}

	// Win rate bonus.
	if wallet.WinRate > 0.7 {
		base += 0.2
	} else if wallet.WinRate > 0.5 {
		base += 0.1
	}

	// Multi-wallet bonus.
	if sig.WalletCount >= 3 {
		base += 0.2
	} else if sig.WalletCount >= 2 {
		base += 0.1
	}

	if base > 1.0 {
		base = 1.0
	}
	return base
}

// tierRank returns numeric rank for tier comparison.
func tierRank(t WalletTier) int {
	switch t {
	case TierSmartMoney:
		return 5
	case TierWhale:
		return 4
	case TierKOL:
		return 3
	case TierInsider:
		return 2
	case TierFresh:
		return 1
	default:
		return 0
	}
}
