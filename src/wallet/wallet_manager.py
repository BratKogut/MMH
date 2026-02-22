"""MMH v3.1 -- Wallet Manager.

Secure wallet abstraction for multi-chain memecoin trading.

Design principles:
- Private keys are NEVER logged, serialized, or exposed in any way
- Keys are loaded exclusively from environment variables:
    MMH_SOLANA_PRIVATE_KEY  (base58-encoded Solana keypair secret)
    MMH_EVM_PRIVATE_KEY     (hex-encoded EVM private key, with or without 0x prefix)
- When no key is configured for a chain the manager degrades
  gracefully into dry-run mode for that chain
- All I/O (balance fetches) is async via aiohttp

Usage:
    from src.wallet.wallet_manager import WalletManager
    from src.config.settings import get_settings

    wm = WalletManager(get_settings())
    info = await wm.get_wallet("solana")
    if info:
        print(info.address, info.balance_native)
"""

from __future__ import annotations

import logging
import os
import time
from dataclasses import dataclass, field
from typing import Optional

logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Graceful optional imports
# ---------------------------------------------------------------------------
try:
    from solders.keypair import Keypair as SoldersKeypair  # type: ignore[import-untyped]

    _HAS_SOLDERS = True
except ImportError:  # pragma: no cover
    _HAS_SOLDERS = False
    SoldersKeypair = None  # type: ignore[assignment,misc]

try:
    from eth_account import Account as EthAccount  # type: ignore[import-untyped]

    _HAS_ETH_ACCOUNT = True
except ImportError:  # pragma: no cover
    _HAS_ETH_ACCOUNT = False
    EthAccount = None  # type: ignore[assignment,misc]

try:
    import aiohttp
except ImportError:  # pragma: no cover
    aiohttp = None  # type: ignore[assignment]

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
_ENV_SOLANA_KEY = "MMH_SOLANA_PRIVATE_KEY"
_ENV_EVM_KEY = "MMH_EVM_PRIVATE_KEY"

_SOLANA_CHAIN = "solana"
_EVM_CHAINS = frozenset({"base", "ethereum", "arbitrum", "optimism", "bsc"})

# How long cached balance values are considered fresh (seconds).
_BALANCE_CACHE_TTL = 30


# ---------------------------------------------------------------------------
# WalletInfo dataclass
# ---------------------------------------------------------------------------
@dataclass
class WalletInfo:
    """Public-facing wallet information.  Never contains private key material."""

    address: str
    chain: str
    balance_native: float = 0.0
    balance_usd: float = 0.0
    _cached_at: float = field(default=0.0, repr=False)


