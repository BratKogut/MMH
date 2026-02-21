package intel

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus-trading/nexus/internal/bus"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newTestBrain creates a Brain with a single healthy StubProvider.
func newTestBrain(responses []QueryResponse) (*Brain, *bus.StubProducer) {
	producer := bus.NewStubProducer()
	registry := NewTopicRegistry(DefaultTaxonomy())
	config := DefaultBrainConfig()
	brain := NewBrain(config, registry, producer)

	provider := NewStubProvider("primary", responses)
	brain.RegisterProvider(provider)

	return brain, producer
}

// defaultResponse returns a well-formed QueryResponse for testing.
func defaultResponse() QueryResponse {
	return QueryResponse{
		Text:       "BTC showing bullish momentum on increased volume",
		Confidence: 0.85,
		TokensUsed: 256,
		LatencyMs:  120,
		CostUSD:    0.005,
		Claims: []Claim{
			{Statement: "BTC volume increased 40% in last hour", Confidence: 0.9, Verifiable: true},
			{Statement: "Funding rates turning positive", Confidence: 0.75, Verifiable: true},
		},
		Sources: []Source{
			{Name: "market_data", Type: "api"},
		},
	}
}

// defaultTrigger returns a well-formed Trigger for testing.
func defaultTrigger() Trigger {
	return Trigger{
		Type:      TriggerMarketAnomaly,
		Topic:     TopicVolatilitySpike,
		AssetTags: []string{"BTC-USDT"},
		Prompt:    "Analyze BTC volatility spike",
		Priority:  PriorityP1,
		Source:    "test",
	}
}

// ---------------------------------------------------------------------------
// TestBrain_ProcessTrigger_HappyPath
// ---------------------------------------------------------------------------

func TestBrain_ProcessTrigger_HappyPath(t *testing.T) {
	brain, producer := newTestBrain([]QueryResponse{defaultResponse()})
	ctx := context.Background()
	trigger := defaultTrigger()

	event, err := brain.ProcessTrigger(ctx, trigger)
	require.NoError(t, err)
	require.NotNil(t, event)

	// Event fields should be populated.
	assert.NotEmpty(t, event.EventID)
	assert.Equal(t, TopicVolatilitySpike, event.Topic)
	assert.Equal(t, PriorityP1, event.Priority)
	assert.InDelta(t, 0.85, event.Confidence, 0.001)
	assert.Equal(t, "primary", event.Provider)
	assert.Greater(t, event.TTLSeconds, 0)
	assert.Equal(t, []string{"BTC-USDT"}, event.AssetTags)
	assert.Len(t, event.Claims, 2)
	assert.Len(t, event.Sources, 1)

	// Cost tracking.
	assert.InDelta(t, 0.005, event.Cost.ProviderCostUSD, 0.0001)
	assert.Greater(t, event.Cost.BudgetRemainingToday, 0.0)

	// Should have published to bus.
	assert.Len(t, producer.Messages, 1)
	assert.Equal(t, "intel.events.global", producer.Messages[0].Topic)
}

// ---------------------------------------------------------------------------
// TestBrain_BudgetExhausted
// ---------------------------------------------------------------------------

func TestBrain_BudgetExhausted(t *testing.T) {
	producer := bus.NewStubProducer()
	// Create a topic with very low budget.
	configs := map[Topic]TopicConfig{
		TopicVolatilitySpike: {
			Topic:            TopicVolatilitySpike,
			MaxQueriesPerDay: 2,
			DefaultProvider:  "primary",
			DefaultTTL:       300,
			DefaultPriority:  PriorityP0,
			CostBudgetUSD:    0.01, // tiny budget
		},
	}
	registry := NewTopicRegistry(configs)
	brain := NewBrain(DefaultBrainConfig(), registry, producer)
	brain.RegisterProvider(NewStubProvider("primary", []QueryResponse{defaultResponse()}))

	ctx := context.Background()
	trigger := defaultTrigger()

	// First query should succeed.
	event, err := brain.ProcessTrigger(ctx, trigger)
	require.NoError(t, err)
	require.NotNil(t, event)

	// Second query should succeed (different content won't be deduped because
	// we use the same response, but the brain creates events with UUID-based
	// event IDs, and the content hash might match -- we need a different
	// trigger to avoid dedup). Actually the dedup is based on content hash,
	// which will match. Let's exhaust the budget by recording extra cost.
	registry.RecordQuery(TopicVolatilitySpike, 0.01) // push over budget

	// Now budget should be exhausted.
	_, err = brain.ProcessTrigger(ctx, trigger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "budget exhausted")
}

