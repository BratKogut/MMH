package intel

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/nexus-trading/nexus/internal/bus"
)

// ---------------------------------------------------------------------------
// LLM Provider interface
// ---------------------------------------------------------------------------

// LLMProvider is the interface for all LLM connectors.
// LLM NIE HANDLUJE -- providers perform extraction, classification, and synthesis only.
type LLMProvider interface {
	Name() string
	Query(ctx context.Context, req QueryRequest) (*QueryResponse, error)
	Health() ProviderHealth
}

// QueryRequest is sent to an LLM provider.
type QueryRequest struct {
	Topic       Topic
	Prompt      string
	AssetTags   []string
	MaxTokens   int
	Temperature float64
	TimeoutMs   int
}

// QueryResponse is returned by an LLM provider.
type QueryResponse struct {
	Text       string
	Claims     []Claim
	Sources    []Source
	Confidence float64
	TokensUsed int
	LatencyMs  int
	CostUSD    float64
	Provider   string
}

// ProviderHealth reports the health status of an LLM provider.
type ProviderHealth struct {
	Available    bool
	ErrorRate    float64
	LatencyP95Ms int
	LastError    string
}

// ---------------------------------------------------------------------------
// Trigger types
// ---------------------------------------------------------------------------

// TriggerType classifies what caused the Query Brain to fire a query.
type TriggerType string

const (
	TriggerMarketAnomaly TriggerType = "market_anomaly"
	TriggerSchedule      TriggerType = "schedule"
	TriggerStrategyReq   TriggerType = "strategy_request"
	TriggerCascade       TriggerType = "cascade"
)

// Trigger is an input event that causes the Brain to query an LLM.
type Trigger struct {
	Type      TriggerType
	Topic     Topic
	AssetTags []string
	Prompt    string
	Priority  Priority
	Source    string // who triggered this
}

// ---------------------------------------------------------------------------
// Brain configuration
// ---------------------------------------------------------------------------

// BrainConfig configures the Query Brain.
type BrainConfig struct {
	MaxEventsPerHourNormal   int     // noise control: normal market
	MaxEventsPerHourVolatile int     // noise control: volatile market
	CircuitBreakerErrorPct   float64 // disable provider above this error rate
	CircuitBreakerWindowMs   int
	CircuitBreakerCooldownMs int
}

// DefaultBrainConfig returns sensible defaults.
func DefaultBrainConfig() BrainConfig {
	return BrainConfig{
		MaxEventsPerHourNormal:   60,
		MaxEventsPerHourVolatile: 120,
		CircuitBreakerErrorPct:   0.50,
		CircuitBreakerWindowMs:   60_000,
		CircuitBreakerCooldownMs: 120_000,
	}
}

// ---------------------------------------------------------------------------
// Circuit breaker
// ---------------------------------------------------------------------------

type circuitBreaker struct {
	errors        int
	total         int
	lastError     time.Time
	open          bool
	cooldownUntil time.Time
}

// ---------------------------------------------------------------------------
// Brain -- the intel orchestrator
// ---------------------------------------------------------------------------

// Brain orchestrates LLM queries: it decides WHAT to query, WHEN, from
// WHICH provider, and tracks budgets, dedup, and rate limits.
//
// SELEKTYWNA INTELIGENCJA: Don't mine the internet.
// Ask specific questions with budgets, TTL, and measurable ROI.
type Brain struct {
	mu        sync.RWMutex
	config    BrainConfig
	providers map[string]LLMProvider
	registry  *TopicRegistry
	producer  bus.Producer

	// Circuit breakers per provider.
	breakers map[string]*circuitBreaker

	// Event rate tracking.
	eventsThisHour int
	hourStart      time.Time

	// Recent events for dedup (content hash -> timestamp).
	recentEvents map[string]time.Time

	// Dedup window: events with the same content hash within this window
	// are considered duplicates.
	dedupWindowMs int64

	// Cumulative stats.
	totalQueries atomic.Int64
	totalEvents  atomic.Int64
	totalCostUSD float64 // protected by mu
}

