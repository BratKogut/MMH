package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Producer publishes messages to Kafka/RedPanda.
type Producer interface {
	Produce(ctx context.Context, topic string, key []byte, value []byte) error
	ProduceSync(ctx context.Context, topic string, key []byte, value []byte) error
	PublishJSON(ctx context.Context, topic, key string, value interface{}) error
	Close()
}

// KafkaProducer wraps franz-go client for producing to RedPanda/Kafka.
type KafkaProducer struct {
	client  *kgo.Client
	mu      sync.Mutex
	closed  bool
	brokers []string
}

// NewProducer creates a real Kafka producer using franz-go.
func NewProducer(brokers []string, instanceID string) (Producer, error) {
	opts := []kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.ProducerBatchCompression(kgo.SnappyCompression()),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProducerBatchMaxBytes(1024 * 1024), // 1MB batch
		kgo.ProducerLinger(5 * time.Millisecond),
		kgo.ClientID(instanceID),
	}

	client, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("create kafka producer: %w", err)
	}

	log.Info().
		Strs("brokers", brokers).
		Str("instance_id", instanceID).
		Msg("Kafka producer created (franz-go)")

	return &KafkaProducer{
		client:  client,
		brokers: brokers,
	}, nil
}

// Produce sends a message asynchronously (fire-and-forget with buffering).
func (p *KafkaProducer) Produce(ctx context.Context, topic string, key []byte, value []byte) error {
	if p.closed {
		return fmt.Errorf("producer is closed")
	}

	record := &kgo.Record{
		Topic: topic,
		Key:   key,
		Value: value,
	}

	p.client.Produce(ctx, record, func(r *kgo.Record, err error) {
		if err != nil {
			log.Error().Err(err).
				Str("topic", r.Topic).
				Msg("Async produce failed")
		}
	})

	return nil
}

// ProduceSync sends a message and waits for broker acknowledgement.
func (p *KafkaProducer) ProduceSync(ctx context.Context, topic string, key []byte, value []byte) error {
	if p.closed {
		return fmt.Errorf("producer is closed")
	}

	record := &kgo.Record{
		Topic: topic,
		Key:   key,
		Value: value,
	}

	results := p.client.ProduceSync(ctx, record)
	return results.FirstErr()
}

// PublishJSON serializes value as JSON and publishes asynchronously.
func (p *KafkaProducer) PublishJSON(ctx context.Context, topic, key string, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	return p.Produce(ctx, topic, []byte(key), data)
}

// Close flushes pending messages and shuts down the producer.
func (p *KafkaProducer) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}
	p.closed = true
	p.client.Close()
	log.Info().Msg("Kafka producer closed")
}

// --- Stub producer for development/testing ---

// StubProducer implements Producer by logging messages. Used when Kafka is unavailable.
type StubProducer struct {
	Messages []StubMessage
	mu       sync.Mutex
}

type StubMessage struct {
	Topic string
	Key   string
	Value []byte
}

func NewStubProducer() *StubProducer {
	return &StubProducer{Messages: make([]StubMessage, 0, 1024)}
}

func (p *StubProducer) Produce(_ context.Context, topic string, key []byte, value []byte) error {
	p.mu.Lock()
	p.Messages = append(p.Messages, StubMessage{Topic: topic, Key: string(key), Value: value})
	p.mu.Unlock()
	log.Debug().Str("topic", topic).Int("bytes", len(value)).Msg("stub: produce")
	return nil
}

func (p *StubProducer) ProduceSync(ctx context.Context, topic string, key []byte, value []byte) error {
	return p.Produce(ctx, topic, key, value)
}

func (p *StubProducer) PublishJSON(ctx context.Context, topic, key string, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return p.Produce(ctx, topic, []byte(key), data)
}

func (p *StubProducer) Close() {
	log.Info().Msg("stub: producer closed")
}
