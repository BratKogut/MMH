"""
MMH v3.1 -- Async circuit breaker.

Protects downstream services from cascading failures.  Three states:

    CLOSED   -> normal operation; failures are counted.
    OPEN     -> all calls are rejected immediately (fail-fast).
    HALF_OPEN -> a single probe call is allowed through.

State transitions
-----------------
    CLOSED  --[failure_count >= threshold]--> OPEN
    OPEN    --[timeout elapsed]------------>  HALF_OPEN
    HALF_OPEN --[success]------------------>  CLOSED
    HALF_OPEN --[failure]------------------> OPEN

Usage as a decorator::

    from src.utils.circuit_breaker import circuit_breaker

    @circuit_breaker(name="birdeye", threshold=5, timeout=60.0)
    async def fetch_token_data(mint: str) -> dict:
        ...

Or as a context-managed guard::

    breaker = CircuitBreaker("helius", threshold=3, timeout=30.0)
    async with breaker:
        result = await helius_client.get(...)
"""

from __future__ import annotations

import asyncio
import enum
import functools
import logging
import time
from typing import Any, Callable, Coroutine, TypeVar

logger = logging.getLogger(__name__)

F = TypeVar("F", bound=Callable[..., Coroutine[Any, Any, Any]])


# --------------------------------------------------------------------------- #
# Exceptions
# --------------------------------------------------------------------------- #


class CircuitBreakerOpen(Exception):
    """Raised when the circuit is OPEN and calls are rejected."""

    def __init__(self, name: str, remaining_seconds: float) -> None:
        self.name = name
        self.remaining_seconds = remaining_seconds
        super().__init__(
            f"Circuit breaker '{name}' is OPEN; "
            f"retry in {remaining_seconds:.1f}s"
        )


# --------------------------------------------------------------------------- #
# State enum
# --------------------------------------------------------------------------- #


class CircuitState(str, enum.Enum):
    CLOSED = "CLOSED"
    OPEN = "OPEN"
    HALF_OPEN = "HALF_OPEN"


# --------------------------------------------------------------------------- #
# Core implementation
# --------------------------------------------------------------------------- #


