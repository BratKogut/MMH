package intel

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/nexus-trading/nexus/internal/clickhouse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// E2E: Trigger → Brain → IntelEvent → Callback → ROI
// ---------------------------------------------------------------------------

func TestIntelService_E2E_VolSpikeTriggerToROI(t *testing.T) {
	producer := bus.NewStubProducer()
	config := DefaultIntelServiceConfig()
	config.BrainConfig.MaxEventsPerHourNormal = 100
	config.TriggerEngineConfig.VolWindowSize = 5
	config.TriggerEngineConfig.VolSpikeThreshold = 2.0
	config.ROILinkWindowMs = 60_000

	svc := NewIntelService(config, producer, nil)

	// Register a stub provider so Brain can process triggers.
	stubResp := QueryResponse{
		Text:       "Market sentiment has shifted bullish due to ETF approval.",
		Confidence: 0.85,
		TokensUsed: 128,
		CostUSD:    0.005,
		Claims: []Claim{
			{Statement: "ETF approved, bullish momentum expected", Confidence: 0.85, Verifiable: true},
		},
		Sources: []Source{
			{Name: "reuters", Type: "news", URL: "https://reuters.com/etf"},
		},
	}
	svc.RegisterProvider(NewStubProvider("test-llm", []QueryResponse{stubResp}))

	// Track intel events delivered to strategies.
	var mu sync.Mutex
	var deliveredEvents []*IntelEvent
	svc.SetOnIntelEvent(func(_ context.Context, event *IntelEvent) {
		mu.Lock()
		deliveredEvents = append(deliveredEvents, event)
		mu.Unlock()
	})

	ctx := context.Background()

	// Phase 1: Feed normal volatility to build up history.
	for i := 0; i < 5; i++ {
		svc.OnVolatility(ctx, "BTC-USDT", 0.01)
	}

	// Phase 2: Vol spike → triggers Brain → produces intel event.
	svc.OnVolatility(ctx, "BTC-USDT", 0.05)

	// Verify intel event was produced and delivered.
	mu.Lock()
	assert.Len(t, deliveredEvents, 1, "One intel event should be delivered to strategy callback")
	event := deliveredEvents[0]
	mu.Unlock()

	assert.Equal(t, TopicVolatilitySpike, event.Topic)
	assert.InDelta(t, 0.85, event.Confidence, 0.01)
	assert.Equal(t, "neutral", string(event.DirectionalBias)) // Brain defaults to neutral for high-confidence
	assert.Contains(t, event.AssetTags, "BTC-USDT")
	assert.Len(t, event.Claims, 1)
	assert.Contains(t, event.Claims[0].Statement, "ETF")

	// Phase 3: Record a trade linked to this intel event.
	links := svc.OnTrade("trade-001", "strat-btc", "BTC-USDT", 150.0, time.Now())
	assert.NotEmpty(t, links, "Trade should be linked to the intel event")
	assert.Equal(t, event.EventID, links[0].IntelEventID)

	// Phase 4: Verify ROI computation.
	roi := svc.ROITracker().ComputeROI()
	assert.Equal(t, 1, roi.TotalLinks)
	assert.InDelta(t, 0.005, roi.TotalIntelCostUSD, 0.001)
	assert.InDelta(t, 150.0, roi.TotalLinkedPnL, 0.01)
	assert.True(t, roi.NetROI > 0, "ROI should be positive")

	// Phase 5: Verify stats.
	stats := svc.Stats()
	assert.Equal(t, int64(1), stats.EventsProduced)
	assert.Equal(t, int64(1), stats.BrainStats.TotalQueries)
	assert.Equal(t, int64(1), stats.TriggerStats.TriggersEmitted)
}

func TestIntelService_E2E_PriceGapTrigger(t *testing.T) {
	producer := bus.NewStubProducer()
	config := DefaultIntelServiceConfig()
	config.BrainConfig.MaxEventsPerHourNormal = 100
	config.TriggerEngineConfig.GapThresholdPct = 3.0

	svc := NewIntelService(config, producer, nil)

	stubResp := QueryResponse{
		Text:       "Flash crash detected, likely caused by large sell order.",
		Confidence: 0.72,
		TokensUsed: 96,
		CostUSD:    0.003,
		Claims: []Claim{
			{Statement: "Flash crash from large market sell", Confidence: 0.72, Verifiable: true},
		},
		Sources: []Source{
			{Name: "exchange_data", Type: "internal"},
		},
	}
	svc.RegisterProvider(NewStubProvider("test-llm", []QueryResponse{stubResp}))

	var mu sync.Mutex
	var events []*IntelEvent
	svc.SetOnIntelEvent(func(_ context.Context, event *IntelEvent) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	})

	ctx := context.Background()

	// Establish baseline price.
	svc.OnPrice(ctx, "ETH-USDT", 3000.0)

	// Price gap > 3%.
	svc.OnPrice(ctx, "ETH-USDT", 2890.0)

	mu.Lock()
	require.Len(t, events, 1)
	assert.Contains(t, events[0].Claims[0].Statement, "Flash crash")
	mu.Unlock()
}

