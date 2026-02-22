"""Tests for collector modules (base, solana, evm)."""

import asyncio
import time
import pytest
from unittest.mock import AsyncMock, MagicMock, patch

from src.collector.base import BaseCollector
from src.collector.solana import SolanaCollector
from src.collector.evm import EVMCollector, UNISWAP_V3_POOL_CREATED, AERODROME_POOL_CREATED


class ConcreteCollector(BaseCollector):
    """Concrete collector for testing BaseCollector."""

    def __init__(self, chain, bus, wal, config):
        super().__init__(chain, bus, wal, config)
        self.connected = False
        self.disconnected = False

    async def _connect(self):
        self.connected = True

    async def _disconnect(self):
        self.disconnected = True

    async def _collect_loop(self):
        pass

    def _parse_event(self, raw_data):
        return raw_data


class TestBaseCollector:
    """Tests for BaseCollector base class."""

    @pytest.fixture
    def bus(self):
        bus = AsyncMock()
        bus.publish = AsyncMock()
        return bus

    @pytest.fixture
    def wal(self):
        wal = AsyncMock()
        wal.append = AsyncMock(return_value=0)
        return wal

    @pytest.fixture
    def collector(self, bus, wal):
        return ConcreteCollector("test_chain", bus, wal, {"key": "value"})

    def test_chain_property(self, collector):
        assert collector.chain == "test_chain"

    def test_initial_state(self, collector):
        assert not collector.is_frozen

    @pytest.mark.asyncio
    async def test_process_event_writes_wal_then_publishes(self, collector, bus, wal):
        event = {"event_type": "TestEvent", "event_id": "test:1"}
        await collector._process_event(event)
        wal.append.assert_called_once()
        bus.publish.assert_called_once()

    @pytest.mark.asyncio
    async def test_process_event_frozen_drops_event(self, collector, bus, wal):
        collector._frozen = True
        await collector._process_event({"event_type": "Test"})
        wal.append.assert_not_called()
        bus.publish.assert_not_called()

    @pytest.mark.asyncio
    async def test_wal_failure_freezes_collector(self, collector, wal, bus):
        wal.append.side_effect = Exception("disk full")
        await collector._process_event({"event_type": "Test", "event_id": "t:1"})
        assert collector.is_frozen
        bus.publish.assert_called()  # freeze event published

    @pytest.mark.asyncio
    async def test_reorg_detection(self, collector, bus, wal):
        collector._last_block_number = 100
        collector._last_block_hash = "0xabc"
        event = {
            "event_type": "Test",
            "event_id": "t:1",
            "block_number": "99",
            "block_hash": "0xdef",
        }
        await collector._process_event(event)
        assert collector._reorgs_detected == 1

    def test_get_metrics(self, collector):
        metrics = collector.get_metrics()
        assert metrics["chain"] == "test_chain"
        assert metrics["frozen"] is False
        assert metrics["events_collected"] == 0


class TestSolanaCollector:
    """Tests for SolanaCollector event parsing."""

    @pytest.fixture
    def collector(self):
        bus = AsyncMock()
        wal = AsyncMock()
        return SolanaCollector(bus, wal, {
            "pumpportal_ws_url": "wss://pumpportal.fun/api/data",
            "helius_ws_url": "",
        })

    def test_parse_pumpportal_event_valid(self, collector):
        data = {
            "mint": "TokenMint123",
            "name": "TestToken",
            "symbol": "TEST",
            "signature": "sig123",
            "traderPublicKey": "creator123",
            "bondingCurveKey": "curve123",
            "initialBuy": 0.5,
        }
        event = collector._parse_pumpportal_event(data)
        assert event is not None
        assert event["event_type"] == "TokenDiscovered"
        assert event["chain"] == "solana"
        assert event["address"] == "TokenMint123"
        assert event["launchpad"] == "pump.fun"
        assert event["source"] == "pumpportal"

    def test_parse_pumpportal_event_no_mint(self, collector):
        event = collector._parse_pumpportal_event({"name": "test"})
        assert event is None

    def test_parse_helius_event_pool_init(self, collector):
        data = {
            "result": {
                "value": {
                    "signature": "heliusSig123",
                    "logs": ["InitializeInstruction successful"],
                }
            }
        }
        event = collector._parse_helius_event(data)
        assert event is not None
        assert event["source"] == "helius"
        assert event["launchpad"] == "raydium"

    def test_parse_helius_event_not_pool_init(self, collector):
        data = {
            "result": {
                "value": {
                    "signature": "sig",
                    "logs": ["Transfer successful"],
                }
            }
        }
        event = collector._parse_helius_event(data)
        assert event is None


class TestEVMCollector:
    """Tests for EVMCollector event parsing."""

    @pytest.fixture
    def collector(self):
        bus = AsyncMock()
        wal = AsyncMock()
        return EVMCollector("base", bus, wal, {
            "base_ws_url": "wss://base-ws.example.com",
        })

    def test_parse_uniswap_v3_event(self, collector):
        raw = {
            "params": {
                "result": {
                    "topics": [
                        UNISWAP_V3_POOL_CREATED,
                        "0x000000000000000000000000" + "a" * 40,
                        "0x000000000000000000000000" + "b" * 40,
                    ],
                    "transactionHash": "0xtx123",
                    "blockNumber": "0xa",
                    "blockHash": "0xblock123",
                    "logIndex": "0x0",
                    "address": "0xpool123",
                }
            }
        }
        event = collector._parse_event(raw)
        assert event is not None
        assert event["dex"] == "uniswap_v3"
        assert event["chain"] == "base"
        assert event["block_number"] == "10"

    def test_parse_aerodrome_event(self, collector):
        raw = {
            "params": {
                "result": {
                    "topics": [
                        AERODROME_POOL_CREATED,
                        "0x000000000000000000000000" + "c" * 40,
                        "0x000000000000000000000000" + "d" * 40,
                    ],
                    "transactionHash": "0xtx456",
                    "blockNumber": "0x14",
                    "blockHash": "0xblock456",
                    "logIndex": "0x1",
                    "address": "0xpool456",
                }
            }
        }
        event = collector._parse_event(raw)
        assert event is not None
        assert event["dex"] == "aerodrome"

    def test_parse_unknown_topic(self, collector):
        raw = {
            "params": {
                "result": {
                    "topics": ["0xunknown"],
                    "transactionHash": "0x",
                    "blockNumber": "0x0",
                    "logIndex": "0x0",
                }
            }
        }
        event = collector._parse_event(raw)
        assert event is None

    def test_parse_no_topics(self, collector):
        raw = {"params": {"result": {}}}
        event = collector._parse_event(raw)
        assert event is None
