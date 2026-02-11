package bus

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// BaseEvent contains fields common to all events.
type BaseEvent struct {
	EventID       string    `json:"event_id"`
	Timestamp     time.Time `json:"ts"`
	SchemaVersion string    `json:"schema_version"`
	Producer      string    `json:"producer"`
	TraceID       string    `json:"trace_id,omitempty"`
	CausationID   string    `json:"causation_id,omitempty"`
	CorrelationID string    `json:"correlation_id,omitempty"`
}

// NewBaseEvent creates a new BaseEvent with generated IDs.
func NewBaseEvent(producer, schemaVersion string) BaseEvent {
	return BaseEvent{
		EventID:       uuid.New().String(),
		Timestamp:     time.Now(),
		SchemaVersion: schemaVersion,
		Producer:      producer,
		TraceID:       uuid.New().String()[:16],
	}
}

// --- Market Data Events ---

type Tick struct {
	BaseEvent
	Exchange string          `json:"exchange"`
	Symbol   string          `json:"symbol"`
	Bid      decimal.Decimal `json:"bid"`
	Ask      decimal.Decimal `json:"ask"`
	BidSize  decimal.Decimal `json:"bid_size"`
	AskSize  decimal.Decimal `json:"ask_size"`
}

type Trade struct {
	BaseEvent
	Exchange string          `json:"exchange"`
	Symbol   string          `json:"symbol"`
	Price    decimal.Decimal `json:"price"`
	Qty      decimal.Decimal `json:"qty"`
	Side     string          `json:"side"` // buy|sell|unknown
	TradeID  string          `json:"trade_id"`
}

type OrderbookUpdate struct {
	BaseEvent
	Exchange string           `json:"exchange"`
	Symbol   string           `json:"symbol"`
	Type     string           `json:"type"` // snapshot|delta
	Bids     []OrderbookLevel `json:"bids"`
	Asks     []OrderbookLevel `json:"asks"`
	Sequence int64            `json:"sequence"`
}

type OrderbookLevel struct {
	Price decimal.Decimal `json:"price"`
	Qty   decimal.Decimal `json:"qty"`
}

// --- Execution Events ---

type OrderIntent struct {
	BaseEvent
	IntentID    string          `json:"intent_id"`
	StrategyID  string          `json:"strategy_id"`
	Exchange    string          `json:"exchange"`
	Symbol      string          `json:"symbol"`
	Side        string          `json:"side"`       // buy|sell
	OrderType   string          `json:"order_type"` // market|limit
	Qty         decimal.Decimal `json:"qty"`
	LimitPrice  decimal.Decimal `json:"limit_price,omitempty"`
	TimeInForce string          `json:"time_in_force"` // GTC|IOC|FOK
	Tags        []string        `json:"tags"`
}

type Fill struct {
	BaseEvent
	Exchange      string          `json:"exchange"`
	Symbol        string          `json:"symbol"`
	OrderID       string          `json:"order_id"`
	IntentID      string          `json:"intent_id"`
	StrategyID    string          `json:"strategy_id"`
	Side          string          `json:"side"`
	Qty           decimal.Decimal `json:"qty"`
	Price         decimal.Decimal `json:"price"`
	Fee           decimal.Decimal `json:"fee"`
	FeeCurrency   string          `json:"fee_currency"`
	ExpectedPrice decimal.Decimal `json:"expected_price"` // for slippage tracking
	SlippageBps   float64         `json:"slippage_bps"`
}

// --- Risk Events ---

type RiskDecision struct {
	BaseEvent
	IntentID    string   `json:"intent_id"`
	Decision    string   `json:"decision"` // allow|deny|freeze
	ReasonCodes []string `json:"reason_codes"`
	Exposure    float64  `json:"exposure_snapshot_usd"`
	DailyPnL    float64  `json:"daily_pnl_usd"`
}

// --- Signal Events ---

type StrategySignal struct {
	BaseEvent
	StrategyID     string          `json:"strategy_id"`
	Symbol         string          `json:"symbol"`
	SignalType     string          `json:"signal_type"` // target_position|order_intent|risk_adjustment
	Confidence     float64         `json:"confidence"`
	TargetPosition decimal.Decimal `json:"target_position,omitempty"`
	OrdersIntent   []OrderIntent   `json:"orders_intent,omitempty"`
	ReasonCodes    []string        `json:"reason_codes"`
	Metadata       map[string]any  `json:"metadata,omitempty"`
}

type RegimeUpdate struct {
	BaseEvent
	Symbol     string  `json:"symbol"`
	Regime     string  `json:"regime"` // TRENDING_UP|TRENDING_DOWN|MEAN_REVERTING|HIGH_VOL|LOW_VOL|BREAKOUT|UNKNOWN
	Confidence float64 `json:"confidence"`
	PrevRegime string  `json:"prev_regime,omitempty"`
}

// --- Heartbeat ---

type Heartbeat struct {
	BaseEvent
	Component  string             `json:"component"`
	Status     string             `json:"status"` // healthy|degraded|unhealthy
	ConfigHash string             `json:"config_hash"`
	Uptime     time.Duration      `json:"uptime_seconds"`
	Metrics    map[string]float64 `json:"metrics,omitempty"`
}
