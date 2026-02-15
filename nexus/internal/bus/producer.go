package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Message represents a message to be published to or consumed from Kafka.
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
	// Publish sends a Message synchronously, waiting for broker acknowledgement.
	Publish(ctx context.Context, msg Message) error
	// PublishJSON marshals value as JSON and publishes synchronously.
	PublishJSON(ctx context.Context, topic, key string, value interface{}) error
	// Produce sends a raw record asynchronously. Delivery errors are logged.
	Produce(ctx context.Context, topic string, key []byte, value []byte) error
	// ProduceSync sends a raw record and waits for broker acknowledgement.
	ProduceSync(ctx context.Context, topic string, key []byte, value []byte) error
	// Flush waits for all buffered records to be delivered. Returns 0 on success.
	Flush(timeout time.Duration) int
	// Close flushes pending records and shuts down the producer.
	Close()
}

// ProducerOption configures a KafkaProducer.
type ProducerOption func(*producerConfig)

type producerConfig struct {
	instanceID         string
	schemaVersion      string
	maxBufferedRecords int
	linger             time.Duration
	batchMaxBytes      int32
}

// WithInstanceID sets the producer instance identifier used as ClientID and in message headers.
func WithInstanceID(id string) ProducerOption {
	return func(c *producerConfig) { c.instanceID = id }
}

// WithSchemaVersion sets the schema version included in message headers.
func WithSchemaVersion(v string) ProducerOption {
	return func(c *producerConfig) { c.schemaVersion = v }
}

// WithMaxBufferedRecords sets the maximum number of records buffered before blocking.
func WithMaxBufferedRecords(n int) ProducerOption {
	return func(c *producerConfig) { c.maxBufferedRecords = n }
}

// WithLinger sets the time to wait for batching before sending.
func WithLinger(d time.Duration) ProducerOption {
	return func(c *producerConfig) { c.linger = d }
}

// WithBatchMaxBytes sets the maximum batch size in bytes.
func WithBatchMaxBytes(n int32) ProducerOption {
	return func(c *producerConfig) { c.batchMaxBytes = n }
}

// KafkaProducer is a real Kafka producer backed by franz-go.
type KafkaProducer struct {
	client         *kgo.Client
	defaultHeaders map[string]string
	mu             sync.RWMutex
	closed         bool
}

// NewProducer creates a new Kafka producer backed by franz-go.
// The producer uses Snappy compression and waits for all ISR acknowledgements.
func NewProducer(brokers []string, opts ...ProducerOption) (*KafkaProducer, error) {
	cfg := &producerConfig{
		instanceID:         "nexus-producer",
		schemaVersion:      "1.0.0",
		maxBufferedRecords: 10000,
		linger:             5 * time.Millisecond,
		batchMaxBytes:      1024 * 1024, // 1MB
	}
	for _, opt := range opts {
		opt(cfg)
	}

	kgoOpts := []kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.ClientID(cfg.instanceID),
		kgo.ProducerBatchCompression(kgo.SnappyCompression()),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProducerLinger(cfg.linger),
		kgo.MaxBufferedRecords(cfg.maxBufferedRecords),
		kgo.ProducerBatchMaxBytes(cfg.batchMaxBytes),
	}

	client, err := kgo.NewClient(kgoOpts...)
	if err != nil {
		return nil, fmt.Errorf("create kafka producer: %w", err)
	}

	p := &KafkaProducer{
		client: client,
		defaultHeaders: map[string]string{
			"producer":       cfg.instanceID,
			"schema_version": cfg.schemaVersion,
		},
	}

	log.Info().
		Strs("brokers", brokers).
		Str("instance_id", cfg.instanceID).
		Msg("kafka producer created (franz-go)")

	return p, nil
}

// messageToRecord converts a bus.Message to a kgo.Record, injecting default headers.
func (p *KafkaProducer) messageToRecord(msg Message) *kgo.Record {
	if msg.Headers == nil {
		msg.Headers = make(map[string]string)
	}
	for k, v := range p.defaultHeaders {
		if _, exists := msg.Headers[k]; !exists {
			msg.Headers[k] = v
		}
	}
	if _, ok := msg.Headers["event_id"]; !ok {
		msg.Headers["event_id"] = uuid.New().String()
	}

	headers := make([]kgo.RecordHeader, 0, len(msg.Headers))
	for k, v := range msg.Headers {
		headers = append(headers, kgo.RecordHeader{Key: k, Value: []byte(v)})
	}

	ts := msg.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	return &kgo.Record{
		Topic:     msg.Topic,
		Key:       []byte(msg.Key),
		Value:     msg.Value,
		Headers:   headers,
		Timestamp: ts,
	}
}

