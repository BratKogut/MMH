-- NEXUS Trading Platform - ClickHouse Schema
-- All tables for market data, execution, intel, and audit

CREATE DATABASE IF NOT EXISTS nexus;
USE nexus;

-- ============================================
-- MARKET DATA
-- ============================================

-- Ticks (BBO)
CREATE TABLE IF NOT EXISTS md_ticks (
    exchange     LowCardinality(String),
    symbol       LowCardinality(String),
    ts           DateTime64(6),
    bid          Decimal64(8),
    ask          Decimal64(8),
    bid_size     Decimal64(8),
    ask_size     Decimal64(8),
    event_id     String
) ENGINE = ReplacingMergeTree(ts)
PARTITION BY (exchange, toYYYYMMDD(ts))
ORDER BY (symbol, ts)
TTL toDateTime(ts) + INTERVAL 90 DAY;

-- Trades
CREATE TABLE IF NOT EXISTS md_trades (
    exchange     LowCardinality(String),
    symbol       LowCardinality(String),
    ts           DateTime64(6),
    price        Decimal64(8),
    qty          Decimal64(8),
    side         Enum8('buy'=1, 'sell'=2, 'unknown'=0),
    trade_id     String,
    event_id     String
) ENGINE = ReplacingMergeTree(ts)
PARTITION BY (exchange, toYYYYMMDD(ts))
ORDER BY (symbol, ts, trade_id)
TTL toDateTime(ts) + INTERVAL 180 DAY;

-- OHLCV 1-minute (materialized view from trades)
CREATE TABLE IF NOT EXISTS md_ohlcv_1m (
    exchange     LowCardinality(String),
    symbol       LowCardinality(String),
    ts_bucket    DateTime,
    open         AggregateFunction(argMin, Decimal64(8), DateTime64(6)),
    high         AggregateFunction(max, Decimal64(8)),
    low          AggregateFunction(min, Decimal64(8)),
    close        AggregateFunction(argMax, Decimal64(8), DateTime64(6)),
    volume       AggregateFunction(sum, Decimal64(8)),
    trade_count  AggregateFunction(count)
) ENGINE = AggregatingMergeTree()
PARTITION BY (exchange, toYYYYMMDD(ts_bucket))
ORDER BY (symbol, ts_bucket)
TTL ts_bucket + INTERVAL 365 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS md_ohlcv_1m_mv
TO md_ohlcv_1m AS
SELECT
    exchange,
    symbol,
    toStartOfMinute(ts) as ts_bucket,
    argMinState(price, ts) as open,
    maxState(price) as high,
    minState(price) as low,
    argMaxState(price, ts) as close,
    sumState(qty) as volume,
    countState() as trade_count
FROM md_trades
GROUP BY exchange, symbol, ts_bucket;

-- Orderbook snapshots (sampled)
CREATE TABLE IF NOT EXISTS md_orderbook_snapshots (
    exchange     LowCardinality(String),
    symbol       LowCardinality(String),
    ts           DateTime64(6),
    bid_prices   Array(Decimal64(8)),
    bid_sizes    Array(Decimal64(8)),
    ask_prices   Array(Decimal64(8)),
    ask_sizes    Array(Decimal64(8)),
    spread_bps   Float32,
    mid_price    Decimal64(8),
    imbalance    Float32
) ENGINE = MergeTree()
PARTITION BY (exchange, toYYYYMMDD(ts))
ORDER BY (symbol, ts)
TTL toDateTime(ts) + INTERVAL 30 DAY;

-- ============================================
-- EXECUTION
-- ============================================

-- Orders
CREATE TABLE IF NOT EXISTS exec_orders (
    exchange       LowCardinality(String),
    symbol         LowCardinality(String),
    ts             DateTime64(6),
    intent_id      String,
    order_id       String,
    strategy_id    LowCardinality(String),
    side           Enum8('buy'=1, 'sell'=2),
    order_type     LowCardinality(String),
    qty            Decimal64(8),
    limit_price    Decimal64(8),
    status         LowCardinality(String),
    reason         String,
    trace_id       String
) ENGINE = MergeTree()
PARTITION BY (exchange, toYYYYMMDD(ts))
ORDER BY (strategy_id, symbol, ts);

-- Fills
CREATE TABLE IF NOT EXISTS exec_fills (
    exchange         LowCardinality(String),
    symbol           LowCardinality(String),
    ts               DateTime64(6),
    order_id         String,
    intent_id        String,
    strategy_id      LowCardinality(String),
    side             Enum8('buy'=1, 'sell'=2),
    qty              Decimal64(8),
    price            Decimal64(8),
    fee              Decimal64(8),
    fee_currency     LowCardinality(String),
    expected_price   Decimal64(8),
    slippage_bps     Float32,
    trace_id         String
) ENGINE = MergeTree()
PARTITION BY (exchange, toYYYYMMDD(ts))
ORDER BY (strategy_id, symbol, ts);

