package intel

import (
	"crypto/sha256"
	"fmt"
	"time"
)

// Topic represents an intel topic category.
type Topic string

const (
	TopicExchangeOutage   Topic = "exchange_outage"
	TopicRegulatoryAction Topic = "regulatory_action"
	TopicMacroEvent       Topic = "macro_event"
	TopicSentimentShift   Topic = "sentiment_shift"
	TopicWhaleMovement    Topic = "whale_movement"
	TopicProtocolUpdate   Topic = "protocol_update"
	TopicLiquidityEvent   Topic = "liquidity_event"
	TopicCorrelationBreak Topic = "correlation_break"
	TopicVolatilitySpike  Topic = "volatility_spike"
	TopicListingDeListing Topic = "listing_delisting"
)

// validTopics is the set of all recognised topics.
var validTopics = map[Topic]bool{
	TopicExchangeOutage:   true,
	TopicRegulatoryAction: true,
	TopicMacroEvent:       true,
	TopicSentimentShift:   true,
	TopicWhaleMovement:    true,
	TopicProtocolUpdate:   true,
	TopicLiquidityEvent:   true,
	TopicCorrelationBreak: true,
	TopicVolatilitySpike:  true,
	TopicListingDeListing: true,
}

// Priority levels.
type Priority string

const (
	PriorityP0 Priority = "P0" // Critical: immediate action required
	PriorityP1 Priority = "P1" // High: act within minutes
	PriorityP2 Priority = "P2" // Medium: informational, may affect strategy
	PriorityP3 Priority = "P3" // Low: background context
)

// validPriorities is the set of all recognised priorities.
var validPriorities = map[Priority]bool{
	PriorityP0: true,
	PriorityP1: true,
	PriorityP2: true,
	PriorityP3: true,
}

// ImpactHorizon describes when the intel will have effect.
type ImpactHorizon string

const (
	HorizonImmediate  ImpactHorizon = "immediate"   // seconds to minutes
	HorizonShortTerm  ImpactHorizon = "short_term"  // hours
	HorizonMediumTerm ImpactHorizon = "medium_term" // days
	HorizonLongTerm   ImpactHorizon = "long_term"   // weeks+
)

// validHorizons is the set of all recognised impact horizons.
var validHorizons = map[ImpactHorizon]bool{
	HorizonImmediate:  true,
	HorizonShortTerm:  true,
	HorizonMediumTerm: true,
	HorizonLongTerm:   true,
}

// DirectionalBias indicates expected market direction.
type DirectionalBias string

const (
	BiasBullish DirectionalBias = "bullish"
	BiasBearish DirectionalBias = "bearish"
	BiasNeutral DirectionalBias = "neutral"
	BiasUnknown DirectionalBias = "unknown"
)

// validBiases is the set of all recognised directional biases.
var validBiases = map[DirectionalBias]bool{
	BiasBullish: true,
	BiasBearish: true,
	BiasNeutral: true,
	BiasUnknown: true,
}

// IntelEvent is the core intel event structure.
// LLM NIE HANDLUJE -- this is extraction, classification, synthesis only.
type IntelEvent struct {
	EventID         string          `json:"event_id"`
	Timestamp       time.Time       `json:"ts"`
	Topic           Topic           `json:"topic"`
	Priority        Priority        `json:"priority"`
	TTLSeconds      int             `json:"ttl_seconds"`
	Confidence      float64         `json:"confidence"`      // [0, 1]
	ImpactHorizon   ImpactHorizon   `json:"impact_horizon"`
	DirectionalBias DirectionalBias `json:"directional_bias"`
	AssetTags       []string        `json:"asset_tags"` // affected symbols
	Title           string          `json:"title"`
	Summary         string          `json:"summary"`
	Claims          []Claim         `json:"claims"`
	Sources         []Source        `json:"sources"`
	Provider        string          `json:"provider"` // which LLM/source produced this
	Cost            CostInfo        `json:"cost"`
	LatencyMs       int             `json:"latency_ms"`
}

// Claim is a single verifiable assertion extracted from intel.
type Claim struct {
	Statement  string  `json:"statement"`
	Confidence float64 `json:"confidence"`
	Verifiable bool    `json:"verifiable"`
}

// Source identifies the provenance of a piece of intel.
type Source struct {
	URL       string    `json:"url,omitempty"`
	Name      string    `json:"name"`
	Type      string    `json:"type"` // api|website|social|official
	Timestamp time.Time `json:"ts,omitempty"`
}

// CostInfo tracks the monetary cost of producing intel.
type CostInfo struct {
	ProviderCostUSD          float64 `json:"provider_cost_usd"`
	CumulativeTopicCostToday float64 `json:"cumulative_topic_cost_today"`
	BudgetRemainingToday     float64 `json:"budget_remaining_today"`
}

// Validate checks that an IntelEvent is well-formed.
func (e *IntelEvent) Validate() error {
	if e.EventID == "" {
		return fmt.Errorf("event_id is required")
	}
	if e.Timestamp.IsZero() {
		return fmt.Errorf("timestamp is required")
	}
	if !validTopics[e.Topic] {
		return fmt.Errorf("invalid topic: %q", e.Topic)
	}
	if !validPriorities[e.Priority] {
		return fmt.Errorf("invalid priority: %q", e.Priority)
	}
	if e.TTLSeconds <= 0 {
		return fmt.Errorf("ttl_seconds must be positive, got %d", e.TTLSeconds)
	}
	if e.Confidence < 0 || e.Confidence > 1 {
		return fmt.Errorf("confidence must be in [0, 1], got %f", e.Confidence)
	}
	if !validHorizons[e.ImpactHorizon] {
		return fmt.Errorf("invalid impact_horizon: %q", e.ImpactHorizon)
	}
	if !validBiases[e.DirectionalBias] {
		return fmt.Errorf("invalid directional_bias: %q", e.DirectionalBias)
	}
	if e.Title == "" {
		return fmt.Errorf("title is required")
	}
	if e.Summary == "" {
		return fmt.Errorf("summary is required")
	}
	if e.Provider == "" {
		return fmt.Errorf("provider is required")
	}
	if len(e.AssetTags) == 0 {
		return fmt.Errorf("at least one asset_tag is required")
	}
	// Validate each claim's confidence.
	for i, c := range e.Claims {
		if c.Confidence < 0 || c.Confidence > 1 {
			return fmt.Errorf("claim[%d] confidence must be in [0, 1], got %f", i, c.Confidence)
		}
		if c.Statement == "" {
			return fmt.Errorf("claim[%d] statement is required", i)
		}
	}
	return nil
}

// IsExpired checks if the event has exceeded its TTL.
func (e *IntelEvent) IsExpired() bool {
	if e.TTLSeconds <= 0 {
		return true
	}
	expiry := e.Timestamp.Add(time.Duration(e.TTLSeconds) * time.Second)
	return time.Now().After(expiry)
}

// ContentHash returns a SHA-256 hash of the event's semantic content
// (topic + title + summary + asset tags). Used for deduplication.
func (e *IntelEvent) ContentHash() string {
	h := sha256.New()
	h.Write([]byte(string(e.Topic)))
	h.Write([]byte(e.Title))
	h.Write([]byte(e.Summary))
	for _, tag := range e.AssetTags {
		h.Write([]byte(tag))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
