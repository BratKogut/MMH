-- MMH v3.1 Database Initialization
-- Requires: TimescaleDB extension

-- Extensions
CREATE EXTENSION IF NOT EXISTS timescaledb;

-- ============================================================
-- Core Tables
-- ============================================================

CREATE TABLE IF NOT EXISTS positions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chain VARCHAR(20) NOT NULL,
    token_address VARCHAR(128) NOT NULL,
    side VARCHAR(10) NOT NULL DEFAULT 'BUY',
    status VARCHAR(20) NOT NULL DEFAULT 'OPEN',
    entry_price NUMERIC(28, 18),
    current_price NUMERIC(28, 18),
    amount_tokens NUMERIC(28, 18),
    amount_usd NUMERIC(18, 6),
    exposure_usd NUMERIC(18, 6),
    pnl_usd NUMERIC(18, 6) DEFAULT 0,
    pnl_pct NUMERIC(10, 4) DEFAULT 0,
    entry_tx_hash VARCHAR(128),
    exit_tx_hash VARCHAR(128),
    intent_id VARCHAR(64),
    scoring_ref_id VARCHAR(128),
    stop_loss_pct NUMERIC(10, 4),
    take_profit_levels JSONB DEFAULT '[]',
    trailing_stop_pct NUMERIC(10, 4),
    max_holding_seconds INTEGER DEFAULT 3600,
    opened_at TIMESTAMPTZ DEFAULT NOW(),
    closed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS trades (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    position_id UUID REFERENCES positions(id),
    chain VARCHAR(20) NOT NULL,
    token_address VARCHAR(128) NOT NULL,
    side VARCHAR(10) NOT NULL,
    amount_tokens NUMERIC(28, 18),
    amount_usd NUMERIC(18, 6),
    price NUMERIC(28, 18),
    tx_hash VARCHAR(128),
    gas_cost_usd NUMERIC(18, 6),
    slippage_bps INTEGER,
    status VARCHAR(20) NOT NULL DEFAULT 'PENDING',
    error_message TEXT,
    executed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS decision_journal (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    decision_type VARCHAR(30) NOT NULL,
    module VARCHAR(50) NOT NULL,
    input_event_ids JSONB DEFAULT '[]',
    output_event_id VARCHAR(128),
    reason_codes JSONB DEFAULT '[]',
    config_hash VARCHAR(64),
    decision_data JSONB DEFAULT '{}',
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS scoring_results (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chain VARCHAR(20) NOT NULL,
    token_address VARCHAR(128) NOT NULL,
    risk_score INTEGER,
    momentum_score INTEGER,
    overall_score INTEGER,
    recommendation VARCHAR(20),
    flags JSONB DEFAULT '[]',
    details JSONB DEFAULT '{}',
    config_hash VARCHAR(64),
    source_event_id VARCHAR(128),
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS security_checks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chain VARCHAR(20) NOT NULL,
    token_address VARCHAR(128) NOT NULL,
    provider VARCHAR(30) NOT NULL,
    is_safe BOOLEAN,
    risk_level VARCHAR(20),
    flags JSONB DEFAULT '[]',
    raw_data JSONB DEFAULT '{}',
    latency_ms NUMERIC(10, 2),
    cached BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS token_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type VARCHAR(30) NOT NULL,
    chain VARCHAR(20) NOT NULL,
    token_address VARCHAR(128) NOT NULL,
    name VARCHAR(128),
    symbol VARCHAR(20),
    creator VARCHAR(128),
    launchpad VARCHAR(50),
    dex VARCHAR(50),
    pool_address VARCHAR(128),
    tx_hash VARCHAR(128),
    block_number BIGINT,
    initial_liquidity_usd NUMERIC(18, 6),
    source VARCHAR(50),
    raw_data JSONB DEFAULT '{}',
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS risk_decisions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    intent_id VARCHAR(64) NOT NULL,
    decision VARCHAR(10) NOT NULL,
    reason_codes JSONB DEFAULT '[]',
    policy_hash VARCHAR(64),
    exposure_snapshot JSONB DEFAULT '{}',
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS system_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type VARCHAR(30) NOT NULL,
    chain VARCHAR(20),
    source_module VARCHAR(50),
    reason TEXT,
    metadata JSONB DEFAULT '{}',
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS daily_stats (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    date DATE NOT NULL UNIQUE,
    total_trades INTEGER DEFAULT 0,
    winning_trades INTEGER DEFAULT 0,
    losing_trades INTEGER DEFAULT 0,
    total_pnl_usd NUMERIC(18, 6) DEFAULT 0,
    max_drawdown_usd NUMERIC(18, 6) DEFAULT 0,
    max_exposure_usd NUMERIC(18, 6) DEFAULT 0,
    gas_costs_usd NUMERIC(18, 6) DEFAULT 0,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- ============================================================
-- TimescaleDB Hypertables (time-series data)
-- ============================================================

SELECT create_hypertable('decision_journal', 'created_at', if_not_exists => TRUE);
SELECT create_hypertable('scoring_results', 'created_at', if_not_exists => TRUE);
SELECT create_hypertable('security_checks', 'created_at', if_not_exists => TRUE);
SELECT create_hypertable('token_events', 'created_at', if_not_exists => TRUE);
SELECT create_hypertable('risk_decisions', 'created_at', if_not_exists => TRUE);
SELECT create_hypertable('system_events', 'created_at', if_not_exists => TRUE);

-- ============================================================
-- Indexes (compound + partial for common query patterns)
-- ============================================================

-- Positions
CREATE INDEX IF NOT EXISTS idx_positions_chain_status ON positions (chain, status);
CREATE INDEX IF NOT EXISTS idx_positions_token_status ON positions (token_address, status);
CREATE INDEX IF NOT EXISTS idx_positions_status_opened ON positions (status, opened_at DESC);
CREATE INDEX IF NOT EXISTS idx_positions_intent_id ON positions (intent_id);

-- Trades
CREATE INDEX IF NOT EXISTS idx_trades_position ON trades (position_id);
CREATE INDEX IF NOT EXISTS idx_trades_chain_status ON trades (chain, status);
CREATE INDEX IF NOT EXISTS idx_trades_tx_hash ON trades (tx_hash);
CREATE INDEX IF NOT EXISTS idx_trades_executed ON trades (executed_at DESC);

-- Decision Journal
CREATE INDEX IF NOT EXISTS idx_journal_type_module ON decision_journal (decision_type, module);
CREATE INDEX IF NOT EXISTS idx_journal_created ON decision_journal (created_at DESC);

-- Scoring Results
CREATE INDEX IF NOT EXISTS idx_scoring_chain_token ON scoring_results (chain, token_address);
CREATE INDEX IF NOT EXISTS idx_scoring_recommendation ON scoring_results (recommendation, created_at DESC);

-- Security Checks
CREATE INDEX IF NOT EXISTS idx_security_chain_token ON security_checks (chain, token_address);
CREATE INDEX IF NOT EXISTS idx_security_provider ON security_checks (provider, created_at DESC);

-- Token Events
CREATE INDEX IF NOT EXISTS idx_events_chain_token ON token_events (chain, token_address);
CREATE INDEX IF NOT EXISTS idx_events_type_created ON token_events (event_type, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_block ON token_events (chain, block_number) WHERE block_number IS NOT NULL;

-- Risk Decisions
CREATE INDEX IF NOT EXISTS idx_risk_intent ON risk_decisions (intent_id);
CREATE INDEX IF NOT EXISTS idx_risk_decision ON risk_decisions (decision, created_at DESC);

-- ============================================================
-- Retention Policies (TimescaleDB automatic data management)
-- ============================================================

-- Keep decision journal for 90 days
SELECT add_retention_policy('decision_journal', INTERVAL '90 days', if_not_exists => TRUE);

-- Keep scoring results for 30 days
SELECT add_retention_policy('scoring_results', INTERVAL '30 days', if_not_exists => TRUE);

-- Keep security checks for 14 days
SELECT add_retention_policy('security_checks', INTERVAL '14 days', if_not_exists => TRUE);

-- Keep token events for 30 days
SELECT add_retention_policy('token_events', INTERVAL '30 days', if_not_exists => TRUE);

-- Keep risk decisions for 60 days
SELECT add_retention_policy('risk_decisions', INTERVAL '60 days', if_not_exists => TRUE);

-- Keep system events for 30 days
SELECT add_retention_policy('system_events', INTERVAL '30 days', if_not_exists => TRUE);

-- ============================================================
-- Continuous Aggregates (pre-computed rollups)
-- ============================================================

-- Hourly scoring stats
CREATE MATERIALIZED VIEW IF NOT EXISTS scoring_hourly
WITH (timescaledb.continuous) AS
SELECT
    time_bucket('1 hour', created_at) AS hour,
    chain,
    recommendation,
    COUNT(*) AS total,
    AVG(overall_score) AS avg_score,
    MIN(overall_score) AS min_score,
    MAX(overall_score) AS max_score
FROM scoring_results
GROUP BY hour, chain, recommendation
WITH NO DATA;

SELECT add_continuous_aggregate_policy('scoring_hourly',
    start_offset => INTERVAL '3 hours',
    end_offset => INTERVAL '1 hour',
    schedule_interval => INTERVAL '1 hour',
    if_not_exists => TRUE);

-- ============================================================
-- Functions
-- ============================================================

-- Auto-update updated_at timestamp
CREATE OR REPLACE FUNCTION update_updated_at_column()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ language 'plpgsql';

CREATE OR REPLACE TRIGGER update_positions_updated_at
    BEFORE UPDATE ON positions
    FOR EACH ROW EXECUTE PROCEDURE update_updated_at_column();

CREATE OR REPLACE TRIGGER update_daily_stats_updated_at
    BEFORE UPDATE ON daily_stats
    FOR EACH ROW EXECUTE PROCEDURE update_updated_at_column();
