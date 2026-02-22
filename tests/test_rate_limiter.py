"""Tests for rate limiter module."""

import asyncio
import time
import pytest

from src.utils.rate_limiter import AsyncRateLimiter, RateLimiterRegistry


class TestAsyncRateLimiter:
    """Tests for AsyncRateLimiter token bucket."""

    @pytest.mark.asyncio
    async def test_immediate_acquire_with_burst(self):
        limiter = AsyncRateLimiter("test", rate=1.0, burst=5)
        start = time.monotonic()
        await limiter.acquire()
        elapsed = time.monotonic() - start
        assert elapsed < 0.1

    @pytest.mark.asyncio
    async def test_blocks_when_no_tokens(self):
        limiter = AsyncRateLimiter("test", rate=10.0, burst=1)
        await limiter.acquire()  # use the one token
        start = time.monotonic()
        await limiter.acquire()  # should wait ~0.1s
        elapsed = time.monotonic() - start
        assert elapsed >= 0.05

    @pytest.mark.asyncio
    async def test_try_acquire_success(self):
        limiter = AsyncRateLimiter("test", rate=10.0, burst=5)
        assert await limiter.try_acquire() is True
        assert limiter.total_acquired == 1

    @pytest.mark.asyncio
    async def test_try_acquire_failure(self):
        limiter = AsyncRateLimiter("test", rate=0.1, burst=1)
        await limiter.acquire()  # use the token
        assert await limiter.try_acquire() is False

    @pytest.mark.asyncio
    async def test_context_manager(self):
        limiter = AsyncRateLimiter("test", rate=10.0, burst=5)
        async with limiter:
            pass
        assert limiter.total_acquired == 1

    def test_invalid_rate(self):
        with pytest.raises(ValueError, match="rate must be > 0"):
            AsyncRateLimiter("test", rate=0)

    @pytest.mark.asyncio
    async def test_invalid_tokens(self):
        limiter = AsyncRateLimiter("test", rate=1.0)
        with pytest.raises(ValueError, match="tokens must be >= 1"):
            await limiter.acquire(tokens=0)

    def test_available_tokens(self):
        limiter = AsyncRateLimiter("test", rate=10.0, burst=10)
        assert limiter.available_tokens > 0

    def test_repr(self):
        limiter = AsyncRateLimiter("test", rate=5.0, burst=10)
        r = repr(limiter)
        assert "test" in r
        assert "rate=5.0" in r


class TestRateLimiterRegistry:
    """Tests for RateLimiterRegistry singleton."""

    def test_register_and_get(self):
        registry = RateLimiterRegistry()
        registry.reset()
        limiter = registry.register("birdeye", rate=1.0)
        assert registry.get("birdeye") is limiter

    def test_get_nonexistent_raises(self):
        registry = RateLimiterRegistry()
        registry.reset()
        with pytest.raises(KeyError, match="No rate limiter registered"):
            registry.get("nonexistent")

    def test_get_or_create(self):
        registry = RateLimiterRegistry()
        registry.reset()
        l1 = registry.get_or_create("test", rate=1.0)
        l2 = registry.get_or_create("test", rate=1.0)
        assert l1 is l2

    def test_all_limiters(self):
        registry = RateLimiterRegistry()
        registry.reset()
        registry.register("a", rate=1.0)
        registry.register("b", rate=2.0)
        assert len(registry.all_limiters()) == 2

    def test_reset(self):
        registry = RateLimiterRegistry()
        registry.register("test", rate=1.0)
        registry.reset()
        assert len(registry.all_limiters()) == 0