// ---------------------------------------------------------------------------
// TestBrain_CircuitBreaker_Opens
// ---------------------------------------------------------------------------

func TestBrain_CircuitBreaker_Opens(t *testing.T) {
	producer := bus.NewStubProducer()
	registry := NewTopicRegistry(DefaultTaxonomy())

	config := DefaultBrainConfig()
	config.CircuitBreakerErrorPct = 0.50
	config.CircuitBreakerCooldownMs = 60_000

	brain := NewBrain(config, registry, producer)

	// Register an unhealthy provider.
	unhealthy := NewStubProvider("primary", nil)
	unhealthy.SetHealthy(false)
	brain.RegisterProvider(unhealthy)

	ctx := context.Background()
	trigger := defaultTrigger()

	// Fire enough queries to trip the circuit breaker (5 minimum sample, 50% error threshold).
	for i := 0; i < 6; i++ {
		_, _ = brain.ProcessTrigger(ctx, trigger)
	}

	// Now the circuit breaker should be open.
	_, err := brain.ProcessTrigger(ctx, trigger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "circuit breaker")
}

// ---------------------------------------------------------------------------
// TestBrain_CircuitBreaker_Recovers
// ---------------------------------------------------------------------------

func TestBrain_CircuitBreaker_Recovers(t *testing.T) {
	producer := bus.NewStubProducer()
	registry := NewTopicRegistry(DefaultTaxonomy())

	config := DefaultBrainConfig()
	config.CircuitBreakerErrorPct = 0.50
	config.CircuitBreakerCooldownMs = 1 // 1ms cooldown for fast test

	brain := NewBrain(config, registry, producer)

	unhealthy := NewStubProvider("primary", nil)
	unhealthy.SetHealthy(false)
	brain.RegisterProvider(unhealthy)

	ctx := context.Background()
	trigger := defaultTrigger()

	// Trip the breaker.
	for i := 0; i < 6; i++ {
		_, _ = brain.ProcessTrigger(ctx, trigger)
	}

	// Wait for cooldown to elapse.
	time.Sleep(5 * time.Millisecond)

	// Now make the provider healthy and give it responses.
	// We need to replace the provider with a healthy one.
	healthy := NewStubProvider("primary", []QueryResponse{defaultResponse()})
	brain.mu.Lock()
	brain.providers["primary"] = healthy
	brain.mu.Unlock()

	// Should recover: the circuit breaker closes after cooldown.
	event, err := brain.ProcessTrigger(ctx, trigger)
	require.NoError(t, err)
	require.NotNil(t, event)
}

// ---------------------------------------------------------------------------
// TestBrain_RateLimit
// ---------------------------------------------------------------------------

