"""Database models for MMH v3.1.

Uses SQLAlchemy 2.0 style with async support.
TimescaleDB hypertables for time-series data.
"""

from sqlalchemy import (
    Column, String, Integer, Float, Boolean, DateTime, JSON, Text,
    ForeignKey, UniqueConstraint, Index, Numeric, SmallInteger,
    func, text
)
from sqlalchemy.dialects.postgresql import UUID, JSONB
from sqlalchemy.orm import DeclarativeBase, relationship, Mapped, mapped_column
from datetime import datetime
import uuid


class Base(DeclarativeBase):
    pass


class Token(Base):
    """Discovered tokens."""
    __tablename__ = "tokens"

    id: Mapped[uuid.UUID] = mapped_column(UUID(as_uuid=True), primary_key=True, default=uuid.uuid4)
    chain: Mapped[str] = mapped_column(String(20), nullable=False)
    address: Mapped[str] = mapped_column(String(100), nullable=False)
    symbol: Mapped[str] = mapped_column(String(50), nullable=True)
    name: Mapped[str] = mapped_column(String(200), nullable=True)
    creator_address: Mapped[str] = mapped_column(String(100), nullable=True)
    launchpad: Mapped[str] = mapped_column(String(50), nullable=True)
    dex: Mapped[str] = mapped_column(String(50), nullable=True)
    pool_address: Mapped[str] = mapped_column(String(100), nullable=True)
    discovered_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), nullable=True)

    __table_args__ = (
        UniqueConstraint('chain', 'address', name='uq_token_chain_address'),
    )


class TokenScore(Base):
    """Token scoring history - TimescaleDB hypertable."""
    __tablename__ = "token_scores"

    token_id: Mapped[uuid.UUID] = mapped_column(UUID(as_uuid=True), ForeignKey("tokens.id"), primary_key=True)
    scored_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), primary_key=True, server_default=func.now())
    risk_score: Mapped[int] = mapped_column(SmallInteger, nullable=True)
    momentum_score: Mapped[int] = mapped_column(SmallInteger, nullable=True)
    overall_score: Mapped[int] = mapped_column(SmallInteger, nullable=True)
    flags: Mapped[dict] = mapped_column(JSONB, nullable=True)
    recommendation: Mapped[str] = mapped_column(String(20), nullable=True)
    details: Mapped[dict] = mapped_column(JSONB, nullable=True)
    config_hash: Mapped[str] = mapped_column(String(64), nullable=True)


class Position(Base):
    """Trading positions - Ledger of Record.

    Position Manager is THE source of truth for positions (Option A).
    Event-sourced from TradeExecuted events.
    """
    __tablename__ = "positions"

    id: Mapped[uuid.UUID] = mapped_column(UUID(as_uuid=True), primary_key=True, default=uuid.uuid4)
    chain: Mapped[str] = mapped_column(String(20), nullable=False)
    token_address: Mapped[str] = mapped_column(String(100), nullable=False)
    token_symbol: Mapped[str] = mapped_column(String(50), nullable=True)
    entry_price = mapped_column(Numeric(30, 18), nullable=True)
    entry_amount = mapped_column(Numeric(30, 18), nullable=True)
    entry_timestamp: Mapped[datetime] = mapped_column(DateTime(timezone=True), nullable=True)
    entry_tx_hash: Mapped[str] = mapped_column(String(100), nullable=True)
    exit_price = mapped_column(Numeric(30, 18), nullable=True)
    exit_timestamp: Mapped[datetime] = mapped_column(DateTime(timezone=True), nullable=True)
    take_profit_levels: Mapped[dict] = mapped_column(JSONB, default=lambda: [50, 100, 200])
    stop_loss_pct = mapped_column(Numeric(10, 4), default=-30)
    trailing_stop_pct = mapped_column(Numeric(10, 4), default=0)
    status: Mapped[str] = mapped_column(String(20), default="OPEN")  # OPEN | PARTIAL_EXIT | CLOSED
    pnl_usd = mapped_column(Numeric(20, 2), nullable=True)
    pnl_pct = mapped_column(Numeric(10, 4), nullable=True)
    exposure_usd = mapped_column(Numeric(20, 2), nullable=True)
    intent_id: Mapped[str] = mapped_column(String(64), nullable=True, unique=True)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())
    updated_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now(), onupdate=func.now())

    transactions = relationship("Transaction", back_populates="position")

    __table_args__ = (
        Index('idx_position_status', 'status'),
        Index('idx_position_chain', 'chain'),
    )