// NewBrain creates a new Brain.
func NewBrain(config BrainConfig, registry *TopicRegistry, producer bus.Producer) *Brain {
	return &Brain{
		config:       config,
		providers:    make(map[string]LLMProvider),
		registry:     registry,
		producer:     producer,
		breakers:     make(map[string]*circuitBreaker),
		hourStart:    time.Now().Truncate(time.Hour),
		recentEvents: make(map[string]time.Time),
		dedupWindowMs: 300_000, // 5 minutes default dedup window
	}
}

// RegisterProvider adds an LLM provider to the Brain.
func (b *Brain) RegisterProvider(provider LLMProvider) {
	b.mu.Lock()
	defer b.mu.Unlock()

	name := provider.Name()
	b.providers[name] = provider
	b.breakers[name] = &circuitBreaker{}

	log.Info().Str("provider", name).Msg("intel: provider registered")
}

// ProcessTrigger processes a trigger and potentially produces an IntelEvent.
// This is the main entry point for the Brain.
func (b *Brain) ProcessTrigger(ctx context.Context, trigger Trigger) (*IntelEvent, error) {
	b.mu.Lock()
	// Rate limit check.
	if !b.checkRateLimitLocked() {
		b.mu.Unlock()
		return nil, fmt.Errorf("rate limit exceeded: %d events this hour", b.eventsThisHour)
	}
	b.mu.Unlock()

	// Budget check via registry.
	ok, reason := b.registry.CanQuery(trigger.Topic)
	if !ok {
		return nil, fmt.Errorf("budget exhausted: %s", reason)
	}

	// Select provider.
	provider, err := b.selectProvider(trigger.Topic)
	if err != nil {
		return nil, fmt.Errorf("no available provider: %w", err)
	}

	// Build query.
	req := b.buildQuery(trigger)

	// Execute query.
	start := time.Now()
	resp, err := provider.Query(ctx, req)
	latency := time.Since(start)

	if err != nil {
		b.recordResult(provider.Name(), false)
		return nil, fmt.Errorf("provider %s query failed: %w", provider.Name(), err)
	}

	// Record success.
	b.recordResult(provider.Name(), true)

	// Record cost in registry.
	b.registry.RecordQuery(trigger.Topic, resp.CostUSD)

	// Build IntelEvent.
	resp.LatencyMs = int(latency.Milliseconds())
	resp.Provider = provider.Name()
	event := b.responseToEvent(trigger, resp)

	// Validate.
	if err := event.Validate(); err != nil {
		return nil, fmt.Errorf("event validation failed: %w", err)
	}

	// Dedup check.
	b.mu.Lock()
	if b.isDuplicateLocked(event) {
		b.mu.Unlock()
		return nil, fmt.Errorf("duplicate event suppressed (hash=%s)", event.ContentHash()[:16])
	}
	b.recentEvents[event.ContentHash()] = time.Now()
	b.eventsThisHour++
	b.totalCostUSD += resp.CostUSD
	b.mu.Unlock()

	// Publish to bus.
	if b.producer != nil {
		if pubErr := b.producer.PublishJSON(ctx, bus.Topics.IntelEvents(), event.EventID, event); pubErr != nil {
			log.Error().Err(pubErr).Str("event_id", event.EventID).Msg("intel: failed to publish event")
			// Don't fail the whole operation -- the event was produced, just not published.
		}
	}

	b.totalQueries.Add(1)
	b.totalEvents.Add(1)

	log.Info().
		Str("event_id", event.EventID).
		Str("topic", string(event.Topic)).
		Str("provider", event.Provider).
		Float64("confidence", event.Confidence).
		Int("latency_ms", event.LatencyMs).
		Float64("cost_usd", event.Cost.ProviderCostUSD).
		Msg("intel: event produced")

	return event, nil
}