func TestBrain_RateLimit(t *testing.T) {
	producer := bus.NewStubProducer()
	registry := NewTopicRegistry(DefaultTaxonomy())

	config := DefaultBrainConfig()
	config.MaxEventsPerHourNormal = 3 // very low limit for testing

	brain := NewBrain(config, registry, producer)

	// We need unique responses to avoid dedup.
	responses := []QueryResponse{
		{Text: "analysis alpha", Confidence: 0.8, CostUSD: 0.001, Claims: []Claim{{Statement: "alpha claim", Confidence: 0.8}}, Sources: []Source{{Name: "s1", Type: "api"}}},
		{Text: "analysis beta", Confidence: 0.7, CostUSD: 0.001, Claims: []Claim{{Statement: "beta claim", Confidence: 0.7}}, Sources: []Source{{Name: "s2", Type: "api"}}},
		{Text: "analysis gamma", Confidence: 0.9, CostUSD: 0.001, Claims: []Claim{{Statement: "gamma claim", Confidence: 0.9}}, Sources: []Source{{Name: "s3", Type: "api"}}},
		{Text: "analysis delta", Confidence: 0.6, CostUSD: 0.001, Claims: []Claim{{Statement: "delta claim", Confidence: 0.6}}, Sources: []Source{{Name: "s4", Type: "api"}}},
	}
	brain.RegisterProvider(NewStubProvider("primary", responses))

	ctx := context.Background()

	// Use unique triggers to avoid dedup by varying the prompt.
	triggers := []Trigger{
		{Type: TriggerMarketAnomaly, Topic: TopicVolatilitySpike, AssetTags: []string{"BTC-USDT"}, Prompt: "prompt-1", Priority: PriorityP1, Source: "test"},
		{Type: TriggerMarketAnomaly, Topic: TopicSentimentShift, AssetTags: []string{"ETH-USDT"}, Prompt: "prompt-2", Priority: PriorityP2, Source: "test"},
		{Type: TriggerMarketAnomaly, Topic: TopicWhaleMovement, AssetTags: []string{"SOL-USDT"}, Prompt: "prompt-3", Priority: PriorityP1, Source: "test"},
	}

	// First 3 should succeed.
	for i := 0; i < 3; i++ {
		event, err := brain.ProcessTrigger(ctx, triggers[i])
		require.NoError(t, err, "trigger %d should succeed", i)
		require.NotNil(t, event)
	}

	// 4th should be rate-limited.
	_, err := brain.ProcessTrigger(ctx, Trigger{
		Type: TriggerMarketAnomaly, Topic: TopicMacroEvent,
		AssetTags: []string{"DOGE-USDT"}, Prompt: "prompt-4", Priority: PriorityP3, Source: "test",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rate limit exceeded")
}

// ---------------------------------------------------------------------------
// TestBrain_Dedup
// ---------------------------------------------------------------------------

func TestBrain_Dedup(t *testing.T) {
	brain, _ := newTestBrain([]QueryResponse{defaultResponse()})
	ctx := context.Background()
	trigger := defaultTrigger()

	// First query succeeds.
	event1, err := brain.ProcessTrigger(ctx, trigger)
	require.NoError(t, err)
	require.NotNil(t, event1)

	// Exact same trigger again -- should be deduplicated.
	_, err = brain.ProcessTrigger(ctx, trigger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")
}

// ---------------------------------------------------------------------------
// TestBrain_ProviderSelection
// ---------------------------------------------------------------------------

func TestBrain_ProviderSelection(t *testing.T) {
	producer := bus.NewStubProducer()
	registry := NewTopicRegistry(DefaultTaxonomy())
	brain := NewBrain(DefaultBrainConfig(), registry, producer)

	primaryResp := defaultResponse()
	primaryResp.Text = "primary analysis"
	fallbackResp := defaultResponse()
	fallbackResp.Text = "fallback analysis"

	primary := NewStubProvider("primary", []QueryResponse{primaryResp})
	fallback := NewStubProvider("fallback", []QueryResponse{fallbackResp})

	brain.RegisterProvider(primary)
	brain.RegisterProvider(fallback)

	ctx := context.Background()
	trigger := defaultTrigger()

	// Should use primary (it's the default provider in taxonomy).
	event, err := brain.ProcessTrigger(ctx, trigger)
	require.NoError(t, err)
	assert.Equal(t, "primary", event.Provider)
	assert.Equal(t, 1, primary.Calls())

	// Now disable primary via circuit breaker.
	brain.mu.Lock()
	brain.breakers["primary"].open = true
	brain.breakers["primary"].cooldownUntil = time.Now().Add(1 * time.Hour)
	// Clear dedup cache so same trigger goes through.
	brain.recentEvents = make(map[string]time.Time)
	brain.mu.Unlock()

	// Should fall back to the other provider.
	event2, err := brain.ProcessTrigger(ctx, trigger)
	require.NoError(t, err)
	assert.Equal(t, "fallback", event2.Provider)
}

// ---------------------------------------------------------------------------
// TestBrain_EventValidation
// ---------------------------------------------------------------------------

func TestBrain_EventValidation(t *testing.T) {
	t.Run("valid event passes", func(t *testing.T) {
		event := &IntelEvent{
			EventID:         "evt-001",
			Timestamp:       time.Now(),
			Topic:           TopicVolatilitySpike,
			Priority:        PriorityP0,
			TTLSeconds:      300,
			Confidence:      0.85,
			ImpactHorizon:   HorizonImmediate,
			DirectionalBias: BiasBullish,
			AssetTags:       []string{"BTC-USDT"},
			Title:           "Vol spike detected",
			Summary:         "BTC 30m realized vol jumped 200%",
			Provider:        "primary",
			Claims: []Claim{
				{Statement: "vol up 200%", Confidence: 0.9, Verifiable: true},
			},
		}
		assert.NoError(t, event.Validate())
	})

	t.Run("missing event_id fails", func(t *testing.T) {
		event := &IntelEvent{
			Timestamp:       time.Now(),
			Topic:           TopicVolatilitySpike,
			Priority:        PriorityP0,
			TTLSeconds:      300,
			Confidence:      0.85,
			ImpactHorizon:   HorizonImmediate,
			DirectionalBias: BiasBullish,
			AssetTags:       []string{"BTC-USDT"},
			Title:           "test",
			Summary:         "test",
			Provider:        "primary",
		}
		err := event.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "event_id")
	})

	t.Run("confidence out of range fails", func(t *testing.T) {
		event := &IntelEvent{
			EventID:         "evt-002",
			Timestamp:       time.Now(),
			Topic:           TopicVolatilitySpike,
			Priority:        PriorityP0,
			TTLSeconds:      300,
			Confidence:      1.5, // invalid
			ImpactHorizon:   HorizonImmediate,
			DirectionalBias: BiasBullish,
			AssetTags:       []string{"BTC-USDT"},
			Title:           "test",
			Summary:         "test",
			Provider:        "primary",
		}
		err := event.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "confidence")
	})

	t.Run("invalid topic fails", func(t *testing.T) {
		event := &IntelEvent{
			EventID:         "evt-003",
			Timestamp:       time.Now(),
			Topic:           Topic("invalid_topic"),
			Priority:        PriorityP0,
			TTLSeconds:      300,
			Confidence:      0.5,
			ImpactHorizon:   HorizonImmediate,
			DirectionalBias: BiasBullish,
			AssetTags:       []string{"BTC-USDT"},
			Title:           "test",
			Summary:         "test",
			Provider:        "primary",
		}
		err := event.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "topic")
	})

	t.Run("negative TTL fails", func(t *testing.T) {
		event := &IntelEvent{
			EventID:         "evt-004",
			Timestamp:       time.Now(),
			Topic:           TopicMacroEvent,
			Priority:        PriorityP1,
			TTLSeconds:      -10,
			Confidence:      0.5,
			ImpactHorizon:   HorizonShortTerm,
			DirectionalBias: BiasNeutral,
			AssetTags:       []string{"BTC-USDT"},
			Title:           "test",
			Summary:         "test",
			Provider:        "primary",
		}
		err := event.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ttl_seconds")
	})

	t.Run("no asset tags fails", func(t *testing.T) {
		event := &IntelEvent{
			EventID:         "evt-005",
			Timestamp:       time.Now(),
			Topic:           TopicMacroEvent,
			Priority:        PriorityP1,
			TTLSeconds:      300,
			Confidence:      0.5,
			ImpactHorizon:   HorizonShortTerm,
			DirectionalBias: BiasNeutral,
			AssetTags:       []string{},
			Title:           "test",
			Summary:         "test",
			Provider:        "primary",
		}
		err := event.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "asset_tag")
	})

	t.Run("claim with bad confidence fails", func(t *testing.T) {
		event := &IntelEvent{
			EventID:         "evt-006",
			Timestamp:       time.Now(),
			Topic:           TopicMacroEvent,
			Priority:        PriorityP1,
			TTLSeconds:      300,
			Confidence:      0.5,
			ImpactHorizon:   HorizonShortTerm,
			DirectionalBias: BiasNeutral,
			AssetTags:       []string{"ETH-USDT"},
			Title:           "test",
			Summary:         "test",
			Provider:        "primary",
			Claims: []Claim{
				{Statement: "something", Confidence: 2.0}, // invalid
			},
		}
		err := event.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "claim[0]")
	})
}

