"""
MMH v3.1 -- Async token-bucket rate limiter.

Each provider (Birdeye, GoPlus, Helius, ...) gets its own named limiter
with a configurable steady-state rate and burst capacity.

Usage::

    from src.utils.rate_limiter import RateLimiterRegistry

    registry = RateLimiterRegistry.get_instance()
    registry.register("birdeye", rate=1.0, burst=3)

    limiter = registry.get("birdeye")
    await limiter.acquire()          # blocks until a token is available
    result = await birdeye_client.get_price(mint)

Or inline::

    limiter = AsyncRateLimiter("helius", rate=10.0, burst=15)
    async with limiter:
        ...
"""

from __future__ import annotations

import asyncio
import logging
import time
from typing import Any

logger = logging.getLogger(__name__)


class AsyncRateLimiter:
    """Token-bucket rate limiter with async ``acquire()`` support.

    Parameters
    ----------
    name:
        Human-readable identifier (used in logs / metrics).
    rate:
        Sustained rate in calls per second.
    burst:
        Maximum number of tokens that can accumulate.  Defaults to
        ``max(1, int(rate))`` so that at least one call is always
        allowed immediately after an idle period.
    """

    def __init__(
        self,
        name: str,
        rate: float,
        burst: int | None = None,
    ) -> None:
        if rate <= 0:
            raise ValueError(f"rate must be > 0, got {rate}")

        self.name = name
        self.rate = rate
        self.burst = burst if burst is not None else max(1, int(rate))

        self._tokens: float = float(self.burst)
        self._last_refill: float = time.monotonic()
        self._lock = asyncio.Lock()

        # Observability counters
        self.total_acquired: int = 0
        self.total_waited: int = 0  # number of acquire() calls that had to wait

    # ------------------------------------------------------------------ #
    # Internal helpers
    # ------------------------------------------------------------------ #

    def _refill(self) -> None:
        """Add tokens based on elapsed time since last refill."""
        now = time.monotonic()
        elapsed = now - self._last_refill
        self._tokens = min(self.burst, self._tokens + elapsed * self.rate)
        self._last_refill = now

    # ------------------------------------------------------------------ #
    # Public API
    # ------------------------------------------------------------------ #

    async def acquire(self, tokens: int = 1) -> None:
        """Wait until *tokens* are available, then consume them.

        This method is fair: callers are serialised by the internal lock
        so they proceed in FIFO order.
        """
        if tokens < 1:
            raise ValueError(f"tokens must be >= 1, got {tokens}")

        async with self._lock:
            self._refill()

            if self._tokens >= tokens:
                # Fast path -- enough tokens available right now.
                self._tokens -= tokens
                self.total_acquired += 1
                return

            # Slow path -- wait for tokens to replenish.
            self.total_waited += 1
            deficit = tokens - self._tokens
            wait_seconds = deficit / self.rate

            logger.debug(
                "rate_limiter.waiting",
                extra={
                    "limiter": self.name,
                    "wait_seconds": round(wait_seconds, 4),
                    "deficit": round(deficit, 2),
                },
            )

            await asyncio.sleep(wait_seconds)
            self._refill()
            self._tokens -= tokens
            self.total_acquired += 1

    async def try_acquire(self, tokens: int = 1) -> bool:
        """Non-blocking variant -- returns False instead of waiting."""
        async with self._lock:
            self._refill()
            if self._tokens >= tokens:
                self._tokens -= tokens
                self.total_acquired += 1
                return True
            return False

    @property
    def available_tokens(self) -> float:
        """Snapshot of currently available tokens (no lock; approximate)."""
        elapsed = time.monotonic() - self._last_refill
        return min(self.burst, self._tokens + elapsed * self.rate)

    # ------------------------------------------------------------------ #
    # Context manager sugar
    # ------------------------------------------------------------------ #

    async def __aenter__(self) -> AsyncRateLimiter:
        await self.acquire()
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc_val: BaseException | None,
        exc_tb: Any,
    ) -> bool:
        return False

    # ------------------------------------------------------------------ #
    # repr
    # ------------------------------------------------------------------ #

    def __repr__(self) -> str:
        return (
            f"AsyncRateLimiter(name={self.name!r}, rate={self.rate}, "
            f"burst={self.burst}, tokens~{self.available_tokens:.1f})"
        )


# ---------------------------------------------------------------------- #
# Registry (singleton)
# ---------------------------------------------------------------------- #


class RateLimiterRegistry:
    """Global registry of named rate limiters.

    Guarantees that each provider gets exactly one limiter instance across
    the entire process.
    """

    _instance: RateLimiterRegistry | None = None

    def __init__(self) -> None:
        self._limiters: dict[str, AsyncRateLimiter] = {}

    @classmethod
    def get_instance(cls) -> RateLimiterRegistry:
        if cls._instance is None:
            cls._instance = cls()
        return cls._instance

    # ------------------------------------------------------------------ #
    # Limiter management
    # ------------------------------------------------------------------ #

    def register(
        self,
        name: str,
        rate: float,
        burst: int | None = None,
    ) -> AsyncRateLimiter:
        """Create (or return existing) limiter for *name*."""
        if name not in self._limiters:
            self._limiters[name] = AsyncRateLimiter(
                name=name,
                rate=rate,
                burst=burst,
            )
            logger.info(
                "rate_limiter.registered",
                extra={"limiter": name, "rate": rate, "burst": burst},
            )
        return self._limiters[name]

    def get(self, name: str) -> AsyncRateLimiter:
        """Retrieve a limiter by name.  Raises ``KeyError`` if not found."""
        try:
            return self._limiters[name]
        except KeyError:
            raise KeyError(
                f"No rate limiter registered for '{name}'. "
                f"Available: {list(self._limiters)}"
            ) from None

    def get_or_create(
        self,
        name: str,
        rate: float,
        burst: int | None = None,
    ) -> AsyncRateLimiter:
        """Convenience shortcut: return existing or register new."""
        return self.register(name, rate, burst)

    def all_limiters(self) -> dict[str, AsyncRateLimiter]:
        """Return a shallow copy of all registered limiters."""
        return dict(self._limiters)

    def reset(self) -> None:
        """Clear all limiters (useful in tests)."""
        self._limiters.clear()
