"""Tests for event schemas and serialization."""
import pytest
import time
import json

from src.events.base import BaseEvent, generate_uuid7
from src.events.schemas import (
    TokenDiscovered, ReorgNotice, SecurityCheckResult, TokenScored,
    TradeIntent, RiskDecision, ApprovedIntent, ExecutionAttempt,
    TradeExecuted, PositionUpdated, ControlCommand, HeartbeatEvent,
    FreezeEvent, DecisionJournalEntry
)
from src.events.serialization import serialize_event, deserialize_event


class TestBaseEvent:
    def test_create_base_event(self):
        event = BaseEvent(
            event_type="test",
            chain="solana",
            payload={"key": "value"},
        )
        assert event.event_id
        assert event.schema_version == "3.1.0"
        assert event.chain == "solana"
        assert event.event_time > 0
        assert event.ingest_time > 0

    def test_event_id_uniqueness(self):
        ids = set()
        for _ in range(1000):
            event = BaseEvent(event_type="test", chain="solana")
            ids.add(event.event_id)
        assert len(ids) == 1000

    def test_correlation_and_causation(self):
        parent = BaseEvent(event_type="parent", chain="solana")
        child = BaseEvent(
            event_type="child",
            chain="solana",
            correlation_id=parent.event_id,
            causation_id=parent.event_id,
        )
        assert child.correlation_id == parent.event_id
        assert child.causation_id == parent.event_id


class TestEventSchemas:
    def test_token_discovered(self):
        event = TokenDiscovered(
            chain="solana",
            address="So11111111111111111111111111111111111111112",
            name="Test Token",
            symbol="TEST",
            creator="creator_address",
            launchpad="pump.fun",
            initial_liquidity_usd=1000.0,
            block_number=12345,
            block_hash="0xabcdef1234567890abcdef1234567890",
            tx_hash="0x1234567890abcdef1234567890abcdef",
        )
        assert event.event_type == "TokenDiscovered"
        assert event.chain == "solana"
        assert event.address == "So11111111111111111111111111111111111111112"

    def test_trade_intent(self):
        event = TradeIntent(
            chain="base",
            intent_id="test-intent-123",
            token_address="0x1234567890abcdef1234567890abcdef12345678",
            side="BUY",
            amount_usd=50.0,
        )
        assert event.event_type == "TradeIntent"
        assert event.side == "BUY"

    def test_risk_decision(self):
        event = RiskDecision(
            chain="solana",
            intent_id="test-intent-123",
            decision="APPROVE",
            reason_codes=[],
            policy_hash="abc123",
        )
        assert event.decision == "APPROVE"

    def test_freeze_event(self):
        event = FreezeEvent(
            chain="solana",
            reason="gap_detected",
            triggered_by="L0Sanitizer",
            auto=True,
        )
        assert event.auto is True


class TestSerialization:
    def test_serialize_deserialize_roundtrip(self):
        original = TokenDiscovered(
            chain="solana",
            address="test_address_12345678901234567890",
            name="Test",
            symbol="TST",
            block_number=100,
            block_hash="0xdeadbeef" * 4,
            tx_hash="0xcafebabe" * 4,
        )
        data = serialize_event(original)
        assert isinstance(data, bytes)

        restored = deserialize_event(data)
        assert restored.event_type == original.event_type
        assert restored.event_id == original.event_id
        assert restored.chain == original.chain

    def test_serialize_all_event_types(self):
        """Ensure all event types can be serialized and deserialized."""
        events = [
            TokenDiscovered(
                chain="solana",
                address="a" * 32,
                block_number=1,
                block_hash="h" * 32,
                tx_hash="t" * 32,
            ),
            TradeIntent(
                chain="base",
                intent_id="i1",
                token_address="0x" + "a" * 40,
                side="BUY",
                amount_usd=10.0,
            ),
            RiskDecision(
                chain="solana",
                intent_id="i1",
                decision="APPROVE",
                reason_codes=[],
                policy_hash="h",
            ),
            FreezeEvent(
                chain="solana",
                reason="test",
                triggered_by="test",
                auto=True,
            ),
            HeartbeatEvent(
                chain="solana",
                source_module="test",
                config_hash="h",
                uptime_seconds=100.0,
                status="running",
            ),
        ]
        for event in events:
            data = serialize_event(event)
            restored = deserialize_event(data)
            assert restored.event_type == event.event_type
