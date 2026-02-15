package audit

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/nexus-trading/nexus/internal/risk"
	"github.com/rs/zerolog/log"
)

const (
	// Topic is the Kafka/RedPanda topic for audit events.
	Topic = "audit.event_store"

	// Entry event types.
	EventSignal         = "signal"
	EventRiskCheck      = "risk_check"
	EventOrder          = "order"
	EventFill           = "fill"
	EventPositionUpdate = "position_update"
)

// Entry represents a single audit trail entry. Every decision in the system
// gets recorded as an Entry, creating an immutable log for replay, debugging,
// and compliance.
type Entry struct {
	TraceID     string    `json:"trace_id"`
	CausationID string   `json:"causation_id"`
	EventType   string    `json:"event_type"` // signal|risk_check|order|fill|position_update
	Timestamp   time.Time `json:"ts"`
	StrategyID  string    `json:"strategy_id,omitempty"`
	IntentID    string    `json:"intent_id,omitempty"`
	OrderID     string    `json:"order_id,omitempty"`
	Symbol      string    `json:"symbol,omitempty"`
	Decision    string    `json:"decision,omitempty"` // for risk checks: allow|deny
	Payload     string    `json:"payload"`            // JSON of the full event
}

// Trail records the full decision chain for every action in the system.
// It maintains an in-memory buffer (capped at maxBuf) for querying and
// also publishes every entry to the audit topic via the producer.
type Trail struct {
	mu       sync.Mutex
	producer bus.Producer
	entries  []Entry
	maxBuf   int
}

// NewTrail creates a new audit trail.
// maxBuf controls the maximum number of entries kept in the in-memory buffer.
// Once the buffer is full, the oldest entries are discarded (FIFO).
// A maxBuf of 0 means no in-memory buffering (entries are only published).
func NewTrail(producer bus.Producer, maxBuf int) *Trail {
	if maxBuf < 0 {
		maxBuf = 0
	}
	entries := make([]Entry, 0, maxBuf)
	return &Trail{
		producer: producer,
		entries:  entries,
		maxBuf:   maxBuf,
	}
}

// RecordSignal logs a strategy signal to the audit trail.
func (t *Trail) RecordSignal(signal bus.StrategySignal) {
	payload := mustMarshal(signal)

	// Determine causation: if there are order intents, link to the first one.
	causationID := signal.CausationID
	if causationID == "" {
		causationID = signal.EventID
	}

	entry := Entry{
		TraceID:     signal.TraceID,
		CausationID: causationID,
		EventType:   EventSignal,
		Timestamp:   signal.Timestamp,
		StrategyID:  signal.StrategyID,
		Symbol:      signal.Symbol,
		Payload:     payload,
	}

	// If the signal has order intents, also record the intent ID.
	if len(signal.OrdersIntent) > 0 {
		entry.IntentID = signal.OrdersIntent[0].IntentID
	}

	t.record(entry)
}

// RecordRiskCheck logs a risk decision to the audit trail.
func (t *Trail) RecordRiskCheck(intentID string, decision risk.Decision) {
	payload := mustMarshal(decision)

	decisionStr := "deny"
	if decision.Allowed {
		decisionStr = "allow"
	}

	entry := Entry{
		TraceID:     "", // Caller should set trace ID on the decision if available.
		CausationID: intentID,
		EventType:   EventRiskCheck,
		Timestamp:   time.Unix(0, decision.Timestamp*1000), // Timestamp is in microseconds.
		IntentID:    intentID,
		Decision:    decisionStr,
		Payload:     payload,
	}

	t.record(entry)
}

// RecordOrder logs an order state transition to the audit trail.
// The event parameter describes the transition (e.g., "created", "submitted",
// "partially_filled", "filled", "cancelled", "rejected").
func (t *Trail) RecordOrder(order interface{}, event string) {
	payload := mustMarshal(order)

	entry := Entry{
		EventType: EventOrder,
		Timestamp: time.Now(),
		Decision:  event,
		Payload:   payload,
	}

	// Try to extract fields from common order types via JSON round-trip.
	extracted := extractOrderFields(payload)
	if extracted.traceID != "" {
		entry.TraceID = extracted.traceID
	}
	if extracted.intentID != "" {
		entry.IntentID = extracted.intentID
	}
	if extracted.orderID != "" {
		entry.OrderID = extracted.orderID
	}
	if extracted.strategyID != "" {
		entry.StrategyID = extracted.strategyID
	}
	if extracted.symbol != "" {
		entry.Symbol = extracted.symbol
	}

	t.record(entry)
}

