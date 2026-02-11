package bus

import (
	"context"
	"fmt"
	"sync"

	"github.com/rs/zerolog/log"
)

// MessageHandler processes a consumed message. Returns error to trigger retry.
type MessageHandler func(ctx context.Context, msg Message) error

// Consumer reads messages from Kafka/RedPanda topics.
type Consumer interface {
	Subscribe(topics []string) error
	Consume(ctx context.Context, handler MessageHandler) error
	Close()
}

// KafkaConsumer wraps confluent-kafka-go consumer.
type KafkaConsumer struct {
	brokers []string
	groupID string
	topics  []string
	handler MessageHandler
	mu      sync.Mutex
	closed  bool
}

// NewConsumer creates a new Kafka consumer.
func NewConsumer(brokers []string, groupID string) (Consumer, error) {
	c := &KafkaConsumer{
		brokers: brokers,
		groupID: groupID,
	}

	log.Info().
		Strs("brokers", brokers).
		Str("group_id", groupID).
		Msg("Kafka consumer created")

	return c, nil
}

// Subscribe registers topics to consume from.
func (c *KafkaConsumer) Subscribe(topics []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.topics = topics
	log.Info().Strs("topics", topics).Str("group", c.groupID).Msg("Subscribed to topics")
	return nil
}

// Consume starts consuming messages. Blocks until context is cancelled.
func (c *KafkaConsumer) Consume(ctx context.Context, handler MessageHandler) error {
	if len(c.topics) == 0 {
		return fmt.Errorf("no topics subscribed")
	}

	log.Info().
		Strs("topics", c.topics).
		Str("group", c.groupID).
		Msg("Starting consumer loop")

	// TODO: Replace with real Kafka consumer loop
	// For now, just wait for context cancellation
	<-ctx.Done()
	return ctx.Err()
}

// Close shuts down the consumer.
func (c *KafkaConsumer) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
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
