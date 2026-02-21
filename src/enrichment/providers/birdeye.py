"""Birdeye security provider for Solana tokens."""

import aiohttp
import logging
from .base_provider import BaseSecurityProvider, ProviderResult

logger = logging.getLogger(__name__)


class BirdeyeProvider(BaseSecurityProvider):
    """Birdeye API provider for Solana token security and overview data.

    Checks: isMintable, isFreezable, top10HolderPercent, lpBurnedPercent
    Rate limit: 1 req/sec
    """

    BASE_URL = "https://public-api.birdeye.so"

    def __init__(self, api_key: str, rate_limit: float = 1.0):
        super().__init__("birdeye", rate_limit)
        self._api_key = api_key
        self._session: aiohttp.ClientSession | None = None

    async def _get_session(self) -> aiohttp.ClientSession:
        if self._session is None or self._session.closed:
            self._session = aiohttp.ClientSession(
                headers={"X-API-KEY": self._api_key}
            )
        return self._session

    async def check(self, chain: str, token_address: str) -> ProviderResult:
        """Check token security via Birdeye API."""
        import time
        start = time.time()
        self._call_count += 1

        try:
            await self._rate_limit_wait()
            session = await self._get_session()

            # Security endpoint
            flags = []
            risk_level = "LOW"
            is_safe = True
            data = {}

            try:
                async with session.get(
                    f"{self.BASE_URL}/defi/token_security",
                    params={"address": token_address},
                    timeout=aiohttp.ClientTimeout(total=10)
                ) as resp:
                    if resp.status == 200:
                        result = await resp.json()
                        sec_data = result.get("data", {})
                        data["security"] = sec_data

                        if sec_data.get("isMintable"):
                            flags.append("MINTABLE")
                            risk_level = "CRITICAL"
                            is_safe = False
                        if sec_data.get("isFreezable"):
                            flags.append("FREEZABLE")
                            if risk_level != "CRITICAL":
                                risk_level = "HIGH"
                            is_safe = False

                        top10 = sec_data.get("top10HolderPercent", 0)
                        if top10 and float(top10) > 80:
                            flags.append(f"TOP10_HOLDERS:{top10}%")
                            if risk_level not in ("CRITICAL", "HIGH"):
                                risk_level = "MEDIUM"

                        lp_burned = sec_data.get("lpBurnedPercent", 0)
                        if lp_burned is not None and float(lp_burned) < 50:
                            flags.append(f"LP_NOT_BURNED:{lp_burned}%")
                    else:
                        logger.warning(f"Birdeye security API returned {resp.status}")
                        self._error_count += 1
            except Exception as e:
                logger.error(f"Birdeye security check failed: {e}")
                self._error_count += 1

            # Overview endpoint
            try:
                async with session.get(
                    f"{self.BASE_URL}/defi/token_overview",
                    params={"address": token_address},
                    timeout=aiohttp.ClientTimeout(total=10)
                ) as resp:
                    if resp.status == 200:
                        result = await resp.json()
                        overview = result.get("data", {})
                        data["overview"] = overview
            except Exception as e:
                logger.error(f"Birdeye overview failed: {e}")

            latency = (time.time() - start) * 1000
            return ProviderResult(
                provider_name=self.name,
                data=data,
                is_safe=is_safe,
                risk_level=risk_level,
                flags=flags,
                latency_ms=latency
            )

        except Exception as e:
            self._error_count += 1
            logger.error(f"Birdeye check failed for {token_address}: {e}")
            return ProviderResult(
                provider_name=self.name,
                data={"error": str(e)},
                is_safe=None,
                risk_level="UNKNOWN",
                flags=["PROVIDER_ERROR"],
                latency_ms=(time.time() - start) * 1000
            )

    async def close(self):
        if self._session and not self._session.closed:
            await self._session.close()
