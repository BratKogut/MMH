package scanner

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nexus-trading/nexus/internal/graph"
	"github.com/nexus-trading/nexus/internal/solana"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// L0 Sanitizer — fast-filter layer, ZERO external API calls
// Target: <10ms total, ~85% rejection rate
// ---------------------------------------------------------------------------

// SanitizerConfig configures the L0 sanitizer.
type SanitizerConfig struct {
	// Minimum liquidity in USD.
	MinLiquidityUSD float64 `yaml:"min_liquidity_usd"`

	// Minimum SOL in quote reserve.
	MinQuoteReserveSOL float64 `yaml:"min_quote_reserve_sol"`

	// Maximum tokens deployed by same deployer in 24h.
	MaxDeployerTokens24h int `yaml:"max_deployer_tokens_24h"`

	// Sybil cluster size threshold.
	SybilClusterThreshold int `yaml:"sybil_cluster_threshold"`
}

// DefaultSanitizerConfig returns production defaults.
func DefaultSanitizerConfig() SanitizerConfig {
	return SanitizerConfig{
		MinLiquidityUSD:       500,
		MinQuoteReserveSOL:    1.0,
		MaxDeployerTokens24h:  5,
		SybilClusterThreshold: 20,
	}
}

// SanitizerResult is the outcome of L0 filtering.
type SanitizerResult struct {
	Passed    bool   `json:"passed"`
	Reason    string `json:"reason,omitempty"`     // drop reason if !Passed
	Filter    string `json:"filter,omitempty"`     // which filter caught it
	LatencyUs int64  `json:"latency_us"`
}

// Sanitizer performs fast pre-filtering before expensive enrichment.
type Sanitizer struct {
	config     SanitizerConfig
	graph      *graph.Engine
	poolState  *PoolStateManager

	// Blacklisted deployer addresses (known scammers).
	mu         sync.RWMutex
	blacklist  map[string]bool

	// Deployer history: address -> token count in 24h window.
	deployerMu    sync.RWMutex
	deployerHist  map[string]*deployerEntry

	// Stats.
	totalChecked  atomic.Int64
	totalPassed   atomic.Int64
	totalDropped  atomic.Int64
	filterCounts  sync.Map // filter_name -> *atomic.Int64
}

type deployerEntry struct {
	count     int
	firstSeen time.Time
}

// NewSanitizer creates a new L0 sanitizer.
func NewSanitizer(config SanitizerConfig, graphEngine *graph.Engine, poolState *PoolStateManager) *Sanitizer {
	return &Sanitizer{
		config:       config,
		graph:        graphEngine,
		poolState:    poolState,
		blacklist:    make(map[string]bool),
		deployerHist: make(map[string]*deployerEntry),
	}
}

// Check runs all L0 filters on a pool discovery event. <10ms budget.
func (s *Sanitizer) Check(discovery PoolDiscovery) SanitizerResult {
	start := time.Now()
	s.totalChecked.Add(1)

	pool := discovery.Pool

	// F1: Blacklist check (~0.1ms).
	if r := s.checkBlacklist(pool); !r.Passed {
		r.LatencyUs = time.Since(start).Microseconds()
		s.recordDrop("blacklist")
		return r
	}

	// F2: Name/symbol filter (~0.1ms).
	if discovery.Token != nil {
		if r := s.checkNamePatterns(discovery.Token); !r.Passed {
			r.LatencyUs = time.Since(start).Microseconds()
			s.recordDrop("name_pattern")
			return r
		}
	}

	// F3: Mint/freeze authority check (~0.1ms).
	if discovery.Token != nil {
		if r := s.checkAuthorities(discovery.Token); !r.Passed {
			r.LatencyUs = time.Since(start).Microseconds()
			s.recordDrop("authority")
			return r
		}
	}

	// F4: Liquidity check from in-memory state (~0.1ms).
	if r := s.checkLiquidity(pool); !r.Passed {
		r.LatencyUs = time.Since(start).Microseconds()
		s.recordDrop("liquidity")
		return r
	}

	// F5: Entity graph check (~2ms). Returns deployer address for F6.
	var deployerAddr string
	if s.graph != nil && pool.TokenMint != "" {
		var r SanitizerResult
		r, deployerAddr = s.checkEntityGraph(pool)
		if !r.Passed {
			r.LatencyUs = time.Since(start).Microseconds()
			s.recordDrop("entity_graph")
			return r
		}
	}

	// F6: Deployer history (~0.1ms). Uses real deployer from entity graph.
	if pool.TokenMint != "" {
		addr := deployerAddr
		if addr == "" {
			addr = string(pool.TokenMint) // fallback if no graph
		}
		if r := s.checkDeployerHistory(addr); !r.Passed {
			r.LatencyUs = time.Since(start).Microseconds()
			s.recordDrop("deployer_hist")
			return r
		}
	}

	s.totalPassed.Add(1)
	return SanitizerResult{
		Passed:    true,
		LatencyUs: time.Since(start).Microseconds(),
	}
}

