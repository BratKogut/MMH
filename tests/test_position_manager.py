"""Tests for Position Manager."""
import pytest
import time

from src.position.position_manager import Position, PositionStatus


class TestPosition:
    def test_create_position(self):
        pos = Position("pos-1", "solana", "token1")
        assert pos.status == PositionStatus.OPEN
        assert pos.pnl_usd == 0

    def test_pnl_calculation(self):
        pos = Position("pos-1", "solana", "token1")
        pos.entry_price = 1.0
        pos.entry_amount = 100
        pos.remaining_amount = 100

        pos.update_price(1.5)
        assert pos.pnl_pct == pytest.approx(50.0)
        assert pos.pnl_usd == pytest.approx(50.0)

    def test_stop_loss_trigger(self):
        pos = Position("pos-1", "solana", "token1")
        pos.entry_price = 1.0
        pos.entry_amount = 100
        pos.remaining_amount = 100
        pos.stop_loss_pct = -30.0

        pos.update_price(0.65)
        reason = pos.check_exit_conditions()
        assert reason is not None
        assert "STOP_LOSS" in reason

    def test_take_profit_trigger(self):
        pos = Position("pos-1", "solana", "token1")
        pos.entry_price = 1.0
        pos.entry_amount = 100
        pos.remaining_amount = 100
        pos.take_profit_levels = [50, 100, 200]
        pos.entry_timestamp = time.time()  # set to now so max_holding doesn't trigger

        pos.update_price(1.6)  # +60%
        reason = pos.check_exit_conditions()
        assert reason is not None
        assert "TAKE_PROFIT:50%" in reason

    def test_max_holding_time_trigger(self):
        pos = Position("pos-1", "solana", "token1")
        pos.entry_price = 1.0
        pos.entry_amount = 100
        pos.remaining_amount = 100
        pos.entry_timestamp = time.time() - 7200  # 2 hours ago
        pos.max_holding_seconds = 3600  # 1 hour max

        reason = pos.check_exit_conditions()
        assert reason is not None
        assert "MAX_HOLDING_TIME" in reason

    def test_closed_position_no_trigger(self):
        pos = Position("pos-1", "solana", "token1")
        pos.status = PositionStatus.CLOSED
        pos.entry_price = 1.0
        pos.update_price(0.1)  # massive loss

        reason = pos.check_exit_conditions()
        assert reason is None

    def test_to_dict_complete(self):
        pos = Position("pos-1", "solana", "token1", "TEST", "intent-1")
        d = pos.to_dict()
        assert d["id"] == "pos-1"
        assert d["chain"] == "solana"
        assert d["status"] == "OPEN"