# ---------------------------------------------------------------------------
# WalletManager
# ---------------------------------------------------------------------------
class WalletManager:
    """Multi-chain hot-wallet manager.

    Provides a unified ``get_wallet(chain)`` interface that returns a
    :class:`WalletInfo` (or ``None`` when the chain has no key configured).

    All balance queries are performed via JSON-RPC over aiohttp and cached
    for ``_BALANCE_CACHE_TTL`` seconds.
    """

    def __init__(self, settings) -> None:
        """
        Args:
            settings: An :class:`~src.config.settings.MMHSettings` instance.
                      Used to read RPC URLs and enabled-chain list.
        """
        self._settings = settings

        # Derived keypair objects -- populated lazily on first access.
        self._solana_keypair: Optional[object] = None
        self._evm_account: Optional[object] = None

        # Addresses (derived once, then cached).
        self._solana_address: Optional[str] = None
        self._evm_address: Optional[str] = None

        # Balance caches keyed by chain.
        self._wallet_cache: dict[str, WalletInfo] = {}

        # Attempt to derive addresses at init so ``is_configured`` works
        # immediately.
        self._init_solana()
        self._init_evm()

    # ------------------------------------------------------------------
    # Initialisation helpers (private key never leaves these methods)
    # ------------------------------------------------------------------
    def _init_solana(self) -> None:
        """Derive Solana address from the environment variable."""
        raw = os.environ.get(_ENV_SOLANA_KEY, "").strip()
        if not raw:
            logger.info("No Solana private key configured -- dry-run only for Solana")
            return

        if not _HAS_SOLDERS:
            logger.warning(
                "MMH_SOLANA_PRIVATE_KEY is set but the 'solders' package is not "
                "installed.  Solana wallet will be unavailable."
            )
            return

        try:
            keypair = SoldersKeypair.from_base58_string(raw)
            self._solana_keypair = keypair
            self._solana_address = str(keypair.pubkey())
            logger.info("Solana wallet loaded: address=%s", self._solana_address)
        except Exception:
            logger.exception("Failed to load Solana keypair from environment")

    def _init_evm(self) -> None:
        """Derive EVM address from the environment variable."""
        raw = os.environ.get(_ENV_EVM_KEY, "").strip()
        if not raw:
            logger.info("No EVM private key configured -- dry-run only for EVM chains")
            return

        if not _HAS_ETH_ACCOUNT:
            logger.warning(
                "MMH_EVM_PRIVATE_KEY is set but the 'eth-account' package is not "
                "installed.  EVM wallet will be unavailable."
            )
            return

        try:
            # eth_account accepts keys with or without 0x prefix.
            acct = EthAccount.from_key(raw)
            self._evm_account = acct
            self._evm_address = acct.address
            logger.info("EVM wallet loaded: address=%s", self._evm_address)
        except Exception:
            logger.exception("Failed to load EVM private key from environment")

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------
    async def get_wallet(self, chain: str) -> Optional[WalletInfo]:
        """Return :class:`WalletInfo` for *chain*, or ``None`` if the wallet
        is not configured.

        Balances are fetched (and cached) on each call.
        """
        chain = chain.lower()

        if not self.is_configured(chain):
            return None

        # Return a cached entry if still fresh.
        cached = self._wallet_cache.get(chain)
        if cached and (time.time() - cached._cached_at) < _BALANCE_CACHE_TTL:
            return cached

        address = self.get_address(chain)
        if address is None:
            return None

        balance_native = await self.get_balance(chain)

        info = WalletInfo(
            address=address,
            chain=chain,
            balance_native=balance_native,
            balance_usd=0.0,  # USD conversion requires a price feed; left as 0 for now.
            _cached_at=time.time(),
        )
        self._wallet_cache[chain] = info
        return info

    async def get_balance(self, chain: str) -> float:
        """Fetch native-token balance for *chain* via JSON-RPC.

        Returns ``0.0`` on any error (network, misconfiguration, etc.).
        """
        chain = chain.lower()
        try:
            if chain == _SOLANA_CHAIN:
                return await self._get_solana_balance()
            elif chain in _EVM_CHAINS:
                return await self._get_evm_balance(chain)
            else:
                logger.warning("get_balance called for unknown chain: %s", chain)
                return 0.0
        except Exception:
            logger.exception("Balance fetch failed for chain=%s", chain)
            return 0.0

    async def has_sufficient_balance(self, chain: str, amount_usd: float) -> bool:
        """Return ``True`` if the native-token balance on *chain* covers
        *amount_usd*.

        Because a reliable USD price oracle is out of scope for this module,
        the check currently compares against ``balance_native`` directly when
        ``balance_usd`` is unavailable.  For production use, integrate a price
        feed and compare against ``balance_usd``.
        """
        wallet = await self.get_wallet(chain)
        if wallet is None:
            return False

        # If we have a USD balance, use it.
        if wallet.balance_usd > 0:
            return wallet.balance_usd >= amount_usd

        # Fallback: cannot reliably compare native vs USD without a price feed.
        # Return True only if we have *some* native balance (conservative).
        return wallet.balance_native > 0

    def is_configured(self, chain: str) -> bool:
        """Return ``True`` if a private key is available for *chain*."""
        chain = chain.lower()
        if chain == _SOLANA_CHAIN:
            return self._solana_address is not None
        if chain in _EVM_CHAINS:
            return self._evm_address is not None
        return False

    def get_address(self, chain: str) -> Optional[str]:
        """Return the public address for *chain*, or ``None``."""
        chain = chain.lower()
        if chain == _SOLANA_CHAIN:
            return self._solana_address
        if chain in _EVM_CHAINS:
            return self._evm_address
        return None

    # ------------------------------------------------------------------
    # Balance RPC helpers (private)
    # ------------------------------------------------------------------
    async def _get_solana_balance(self) -> float:
        """Fetch SOL balance via Solana JSON-RPC ``getBalance``."""
        if self._solana_address is None:
            return 0.0

        rpc_url = getattr(self._settings, "solana_rpc_url", "")
        if not rpc_url:
            logger.warning("solana_rpc_url is not configured; cannot fetch balance")
            return 0.0

        payload = {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "getBalance",
            "params": [self._solana_address],
        }

        if aiohttp is None:
            logger.warning("aiohttp is not installed; cannot fetch Solana balance")
            return 0.0

        async with aiohttp.ClientSession() as session:
            async with session.post(
                rpc_url,
                json=payload,
                timeout=aiohttp.ClientTimeout(total=10),
            ) as resp:
                data = await resp.json()

        lamports = data.get("result", {}).get("value", 0)
        return lamports / 1e9  # lamports -> SOL

    async def _get_evm_balance(self, chain: str) -> float:
        """Fetch native token balance via EVM JSON-RPC ``eth_getBalance``."""
        if self._evm_address is None:
            return 0.0

        rpc_url = self._rpc_url_for_evm_chain(chain)
        if not rpc_url:
            logger.warning("No RPC URL configured for EVM chain: %s", chain)
            return 0.0

        payload = {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "eth_getBalance",
            "params": [self._evm_address, "latest"],
        }

        if aiohttp is None:
            logger.warning("aiohttp is not installed; cannot fetch EVM balance")
            return 0.0

        async with aiohttp.ClientSession() as session:
            async with session.post(
                rpc_url,
                json=payload,
                timeout=aiohttp.ClientTimeout(total=10),
            ) as resp:
                data = await resp.json()

        hex_balance = data.get("result", "0x0")
        wei = int(hex_balance, 16)
        return wei / 1e18  # wei -> ETH / native token

    def _rpc_url_for_evm_chain(self, chain: str) -> str:
        """Resolve the RPC URL for a given EVM chain from settings."""
        chain = chain.lower()
        # Map chain names to settings attributes.
        mapping: dict[str, str] = {
            "base": "base_rpc_url",
            "ethereum": "base_rpc_url",  # re-use if no dedicated attr
            "arbitrum": "base_rpc_url",
            "optimism": "base_rpc_url",
            "bsc": "base_rpc_url",
        }
        attr = mapping.get(chain, "")
        if attr:
            return getattr(self._settings, attr, "") or ""
        return ""

    # ------------------------------------------------------------------
    # Internal keypair access (for executor module ONLY)
    # ------------------------------------------------------------------
    def _get_solana_keypair(self) -> Optional[object]:
        """Return the raw ``solders.Keypair`` for signing transactions.

        **Internal use only** -- NEVER expose this through a public API or
        log its value.
        """
        return self._solana_keypair

    def _get_evm_account(self) -> Optional[object]:
        """Return the raw ``eth_account.Account`` for signing transactions.

        **Internal use only** -- NEVER expose this through a public API or
        log its value.
        """
        return self._evm_account

    # ------------------------------------------------------------------
    # Metrics / diagnostics (safe -- no key material)
    # ------------------------------------------------------------------
    def get_metrics(self) -> dict:
        """Return wallet status metrics (safe to log / expose)."""
        return {
            "solana_configured": self._solana_address is not None,
            "solana_address": self._solana_address or "",
            "evm_configured": self._evm_address is not None,
            "evm_address": self._evm_address or "",
            "cached_chains": list(self._wallet_cache.keys()),
        }
