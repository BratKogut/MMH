"""Tests for idempotency utilities."""

import time
import pytest
from src.utils.idempotency import generate_intent_id, generate_dedup_key, hash_config


class TestGenerateIntentId:
    """Tests for deterministic intent ID generation."""

    def test_deterministic_same_input(self):
        ts = 1700000000.123456
        id1 = generate_intent_id("solana", "TokenMint123", "buy", timestamp=ts)
        id2 = generate_intent_id("solana", "TokenMint123", "buy", timestamp=ts)
        assert id1 == id2

    def test_different_inputs_different_ids(self):
        ts = 1700000000.0
        id1 = generate_intent_id("solana", "Token1", "buy", timestamp=ts)
        id2 = generate_intent_id("solana", "Token2", "buy", timestamp=ts)
        assert id1 != id2

    def test_uuid_format(self):
        intent_id = generate_intent_id("solana", "Token", "buy", timestamp=1700000000.0)
        # UUID format: xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
        parts = intent_id.split("-")
        assert len(parts) == 5
        assert len(parts[0]) == 8
        assert len(parts[1]) == 4
        assert len(parts[2]) == 4
        assert len(parts[3]) == 4
        assert len(parts[4]) == 12

    def test_different_chains(self):
        ts = 1700000000.0
        sol_id = generate_intent_id("solana", "Token", "buy", timestamp=ts)
        base_id = generate_intent_id("base", "Token", "buy", timestamp=ts)
        assert sol_id != base_id

    def test_different_sides(self):
        ts = 1700000000.0
        buy_id = generate_intent_id("solana", "Token", "buy", timestamp=ts)
        sell_id = generate_intent_id("solana", "Token", "sell", timestamp=ts)
        assert buy_id != sell_id

    def test_default_timestamp(self):
        """Should use current time if no timestamp provided."""
        id1 = generate_intent_id("solana", "Token", "buy")
        assert isinstance(id1, str)
        assert len(id1) == 36  # standard UUID length


class TestGenerateDedupKey:
    """Tests for blockchain event dedup key generation."""

    def test_deterministic(self):
        k1 = generate_dedup_key("solana", "tx_abc123", 0)
        k2 = generate_dedup_key("solana", "tx_abc123", 0)
        assert k1 == k2

    def test_different_tx_hash(self):
        k1 = generate_dedup_key("solana", "tx_abc", 0)
        k2 = generate_dedup_key("solana", "tx_xyz", 0)
        assert k1 != k2

    def test_different_log_index(self):
        k1 = generate_dedup_key("base", "0xabc", 0)
        k2 = generate_dedup_key("base", "0xabc", 1)
        assert k1 != k2

    def test_hex_format(self):
        key = generate_dedup_key("solana", "tx", 0)
        assert len(key) == 64
        assert all(c in "0123456789abcdef" for c in key)


class TestHashConfig:
    """Tests for config hash utility."""

    def test_deterministic(self):
        config = {"a": 1, "b": "hello", "c": [1, 2, 3]}
        h1 = hash_config(config)
        h2 = hash_config(config)
        assert h1 == h2

    def test_order_independent(self):
        h1 = hash_config({"a": 1, "b": 2})
        h2 = hash_config({"b": 2, "a": 1})
        assert h1 == h2

    def test_different_configs(self):
        h1 = hash_config({"a": 1})
        h2 = hash_config({"a": 2})
        assert h1 != h2

    def test_hex_format(self):
        h = hash_config({"test": True})
        assert len(h) == 64
        assert all(c in "0123456789abcdef" for c in h)
