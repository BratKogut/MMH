"""Tests for circuit breaker module."""

import asyncio
import time
import pytest
from unittest.mock import patch

from src.utils.circuit_breaker import (
    CircuitBreaker,
    CircuitBreakerOpen,
    CircuitBreakerRegistry,
    CircuitState,
    circuit_breaker,
)


class TestCircuitBreaker:
    """Tests for CircuitBreaker state machine."""

    @pytest.mark.asyncio
    async def test_initial_state_closed(self):
        cb = CircuitBreaker("test", threshold=3, timeout=1.0)
        assert cb.state == CircuitState.CLOSED

    @pytest.mark.asyncio
    async def test_stays_closed_under_threshold(self):
        cb = CircuitBreaker("test", threshold=3, timeout=1.0)
        await cb.on_failure(Exception("e1"))
        await cb.on_failure(Exception("e2"))
        assert cb.state == CircuitState.CLOSED
        assert cb.failure_count == 2

    @pytest.mark.asyncio
    async def test_opens_at_threshold(self):
        cb = CircuitBreaker("test", threshold=3, timeout=60.0)
        for i in range(3):
            await cb.on_failure(Exception(f"e{i}"))
        assert cb.state == CircuitState.OPEN

    @pytest.mark.asyncio
    async def test_rejects_when_open(self):
        cb = CircuitBreaker("test", threshold=1, timeout=60.0)
        await cb.on_failure(Exception("fail"))
        with pytest.raises(CircuitBreakerOpen):
            await cb.before_call()
        assert cb.total_rejections == 1

    @pytest.mark.asyncio
    async def test_transitions_to_half_open_after_timeout(self):
        cb = CircuitBreaker("test", threshold=1, timeout=0.1)
        await cb.on_failure(Exception("fail"))
        assert cb.state == CircuitState.OPEN
        await asyncio.sleep(0.15)
        assert cb.state == CircuitState.HALF_OPEN

    @pytest.mark.asyncio
    async def test_recovers_on_success(self):
        cb = CircuitBreaker("test", threshold=1, timeout=0.1)
        await cb.on_failure(Exception("fail"))
        await asyncio.sleep(0.15)
        await cb.on_success()
        assert cb.state == CircuitState.CLOSED
        assert cb.failure_count == 0

    @pytest.mark.asyncio
    async def test_context_manager_success(self):
        cb = CircuitBreaker("test", threshold=3, timeout=1.0)
        async with cb:
            pass
        assert cb.state == CircuitState.CLOSED
        assert cb.total_calls == 1

    @pytest.mark.asyncio
    async def test_context_manager_failure(self):
        cb = CircuitBreaker("test", threshold=3, timeout=1.0)
        with pytest.raises(ValueError):
            async with cb:
                raise ValueError("test error")
        assert cb.failure_count == 1

    def test_repr(self):
        cb = CircuitBreaker("test", threshold=5, timeout=60.0)
        r = repr(cb)
        assert "test" in r
        assert "CLOSED" in r


class TestCircuitBreakerRegistry:
    """Tests for CircuitBreakerRegistry singleton."""

    def test_get_or_create(self):
        registry = CircuitBreakerRegistry()
        cb1 = registry.get_or_create("birdeye", threshold=5)
        cb2 = registry.get_or_create("birdeye", threshold=5)
        assert cb1 is cb2

    def test_get_nonexistent(self):
        registry = CircuitBreakerRegistry()
        registry.reset()
        assert registry.get("nonexistent") is None

    def test_all_breakers(self):
        registry = CircuitBreakerRegistry()
        registry.reset()
        registry.get_or_create("a")
        registry.get_or_create("b")
        assert len(registry.all_breakers()) == 2

    def test_reset(self):
        registry = CircuitBreakerRegistry()
        registry.get_or_create("test")
        registry.reset()
        assert len(registry.all_breakers()) == 0


class TestCircuitBreakerDecorator:
    """Tests for @circuit_breaker decorator."""

    @pytest.mark.asyncio
    async def test_decorator_success(self):
        @circuit_breaker("test_dec", threshold=3, timeout=1.0)
        async def my_func():
            return 42

        result = await my_func()
        assert result == 42

    @pytest.mark.asyncio
    async def test_decorator_failure_propagates(self):
        @circuit_breaker("test_dec_fail", threshold=3, timeout=1.0)
        async def failing_func():
            raise ValueError("boom")

        with pytest.raises(ValueError, match="boom"):
            await failing_func()