// Publish sends a Message synchronously, waiting for broker acknowledgement.
// This is the high-level API that supports headers and timestamps from the Message struct.
func (p *KafkaProducer) Publish(ctx context.Context, msg Message) error {
	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return fmt.Errorf("producer is closed")
	}
	p.mu.RUnlock()

	record := p.messageToRecord(msg)
	results := p.client.ProduceSync(ctx, record)
	if err := results.FirstErr(); err != nil {
		log.Error().Err(err).
			Str("topic", msg.Topic).
			Str("key", msg.Key).
			Msg("failed to publish message")
		return fmt.Errorf("publish to %s: %w", msg.Topic, err)
	}

	r := results[0].Record
	log.Debug().
		Str("topic", r.Topic).
		Int32("partition", r.Partition).
		Int64("offset", r.Offset).
		Msg("message published")

	return nil
}

// PublishJSON marshals value as JSON and publishes synchronously.
func (p *KafkaProducer) PublishJSON(ctx context.Context, topic, key string, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}

	return p.Publish(ctx, Message{
		Topic: topic,
		Key:   key,
		Value: data,
	})
}

// Produce sends a raw record asynchronously. Delivery errors are logged but not returned.
// This is the fire-and-forget path for high-throughput market data scenarios.
func (p *KafkaProducer) Produce(ctx context.Context, topic string, key []byte, value []byte) error {
	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return fmt.Errorf("producer is closed")
	}
	p.mu.RUnlock()

	record := &kgo.Record{
		Topic: topic,
		Key:   key,
		Value: value,
	}

	p.client.Produce(ctx, record, func(r *kgo.Record, err error) {
		if err != nil {
			log.Error().Err(err).
				Str("topic", topic).
				Msg("async produce failed")
			return
		}
		log.Debug().
			Str("topic", r.Topic).
			Int32("partition", r.Partition).
			Int64("offset", r.Offset).
			Msg("async produce succeeded")
	})

	return nil
}

// ProduceSync sends a raw record and waits for broker acknowledgement.
func (p *KafkaProducer) ProduceSync(ctx context.Context, topic string, key []byte, value []byte) error {
	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return fmt.Errorf("producer is closed")
	}
	p.mu.RUnlock()

	record := &kgo.Record{
		Topic: topic,
		Key:   key,
		Value: value,
	}

	results := p.client.ProduceSync(ctx, record)
	if err := results.FirstErr(); err != nil {
		log.Error().Err(err).
			Str("topic", topic).
			Msg("sync produce failed")
		return fmt.Errorf("produce sync to %s: %w", topic, err)
	}

	return nil
}

// Flush waits for all buffered records to be delivered. Returns 0 on success, 1 on error.
func (p *KafkaProducer) Flush(timeout time.Duration) int {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := p.client.Flush(ctx); err != nil {
		log.Error().Err(err).Msg("flush failed")
		return 1
	}
	return 0
}

// Close flushes pending records and shuts down the producer.
func (p *KafkaProducer) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()

	p.client.Close()
	log.Info().Msg("kafka producer closed")
}

// --- Stub producer for development/testing ---

// StubProducer implements Producer by buffering messages in memory.
// Used when Kafka is unavailable or in unit tests.
type StubProducer struct {
	Messages []StubMessage
	mu       sync.Mutex
}

// StubMessage is a message captured by StubProducer.
type StubMessage struct {
	Topic string
	Key   string
	Value []byte
}

// NewStubProducer creates a new in-memory stub producer.
func NewStubProducer() *StubProducer {
	return &StubProducer{Messages: make([]StubMessage, 0, 1024)}
}

func (p *StubProducer) Publish(_ context.Context, msg Message) error {
	p.mu.Lock()
	p.Messages = append(p.Messages, StubMessage{Topic: msg.Topic, Key: msg.Key, Value: msg.Value})
	p.mu.Unlock()
	return nil
}

func (p *StubProducer) PublishJSON(ctx context.Context, topic, key string, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return p.Produce(ctx, topic, []byte(key), data)
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

func (p *StubProducer) Flush(_ time.Duration) int { return 0 }

func (p *StubProducer) Close() {
	log.Info().Msg("stub: producer closed")
}
