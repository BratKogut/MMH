package bus

import (
	"context"
	"fmt"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/twmb/franz-go/pkg/kgo"
)

// MessageHandler processes a consumed message. Returns error to trigger retry.
type MessageHandler func(ctx context.Context, topic string, key []byte, value []byte) error

// Consumer reads messages from Kafka/RedPanda topics.
type Consumer interface {
	Consume(ctx context.Context, handler MessageHandler) error
	Close()
}

// KafkaConsumer wraps franz-go client for consuming from RedPanda/Kafka.
type KafkaConsumer struct {
	client  *kgo.Client
	groupID string
	topics  []string
	mu      sync.Mutex
	closed  bool
}

// NewConsumer creates a real Kafka consumer using franz-go.
func NewConsumer(brokers []string, groupID string, topics []string) (Consumer, error) {
	opts := []kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(groupID),
		kgo.ConsumeTopics(topics...),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.ClientID(groupID),
	}

	client, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("create kafka consumer: %w", err)
	}

	log.Info().
		Strs("brokers", brokers).
		Str("group_id", groupID).
		Strs("topics", topics).
		Msg("Kafka consumer created (franz-go)")

	return &KafkaConsumer{
		client:  client,
		groupID: groupID,
		topics:  topics,
	}, nil
}

// Consume starts the consume loop. Blocks until context is cancelled.
func (c *KafkaConsumer) Consume(ctx context.Context, handler MessageHandler) error {
	if len(c.topics) == 0 {
		return fmt.Errorf("no topics configured")
	}

	log.Info().
		Strs("topics", c.topics).
		Str("group", c.groupID).
		Msg("Starting consumer loop")

	for {
		fetches := c.client.PollFetches(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if errs := fetches.Errors(); len(errs) > 0 {
			for _, e := range errs {
				log.Error().
					Err(e.Err).
					Str("topic", e.Topic).
					Int32("partition", e.Partition).
					Msg("Fetch error")
			}
		}

		fetches.EachRecord(func(record *kgo.Record) {
			if err := handler(ctx, record.Topic, record.Key, record.Value); err != nil {
				log.Error().Err(err).
					Str("topic", record.Topic).
					Int64("offset", record.Offset).
					Msg("Handler error")
			}
		})

		c.client.AllowRebalance()
	}
}

// Close shuts down the consumer.
func (c *KafkaConsumer) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return
	}
	c.closed = true
	c.client.Close()
	log.Info().Str("group", c.groupID).Msg("Kafka consumer closed")
}

// TopicNaming provides canonical topic names following NEXUS naming convention.
// Pattern: <domain>.<category>.<entity>.<variant>
type TopicNaming struct{}

func (TopicNaming) Heartbeat() string                         { return "md.heartbeat" }
func (TopicNaming) Ticks(exchange, symbol string) string       { return fmt.Sprintf("md.ticks.%s.%s", exchange, symbol) }
func (TopicNaming) Trades(exchange, symbol string) string      { return fmt.Sprintf("md.trades.%s.%s", exchange, symbol) }
func (TopicNaming) OrderbookL2(exchange, symbol string) string { return fmt.Sprintf("md.orderbook_l2.%s.%s", exchange, symbol) }
func (TopicNaming) OHLCV1m(exchange, symbol string) string     { return fmt.Sprintf("md.ohlcv_1m.%s.%s", exchange, symbol) }
func (TopicNaming) Orders(exchange string) string              { return fmt.Sprintf("exec.orders.%s", exchange) }
func (TopicNaming) Acks(exchange string) string                { return fmt.Sprintf("exec.acks.%s", exchange) }
func (TopicNaming) Fills(exchange string) string               { return fmt.Sprintf("exec.fills.%s", exchange) }
func (TopicNaming) Positions(exchange string) string           { return fmt.Sprintf("exec.positions.%s", exchange) }
func (TopicNaming) Balances(exchange string) string            { return fmt.Sprintf("exec.balances.%s", exchange) }
func (TopicNaming) RiskPretradeChecks() string                 { return "risk.pretrade_checks" }
func (TopicNaming) Features(symbol string) string              { return fmt.Sprintf("signals.features.%s", symbol) }
func (TopicNaming) Strategy(strategyID string) string          { return fmt.Sprintf("signals.strategy.%s", strategyID) }
func (TopicNaming) Regime(symbol string) string                { return fmt.Sprintf("signals.regime.%s", symbol) }
func (TopicNaming) IntelRaw(source string) string              { return fmt.Sprintf("intel.raw.%s", source) }
func (TopicNaming) IntelEvents() string                        { return "intel.events.global" }
func (TopicNaming) IntelAlerts() string                        { return "intel.alerts" }
func (TopicNaming) OpsLogs() string                            { return "ops.logs.core" }
func (TopicNaming) OpsAlerts() string                          { return "ops.alerts.core" }
func (TopicNaming) AuditEventStore() string                    { return "audit.event_store" }

// Topics is the global topic naming instance.
var Topics = TopicNaming{}

// TopicRetention maps topics to their retention in hours.
var TopicRetention = map[string]int{
	"md.heartbeat":         24,
	"md.ticks.*":           168,
	"md.trades.*":          168,
	"md.orderbook_l2.*":    72,
	"md.ohlcv_1m.*":        720,
	"exec.orders.*":        720,
	"exec.acks.*":          720,
	"exec.fills.*":         2160,
	"exec.positions.*":     2160,
	"exec.balances.*":      2160,
	"risk.pretrade_checks": 720,
	"signals.features.*":   720,
	"signals.strategy.*":   720,
	"signals.regime.*":     720,
	"intel.raw.*":          72,
	"intel.events.global":  2160,
	"intel.alerts":         720,
	"ops.logs.core":        168,
	"ops.alerts.core":      720,
	"audit.event_store":    8760,
}

// AllTopicPrefixes returns all topic prefixes for provisioning.
func AllTopicPrefixes() []string {
	return []string{
		"md.heartbeat",
		"md.ticks",
		"md.trades",
		"md.orderbook_l2",
		"md.ohlcv_1m",
		"exec.orders",
		"exec.acks",
		"exec.fills",
		"exec.positions",
		"exec.balances",
		"risk.pretrade_checks",
		"signals.features",
		"signals.strategy",
		"signals.regime",
		"intel.raw",
		"intel.events.global",
		"intel.alerts",
		"ops.logs.core",
		"ops.alerts.core",
		"audit.event_store",
	}
}
