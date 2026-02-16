"""GoPlus security provider for EVM tokens (Base, BSC, etc.)."""

import aiohttp
import logging
from .base_provider import BaseSecurityProvider, ProviderResult

logger = logging.getLogger(__name__)

# Chain ID mapping for GoPlus
CHAIN_IDS = {
    "base": "8453",
    "bsc": "56",
    "arbitrum": "42161",
    "ethereum": "1",
}


class GoPlusProvider(BaseSecurityProvider):
    """GoPlus Security API provider for EVM chain tokens.

    Checks: honeypot, mintable, owner_can_drain, hidden_owner,
            selfdestruct, pausable, buy/sell tax
    Rate limit: 0.5 req/sec (30/min)
    """

    BASE_URL = "https://api.gopluslabs.io/api/v1"

    def __init__(self, rate_limit: float = 0.5):
        super().__init__("goplus", rate_limit)
        self._session: aiohttp.ClientSession | None = None

    async def _get_session(self) -> aiohttp.ClientSession:
        if self._session is None or self._session.closed:
            self._session = aiohttp.ClientSession()
        return self._session

    async def check(self, chain: str, token_address: str) -> ProviderResult:
        """Check token security via GoPlus API."""
        import time
        start = time.time()
        self._call_count += 1

        chain_id = CHAIN_IDS.get(chain)
        if not chain_id:
            return ProviderResult(
                provider_name=self.name,
                data={"error": f"Unsupported chain: {chain}"},
                is_safe=None,
                risk_level="UNKNOWN",
                flags=["UNSUPPORTED_CHAIN"]
            )

        try:
            await self._rate_limit_wait()
            session = await self._get_session()

            async with session.get(
                f"{self.BASE_URL}/token_security/{chain_id}",
                params={"contract_addresses": token_address.lower()},
                timeout=aiohttp.ClientTimeout(total=15)
            ) as resp:
                if resp.status != 200:
                    self._error_count += 1
                    return ProviderResult(
                        provider_name=self.name,
                        data={"error": f"HTTP {resp.status}"},
                        is_safe=None,
                        risk_level="UNKNOWN",
                        flags=["API_ERROR"]
                    )

                result = await resp.json()
                token_data = result.get("result", {}).get(token_address.lower(), {})

                flags = []
                risk_level = "LOW"
                is_safe = True

                # Critical checks (instant block)
                if token_data.get("is_honeypot") == "1":
                    flags.append("HONEYPOT")
                    risk_level = "CRITICAL"
                    is_safe = False

                if token_data.get("is_mintable") == "1":
                    flags.append("MINTABLE")
                    risk_level = "CRITICAL"
                    is_safe = False

                if token_data.get("selfdestruct") == "1":
                    flags.append("SELFDESTRUCT")
                    risk_level = "CRITICAL"
                    is_safe = False

                if token_data.get("owner_change_balance") == "1":
                    flags.append("OWNER_CAN_DRAIN")
                    risk_level = "CRITICAL"
                    is_safe = False

                # High risk checks
                if token_data.get("hidden_owner") == "1":
                    flags.append("HIDDEN_OWNER")
                    if risk_level not in ("CRITICAL",):
                        risk_level = "HIGH"
                    is_safe = False

                if token_data.get("can_take_back_ownership") == "1":
                    flags.append("CAN_TAKE_BACK_OWNERSHIP")
                    if risk_level not in ("CRITICAL",):
                        risk_level = "HIGH"
                    is_safe = False

                if token_data.get("is_anti_whale") == "1":
                    flags.append("ANTI_WHALE")

                if token_data.get("trading_cooldown") == "1":
                    flags.append("TRADING_COOLDOWN")

                # Tax checks
                buy_tax = float(token_data.get("buy_tax", "0") or "0")
                sell_tax = float(token_data.get("sell_tax", "0") or "0")

                if buy_tax > 0.10:  # >10%
                    flags.append(f"HIGH_BUY_TAX:{buy_tax*100:.1f}%")
                    if risk_level not in ("CRITICAL", "HIGH"):
                        risk_level = "MEDIUM"

                if sell_tax > 0.10:
                    flags.append(f"HIGH_SELL_TAX:{sell_tax*100:.1f}%")
                    if risk_level not in ("CRITICAL", "HIGH"):
                        risk_level = "MEDIUM"

                latency = (time.time() - start) * 1000
                return ProviderResult(
                    provider_name=self.name,
                    data=token_data,
                    is_safe=is_safe,
                    risk_level=risk_level,
                    flags=flags,
                    latency_ms=latency
                )

        except Exception as e:
            self._error_count += 1
            logger.error(f"GoPlus check failed for {token_address}: {e}")
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