// selectProvider picks the best available provider for the given topic.
// It respects circuit breakers and prefers the topic's default provider.
func (b *Brain) selectProvider(topic Topic) (LLMProvider, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if len(b.providers) == 0 {
		return nil, fmt.Errorf("no providers registered")
	}

	// Check topic config for preferred provider.
	cfg, hasCfg := b.registry.GetConfig(topic)

	// Try preferred provider first.
	if hasCfg && cfg.DefaultProvider != "" {
		if p, ok := b.providers[cfg.DefaultProvider]; ok {
			if b.checkCircuitBreakerLocked(cfg.DefaultProvider) {
				return p, nil
			}
		}
	}

	// Fallback: pick any healthy provider.
	for name, p := range b.providers {
		if b.checkCircuitBreakerLocked(name) {
			return p, nil
		}
	}

	return nil, fmt.Errorf("all providers have tripped circuit breakers")
}

// buildQuery transforms a Trigger into a QueryRequest.
func (b *Brain) buildQuery(trigger Trigger) QueryRequest {
	cfg, hasCfg := b.registry.GetConfig(trigger.Topic)

	maxTokens := 1024
	temperature := 0.3 // low temperature for factual extraction
	timeoutMs := 10_000

	// P0 triggers get tighter timeouts.
	if trigger.Priority == PriorityP0 {
		timeoutMs = 5_000
	}

	prompt := trigger.Prompt
	if prompt == "" && hasCfg {
		prompt = fmt.Sprintf("Analyze the current %s situation for assets: %v. "+
			"Extract key claims with confidence levels. "+
			"Identify directional bias and impact horizon.",
			trigger.Topic, trigger.AssetTags)
	}

	_ = cfg

	return QueryRequest{
		Topic:       trigger.Topic,
		Prompt:      prompt,
		AssetTags:   trigger.AssetTags,
		MaxTokens:   maxTokens,
		Temperature: temperature,
		TimeoutMs:   timeoutMs,
	}
}

// responseToEvent converts an LLM response + trigger into an IntelEvent.
func (b *Brain) responseToEvent(trigger Trigger, resp *QueryResponse) *IntelEvent {
	cfg, hasCfg := b.registry.GetConfig(trigger.Topic)

	ttl := 600 // default 10 minutes
	priority := PriorityP2
	if hasCfg {
		ttl = cfg.DefaultTTL
		priority = cfg.DefaultPriority
	}
	if trigger.Priority != "" {
		priority = trigger.Priority
	}

	// Determine directional bias from claims, defaulting to unknown.
	bias := BiasUnknown
	if resp.Confidence > 0.7 && len(resp.Claims) > 0 {
		bias = BiasNeutral // high confidence but no strong direction signal
	}

	// Compute cumulative cost info.
	_, cumCost := b.registry.GetUsage(trigger.Topic)
	cumCost += resp.CostUSD

	budgetRemaining := 0.0
	if hasCfg {
		budgetRemaining = cfg.CostBudgetUSD - cumCost
		if budgetRemaining < 0 {
			budgetRemaining = 0
		}
	}

	title := fmt.Sprintf("[%s] Intel for %v", trigger.Topic, trigger.AssetTags)
	summary := resp.Text
	if len(summary) > 500 {
		summary = summary[:500]
	}

	return &IntelEvent{
		EventID:         uuid.New().String(),
		Timestamp:       time.Now(),
		Topic:           trigger.Topic,
		Priority:        priority,
		TTLSeconds:      ttl,
		Confidence:      resp.Confidence,
		ImpactHorizon:   HorizonShortTerm,
		DirectionalBias: bias,
		AssetTags:       trigger.AssetTags,
		Title:           title,
		Summary:         summary,
		Claims:          resp.Claims,
		Sources:         resp.Sources,
		Provider:        resp.Provider,
		Cost: CostInfo{
			ProviderCostUSD:          resp.CostUSD,
			CumulativeTopicCostToday: cumCost,
			BudgetRemainingToday:     budgetRemaining,
		},
		LatencyMs: resp.LatencyMs,
	}
}

