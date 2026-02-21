"""MMH v3.1 -- Shared utility modules."""

from src.utils.circuit_breaker import (
    CircuitBreaker,
    CircuitBreakerOpen,
    CircuitBreakerRegistry,
    CircuitState,
    circuit_breaker,
)
from src.utils.idempotency import generate_dedup_key, generate_intent_id, hash_config
from src.utils.rate_limiter import AsyncRateLimiter, RateLimiterRegistry

__all__ = [
    # circuit breaker
    "CircuitBreaker",
    "CircuitBreakerOpen",
    "CircuitBreakerRegistry",
    "CircuitState",
    "circuit_breaker",
    # rate limiter
    "AsyncRateLimiter",
    "RateLimiterRegistry",
    # idempotency
    "generate_intent_id",
    "generate_dedup_key",
    "hash_config",
]
