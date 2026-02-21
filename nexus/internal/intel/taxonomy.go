package intel

import (
	"fmt"
	"sync"
	"time"
)

// TopicConfig holds per-topic configuration.
// SELEKTYWNA INTELIGENCJA: every topic has strict budgets and query limits.
type TopicConfig struct {
	Topic            Topic
	MaxQueriesPerDay int
	DefaultProvider  string
	DefaultTTL       int // seconds
	DefaultPriority  Priority
	CostBudgetUSD    float64 // daily budget in USD
}

// topicUsage tracks daily usage counters for a single topic.
type topicUsage struct {
	QueriesUsed  int
	CostUsedUSD  float64
	LastResetDay time.Time
}

// DefaultTaxonomy returns the default topic taxonomy with budgets.
// Each topic is carefully budgeted -- we don't mine the internet,
// we ask specific questions with measurable ROI.
func DefaultTaxonomy() map[Topic]TopicConfig {
	return map[Topic]TopicConfig{
		TopicExchangeOutage: {
			Topic:            TopicExchangeOutage,
			MaxQueriesPerDay: 20,
			DefaultProvider:  "primary",
			DefaultTTL:       300, // 5 minutes -- outages are urgent and short-lived
			DefaultPriority:  PriorityP0,
			CostBudgetUSD:    2.00,
		},
		TopicRegulatoryAction: {
			Topic:            TopicRegulatoryAction,
			MaxQueriesPerDay: 10,
			DefaultProvider:  "primary",
			DefaultTTL:       3600, // 1 hour
			DefaultPriority:  PriorityP1,
			CostBudgetUSD:    1.50,
		},
		TopicMacroEvent: {
			Topic:            TopicMacroEvent,
			MaxQueriesPerDay: 15,
			DefaultProvider:  "primary",
			DefaultTTL:       1800, // 30 minutes
			DefaultPriority:  PriorityP1,
			CostBudgetUSD:    2.00,
		},
		TopicSentimentShift: {
			Topic:            TopicSentimentShift,
			MaxQueriesPerDay: 30,
			DefaultProvider:  "primary",
			DefaultTTL:       900, // 15 minutes -- sentiment moves fast
			DefaultPriority:  PriorityP2,
			CostBudgetUSD:    3.00,
		},
		TopicWhaleMovement: {
			Topic:            TopicWhaleMovement,
			MaxQueriesPerDay: 25,
			DefaultProvider:  "primary",
			DefaultTTL:       600, // 10 minutes
			DefaultPriority:  PriorityP1,
			CostBudgetUSD:    2.50,
		},
		TopicProtocolUpdate: {
			Topic:            TopicProtocolUpdate,
			MaxQueriesPerDay: 10,
			DefaultProvider:  "primary",
			DefaultTTL:       7200, // 2 hours
			DefaultPriority:  PriorityP2,
			CostBudgetUSD:    1.00,
		},
		TopicLiquidityEvent: {
			Topic:            TopicLiquidityEvent,
			MaxQueriesPerDay: 20,
			DefaultProvider:  "primary",
			DefaultTTL:       600, // 10 minutes
			DefaultPriority:  PriorityP1,
			CostBudgetUSD:    2.00,
		},
		TopicCorrelationBreak: {
			Topic:            TopicCorrelationBreak,
			MaxQueriesPerDay: 15,
			DefaultProvider:  "primary",
			DefaultTTL:       1800, // 30 minutes
			DefaultPriority:  PriorityP2,
			CostBudgetUSD:    1.50,
		},
		TopicVolatilitySpike: {
			Topic:            TopicVolatilitySpike,
			MaxQueriesPerDay: 30,
			DefaultProvider:  "primary",
			DefaultTTL:       300, // 5 minutes -- vol spikes are immediate
			DefaultPriority:  PriorityP0,
			CostBudgetUSD:    3.00,
		},
		TopicListingDeListing: {
			Topic:            TopicListingDeListing,
			MaxQueriesPerDay: 10,
			DefaultProvider:  "primary",
			DefaultTTL:       3600, // 1 hour
			DefaultPriority:  PriorityP2,
			CostBudgetUSD:    1.00,
		},
	}
}

// TopicRegistry manages topic configurations and budget tracking.
// Thread-safe: all methods can be called concurrently.
type TopicRegistry struct {
	configs map[Topic]TopicConfig
	usage   map[Topic]*topicUsage // daily usage tracking
	mu      sync.RWMutex
}

// NewTopicRegistry creates a new TopicRegistry with the given topic configs.
func NewTopicRegistry(configs map[Topic]TopicConfig) *TopicRegistry {
	usage := make(map[Topic]*topicUsage, len(configs))
	now := time.Now()
	for topic := range configs {
		usage[topic] = &topicUsage{
			LastResetDay: truncateToDay(now),
		}
	}
	return &TopicRegistry{
		configs: configs,
		usage:   usage,
	}
}

// CanQuery checks whether a query is allowed for the given topic.
// Returns (allowed, reason). If allowed is false, reason explains why.
func (r *TopicRegistry) CanQuery(topic Topic) (bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cfg, ok := r.configs[topic]
	if !ok {
		return false, fmt.Sprintf("unknown topic: %s", topic)
	}

	u := r.usage[topic]
	r.maybeResetLocked(topic, u)

	if u.QueriesUsed >= cfg.MaxQueriesPerDay {
		return false, fmt.Sprintf("topic %s: daily query limit reached (%d/%d)",
			topic, u.QueriesUsed, cfg.MaxQueriesPerDay)
	}

	if u.CostUsedUSD >= cfg.CostBudgetUSD {
		return false, fmt.Sprintf("topic %s: daily cost budget exhausted ($%.4f/$%.4f)",
			topic, u.CostUsedUSD, cfg.CostBudgetUSD)
	}

	return true, ""
}

// RecordQuery records that a query was executed for the given topic with the
// specified cost. Call this after a successful LLM query.
func (r *TopicRegistry) RecordQuery(topic Topic, costUSD float64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	u, ok := r.usage[topic]
	if !ok {
		// Unknown topic -- create usage entry on the fly.
		u = &topicUsage{LastResetDay: truncateToDay(time.Now())}
		r.usage[topic] = u
	}

	r.maybeResetLocked(topic, u)
	u.QueriesUsed++
	u.CostUsedUSD += costUSD
}

// GetUsage returns the current daily usage counters for a topic.
func (r *TopicRegistry) GetUsage(topic Topic) (queries int, costUSD float64) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	u, ok := r.usage[topic]
	if !ok {
		return 0, 0
	}
	return u.QueriesUsed, u.CostUsedUSD
}

// GetConfig returns the configuration for a topic.
func (r *TopicRegistry) GetConfig(topic Topic) (TopicConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cfg, ok := r.configs[topic]
	return cfg, ok
}

// ResetDaily forces a reset of all daily usage counters.
func (r *TopicRegistry) ResetDaily() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := truncateToDay(time.Now())
	for topic, u := range r.usage {
		_ = topic
		u.QueriesUsed = 0
		u.CostUsedUSD = 0
		u.LastResetDay = now
	}
}

// maybeResetLocked auto-resets usage if we've crossed into a new day.
// Caller must hold r.mu (write lock).
func (r *TopicRegistry) maybeResetLocked(_ Topic, u *topicUsage) {
	today := truncateToDay(time.Now())
	if !u.LastResetDay.Equal(today) {
		u.QueriesUsed = 0
		u.CostUsedUSD = 0
		u.LastResetDay = today
	}
}

// truncateToDay returns t truncated to midnight UTC.
func truncateToDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