// ---------------------------------------------------------------------------
// TestBrain_IsExpired
// ---------------------------------------------------------------------------

func TestBrain_IsExpired(t *testing.T) {
	t.Run("not expired", func(t *testing.T) {
		event := &IntelEvent{
			Timestamp:  time.Now(),
			TTLSeconds: 3600,
		}
		assert.False(t, event.IsExpired())
	})

	t.Run("expired", func(t *testing.T) {
		event := &IntelEvent{
			Timestamp:  time.Now().Add(-2 * time.Hour),
			TTLSeconds: 3600,
		}
		assert.True(t, event.IsExpired())
	})

	t.Run("zero TTL is always expired", func(t *testing.T) {
		event := &IntelEvent{
			Timestamp:  time.Now(),
			TTLSeconds: 0,
		}
		assert.True(t, event.IsExpired())
	})
}

// ---------------------------------------------------------------------------
// TestROITracker_LinkTradeToIntel
// ---------------------------------------------------------------------------

func TestROITracker_LinkTradeToIntel(t *testing.T) {
	tracker := NewROITracker(600_000) // 10 minute link window

	now := time.Now()
	event := &IntelEvent{
		EventID:    "intel-001",
		Timestamp:  now,
		TTLSeconds: 3600,
		AssetTags:  []string{"BTC-USDT", "ETH-USDT"},
		Cost: CostInfo{
			ProviderCostUSD: 0.01,
		},
	}
	tracker.RecordIntelEvent(event)

	// Trade within window on matching asset.
	tradeTime := now.Add(5 * time.Minute)
	links := tracker.RecordTrade("trade-001", "strat-alpha", "BTC-USDT", 50.0, tradeTime)

	require.Len(t, links, 1)
	assert.Equal(t, "intel-001", links[0].IntelEventID)
	assert.Equal(t, "trade-001", links[0].TradeID)
	assert.Equal(t, "strat-alpha", links[0].StrategyID)
	assert.InDelta(t, 50.0, links[0].PnL, 0.001)
	assert.InDelta(t, 0.01, links[0].IntelCostUSD, 0.0001)

	// ROI should reflect the link.
	roi := tracker.ComputeROI()
	assert.Equal(t, 1, roi.TotalLinks)
	assert.InDelta(t, 50.0, roi.TotalLinkedPnL, 0.001)
	assert.InDelta(t, 0.01, roi.TotalIntelCostUSD, 0.0001)
	assert.Greater(t, roi.NetROI, 0.0)
}

