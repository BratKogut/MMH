package intel

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/nexus-trading/nexus/internal/clickhouse"
)

// IntelService ties together Brain, TriggerEngine, ROITracker, and ClickHouse
// writer. It consumes market data to feed the trigger engine, publishes intel
// events to Kafka, records them in ClickHouse, and routes them to strategies.
type IntelService struct {
	brain         *Brain
	triggerEngine *TriggerEngine
	roiTracker    *ROITracker
	intelWriter   *clickhouse.IntelWriter // nil if ClickHouse unavailable
	producer      bus.Producer

	// onIntelEvent is called for each produced intel event (e.g. to route to Strategy Runtime).
	onIntelEvent func(ctx context.Context, event *IntelEvent)

	mu      sync.RWMutex
	started bool

	// Stats.
	eventsProduced int64
	eventsConsumed int64
}

// IntelServiceConfig configures the IntelService.
type IntelServiceConfig struct {
	BrainConfig         BrainConfig
	TriggerEngineConfig TriggerEngineConfig
	ROILinkWindowMs     int64  // time window for linking intel to trades
	CHDBPrefix          string // ClickHouse database prefix (e.g. "nexus")
}

// DefaultIntelServiceConfig returns sensible defaults.
func DefaultIntelServiceConfig() IntelServiceConfig {
	return IntelServiceConfig{
		BrainConfig:         DefaultBrainConfig(),
		TriggerEngineConfig: DefaultTriggerEngineConfig(),
		ROILinkWindowMs:     600_000, // 10 minutes
	}
}

// NewIntelService creates a fully wired IntelService.
// chWriter may be nil if ClickHouse is not available (e.g. in stub mode).
func NewIntelService(
	config IntelServiceConfig,
	producer bus.Producer,
	chWriter *clickhouse.IntelWriter,
) *IntelService {
	registry := NewTopicRegistry(DefaultTaxonomy())
	brain := NewBrain(config.BrainConfig, registry, producer)
	roiTracker := NewROITracker(config.ROILinkWindowMs)

	svc := &IntelService{
		brain:       brain,
		roiTracker:  roiTracker,
		intelWriter: chWriter,
		producer:    producer,
	}

	// Wire trigger engine to brain.
	triggerEngine := NewTriggerEngine(config.TriggerEngineConfig, svc.handleTrigger)
	svc.triggerEngine = triggerEngine

	return svc
}

// RegisterProvider adds an LLM provider to the Brain.
func (s *IntelService) RegisterProvider(provider LLMProvider) {
	s.brain.RegisterProvider(provider)
}

// SetOnIntelEvent sets the callback invoked for each produced intel event.
// Typically wired to strategy.Runtime.OnIntelEvent.
func (s *IntelService) SetOnIntelEvent(fn func(ctx context.Context, event *IntelEvent)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onIntelEvent = fn
}

// Brain returns the underlying Brain for direct access.
func (s *IntelService) Brain() *Brain { return s.brain }

// TriggerEngine returns the underlying TriggerEngine for direct access.
func (s *IntelService) TriggerEngine() *TriggerEngine { return s.triggerEngine }

// ROITracker returns the underlying ROITracker.
func (s *IntelService) ROITracker() *ROITracker { return s.roiTracker }

// Start begins background services (trigger engine scheduler, ROI cleanup).
func (s *IntelService) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()

	// Start the trigger engine's scheduled event checker.
	go s.triggerEngine.StartScheduler(ctx)

	// Start periodic ROI tracker cleanup.
	go s.roiCleanupLoop(ctx)

	log.Info().Msg("intel service started")
}

// Stop shuts down background services.
func (s *IntelService) Stop() {
	s.triggerEngine.Stop()
	log.Info().Msg("intel service stopped")
}

// OnVolatility feeds a volatility observation to the trigger engine.
func (s *IntelService) OnVolatility(ctx context.Context, symbol string, vol float64) {
	s.triggerEngine.OnVolatility(ctx, symbol, vol)
}

