"""Tests for L0 Sanitizer."""
import pytest
import time

from src.sanitizer.l0_sanitizer import (
    L0Sanitizer, DedupStore, GapDetector, FatFingerFilter, RejectReason
)


class TestFatFingerFilter:
    def setup_method(self):
        self.filter = FatFingerFilter()

    def test_valid_event_passes(self):
        event = {
            "chain": "solana",
            "event_type": "TokenDiscovered",
            "event_id": "test-123",
            "address": "So11111111111111111111111111111111111111112",
            "event_time": str(time.time()),
        }
        reasons = self.filter.check(event)
        assert len(reasons) == 0

    def test_invalid_chain_rejected(self):
        event = {
            "chain": "invalid_chain",
            "event_type": "TokenDiscovered",
            "event_id": "test-123",
        }
        reasons = self.filter.check(event)
        assert RejectReason.INVALID_CHAIN in reasons

    def test_short_address_rejected(self):
        event = {
            "chain": "solana",
            "event_type": "TokenDiscovered",
            "event_id": "test-123",
            "address": "short",
            "event_time": str(time.time()),
        }
        reasons = self.filter.check(event)
        assert RejectReason.INVALID_ADDRESS in reasons

    def test_fat_finger_liquidity_rejected(self):
        event = {
            "chain": "base",
            "event_type": "TokenDiscovered",
            "event_id": "test-123",
            "address": "0x" + "a" * 40,
            "initial_liquidity_usd": 999999999999,
            "event_time": str(time.time()),
        }
        reasons = self.filter.check(event)
        assert RejectReason.FAT_FINGER_LIQUIDITY in reasons

    def test_stale_event_rejected(self):
        event = {
            "chain": "solana",
            "event_type": "TokenDiscovered",
            "event_id": "test-123",
            "address": "a" * 32,
            "event_time": str(time.time() - 600),  # 10 min old
        }
        reasons = self.filter.check(event)
        assert RejectReason.STALE_EVENT in reasons

    def test_future_event_rejected(self):
        event = {
            "chain": "solana",
            "event_type": "TokenDiscovered",
            "event_id": "test-123",
            "address": "a" * 32,
            "event_time": str(time.time() + 60),  # 60s in future
        }
        reasons = self.filter.check(event)
        assert RejectReason.FUTURE_TIMESTAMP in reasons


class TestGapDetector:
    def setup_method(self):
        self.detector = GapDetector()

    def test_sequential_no_gap(self):
        assert self.detector.check("solana", 1) is None
        assert self.detector.check("solana", 2) is None
        assert self.detector.check("solana", 3) is None

    def test_gap_detected(self):
        self.detector.check("solana", 1)
        gap = self.detector.check("solana", 5)
        assert gap == 3

    def test_independent_chains(self):
        self.detector.check("solana", 1)
        assert self.detector.check("base", 100) is None  # first for base
        assert self.detector.check("solana", 2) is None

    def test_gap_counter(self):
        self.detector.check("solana", 1)
        self.detector.check("solana", 5)
        self.detector.check("solana", 10)
        assert self.detector.get_gap_count("solana") == 7  # 3 + 4