// AddBlacklist adds addresses to the blacklist.
func (s *Sanitizer) AddBlacklist(addresses []string) {
	s.mu.Lock()
	for _, addr := range addresses {
		s.blacklist[addr] = true
	}
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Filter implementations
// ---------------------------------------------------------------------------

func (s *Sanitizer) checkBlacklist(pool solana.PoolInfo) SanitizerResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.blacklist[string(pool.TokenMint)] {
		return SanitizerResult{Passed: false, Reason: "token mint blacklisted", Filter: "blacklist"}
	}
	return SanitizerResult{Passed: true}
}

// Honeypot name patterns - known scam naming conventions.
var honeypotPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^safe.*(moon|mars|elon)`),
	regexp.MustCompile(`(?i)(eloncum|elonmusk|dogelon)`),
	regexp.MustCompile(`(?i)^(test|debug|fake)\s`),
	regexp.MustCompile(`(?i)(honeypot|rugpull|scam)`),
	regexp.MustCompile(`(?i)^(free|airdrop|claim)\s`),
}

// Top token names that scammers impersonate.
var impersonationNames = map[string]bool{
	"bitcoin": true, "ethereum": true, "solana": true, "usdc": true,
	"usdt": true, "bonk": true, "wif": true, "jup": true,
	"raydium": true, "jupiter": true, "marinade": true,
}

func (s *Sanitizer) checkNamePatterns(token *solana.TokenInfo) SanitizerResult {
	name := strings.ToLower(token.Name)
	symbol := strings.ToLower(token.Symbol)

	// Check honeypot name patterns.
	for _, p := range honeypotPatterns {
		if p.MatchString(name) || p.MatchString(symbol) {
			return SanitizerResult{
				Passed: false,
				Reason: "matches honeypot name pattern: " + p.String(),
				Filter: "name_pattern",
			}
		}
	}

	// Check impersonation of known tokens.
	if impersonationNames[name] || impersonationNames[symbol] {
		return SanitizerResult{
			Passed: false,
			Reason: "impersonates known token: " + name,
			Filter: "name_pattern",
		}
	}

	// Check for unicode tricks (Cyrillic lookalikes, confusable scripts).
	for _, r := range name {
		if (r >= 0x0400 && r <= 0x04FF) || // Cyrillic
			(r >= 0x0500 && r <= 0x052F) { // Cyrillic Supplement
			return SanitizerResult{
				Passed: false,
				Reason: "suspicious unicode characters in name (Cyrillic)",
				Filter: "name_pattern",
			}
		}
	}

	return SanitizerResult{Passed: true}
}

func (s *Sanitizer) checkAuthorities(token *solana.TokenInfo) SanitizerResult {
	// Freeze authority active = instant kill (can freeze your tokens).
	if !token.IsFreezeRenounced() {
		return SanitizerResult{
			Passed: false,
			Reason: "freeze authority active",
			Filter: "authority",
		}
	}
	return SanitizerResult{Passed: true}
}

func (s *Sanitizer) checkLiquidity(pool solana.PoolInfo) SanitizerResult {
	minLiq := decimal.NewFromFloat(s.config.MinLiquidityUSD)
	if pool.LiquidityUSD.LessThan(minLiq) {
		return SanitizerResult{
			Passed: false,
			Reason: "liquidity " + pool.LiquidityUSD.String() + " < min " + minLiq.String(),
			Filter: "liquidity",
		}
	}

	// Check quote reserve (SOL).
	minQuote := decimal.NewFromFloat(s.config.MinQuoteReserveSOL)
	if pool.QuoteMint == solana.SOLMint && pool.QuoteReserve.LessThan(minQuote) {
		return SanitizerResult{
			Passed: false,
			Reason: "SOL reserve " + pool.QuoteReserve.String() + " < min " + minQuote.String(),
			Filter: "liquidity",
		}
	}

	return SanitizerResult{Passed: true}
}

func (s *Sanitizer) checkEntityGraph(pool solana.PoolInfo) (SanitizerResult, string) {
	report := s.graph.QueryDeployer(string(pool.TokenMint))
	deployer := report.SeedFunder // actual deployer address from graph

	// 1-hop to serial rugger = instant kill.
	if report.HopsToRugger == 1 {
		return SanitizerResult{
			Passed: false,
			Reason: "deployer 1-hop from SERIAL_RUGGER",
			Filter: "entity_graph",
		}, deployer
	}

	// Sybil cluster too large.
	if report.ClusterSize > s.config.SybilClusterThreshold {
		return SanitizerResult{
			Passed: false,
			Reason: fmt.Sprintf("deployer in large Sybil cluster: %d", report.ClusterSize),
			Filter: "entity_graph",
		}, deployer
	}

	// Wash trader label.
	for _, label := range report.Labels {
		if label == graph.LabelWashTrader {
			return SanitizerResult{
				Passed: false,
				Reason: "deployer labeled WASH_TRADER",
				Filter: "entity_graph",
			}, deployer
		}
	}

	return SanitizerResult{Passed: true}, deployer
}

func (s *Sanitizer) checkDeployerHistory(deployer string) SanitizerResult {
	now := time.Now()

	s.deployerMu.Lock()
	defer s.deployerMu.Unlock()

	entry, exists := s.deployerHist[deployer]
	if !exists {
		s.deployerHist[deployer] = &deployerEntry{count: 1, firstSeen: now}
		return SanitizerResult{Passed: true}
	}

	// Reset if older than 24h.
	if now.Sub(entry.firstSeen) > 24*time.Hour {
		entry.count = 1
		entry.firstSeen = now
		return SanitizerResult{Passed: true}
	}

	entry.count++
	if entry.count > s.config.MaxDeployerTokens24h {
		return SanitizerResult{
			Passed: false,
			Reason: "deployer created too many tokens in 24h",
			Filter: "deployer_hist",
		}
	}

	return SanitizerResult{Passed: true}
}

// ---------------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------------

func (s *Sanitizer) recordDrop(filterName string) {
	s.totalDropped.Add(1)
	val, _ := s.filterCounts.LoadOrStore(filterName, &atomic.Int64{})
	val.(*atomic.Int64).Add(1)
	log.Debug().Str("filter", filterName).Msg("sanitizer: token dropped")
}

// SanitizerStats returns sanitizer statistics.
type SanitizerStats struct {
	TotalChecked int64            `json:"total_checked"`
	TotalPassed  int64            `json:"total_passed"`
	TotalDropped int64            `json:"total_dropped"`
	PassRate     float64          `json:"pass_rate_pct"`
	FilterCounts map[string]int64 `json:"filter_counts"`
}

func (s *Sanitizer) Stats() SanitizerStats {
	checked := s.totalChecked.Load()
	passed := s.totalPassed.Load()
	passRate := 0.0
	if checked > 0 {
		passRate = float64(passed) / float64(checked) * 100
	}

	counts := make(map[string]int64)
	s.filterCounts.Range(func(key, value any) bool {
		counts[key.(string)] = value.(*atomic.Int64).Load()
		return true
	})

	return SanitizerStats{
		TotalChecked: checked,
		TotalPassed:  passed,
		TotalDropped: s.totalDropped.Load(),
		PassRate:     passRate,
		FilterCounts: counts,
	}
}

// CleanupDeployerHistory removes stale deployer entries.
func (s *Sanitizer) CleanupDeployerHistory() {
	cutoff := time.Now().Add(-24 * time.Hour)
	s.deployerMu.Lock()
	for addr, entry := range s.deployerHist {
		if entry.firstSeen.Before(cutoff) {
			delete(s.deployerHist, addr)
		}
	}
	s.deployerMu.Unlock()
}