// OnPrice feeds a price observation to the trigger engine.
func (s *IntelService) OnPrice(ctx context.Context, symbol string, price float64) {
	s.triggerEngine.OnPrice(ctx, symbol, price)
}

// OnTrade records a trade for ROI tracking and links it to any active intel events.
func (s *IntelService) OnTrade(tradeID, strategyID, symbol string, pnl float64, ts time.Time) []TradeLink {
	return s.roiTracker.RecordTrade(tradeID, strategyID, symbol, pnl, ts)
}

// AddScheduledEvent registers a macro calendar event.
func (s *IntelService) AddScheduledEvent(event ScheduledEvent) {
	s.triggerEngine.AddScheduledEvent(event)
}

// handleTrigger is the callback wired to the TriggerEngine. It processes a
// trigger through the Brain, records the result in ClickHouse, updates the
// ROI tracker, and notifies the strategy runtime.
func (s *IntelService) handleTrigger(ctx context.Context, trigger Trigger) (*IntelEvent, error) {
	event, err := s.brain.ProcessTrigger(ctx, trigger)
	if err != nil {
		return nil, err
	}

	// Record in ROI tracker.
	s.roiTracker.RecordIntelEvent(event)

	// Write to ClickHouse.
	if s.intelWriter != nil {
		row := clickhouse.IntelEventToRow(
			event.EventID, event.Timestamp,
			string(event.Topic), string(event.Priority),
			event.TTLSeconds, event.Confidence,
			string(event.ImpactHorizon), string(event.DirectionalBias),
			event.AssetTags, event.Claims, event.Sources,
			event.Provider, event.Cost.ProviderCostUSD, event.LatencyMs,
		)
		if writeErr := s.intelWriter.WriteIntelEvent(ctx, row); writeErr != nil {
			log.Error().Err(writeErr).Str("event_id", event.EventID).
				Msg("intel: failed to write to clickhouse")
		}
	}

	// Notify strategy runtime.
	s.mu.RLock()
	callback := s.onIntelEvent
	s.mu.RUnlock()

	if callback != nil {
		callback(ctx, event)
	}

	s.mu.Lock()
	s.eventsProduced++
	s.mu.Unlock()

	return event, nil
}

// HandleIntelMessage processes a raw Kafka message from the intel.events.global topic.
// This is used when another instance published the event and we need to consume it.
func (s *IntelService) HandleIntelMessage(ctx context.Context, msg bus.Message) error {
	var event IntelEvent
	if err := json.Unmarshal(msg.Value, &event); err != nil {
		return fmt.Errorf("unmarshal intel event: %w", err)
	}

	if err := event.Validate(); err != nil {
		return fmt.Errorf("invalid intel event: %w", err)
	}

	// Record in ROI tracker if not already tracked.
	s.roiTracker.RecordIntelEvent(&event)

	// Route to strategies.
	s.mu.RLock()
	callback := s.onIntelEvent
	s.mu.RUnlock()

	if callback != nil {
		callback(ctx, &event)
	}

	s.mu.Lock()
	s.eventsConsumed++
	s.mu.Unlock()

	return nil
}

// Stats returns aggregate statistics.
func (s *IntelService) Stats() IntelServiceStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	brainStats := s.brain.Stats()
	triggerStats := s.triggerEngine.Stats()
	roi := s.roiTracker.ComputeROI()

	return IntelServiceStats{
		EventsProduced: s.eventsProduced,
		EventsConsumed: s.eventsConsumed,
		BrainStats:     brainStats,
		TriggerStats:   triggerStats,
		ROISummary:     roi,
	}
}

// IntelServiceStats is the aggregate statistics report.
type IntelServiceStats struct {
	EventsProduced int64              `json:"events_produced"`
	EventsConsumed int64              `json:"events_consumed"`
	BrainStats     BrainStats         `json:"brain"`
	TriggerStats   TriggerEngineStats `json:"trigger_engine"`
	ROISummary     ROISummary         `json:"roi"`
}

// roiCleanupLoop periodically removes expired events from the ROI tracker.
func (s *IntelService) roiCleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.roiTracker.CleanExpired()
		}
	}
}
