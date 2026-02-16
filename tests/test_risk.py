"""Tests for RiskFabric - risk invariant property tests.

INVARIANTS (must ALWAYS hold):
- Max exposure never exceeded
- Max positions per chain never exceeded
- Max single position never exceeded
- Daily loss limit triggers FREEZE
- No duplicate positions
- Frozen chain blocks all trades
"""
import pytest
import json
from unittest.mock import AsyncMock, MagicMock

from src.risk.risk_fabric import RiskFabric, RiskContext, RiskDecisionType


class TestRiskInvariants:
    """Property tests for risk invariants."""

    def setup_method(self):
        self.config = {
            "max_portfolio_exposure_usd": 500,
            "max_positions_per_chain": 5,
            "max_single_position_pct": 20,
            "daily_loss_limit_usd": 200,
        }
        self.bus = AsyncMock()
        self.bus.publish = AsyncMock()
        self.bus.ensure_consumer_group = AsyncMock()
        self.redis = AsyncMock()
        self.redis.get = AsyncMock(return_value=None)
        self.fabric = RiskFabric(self.bus, self.redis, self.config)

    @pytest.mark.asyncio
    async def test_max_exposure_blocks_trade(self):
        """Over max exposure -> REJECT."""
        intent = {
            "intent_id": "test-1",
            "chain": "solana",
            "token_address": "token1",
            "side": "BUY",
            "amount_usd": "100",
        }
        # Set existing positions at near-max
        self.redis.get = AsyncMock(side_effect=lambda k: json.dumps([
            {"chain": "solana", "token_address": "t1", "status": "OPEN", "exposure_usd": 450}
        ]) if k == "positions:snapshot" else "0")

        decision = await self.fabric.evaluate(intent)
        assert decision["decision"] in ("REJECT", "FREEZE")

    @pytest.mark.asyncio
    async def test_max_positions_per_chain_blocks(self):
        """Max positions per chain -> REJECT."""
        intent = {
            "intent_id": "test-2",
            "chain": "solana",
            "token_address": "new_token",
            "side": "BUY",
            "amount_usd": "10",
        }
        # 5 existing positions on solana (max is 5)
        positions = [
            {"chain": "solana", "token_address": f"t{i}", "status": "OPEN", "exposure_usd": 10}
            for i in range(5)
        ]
        self.redis.get = AsyncMock(side_effect=lambda k: json.dumps(positions) if k == "positions:snapshot" else "0")

        decision = await self.fabric.evaluate(intent)
        assert decision["decision"] in ("REJECT", "FREEZE")

    @pytest.mark.asyncio
    async def test_daily_loss_limit_freezes(self):
        """Daily loss exceeds limit -> FREEZE."""
        intent = {
            "intent_id": "test-3",
            "chain": "solana",
            "token_address": "token1",
            "side": "BUY",
            "amount_usd": "10",
        }
        # Daily PnL at -250 (limit is -200)
        self.redis.get = AsyncMock(side_effect=lambda k: json.dumps([]) if k == "positions:snapshot" else "-250")

        decision = await self.fabric.evaluate(intent)
        assert decision["decision"] == "FREEZE"

    @pytest.mark.asyncio
    async def test_no_duplicate_positions(self):
        """Buying token already in portfolio -> REJECT."""
        intent = {
            "intent_id": "test-4",
            "chain": "solana",
            "token_address": "existing_token",
            "side": "BUY",
            "amount_usd": "10",
        }
        positions = [
            {"chain": "solana", "token_address": "existing_token", "status": "OPEN", "exposure_usd": 50}
        ]
        self.redis.get = AsyncMock(side_effect=lambda k: json.dumps(positions) if k == "positions:snapshot" else "0")

        decision = await self.fabric.evaluate(intent)
        assert decision["decision"] in ("REJECT", "FREEZE")

    @pytest.mark.asyncio
    async def test_frozen_chain_blocks_trade(self):
        """Trading on frozen chain -> REJECT."""
        await self.fabric.freeze_chain("solana", "test_freeze")

        intent = {
            "intent_id": "test-5",
            "chain": "solana",
            "token_address": "token1",
            "side": "BUY",
            "amount_usd": "10",
        }
        self.redis.get = AsyncMock(side_effect=lambda k: json.dumps([]) if k == "positions:snapshot" else "0")

        decision = await self.fabric.evaluate(intent)
        assert decision["decision"] in ("REJECT", "FREEZE")

    @pytest.mark.asyncio
    async def test_valid_trade_approved(self):
        """Valid trade within all limits -> APPROVE."""
        intent = {
            "intent_id": "test-6",
            "chain": "solana",
            "token_address": "new_token",
            "side": "BUY",
            "amount_usd": "50",
        }
        self.redis.get = AsyncMock(side_effect=lambda k: json.dumps([]) if k == "positions:snapshot" else "0")

        decision = await self.fabric.evaluate(intent)
        assert decision["decision"] == "APPROVE"