class Transaction(Base):
    """Transaction history."""
    __tablename__ = "transactions"

    id: Mapped[uuid.UUID] = mapped_column(UUID(as_uuid=True), primary_key=True, default=uuid.uuid4)
    position_id: Mapped[uuid.UUID] = mapped_column(UUID(as_uuid=True), ForeignKey("positions.id"), nullable=True)
    chain: Mapped[str] = mapped_column(String(20), nullable=False)
    tx_hash: Mapped[str] = mapped_column(String(100), nullable=True)
    tx_type: Mapped[str] = mapped_column(String(20), nullable=False)  # BUY, SELL, PARTIAL_SELL
    amount_in = mapped_column(Numeric(30, 18), nullable=True)
    amount_out = mapped_column(Numeric(30, 18), nullable=True)
    price = mapped_column(Numeric(30, 18), nullable=True)
    gas_cost_usd = mapped_column(Numeric(10, 4), nullable=True)
    executed_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())
    intent_id: Mapped[str] = mapped_column(String(64), nullable=True)

    position = relationship("Position", back_populates="transactions")


class SecurityCheck(Base):
    """Security check results - TimescaleDB hypertable."""
    __tablename__ = "security_checks"

    chain: Mapped[str] = mapped_column(String(20), primary_key=True)
    token_address: Mapped[str] = mapped_column(String(100), primary_key=True)
    checked_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), primary_key=True, server_default=func.now())
    is_safe: Mapped[bool] = mapped_column(Boolean, nullable=True)
    risk_level: Mapped[str] = mapped_column(String(20), nullable=True)
    flags: Mapped[dict] = mapped_column(JSONB, nullable=True)
    raw_data: Mapped[dict] = mapped_column(JSONB, nullable=True)
    check_type: Mapped[str] = mapped_column(String(20), nullable=True)  # cached | fresh


class ControlLog(Base):
    """Persistent control command log.

    Every operator command is logged here for replay after restart.
    """
    __tablename__ = "control_log"

    id: Mapped[uuid.UUID] = mapped_column(UUID(as_uuid=True), primary_key=True, default=uuid.uuid4)
    command_type: Mapped[str] = mapped_column(String(50), nullable=False)  # FREEZE, KILL, RESUME, CONFIG_UPDATE
    target: Mapped[str] = mapped_column(String(100), nullable=True)  # chain/module/global
    params: Mapped[dict] = mapped_column(JSONB, nullable=True)
    operator_id: Mapped[str] = mapped_column(String(100), nullable=True)
    executed_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())
    acknowledged: Mapped[bool] = mapped_column(Boolean, default=False)


class DecisionJournal(Base):
    """Decision journal - every scoring/risk/execution decision recorded.

    Provides:
    - Instant debugging
    - Audit trail
    - ML training data
    """
    __tablename__ = "decision_journal"

    id: Mapped[uuid.UUID] = mapped_column(UUID(as_uuid=True), primary_key=True, default=uuid.uuid4)
    decision_type: Mapped[str] = mapped_column(String(50), nullable=False)  # SCORING, RISK, EXECUTION
    module: Mapped[str] = mapped_column(String(50), nullable=False)
    input_event_ids: Mapped[dict] = mapped_column(JSONB, nullable=True)
    output_event_id: Mapped[str] = mapped_column(String(64), nullable=True)
    reason_codes: Mapped[dict] = mapped_column(JSONB, nullable=True)
    config_hash: Mapped[str] = mapped_column(String(64), nullable=True)
    decision_data: Mapped[dict] = mapped_column(JSONB, nullable=True)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())

    __table_args__ = (
        Index('idx_journal_type', 'decision_type'),
        Index('idx_journal_module', 'module'),
    )


class DailyPnL(Base):
    """Daily PnL tracking for loss limits."""
    __tablename__ = "daily_pnl"

    date: Mapped[datetime] = mapped_column(DateTime(timezone=True), primary_key=True)
    chain: Mapped[str] = mapped_column(String(20), primary_key=True)
    realized_pnl_usd = mapped_column(Numeric(20, 2), default=0)
    unrealized_pnl_usd = mapped_column(Numeric(20, 2), default=0)
    trades_count: Mapped[int] = mapped_column(Integer, default=0)
    wins: Mapped[int] = mapped_column(Integer, default=0)
    losses: Mapped[int] = mapped_column(Integer, default=0)


# SQL for creating TimescaleDB hypertables (run after table creation)
TIMESCALE_SETUP_SQL = """
-- Convert to hypertables
SELECT create_hypertable('token_scores', 'scored_at', if_not_exists => TRUE);
SELECT create_hypertable('security_checks', 'checked_at', if_not_exists => TRUE);

-- Retention policies (optional)
-- SELECT add_retention_policy('token_scores', INTERVAL '30 days');
-- SELECT add_retention_policy('security_checks', INTERVAL '7 days');
"""
