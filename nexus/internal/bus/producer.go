package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// Message represents a message to be published to Kafka.
type Message struct {
	Topic     string
	Key       string // partition key
	Value     []byte
	Headers   map[string]string
	Timestamp time.Time
}

// Producer publishes messages to Kafka/RedPanda.
// This is an interface so we can swap implementations (real Kafka vs in-memory for tests).
type Producer interface {
	Publish(ctx context.Context, msg Message) error
	PublishJSON(ctx context.Context, topic, key string, value interface{}) error
	Flush(timeout time.Duration) int
	Close()
}

// KafkaProducer wraps confluent-kafka-go producer.
// For now, uses a channel-based mock that can be replaced with real Kafka.
type KafkaProducer struct {
	brokers        []string
	defaultHeaders map[string]string
	messages       []Message // in-memory buffer for development
	mu             sync.Mutex
	closed         bool
}

// NewProducer creates a new Kafka producer.
func NewProducer(brokers []string, instanceID string, schemaVersion string) (Producer, error) {
	p := &KafkaProducer{
		brokers: brokers,
		defaultHeaders: map[string]string{
			"producer":       instanceID,
			"schema_version": schemaVersion,
		},
		messages: make([]Message, 0, 1024),
	}

	log.Info().
		Strs("brokers", brokers).
		Str("instance_id", instanceID).
		Msg("Kafka producer created")

	return p, nil
}

// Publish sends a message to Kafka.
func (p *KafkaProducer) Publish(ctx context.Context, msg Message) error {
	if p.closed {
		return fmt.Errorf("producer is closed")
	}

	// Add default headers
	if msg.Headers == nil {
		msg.Headers = make(map[string]string)
	}
	for k, v := range p.defaultHeaders {
		if _, exists := msg.Headers[k]; !exists {
			msg.Headers[k] = v
		}
	}

	// Add event_id if not present
	if _, ok := msg.Headers["event_id"]; !ok {
		msg.Headers["event_id"] = uuid.New().String()
	}

	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}

	p.mu.Lock()
	p.messages = append(p.messages, msg)
	p.mu.Unlock()

	log.Debug().
		Str("topic", msg.Topic).
		Str("key", msg.Key).
		Int("value_size", len(msg.Value)).
		Msg("Message published")

	return nil
}

// PublishJSON serializes value as JSON and publishes.
func (p *KafkaProducer) PublishJSON(ctx context.Context, topic, key string, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}

	return p.Publish(ctx, Message{
		Topic: topic,
		Key:   key,
		Value: data,
	})
}

// Flush waits for all messages to be delivered.
func (p *KafkaProducer) Flush(timeout time.Duration) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	remaining := len(p.messages)
	return remaining
}

// Close shuts down the producer.
func (p *KafkaProducer) Close() {
	p.closed = true
	log.Info().Msg("Kafka producer closed")
}

// GetMessages returns buffered messages (for testing).
func (p *KafkaProducer) GetMessages() []Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]Message, len(p.messages))
	copy(result, p.messages)
	return result
}