// ---------------------------------------------------------------------------
// TestROITracker_NoLinkForUnrelatedAsset
// ---------------------------------------------------------------------------

func TestROITracker_NoLinkForUnrelatedAsset(t *testing.T) {
	tracker := NewROITracker(600_000)

	now := time.Now()
	event := &IntelEvent{
		EventID:    "intel-002",
		Timestamp:  now,
		TTLSeconds: 3600,
		AssetTags:  []string{"BTC-USDT"},
		Cost:       CostInfo{ProviderCostUSD: 0.01},
	}
	tracker.RecordIntelEvent(event)

	// Trade on different asset -- should not link.
	links := tracker.RecordTrade("trade-002", "strat-beta", "DOGE-USDT", 10.0, now.Add(1*time.Minute))
	assert.Empty(t, links)
}

// ---------------------------------------------------------------------------
// TestROITracker_ExpiredEventsNotLinked
// ---------------------------------------------------------------------------

func TestROITracker_ExpiredEventsNotLinked(t *testing.T) {
	tracker := NewROITracker(600_000) // 10 minute window

	// Create an event that's already expired (TTL=1s, created 10s ago).
	event := &IntelEvent{
		EventID:    "intel-003",
		Timestamp:  time.Now().Add(-10 * time.Second),
		TTLSeconds: 1,
		AssetTags:  []string{"BTC-USDT"},
		Cost:       CostInfo{ProviderCostUSD: 0.01},
	}
	tracker.RecordIntelEvent(event)

	// Clean expired events.
	tracker.CleanExpired()

	// Trade comes in but the event is gone.
	links := tracker.RecordTrade("trade-003", "strat-gamma", "BTC-USDT", 25.0, time.Now())
	assert.Empty(t, links)
	assert.Equal(t, 0, tracker.ActiveEventCount())
}

// ---------------------------------------------------------------------------
// TestROITracker_OutsideLinkWindow
// ---------------------------------------------------------------------------

func TestROITracker_OutsideLinkWindow(t *testing.T) {
	tracker := NewROITracker(60_000) // 1 minute window

	now := time.Now()
	event := &IntelEvent{
		EventID:    "intel-004",
		Timestamp:  now,
		TTLSeconds: 3600,
		AssetTags:  []string{"BTC-USDT"},
		Cost:       CostInfo{ProviderCostUSD: 0.01},
	}
	tracker.RecordIntelEvent(event)

	// Trade outside the 1-minute window.
	links := tracker.RecordTrade("trade-004", "strat-delta", "BTC-USDT", 100.0, now.Add(5*time.Minute))
	assert.Empty(t, links)
}