// checkCircuitBreakerLocked checks if the provider's circuit breaker allows requests.
// Caller must hold b.mu (at least RLock).
func (b *Brain) checkCircuitBreakerLocked(providerName string) bool {
	cb, ok := b.breakers[providerName]
	if !ok {
		return true // no breaker = allow
	}

	if !cb.open {
		return true
	}

	// Check if cooldown has elapsed.
	if time.Now().After(cb.cooldownUntil) {
		cb.open = false
		cb.errors = 0
		cb.total = 0
		log.Info().Str("provider", providerName).Msg("intel: circuit breaker closed (cooldown elapsed)")
		return true
	}

	return false
}

// recordResult updates the circuit breaker for a provider after a query.
func (b *Brain) recordResult(providerName string, success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	cb, ok := b.breakers[providerName]
	if !ok {
		cb = &circuitBreaker{}
		b.breakers[providerName] = cb
	}

	cb.total++
	if !success {
		cb.errors++
		cb.lastError = time.Now()
	}

	// Check if we should open the breaker.
	if cb.total >= 5 && !cb.open { // minimum sample size
		errorRate := float64(cb.errors) / float64(cb.total)
		if errorRate >= b.config.CircuitBreakerErrorPct {
			cb.open = true
			cb.cooldownUntil = time.Now().Add(
				time.Duration(b.config.CircuitBreakerCooldownMs) * time.Millisecond)
			log.Warn().
				Str("provider", providerName).
				Float64("error_rate", errorRate).
				Time("cooldown_until", cb.cooldownUntil).
				Msg("intel: circuit breaker OPENED")
		}
	}
}

// isDuplicateLocked checks if we've already produced a semantically identical event recently.
// Caller must hold b.mu (write lock).
func (b *Brain) isDuplicateLocked(event *IntelEvent) bool {
	hash := event.ContentHash()
	if lastSeen, ok := b.recentEvents[hash]; ok {
		windowDuration := time.Duration(b.dedupWindowMs) * time.Millisecond
		if time.Since(lastSeen) < windowDuration {
			return true
		}
	}
	return false
}

// checkRateLimitLocked checks hourly event rate limits.
// Caller must hold b.mu (write lock).
func (b *Brain) checkRateLimitLocked() bool {
	now := time.Now()
	currentHour := now.Truncate(time.Hour)

	// Reset counter on hour boundary.
	if !b.hourStart.Equal(currentHour) {
		b.eventsThisHour = 0
		b.hourStart = currentHour
	}

	return b.eventsThisHour < b.config.MaxEventsPerHourNormal
}

// ---------------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------------

// BrainStats provides aggregate statistics about Brain operation.
type BrainStats struct {
	TotalQueries  int64
	TotalEvents   int64
	TotalCostUSD  float64
	ProviderStats map[string]ProviderStats
	TopicStats    map[Topic]TopicStats
}

// ProviderStats tracks per-provider statistics.
type ProviderStats struct {
	Queries      int
	Errors       int
	AvgLatencyMs int
	TotalCostUSD float64
	CircuitOpen  bool
}

// TopicStats tracks per-topic statistics.
type TopicStats struct {
	QueriesUsed   int
	BudgetUsed    float64
	EventsEmitted int
}

// Stats returns aggregate statistics about Brain operation.
func (b *Brain) Stats() BrainStats {
	b.mu.RLock()
	defer b.mu.RUnlock()

	stats := BrainStats{
		TotalQueries:  b.totalQueries.Load(),
		TotalEvents:   b.totalEvents.Load(),
		TotalCostUSD:  b.totalCostUSD,
		ProviderStats: make(map[string]ProviderStats),
		TopicStats:    make(map[Topic]TopicStats),
	}

	for name, cb := range b.breakers {
		stats.ProviderStats[name] = ProviderStats{
			Queries:     cb.total,
			Errors:      cb.errors,
			CircuitOpen: cb.open,
		}
	}

	return stats
}
