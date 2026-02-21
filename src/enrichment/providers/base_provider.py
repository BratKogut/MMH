"""Base provider interface for security and enrichment data."""

from abc import ABC, abstractmethod
from typing import Optional
import logging
import time

logger = logging.getLogger(__name__)


class ProviderResult:
    """Result from an enrichment/security provider."""
    def __init__(self, provider_name: str, data: dict, is_safe: Optional[bool] = None,
                 risk_level: str = "UNKNOWN", flags: list[str] = None,
                 cached: bool = False, latency_ms: float = 0):
        self.provider_name = provider_name
        self.data = data
        self.is_safe = is_safe
        self.risk_level = risk_level  # LOW | MEDIUM | HIGH | CRITICAL
        self.flags = flags or []
        self.cached = cached
        self.latency_ms = latency_ms
        self.timestamp = time.time()


class BaseSecurityProvider(ABC):
    """Base class for security check providers.

    All providers must:
    1. Implement check() method
    2. Return ProviderResult
    3. Handle their own rate limiting
    4. Handle their own errors gracefully
    """

    def __init__(self, name: str, rate_limit_per_sec: float = 1.0):
        self.name = name
        self._rate_limit = rate_limit_per_sec
        self._last_call_time = 0.0
        self._call_count = 0
        self._error_count = 0

    @abstractmethod
    async def check(self, chain: str, token_address: str) -> ProviderResult:
        """Perform security check. Must be implemented by providers."""
        pass

    async def _rate_limit_wait(self):
        """Wait if needed to respect rate limit."""
        if self._rate_limit <= 0:
            return
        min_interval = 1.0 / self._rate_limit
        elapsed = time.time() - self._last_call_time
        if elapsed < min_interval:
            import asyncio
            await asyncio.sleep(min_interval - elapsed)
        self._last_call_time = time.time()

    def get_stats(self) -> dict:
        return {
            "provider": self.name,
            "calls": self._call_count,
            "errors": self._error_count,
        }