// ---------------------------------------------------------------------------
// TestTopicRegistry_BudgetTracking
// ---------------------------------------------------------------------------

func TestTopicRegistry_BudgetTracking(t *testing.T) {
	configs := map[Topic]TopicConfig{
		TopicMacroEvent: {
			Topic:            TopicMacroEvent,
			MaxQueriesPerDay: 5,
			DefaultProvider:  "primary",
			DefaultTTL:       1800,
			DefaultPriority:  PriorityP1,
			CostBudgetUSD:    0.10,
		},
	}
	registry := NewTopicRegistry(configs)

	// Initially should be able to query.
	ok, reason := registry.CanQuery(TopicMacroEvent)
	assert.True(t, ok)
	assert.Empty(t, reason)

	// Record some queries.
	registry.RecordQuery(TopicMacroEvent, 0.02)
	registry.RecordQuery(TopicMacroEvent, 0.03)

	queries, cost := registry.GetUsage(TopicMacroEvent)
	assert.Equal(t, 2, queries)
	assert.InDelta(t, 0.05, cost, 0.0001)

	// Still under budget.
	ok, _ = registry.CanQuery(TopicMacroEvent)
	assert.True(t, ok)

	// Exhaust the cost budget.
	registry.RecordQuery(TopicMacroEvent, 0.06)
	ok, reason = registry.CanQuery(TopicMacroEvent)
	assert.False(t, ok)
	assert.Contains(t, reason, "cost budget exhausted")
}

// ---------------------------------------------------------------------------
// TestTopicRegistry_QueryLimitExhausted
// ---------------------------------------------------------------------------

func TestTopicRegistry_QueryLimitExhausted(t *testing.T) {
	configs := map[Topic]TopicConfig{
		TopicWhaleMovement: {
			Topic:            TopicWhaleMovement,
			MaxQueriesPerDay: 3,
			DefaultProvider:  "primary",
			DefaultTTL:       600,
			DefaultPriority:  PriorityP1,
			CostBudgetUSD:    100.0, // high budget, low query limit
		},
	}
	registry := NewTopicRegistry(configs)

	// Use up all 3 queries.
	for i := 0; i < 3; i++ {
		ok, _ := registry.CanQuery(TopicWhaleMovement)
		assert.True(t, ok)
		registry.RecordQuery(TopicWhaleMovement, 0.001)
	}

	// 4th should fail.
	ok, reason := registry.CanQuery(TopicWhaleMovement)
	assert.False(t, ok)
	assert.Contains(t, reason, "daily query limit reached")
}

// ---------------------------------------------------------------------------
// TestTopicRegistry_DailyReset
// ---------------------------------------------------------------------------

func TestTopicRegistry_DailyReset(t *testing.T) {
	configs := map[Topic]TopicConfig{
		TopicSentimentShift: {
			Topic:            TopicSentimentShift,
			MaxQueriesPerDay: 5,
			DefaultProvider:  "primary",
			DefaultTTL:       900,
			DefaultPriority:  PriorityP2,
			CostBudgetUSD:    1.00,
		},
	}
	registry := NewTopicRegistry(configs)

	// Record some usage.
	registry.RecordQuery(TopicSentimentShift, 0.50)
	registry.RecordQuery(TopicSentimentShift, 0.50)

	queries, cost := registry.GetUsage(TopicSentimentShift)
	assert.Equal(t, 2, queries)
	assert.InDelta(t, 1.0, cost, 0.001)

	// Budget should be exhausted.
	ok, _ := registry.CanQuery(TopicSentimentShift)
	assert.False(t, ok)

	// Reset.
	registry.ResetDaily()

	queries, cost = registry.GetUsage(TopicSentimentShift)
	assert.Equal(t, 0, queries)
	assert.InDelta(t, 0.0, cost, 0.001)

	// Should be able to query again.
	ok, _ = registry.CanQuery(TopicSentimentShift)
	assert.True(t, ok)
}

// ---------------------------------------------------------------------------
// TestTopicRegistry_UnknownTopic
// ---------------------------------------------------------------------------

func TestTopicRegistry_UnknownTopic(t *testing.T) {
	registry := NewTopicRegistry(DefaultTaxonomy())

	ok, reason := registry.CanQuery(Topic("totally_bogus"))
	assert.False(t, ok)
	assert.Contains(t, reason, "unknown topic")
}