-- Positions
CREATE TABLE IF NOT EXISTS exec_positions (
    ts              DateTime64(6),
    exchange        LowCardinality(String),
    symbol          LowCardinality(String),
    strategy_id     LowCardinality(String),
    side            LowCardinality(String),
    qty             Decimal64(8),
    avg_entry_price Decimal64(8),
    unrealized_pnl  Decimal64(4),
    realized_pnl    Decimal64(4),
    exposure_usd    Decimal64(4)
) ENGINE = ReplacingMergeTree(ts)
PARTITION BY toYYYYMMDD(ts)
ORDER BY (strategy_id, exchange, symbol);

-- ============================================
-- RISK
-- ============================================

CREATE TABLE IF NOT EXISTS risk_decisions (
    ts            DateTime64(6),
    intent_id     String,
    strategy_id   LowCardinality(String),
    decision      Enum8('allow'=1, 'deny'=2, 'freeze'=3),
    reason_codes  Array(String),
    exposure_usd  Float64,
    daily_pnl_usd Float64,
    trace_id      String
) ENGINE = MergeTree()
PARTITION BY toYYYYMMDD(ts)
ORDER BY (strategy_id, ts);

-- ============================================
-- INTEL
-- ============================================

CREATE TABLE IF NOT EXISTS intel_events (
    event_id          String,
    ts                DateTime64(6),
    topic             LowCardinality(String),
    priority          Enum8('P0'=0, 'P1'=1, 'P2'=2, 'P3'=3),
    ttl_seconds       UInt32,
    confidence        Float32,
    impact_horizon    LowCardinality(String),
    directional_bias  LowCardinality(String),
    asset_tags        Array(LowCardinality(String)),
    claims_json       String,
    sources_json      String,
    provider          LowCardinality(String),
    cost_usd          Decimal64(6),
    latency_ms        UInt32,
    downstream_trade_ids Array(String)
) ENGINE = MergeTree()
PARTITION BY toYYYYMMDD(ts)
ORDER BY (topic, ts);

-- ============================================
-- COST ATTRIBUTION
-- ============================================

CREATE TABLE IF NOT EXISTS cost_attribution_daily (
    date               Date,
    strategy_id        LowCardinality(String),
    exchange           LowCardinality(String),
    trading_fees_usd   Decimal64(4),
    slippage_cost_usd  Decimal64(4),
    infra_cost_usd     Decimal64(4),
    llm_cost_usd       Decimal64(4),
    total_cost_usd     Decimal64(4),
    gross_pnl_usd      Decimal64(4),
    net_pnl_usd        Decimal64(4),
    trade_count        UInt32,
    intel_events_used  UInt32
) ENGINE = SummingMergeTree()
PARTITION BY toYYYYMM(date)
ORDER BY (strategy_id, date);

-- ============================================
-- DATA QUALITY
-- ============================================

CREATE TABLE IF NOT EXISTS data_quality (
    ts             DateTime,
    exchange       LowCardinality(String),
    symbol         LowCardinality(String),
    feed_lag_ms    Float32,
    gap_count      UInt32,
    jitter_ms      Float32,
    uptime_pct     Float32,
    quality_score  Float32
) ENGINE = MergeTree()
PARTITION BY toYYYYMMDD(ts)
ORDER BY (exchange, symbol, ts)
TTL ts + INTERVAL 90 DAY;

-- ============================================
-- AUDIT (Source of Truth for Replay)
-- ============================================

CREATE TABLE IF NOT EXISTS audit_events (
    event_id       String,
    trace_id       String,
    causation_id   String,
    correlation_id String,
    event_type     LowCardinality(String),
    ts             DateTime64(6),
    producer       LowCardinality(String),
    schema_version LowCardinality(String),
    payload        String
) ENGINE = MergeTree()
PARTITION BY toYYYYMMDD(ts)
ORDER BY (event_type, ts, event_id)
TTL toDateTime(ts) + INTERVAL 365 DAY;

-- ============================================
-- SIGNALS / FEATURES
-- ============================================

CREATE TABLE IF NOT EXISTS signals_features (
    ts             DateTime64(6),
    symbol         LowCardinality(String),
    feature_name   LowCardinality(String),
    value          Float64,
    producer       LowCardinality(String)
) ENGINE = MergeTree()
PARTITION BY toYYYYMMDD(ts)
ORDER BY (symbol, feature_name, ts)
TTL toDateTime(ts) + INTERVAL 90 DAY;

CREATE TABLE IF NOT EXISTS signals_regime (
    ts             DateTime64(6),
    symbol         LowCardinality(String),
    regime         LowCardinality(String),
    confidence     Float32,
    prev_regime    LowCardinality(String)
) ENGINE = MergeTree()
PARTITION BY toYYYYMMDD(ts)
ORDER BY (symbol, ts)
TTL toDateTime(ts) + INTERVAL 180 DAY;
