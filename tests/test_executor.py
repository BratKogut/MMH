"""Tests for Executor - idempotency is critical."""
import pytest
import json
from unittest.mock import AsyncMock

from src.executor.executor import Executor, ExecutionStatus


class TestExecutorIdempotency:
    """Executor NEVER executes same intent_id twice."""

    def setup_method(self):
        self.bus = AsyncMock()
        self.bus.publish = AsyncMock()
        self.bus.ensure_consumer_group = AsyncMock()
        self.redis = AsyncMock()
        self.config = {
            "circuit_breaker_threshold": 3,
            "circuit_breaker_timeout_seconds": 120,
            "max_execution_retries": 2,
        }

    @pytest.mark.asyncio
    async def test_duplicate_intent_blocked(self):
        """Same intent_id must not execute twice."""
        # Setup: intent already executed
        self.redis.get = AsyncMock(return_value='{"status": "CONFIRMED"}')

        executor = Executor(
            bus=self.bus,
            redis_client=self.redis,
            config=self.config,
            dry_run=True,
        )

        intent = {
            "intent_id": "already-executed-intent",
            "chain": "solana",
            "token_address": "token1",
            "side": "BUY",
        }

        await executor._process_intent(intent)
        assert executor._duplicates_prevented == 1

    @pytest.mark.asyncio
    async def test_dry_run_generates_events(self):
        """Dry-run mode generates events without real TX."""
        self.redis.get = AsyncMock(return_value=None)
        self.redis.set = AsyncMock()

        executor = Executor(
            bus=self.bus,
            redis_client=self.redis,
            config=self.config,
            dry_run=True,
        )

        intent = {
            "intent_id": "new-intent-1",
            "chain": "solana",
            "token_address": "token1",
            "side": "BUY",
        }

        await executor._process_intent(intent)
        assert executor._dry_runs == 1
        # Should have published events
        assert self.bus.publish.called

    @pytest.mark.asyncio
    async def test_circuit_breaker_blocks_after_failures(self):
        """Circuit breaker opens after threshold failures."""
        from src.executor.executor import ExecutorCircuitBreaker

        cb = ExecutorCircuitBreaker(threshold=3, timeout_seconds=120)
        assert not cb.is_open()

        cb.record_failure()
        cb.record_failure()
        assert not cb.is_open()

        cb.record_failure()  # 3rd failure
        assert cb.is_open()
        assert cb.state == "OPEN"

    @pytest.mark.asyncio
    async def test_success_resets_circuit_breaker(self):
        from src.executor.executor import ExecutorCircuitBreaker

        cb = ExecutorCircuitBreaker(threshold=2)
        cb.record_failure()
        cb.record_failure()
        assert cb.is_open()

        cb.record_success()
        assert not cb.is_open()
        assert cb.state == "CLOSED"
