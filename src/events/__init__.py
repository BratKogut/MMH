"""
MMH v3.1 Event Schema System

Public API for the event subsystem. Import event types, serialization
functions, and idempotency contracts from here.

Usage:
    from src.events import TokenDiscovered, serialize_event, deserialize_event
    from src.events import IdempotencyContract
"""
from __future__ import annotations

from src.events.base import BaseEvent, ChainType, generate_uuid7, unix_timestamp_us
from src.events.contracts import (
    IdempotencyContract,
    PostgresUnavailableError,
    RedisUnavailableError,
)
from src.events.schemas import (
    ALL_EVENT_CLASSES,
    ApprovedIntent,
    ControlCommand,
    DecisionJournalEntry,
    EnrichmentResult,
    ExecutionAttempt,
    FreezeEvent,
    HeartbeatEvent,
    PositionUpdated,
    ReorgNotice,
    RiskDecision,
    SecurityCheckResult,
    TokenDiscovered,
    TokenScored,
    TradeExecuted,
    TradeIntent,
)
from src.events.serialization import (
    CURRENT_SCHEMA_VERSION,
    EVENT_TYPE_REGISTRY,
    deserialize_event,
    deserialize_event_json,
    get_event_class,
    serialize_event,
    serialize_event_json,
)

__all__ = [
    # Base
    "BaseEvent",
    "ChainType",
    "generate_uuid7",
    "unix_timestamp_us",
    # Event types
    "TokenDiscovered",
    "ReorgNotice",
    "SecurityCheckResult",
    "EnrichmentResult",
    "TokenScored",
    "TradeIntent",
    "RiskDecision",
    "ApprovedIntent",
    "ExecutionAttempt",
    "TradeExecuted",
    "PositionUpdated",
    "ControlCommand",
    "HeartbeatEvent",
    "FreezeEvent",
    "DecisionJournalEntry",
    "ALL_EVENT_CLASSES",
    # Serialization
    "serialize_event",
    "deserialize_event",
    "serialize_event_json",
    "deserialize_event_json",
    "get_event_class",
    "EVENT_TYPE_REGISTRY",
    "CURRENT_SCHEMA_VERSION",
    # Contracts
    "IdempotencyContract",
    "RedisUnavailableError",
    "PostgresUnavailableError",
]