class CircuitBreaker:
    """Async-safe circuit breaker with metric/log emission on transitions."""

    def __init__(
        self,
        name: str,
        threshold: int = 5,
        timeout: float = 60.0,
    ) -> None:
        self.name = name
        self.threshold = threshold
        self.timeout = timeout

        self._state: CircuitState = CircuitState.CLOSED
        self._failure_count: int = 0
        self._last_failure_time: float = 0.0
        self._lock = asyncio.Lock()

        # Counters for observability (can be scraped by Prometheus)
        self.total_calls: int = 0
        self.total_failures: int = 0
        self.total_rejections: int = 0
        self.total_state_transitions: int = 0

    # -- properties --------------------------------------------------------- #

    @property
    def state(self) -> CircuitState:
        """Return the *effective* state (may auto-transition OPEN -> HALF_OPEN)."""
        if self._state is CircuitState.OPEN:
            elapsed = time.monotonic() - self._last_failure_time
            if elapsed >= self.timeout:
                return CircuitState.HALF_OPEN
        return self._state

    @property
    def failure_count(self) -> int:
        return self._failure_count

    # -- helpers ------------------------------------------------------------ #

    def _transition(self, new_state: CircuitState) -> None:
        old = self._state
        if old is new_state:
            return
        self._state = new_state
        self.total_state_transitions += 1
        logger.warning(
            "circuit_breaker.state_change",
            extra={
                "breaker": self.name,
                "from_state": old.value,
                "to_state": new_state.value,
                "failure_count": self._failure_count,
            },
        )

    # -- public API --------------------------------------------------------- #

    async def before_call(self) -> None:
        """Gate check before executing the protected call."""
        async with self._lock:
            effective = self.state
            if effective is CircuitState.OPEN:
                remaining = self.timeout - (
                    time.monotonic() - self._last_failure_time
                )
                self.total_rejections += 1
                raise CircuitBreakerOpen(self.name, max(remaining, 0.0))

            if effective is CircuitState.HALF_OPEN:
                # Allow exactly one probe call -- the lock serialises access.
                self._transition(CircuitState.HALF_OPEN)

    async def on_success(self) -> None:
        """Record a successful call."""
        async with self._lock:
            if self._state in (CircuitState.HALF_OPEN, CircuitState.OPEN):
                # Recovery confirmed
                logger.info(
                    "circuit_breaker.recovered",
                    extra={"breaker": self.name},
                )
            self._failure_count = 0
            self._transition(CircuitState.CLOSED)

    async def on_failure(self, exc: BaseException | None = None) -> None:
        """Record a failed call."""
        async with self._lock:
            self._failure_count += 1
            self.total_failures += 1
            self._last_failure_time = time.monotonic()
            logger.warning(
                "circuit_breaker.failure",
                extra={
                    "breaker": self.name,
                    "failure_count": self._failure_count,
                    "threshold": self.threshold,
                    "error": str(exc) if exc else None,
                },
            )
            if self._failure_count >= self.threshold:
                self._transition(CircuitState.OPEN)

    # -- context manager ---------------------------------------------------- #

    async def __aenter__(self) -> CircuitBreaker:
        await self.before_call()
        self.total_calls += 1
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc_val: BaseException | None,
        exc_tb: Any,
    ) -> bool:
        if exc_type is None:
            await self.on_success()
        else:
            await self.on_failure(exc_val)
        # Never swallow the exception
        return False

    # -- repr --------------------------------------------------------------- #

    def __repr__(self) -> str:
        return (
            f"CircuitBreaker(name={self.name!r}, state={self.state.value}, "
            f"failures={self._failure_count}/{self.threshold})"
        )


# --------------------------------------------------------------------------- #
# Global registry
# --------------------------------------------------------------------------- #


class CircuitBreakerRegistry:
    """Singleton registry so every subsystem shares the same breaker instances."""

    _instance: CircuitBreakerRegistry | None = None
    _lock: asyncio.Lock | None = None

    def __init__(self) -> None:
        self._breakers: dict[str, CircuitBreaker] = {}

    @classmethod
    def get_instance(cls) -> CircuitBreakerRegistry:
        if cls._instance is None:
            cls._instance = cls()
        return cls._instance

    def get_or_create(
        self,
        name: str,
        threshold: int = 5,
        timeout: float = 60.0,
    ) -> CircuitBreaker:
        if name not in self._breakers:
            self._breakers[name] = CircuitBreaker(
                name=name,
                threshold=threshold,
                timeout=timeout,
            )
        return self._breakers[name]

    def get(self, name: str) -> CircuitBreaker | None:
        return self._breakers.get(name)

    def all_breakers(self) -> dict[str, CircuitBreaker]:
        return dict(self._breakers)

    def reset(self) -> None:
        """Clear all breakers (useful in tests)."""
        self._breakers.clear()


# --------------------------------------------------------------------------- #
# Decorator
# --------------------------------------------------------------------------- #


def circuit_breaker(
    name: str,
    threshold: int = 5,
    timeout: float = 60.0,
) -> Callable[[F], F]:
    """Decorator that wraps an async function with a named circuit breaker.

    Example::

        @circuit_breaker("birdeye", threshold=3, timeout=30.0)
        async def get_price(mint: str) -> float:
            ...
    """
    registry = CircuitBreakerRegistry.get_instance()
    breaker = registry.get_or_create(name, threshold=threshold, timeout=timeout)

    def decorator(fn: F) -> F:
        @functools.wraps(fn)
        async def wrapper(*args: Any, **kwargs: Any) -> Any:
            async with breaker:
                return await fn(*args, **kwargs)

        return wrapper  # type: ignore[return-value]

    return decorator