func TestIntelService_E2E_ScheduledEvent(t *testing.T) {
	producer := bus.NewStubProducer()
	config := DefaultIntelServiceConfig()
	config.BrainConfig.MaxEventsPerHourNormal = 100
	config.TriggerEngineConfig.ScheduleCheckIntervalMs = 10

	svc := NewIntelService(config, producer, nil)

	stubResp := QueryResponse{
		Text:       "FOMC held rates steady. Crypto likely to rally short-term.",
		Confidence: 0.80,
		TokensUsed: 64,
		CostUSD:    0.002,
		Claims: []Claim{
			{Statement: "Rates held steady, risk-on for crypto", Confidence: 0.80, Verifiable: true},
		},
		Sources: []Source{{Name: "fed", Type: "official"}},
	}
	svc.RegisterProvider(NewStubProvider("test-llm", []QueryResponse{stubResp}))

	var mu sync.Mutex
	var events []*IntelEvent
	svc.SetOnIntelEvent(func(_ context.Context, event *IntelEvent) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	})

	// Add a scheduled event in the past (fires immediately).
	svc.AddScheduledEvent(ScheduledEvent{
		Name:      "FOMC Decision",
		Topic:     TopicMacroEvent,
		Time:      time.Now().Add(-1 * time.Second),
		AssetTags: []string{"BTC-USDT", "ETH-USDT"},
		Priority:  PriorityP0,
		Prompt:    "FOMC decision just announced. Analyze impact on crypto markets.",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	svc.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	require.Len(t, events, 1)
	assert.Contains(t, events[0].Claims[0].Statement, "Rates held steady")
	assert.Equal(t, TopicMacroEvent, events[0].Topic)
	mu.Unlock()

	svc.Stop()
}

func TestIntelService_E2E_WithClickHouseWriter(t *testing.T) {
	producer := bus.NewStubProducer()
	config := DefaultIntelServiceConfig()
	config.BrainConfig.MaxEventsPerHourNormal = 100
	config.TriggerEngineConfig.VolWindowSize = 5
	config.TriggerEngineConfig.VolSpikeThreshold = 2.0

	// Create IntelWriter with a flush hook to capture writes.
	intelWriter := clickhouse.NewIntelWriter(nil, "nexus_test", 500, 10*time.Second)

	var writtenMu sync.Mutex
	var writtenTables []string
	var writtenRowCounts []int
	intelWriter.SetFlushHook(func(_ context.Context, table string, rows [][]any) error {
		writtenMu.Lock()
		writtenTables = append(writtenTables, table)
		writtenRowCounts = append(writtenRowCounts, len(rows))
		writtenMu.Unlock()
		return nil
	})

	svc := NewIntelService(config, producer, intelWriter)

	stubResp := QueryResponse{
		Text:       "Analysis complete.",
		Confidence: 0.70,
		TokensUsed: 32,
		CostUSD:    0.001,
		Claims:     []Claim{{Statement: "Test claim", Confidence: 0.7}},
		Sources:    []Source{{Name: "test", Type: "test"}},
	}
	svc.RegisterProvider(NewStubProvider("ch-test", []QueryResponse{stubResp}))

	ctx := context.Background()

	// Trigger a vol spike.
	for i := 0; i < 5; i++ {
		svc.OnVolatility(ctx, "SOL-USDT", 0.02)
	}
	svc.OnVolatility(ctx, "SOL-USDT", 0.06)

	// Manually flush the intel writer.
	require.NoError(t, intelWriter.Flush(ctx))

	writtenMu.Lock()
	assert.Contains(t, writtenTables, "nexus_test.intel_events")
	assert.Equal(t, 1, writtenRowCounts[0])
	writtenMu.Unlock()
}

func TestIntelService_E2E_HandleIntelMessage(t *testing.T) {
	producer := bus.NewStubProducer()
	config := DefaultIntelServiceConfig()

	svc := NewIntelService(config, producer, nil)

	var mu sync.Mutex
	var received []*IntelEvent
	svc.SetOnIntelEvent(func(_ context.Context, event *IntelEvent) {
		mu.Lock()
		received = append(received, event)
		mu.Unlock()
	})

	// Simulate consuming a serialized IntelEvent from Kafka.
	event := IntelEvent{
		EventID:         "consumed-001",
		Timestamp:       time.Now(),
		Topic:           TopicVolatilitySpike,
		Priority:        PriorityP1,
		TTLSeconds:      300,
		Confidence:      0.8,
		ImpactHorizon:   HorizonShortTerm,
		DirectionalBias: BiasNeutral,
		Title:           "Vol spike on BTC",
		Summary:         "Significant volatility increase detected",
		AssetTags:       []string{"BTC-USDT"},
		Claims: []Claim{
			{Statement: "Consumed event claim", Confidence: 0.8},
		},
		Sources: []Source{
			{Name: "external", Type: "news"},
		},
		Provider: "ext-provider",
	}

	msgValue, _ := json.Marshal(event)
	msg := bus.Message{
		Topic: "intel.events.global",
		Key:   "BTC-USDT",
		Value: msgValue,
	}

	err := svc.HandleIntelMessage(context.Background(), msg)
	require.NoError(t, err)

	mu.Lock()
	require.Len(t, received, 1)
	assert.Equal(t, "consumed-001", received[0].EventID)
	mu.Unlock()

	stats := svc.Stats()
	assert.Equal(t, int64(1), stats.EventsConsumed)
}