// ---------------------------------------------------------------------------
// TestStubProvider_Cycling
// ---------------------------------------------------------------------------

func TestStubProvider_Cycling(t *testing.T) {
	responses := []QueryResponse{
		{Text: "response-0", Confidence: 0.5},
		{Text: "response-1", Confidence: 0.6},
	}
	provider := NewStubProvider("test-stub", responses)
	ctx := context.Background()

	resp0, err := provider.Query(ctx, QueryRequest{})
	require.NoError(t, err)
	assert.Equal(t, "response-0", resp0.Text)

	resp1, err := provider.Query(ctx, QueryRequest{})
	require.NoError(t, err)
	assert.Equal(t, "response-1", resp1.Text)

	// Cycles back.
	resp2, err := provider.Query(ctx, QueryRequest{})
	require.NoError(t, err)
	assert.Equal(t, "response-0", resp2.Text)

	assert.Equal(t, 3, provider.Calls())
}

// ---------------------------------------------------------------------------
// TestStubProvider_Unhealthy
// ---------------------------------------------------------------------------

func TestStubProvider_Unhealthy(t *testing.T) {
	provider := NewStubProvider("sick", []QueryResponse{{Text: "ok"}})
	provider.SetHealthy(false)

	ctx := context.Background()
	_, err := provider.Query(ctx, QueryRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unhealthy")

	health := provider.Health()
	assert.False(t, health.Available)
	assert.InDelta(t, 1.0, health.ErrorRate, 0.001)
}

// ---------------------------------------------------------------------------
// TestDefaultTaxonomy_AllTopicsCovered
// ---------------------------------------------------------------------------

func TestDefaultTaxonomy_AllTopicsCovered(t *testing.T) {
	taxonomy := DefaultTaxonomy()

	// Every known topic should have a config.
	for topic := range validTopics {
		_, ok := taxonomy[topic]
		assert.True(t, ok, "topic %s missing from DefaultTaxonomy", topic)
	}

	// Every config should have sane values.
	for topic, cfg := range taxonomy {
		assert.Equal(t, topic, cfg.Topic)
		assert.Greater(t, cfg.MaxQueriesPerDay, 0, "topic %s: MaxQueriesPerDay must be > 0", topic)
		assert.Greater(t, cfg.DefaultTTL, 0, "topic %s: DefaultTTL must be > 0", topic)
		assert.Greater(t, cfg.CostBudgetUSD, 0.0, "topic %s: CostBudgetUSD must be > 0", topic)
		assert.NotEmpty(t, cfg.DefaultProvider, "topic %s: DefaultProvider must not be empty", topic)
	}
}

// ---------------------------------------------------------------------------
// TestROISummary_ComputeROI_EmptyTracker
// ---------------------------------------------------------------------------

func TestROISummary_ComputeROI_EmptyTracker(t *testing.T) {
	tracker := NewROITracker(600_000)
	roi := tracker.ComputeROI()

	assert.Equal(t, 0, roi.TotalLinks)
	assert.InDelta(t, 0.0, roi.TotalIntelCostUSD, 0.001)
	assert.InDelta(t, 0.0, roi.TotalLinkedPnL, 0.001)
	assert.InDelta(t, 0.0, roi.NetROI, 0.001)
	assert.Equal(t, 0, roi.UnlinkedEvents)
}

// ---------------------------------------------------------------------------
// TestContentHash_Deterministic
// ---------------------------------------------------------------------------

func TestContentHash_Deterministic(t *testing.T) {
	e1 := &IntelEvent{
		Topic:     TopicVolatilitySpike,
		Title:     "BTC vol spike",
		Summary:   "Realized vol jumped 200%",
		AssetTags: []string{"BTC-USDT"},
	}
	e2 := &IntelEvent{
		Topic:     TopicVolatilitySpike,
		Title:     "BTC vol spike",
		Summary:   "Realized vol jumped 200%",
		AssetTags: []string{"BTC-USDT"},
	}
	e3 := &IntelEvent{
		Topic:     TopicVolatilitySpike,
		Title:     "ETH vol spike", // different
		Summary:   "Realized vol jumped 200%",
		AssetTags: []string{"ETH-USDT"},
	}

	assert.Equal(t, e1.ContentHash(), e2.ContentHash(), "same content should produce same hash")
	assert.NotEqual(t, e1.ContentHash(), e3.ContentHash(), "different content should produce different hash")
}