// RecordFill logs a fill event to the audit trail.
func (t *Trail) RecordFill(fill bus.Fill) {
	payload := mustMarshal(fill)

	entry := Entry{
		TraceID:     fill.TraceID,
		CausationID: fill.CausationID,
		EventType:   EventFill,
		Timestamp:   fill.Timestamp,
		StrategyID:  fill.StrategyID,
		IntentID:    fill.IntentID,
		OrderID:     fill.OrderID,
		Symbol:      fill.Symbol,
		Payload:     payload,
	}

	t.record(entry)
}

// RecordPositionUpdate logs a position change to the audit trail.
func (t *Trail) RecordPositionUpdate(pos interface{}) {
	payload := mustMarshal(pos)

	entry := Entry{
		EventType: EventPositionUpdate,
		Timestamp: time.Now(),
		Payload:   payload,
	}

	// Try to extract fields from common position types.
	extracted := extractOrderFields(payload)
	if extracted.traceID != "" {
		entry.TraceID = extracted.traceID
	}
	if extracted.strategyID != "" {
		entry.StrategyID = extracted.strategyID
	}
	if extracted.symbol != "" {
		entry.Symbol = extracted.symbol
	}

	t.record(entry)
}

// Query returns all entries matching a given trace ID.
// Searches the in-memory buffer only.
func (t *Trail) Query(traceID string) []Entry {
	t.mu.Lock()
	defer t.mu.Unlock()

	var result []Entry
	for _, e := range t.entries {
		if e.TraceID == traceID {
			result = append(result, e)
		}
	}
	return result
}

// Entries returns a copy of all entries in the in-memory buffer.
func (t *Trail) Entries() []Entry {
	t.mu.Lock()
	defer t.mu.Unlock()

	result := make([]Entry, len(t.entries))
	copy(result, t.entries)
	return result
}

// Len returns the number of entries in the in-memory buffer.
func (t *Trail) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}

// record adds an entry to the in-memory buffer and publishes it to the bus.
func (t *Trail) record(entry Entry) {
	t.mu.Lock()

	// Add to in-memory buffer with FIFO eviction.
	if t.maxBuf > 0 {
		if len(t.entries) >= t.maxBuf {
			// Shift left: discard oldest entry.
			copy(t.entries, t.entries[1:])
			t.entries[len(t.entries)-1] = entry
		} else {
			t.entries = append(t.entries, entry)
		}
	}

	t.mu.Unlock()

	// Publish to audit topic via producer (outside lock).
	if t.producer != nil {
		key := entry.EventType
		if entry.TraceID != "" {
			key = entry.TraceID
		}
		if err := t.producer.PublishJSON(context.Background(), Topic, key, entry); err != nil {
			log.Error().Err(err).
				Str("event_type", entry.EventType).
				Str("trace_id", entry.TraceID).
				Msg("Failed to publish audit entry")
		}
	}
}

// orderFields holds extracted fields from an arbitrary order/position JSON.
type orderFields struct {
	traceID    string
	intentID   string
	orderID    string
	strategyID string
	symbol     string
}

// extractOrderFields attempts to extract common fields from a JSON payload string.
func extractOrderFields(payload string) orderFields {
	var fields struct {
		TraceID    string `json:"trace_id"`
		IntentID   string `json:"intent_id"`
		OrderID    string `json:"order_id"`
		StrategyID string `json:"strategy_id"`
		Symbol     string `json:"symbol"`
	}

	if err := json.Unmarshal([]byte(payload), &fields); err != nil {
		return orderFields{}
	}

	return orderFields{
		traceID:    fields.TraceID,
		intentID:   fields.IntentID,
		orderID:    fields.OrderID,
		strategyID: fields.StrategyID,
		symbol:     fields.Symbol,
	}
}

// mustMarshal marshals v to JSON, returning "{}" on error.
func mustMarshal(v interface{}) string {
	data, err := json.Marshal(v)
	if err != nil {
		log.Error().Err(err).Msg("Failed to marshal audit payload")
		return "{}"
	}
	return string(data)
}
