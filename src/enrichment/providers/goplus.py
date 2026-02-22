"""GoPlus Security Provider — EVM token security checks.

API: https://api.gopluslabs.io/api/v1/token_security/{chain_id}
Provides: honeypot detection, mintable, owner drain, proxy detection, tax analysis.
"""

import logging
import time
from typing import Optional

import aiohttp

from .base_provider import BaseSecurityProvider, ProviderResult

logger = logging.getLogger(__name__)

GOPLUS_BASE_URL = "https://api.gopluslabs.io/api/v1"

CHAIN_IDS = {
    "base": "8453",
    "bsc": "56",
    "arbitrum": "42161",
    "ethereum": "1",
}


class GoPlusProvider(BaseSecurityProvider):
    """GoPlus API provider for EVM token security checks."""

    def __init__(self, rate_limit_per_sec: float = 0.5):
        super().__init__("goplus", rate_limit_per_sec)
        self._session: Optional[aiohttp.ClientSession] = None

    async def _get_session(self) -> aiohttp.ClientSession:
        if self._session is None or self._session.closed:
            self._session = aiohttp.ClientSession(
                headers={"Accept": "application/json"}
            )
        return self._session

    async def check(self, chain: str, token_address: str) -> ProviderResult:
        """Perform security check via GoPlus API."""
        start = time.time()
        self._call_count += 1

        chain_id = CHAIN_IDS.get(chain)
        if not chain_id:
            return ProviderResult(
                provider_name=self.name,
                data={"error": f"unsupported chain: {chain}"},
                is_safe=None,
                risk_level="UNKNOWN",
                flags=["UNSUPPORTED_CHAIN"],
                latency_ms=0,
            )

        try:
            await self._rate_limit_wait()

            session = await self._get_session()
            security_data = await self._fetch_token_security(session, chain_id, token_address)

            if not security_data:
                latency = (time.time() - start) * 1000
                return ProviderResult(
                    provider_name=self.name,
                    data={},
                    is_safe=None,
                    risk_level="UNKNOWN",
                    flags=["NO_DATA"],
                    latency_ms=latency,
                )

            flags = self._analyze_flags(security_data)
            is_safe = len([f for f in flags if "CRITICAL" in f or "HONEYPOT" in f]) == 0
            risk_level = self._determine_risk_level(flags)

            latency = (time.time() - start) * 1000

            return ProviderResult(
                provider_name=self.name,
                data={"security": security_data},
                is_safe=is_safe,
                risk_level=risk_level,
                flags=flags,
                latency_ms=latency,
            )

        except Exception as e:
            self._error_count += 1
            latency = (time.time() - start) * 1000
            logger.error(f"GoPlus check failed for {chain}:{token_address}: {e}")
            return ProviderResult(
                provider_name=self.name,
                data={"error": str(e)},
                is_safe=None,
                risk_level="UNKNOWN",
                flags=["PROVIDER_ERROR"],
                latency_ms=latency,
            )

    async def _fetch_token_security(self, session: aiohttp.ClientSession,
                                    chain_id: str, token_address: str) -> dict:
        """Fetch token security data from GoPlus."""
        url = f"{GOPLUS_BASE_URL}/token_security/{chain_id}"
        params = {"contract_addresses": token_address.lower()}

        async with session.get(url, params=params, timeout=aiohttp.ClientTimeout(total=15)) as resp:
            if resp.status == 200:
                data = await resp.json()
                result = data.get("result", {})
                return result.get(token_address.lower(), {})
            elif resp.status == 429:
                logger.warning("GoPlus rate limited")
                return {}
            else:
                logger.warning(f"GoPlus API returned {resp.status}")
                return {}

    def _analyze_flags(self, data: dict) -> list[str]:
        """Analyze GoPlus data for security flags."""
        flags = []

        # Honeypot detection (CRITICAL)
        if data.get("is_honeypot") == "1":
            flags.append("HONEYPOT")

        # Can't sell
        if data.get("cannot_sell_all") == "1":
            flags.append("CRITICAL:CANNOT_SELL")

        # Mintable (can mint new tokens)
        if data.get("is_mintable") == "1":
            flags.append("CRITICAL:MINTABLE")

        # Owner can change balance
        if data.get("owner_change_balance") == "1":
            flags.append("CRITICAL:OWNER_CAN_DRAIN")

        # Proxy contract (upgradeable)
        if data.get("is_proxy") == "1":
            flags.append("HIGH:PROXY_CONTRACT")

        # External call risk
        if data.get("external_call") == "1":
            flags.append("HIGH:EXTERNAL_CALL")

        # Hidden owner
        if data.get("hidden_owner") == "1":
            flags.append("HIGH:HIDDEN_OWNER")

        # Can take back ownership
        if data.get("can_take_back_ownership") == "1":
            flags.append("HIGH:TAKEBACK_OWNERSHIP")

        # Buy tax
        buy_tax = data.get("buy_tax")
        if buy_tax:
            try:
                tax_pct = float(buy_tax) * 100
                if tax_pct > 10:
                    flags.append(f"HIGH_BUY_TAX:{tax_pct:.1f}%")
                elif tax_pct > 5:
                    flags.append(f"MEDIUM_BUY_TAX:{tax_pct:.1f}%")
            except (ValueError, TypeError):
                pass

        # Sell tax
        sell_tax = data.get("sell_tax")
        if sell_tax:
            try:
                tax_pct = float(sell_tax) * 100
                if tax_pct > 10:
                    flags.append(f"HIGH_SELL_TAX:{tax_pct:.1f}%")
                elif tax_pct > 5:
                    flags.append(f"MEDIUM_SELL_TAX:{tax_pct:.1f}%")
            except (ValueError, TypeError):
                pass

        # LP holder analysis
        lp_holders = data.get("lp_holder_count")
        if lp_holders:
            try:
                if int(lp_holders) < 3:
                    flags.append("HIGH:FEW_LP_HOLDERS")
            except (ValueError, TypeError):
                pass

        # Holder count
        holder_count = data.get("holder_count")
        if holder_count:
            try:
                if int(holder_count) < 20:
                    flags.append("LOW_HOLDER_COUNT")
            except (ValueError, TypeError):
                pass

        # Open source
        if data.get("is_open_source") == "0":
            flags.append("NOT_OPEN_SOURCE")

        return flags

    def _determine_risk_level(self, flags: list[str]) -> str:
        """Determine overall risk level from flags."""
        if "HONEYPOT" in flags:
            return "CRITICAL"
        if any("CRITICAL" in f for f in flags):
            return "CRITICAL"
        if any("HIGH" in f for f in flags):
            return "HIGH"
        if any("MEDIUM" in f or "LOW" in f or "NOT_OPEN" in f for f in flags):
            return "MEDIUM"
        return "LOW"

    async def close(self):
        if self._session and not self._session.closed:
            await self._session.close()
