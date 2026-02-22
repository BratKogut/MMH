"""Birdeye Security Provider — Solana token security data.

API: https://public-api.birdeye.so
Provides: holder distribution, LP status, creator history, trading data.
"""

import logging
import time
from typing import Optional

import aiohttp

from .base_provider import BaseSecurityProvider, ProviderResult

logger = logging.getLogger(__name__)

BIRDEYE_BASE_URL = "https://public-api.birdeye.so"


class BirdeyeProvider(BaseSecurityProvider):
    """Birdeye API provider for Solana token security checks."""

    def __init__(self, api_key: str, rate_limit_per_sec: float = 1.0):
        super().__init__("birdeye", rate_limit_per_sec)
        self._api_key = api_key
        self._session: Optional[aiohttp.ClientSession] = None

    async def _get_session(self) -> aiohttp.ClientSession:
        if self._session is None or self._session.closed:
            self._session = aiohttp.ClientSession(
                headers={
                    "X-API-KEY": self._api_key,
                    "Accept": "application/json",
                }
            )
        return self._session

    async def check(self, chain: str, token_address: str) -> ProviderResult:
        """Perform security check via Birdeye API."""
        start = time.time()
        self._call_count += 1

        try:
            await self._rate_limit_wait()

            session = await self._get_session()
            security_data = await self._fetch_token_security(session, token_address)
            overview_data = await self._fetch_token_overview(session, token_address)

            merged = {**security_data, **overview_data}
            flags = self._analyze_flags(merged)
            is_safe = len([f for f in flags if "CRITICAL" in f or "HIGH" in f]) == 0
            risk_level = self._determine_risk_level(flags)

            latency = (time.time() - start) * 1000

            return ProviderResult(
                provider_name=self.name,
                data={"security": merged},
                is_safe=is_safe,
                risk_level=risk_level,
                flags=flags,
                latency_ms=latency,
            )

        except Exception as e:
            self._error_count += 1
            latency = (time.time() - start) * 1000
            logger.error(f"Birdeye check failed for {token_address}: {e}")
            return ProviderResult(
                provider_name=self.name,
                data={"error": str(e)},
                is_safe=None,
                risk_level="UNKNOWN",
                flags=["PROVIDER_ERROR"],
                latency_ms=latency,
            )

    async def _fetch_token_security(self, session: aiohttp.ClientSession,
                                    token_address: str) -> dict:
        """Fetch token security data from Birdeye."""
        url = f"{BIRDEYE_BASE_URL}/defi/token_security"
        params = {"address": token_address}

        async with session.get(url, params=params, timeout=aiohttp.ClientTimeout(total=10)) as resp:
            if resp.status == 200:
                data = await resp.json()
                return data.get("data", {})
            elif resp.status == 429:
                logger.warning("Birdeye rate limited")
                return {}
            else:
                logger.warning(f"Birdeye security API returned {resp.status}")
                return {}

    async def _fetch_token_overview(self, session: aiohttp.ClientSession,
                                    token_address: str) -> dict:
        """Fetch token overview (price, volume, holders)."""
        url = f"{BIRDEYE_BASE_URL}/defi/token_overview"
        params = {"address": token_address}

        async with session.get(url, params=params, timeout=aiohttp.ClientTimeout(total=10)) as resp:
            if resp.status == 200:
                data = await resp.json()
                return data.get("data", {})
            else:
                return {}

    def _analyze_flags(self, data: dict) -> list[str]:
        """Analyze token data for security flags."""
        flags = []

        # LP analysis
        lp_burned_pct = data.get("lpBurnedPercent") or data.get("lp_burned_percent", 0)
        try:
            lp_pct = float(lp_burned_pct)
            if lp_pct < 50:
                flags.append("LOW_LP_BURN")
            if lp_pct == 0:
                flags.append("HIGH:LP_NOT_BURNED")
        except (ValueError, TypeError):
            flags.append("LP_UNKNOWN")

        # Holder distribution
        top10 = data.get("top10HolderPercent", 0)
        try:
            top10_pct = float(top10)
            if top10_pct > 80:
                flags.append("CRITICAL:HIGH_HOLDER_CONCENTRATION")
            elif top10_pct > 60:
                flags.append("HIGH:HOLDER_CONCENTRATION")
        except (ValueError, TypeError):
            pass

        # Creator analysis
        creator_pct = data.get("creatorPercentage", 0)
        try:
            if float(creator_pct) > 20:
                flags.append("HIGH:CREATOR_HOLDS_LARGE_SUPPLY")
        except (ValueError, TypeError):
            pass

        # Mintable
        if data.get("isMintable") or data.get("mintable"):
            flags.append("CRITICAL:MINTABLE")

        # Freezable
        if data.get("isFreezable") or data.get("freezable"):
            flags.append("HIGH:FREEZABLE")

        return flags

    def _determine_risk_level(self, flags: list[str]) -> str:
        """Determine overall risk level from flags."""
        if any("CRITICAL" in f for f in flags):
            return "CRITICAL"
        if any("HIGH" in f for f in flags):
            return "HIGH"
        if any("LOW" in f or "UNKNOWN" in f for f in flags):
            return "MEDIUM"
        return "LOW"

    async def close(self):
        if self._session and not self._session.closed:
            await self._session.close()
