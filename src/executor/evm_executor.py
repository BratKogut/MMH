"""EVM Chain Executor for MMH v3.1.

Executes token swaps on EVM-compatible chains (Base, BSC, Arbitrum) using:
  1. 0x Swap API (primary route) -- aggregated best-price quotes
  2. Direct Uniswap V3 Router (fallback, not yet wired)

Supports EIP-1559 dynamic gas fees, local nonce tracking with on-chain sync,
transaction receipt polling with configurable timeout, and slippage protection
via minAmountOut embedded in the 0x quote calldata.

Contract:
  - Accepts intent_data dict from the Executor orchestrator
  - Returns ExecutionResult with status, tx_hash, amounts, gas cost
  - NEVER retries internally -- the parent Executor handles retry logic
  - Logs every significant step for post-mortem analysis

Usage (wired via main.py)::

    from src.executor.evm_executor import EVMExecutor

    evm_exec = EVMExecutor(
        chain="base",
        rpc_url=settings.base_rpc_url,
        wallet_manager=wallet_mgr,
        settings={
            "zerox_api_key": settings.zerox_api_key,
            "slippage": 0.03,
            "gas_limit_multiplier": 1.2,
            "receipt_timeout_seconds": 120,
            "receipt_poll_interval": 2.0,
        },
    )
    result = await evm_exec.execute(intent_data)
"""

from __future__ import annotations

import asyncio
import logging
import time
from typing import Any, Optional

import aiohttp
from web3 import Web3
from web3.exceptions import TransactionNotFound

from src.executor.executor import ExecutionResult, ExecutionStatus

logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Per-chain constants
# ---------------------------------------------------------------------------

CHAIN_CONFIG: dict[str, dict[str, Any]] = {
    "base": {
        "chain_id": 8453,
        "native_wrapped": "0x4200000000000000000000000000000000000006",  # WETH
        "native_symbol": "WETH",
        "zerox_api_base": "https://api.0x.org",
    },
    "bsc": {
        "chain_id": 56,
        "native_wrapped": "0xbb4CdB9CBd36B01bD1cBaEBF2De08d9173bc095c",  # WBNB
        "native_symbol": "WBNB",
        "zerox_api_base": "https://bsc.api.0x.org",
    },
    "arbitrum": {
        "chain_id": 42161,
        "native_wrapped": "0x82aF49447D8a07e3bd95BD0d56f35241523fBab1",  # WETH
        "native_symbol": "WETH",
        "zerox_api_base": "https://api.0x.org",
    },
}

# Default slippage percentage for 0x quotes (3%)
DEFAULT_SLIPPAGE = 0.03

# Default gas limit multiplier applied on top of the 0x estimate
DEFAULT_GAS_LIMIT_MULTIPLIER = 1.2

# Default receipt polling timeout in seconds
DEFAULT_RECEIPT_TIMEOUT = 120

# Default receipt poll interval in seconds
DEFAULT_RECEIPT_POLL_INTERVAL = 2.0

# ERC-20 ABI fragment for allowance / approve
ERC20_ABI = [
    {
        "constant": True,
        "inputs": [
            {"name": "owner", "type": "address"},
            {"name": "spender", "type": "address"},
        ],
        "name": "allowance",
        "outputs": [{"name": "", "type": "uint256"}],
        "type": "function",
    },
    {
        "constant": False,
        "inputs": [
            {"name": "spender", "type": "address"},
            {"name": "amount", "type": "uint256"},
        ],
        "name": "approve",
        "outputs": [{"name": "", "type": "bool"}],
        "type": "function",
    },
]

# Maximum uint256 for infinite approval
MAX_UINT256 = 2**256 - 1


class EVMExecutor:
    """EVM chain executor -- executes swaps via the 0x Swap API.

    One instance is created per supported EVM chain.  The parent
    :class:`~src.executor.executor.Executor` calls :meth:`execute` with the
    approved intent payload and expects an :class:`ExecutionResult` back.
    """

    # ------------------------------------------------------------------ #
    # Construction
    # ------------------------------------------------------------------ #

    def __init__(
        self,
        chain: str,
        rpc_url: str,
        wallet_manager: Any,
        settings: dict,
    ) -> None:
        """
        Args:
            chain: Chain identifier (``"base"``, ``"bsc"``, ``"arbitrum"``).
            rpc_url: HTTP JSON-RPC endpoint for the chain.
            wallet_manager: Object exposing ``.address`` (checksummed) and
                ``.private_key`` (hex string without ``0x`` prefix).
            settings: Dict of tunable knobs -- see module docstring.
        """
        if chain not in CHAIN_CONFIG:
            raise ValueError(
                f"Unsupported chain '{chain}'. "
                f"Supported: {list(CHAIN_CONFIG.keys())}"
            )

        self._chain = chain
        self._chain_cfg = CHAIN_CONFIG[chain]
        self._chain_id: int = self._chain_cfg["chain_id"]
        self._native_wrapped: str = self._chain_cfg["native_wrapped"]
        self._zerox_api_base: str = self._chain_cfg["zerox_api_base"]

        # Web3 provider (synchronous JSON-RPC -- web3.py does not natively
        # support async providers in all versions, so we use the standard
        # HTTPProvider and offload blocking calls to the event-loop executor).
        self._w3 = Web3(Web3.HTTPProvider(rpc_url))
        self._rpc_url = rpc_url

        # Wallet
        self._wallet = wallet_manager
        self._account_address: str = Web3.to_checksum_address(
            self._wallet.address
        )

        # Settings
        self._zerox_api_key: str = settings.get("zerox_api_key", "")
        self._slippage: float = settings.get("slippage", DEFAULT_SLIPPAGE)
        self._gas_limit_multiplier: float = settings.get(
            "gas_limit_multiplier", DEFAULT_GAS_LIMIT_MULTIPLIER
        )
        self._receipt_timeout: int = settings.get(
            "receipt_timeout_seconds", DEFAULT_RECEIPT_TIMEOUT
        )
        self._receipt_poll_interval: float = settings.get(
            "receipt_poll_interval", DEFAULT_RECEIPT_POLL_INTERVAL
        )

        # Local nonce tracker -- initialised lazily on first use
        self._nonce: Optional[int] = None
        self._nonce_lock = asyncio.Lock()

        logger.info(
            "EVMExecutor initialised chain=%s chain_id=%d address=%s",
            self._chain,
            self._chain_id,
            self._account_address,
        )

    # ------------------------------------------------------------------ #
    # Public API
    # ------------------------------------------------------------------ #

    async def execute(self, intent_data: dict) -> ExecutionResult:
        """Execute a swap for the given approved intent.

        Args:
            intent_data: Dict containing at minimum:
                - ``intent_id`` (str)
                - ``chain`` (str)
                - ``token_address`` (str) -- the memecoin contract address
                - ``side`` (str) -- ``"BUY"`` or ``"SELL"``
                - ``amount_tokens`` (str/float, required for SELL)
                - ``amount_eth`` (str/float, optional for BUY -- ETH to spend)

        Returns:
            :class:`ExecutionResult` with status CONFIRMED or FAILED.
        """
        intent_id: str = intent_data.get("intent_id", "")
        token_address: str = intent_data.get("token_address", "")
        side: str = intent_data.get("side", "BUY").upper()

        logger.info(
            "EVMExecutor.execute intent=%s chain=%s token=%s side=%s",
            intent_id,
            self._chain,
            token_address,
            side,
        )

        try:
            token_address = Web3.to_checksum_address(token_address)
        except Exception as exc:
            return ExecutionResult(
                intent_id=intent_id,
                status=ExecutionStatus.FAILED,
                error=f"invalid_token_address: {exc}",
            )

        try:
            # ----------------------------------------------------------
            # 1. Determine sell / buy tokens and amounts
            # ----------------------------------------------------------
            if side == "BUY":
                sell_token = self._native_wrapped
                buy_token = token_address
                # amount_eth is denominated in wei (or ETH as a float)
                raw_amount = intent_data.get("amount_eth", "0")
                sell_amount = self._to_wei(raw_amount)
            elif side == "SELL":
                sell_token = token_address
                buy_token = self._native_wrapped
                raw_amount = intent_data.get("amount_tokens", "0")
                sell_amount = self._to_token_units(raw_amount)
            else:
                return ExecutionResult(
                    intent_id=intent_id,
                    status=ExecutionStatus.FAILED,
                    error=f"unknown_side: {side}",
                )

            if sell_amount <= 0:
                return ExecutionResult(
                    intent_id=intent_id,
                    status=ExecutionStatus.FAILED,
                    error="sell_amount_zero_or_negative",
                )

            # ----------------------------------------------------------
            # 2. Ensure token approval for SELL orders
            # ----------------------------------------------------------
            if side == "SELL":
                await self._ensure_token_approval(
                    token_address=sell_token,
                    spender=None,  # resolved after quote
                    amount=sell_amount,
                    intent_id=intent_id,
                )

            # ----------------------------------------------------------
            # 3. Fetch 0x quote
            # ----------------------------------------------------------
            quote = await self._get_0x_quote(
                sell_token=sell_token,
                buy_token=buy_token,
                sell_amount=str(sell_amount),
                slippage=self._slippage,
            )

            if not quote or "to" not in quote:
                return ExecutionResult(
                    intent_id=intent_id,
                    status=ExecutionStatus.FAILED,
                    error="zerox_quote_failed_or_empty",
                )

            logger.info(
                "0x quote received intent=%s estimatedGas=%s buyAmount=%s",
                intent_id,
                quote.get("gas", "?"),
                quote.get("buyAmount", "?"),
            )

            # If SELL, ensure approval to the 0x allowance target
            allowance_target = quote.get("allowanceTarget")
            if side == "SELL" and allowance_target:
                await self._ensure_token_approval(
                    token_address=sell_token,
                    spender=allowance_target,
                    amount=sell_amount,
                    intent_id=intent_id,
                )

            # ----------------------------------------------------------
            # 4. Build EIP-1559 transaction
            # ----------------------------------------------------------
            tx_dict = await self._build_transaction(quote, side, sell_amount)

            # ----------------------------------------------------------
            # 5. Sign and send
            # ----------------------------------------------------------
            tx_hash = await self._sign_and_send(tx_dict)
            logger.info(
                "Transaction sent intent=%s tx_hash=%s", intent_id, tx_hash
            )

            # ----------------------------------------------------------
            # 6. Wait for receipt
            # ----------------------------------------------------------
            receipt = await self._wait_for_receipt(
                tx_hash, timeout=self._receipt_timeout
            )

            if receipt is None:
                return ExecutionResult(
                    intent_id=intent_id,
                    status=ExecutionStatus.FAILED,
                    tx_hash=tx_hash,
                    error="receipt_timeout",
                )

            # ----------------------------------------------------------
            # 7. Interpret receipt
            # ----------------------------------------------------------
            tx_status = receipt.get("status", 0)
            gas_used = int(receipt.get("gasUsed", 0))
            effective_gas_price = int(
                receipt.get("effectiveGasPrice", 0)
            )
            gas_cost_wei = gas_used * effective_gas_price
            gas_cost_eth = float(Web3.from_wei(gas_cost_wei, "ether"))

            buy_amount_raw = quote.get("buyAmount", "0")
            sell_amount_raw = quote.get("sellAmount", "0")

            if tx_status == 1:
                logger.info(
                    "Transaction CONFIRMED intent=%s tx=%s gasUsed=%d",
                    intent_id,
                    tx_hash,
                    gas_used,
                )
                return ExecutionResult(
                    intent_id=intent_id,
                    status=ExecutionStatus.CONFIRMED,
                    tx_hash=tx_hash,
                    amount_in=float(sell_amount_raw),
                    amount_out=float(buy_amount_raw),
                    price=(
                        float(buy_amount_raw) / float(sell_amount_raw)
                        if float(sell_amount_raw) > 0
                        else 0
                    ),
                    gas_cost_usd=gas_cost_eth,  # denominated in native token
                )
            else:
                logger.warning(
                    "Transaction REVERTED intent=%s tx=%s", intent_id, tx_hash
                )
                return ExecutionResult(
                    intent_id=intent_id,
                    status=ExecutionStatus.FAILED,
                    tx_hash=tx_hash,
                    error="tx_reverted",
                    gas_cost_usd=gas_cost_eth,
                )

        except Exception as exc:
            logger.error(
                "EVMExecutor.execute FAILED intent=%s error=%s",
                intent_id,
                exc,
                exc_info=True,
            )
            return ExecutionResult(
                intent_id=intent_id,
                status=ExecutionStatus.FAILED,
                error=str(exc),
            )

    # ------------------------------------------------------------------ #
    # 0x Swap API
    # ------------------------------------------------------------------ #

    async def _get_0x_quote(
        self,
        sell_token: str,
        buy_token: str,
        sell_amount: str,
        slippage: float,
    ) -> dict:
        """Fetch a firm quote from the 0x Swap API.

        GET /swap/v1/quote?sellToken=...&buyToken=...&sellAmount=...
                          &slippagePercentage=0.03&takerAddress=...

        Returns the full quote dict including ``to``, ``data``, ``value``,
        ``gas``, ``gasPrice``, ``buyAmount``, ``sellAmount``, and
        ``allowanceTarget``.
        """
        url = f"{self._zerox_api_base}/swap/v1/quote"
        params = {
            "sellToken": sell_token,
            "buyToken": buy_token,
            "sellAmount": sell_amount,
            "slippagePercentage": str(slippage),
            "takerAddress": self._account_address,
        }
        headers: dict[str, str] = {}
        if self._zerox_api_key:
            headers["0x-api-key"] = self._zerox_api_key

        logger.debug(
            "0x quote request url=%s params=%s",
            url,
            {k: v for k, v in params.items() if k != "takerAddress"},
        )

        try:
            async with aiohttp.ClientSession() as session:
                async with session.get(
                    url, params=params, headers=headers, timeout=aiohttp.ClientTimeout(total=15)
                ) as resp:
                    if resp.status != 200:
                        body = await resp.text()
                        logger.error(
                            "0x API error status=%d body=%s",
                            resp.status,
                            body[:500],
                        )
                        return {}
                    data = await resp.json()
                    return data
        except asyncio.TimeoutError:
            logger.error("0x API request timed out")
            return {}
        except Exception as exc:
            logger.error("0x API request failed: %s", exc)
            return {}

    # ------------------------------------------------------------------ #
    # Transaction building (EIP-1559)
    # ------------------------------------------------------------------ #

    async def _build_transaction(
        self,
        quote_data: dict,
        side: str,
        sell_amount: int,
    ) -> dict:
        """Build an EIP-1559 transaction dict from a 0x quote.

        For BUY orders (ETH -> token) the ``value`` field carries the ETH
        being sold.  For SELL orders (token -> ETH) the ``value`` is
        typically 0 (the tokens are pulled via approval).
        """
        loop = asyncio.get_event_loop()

        # Fetch latest base fee and priority fee
        latest_block = await loop.run_in_executor(
            None, self._w3.eth.get_block, "latest"
        )
        base_fee = latest_block.get("baseFeePerGas", 0)

        # Use a reasonable max priority fee (tip) -- 1.5 gwei default,
        # clamped to at least 0.1 gwei.
        try:
            max_priority = await loop.run_in_executor(
                None, self._w3.eth.max_priority_fee
            )
        except Exception:
            max_priority = Web3.to_wei(1.5, "gwei")

        max_priority = max(max_priority, Web3.to_wei(0.1, "gwei"))

        # maxFeePerGas = 2 * baseFee + maxPriorityFeePerGas
        max_fee_per_gas = 2 * base_fee + max_priority

        # Gas limit from quote with safety multiplier
        estimated_gas = int(quote_data.get("gas", 250_000))
        gas_limit = int(estimated_gas * self._gas_limit_multiplier)

        # Nonce
        nonce = await self._get_nonce()

        # Value field -- for BUY the ETH amount; for SELL typically 0
        value = int(quote_data.get("value", "0"))

        tx: dict[str, Any] = {
            "chainId": self._chain_id,
            "from": self._account_address,
            "to": Web3.to_checksum_address(quote_data["to"]),
            "data": quote_data.get("data", "0x"),
            "value": value,
            "gas": gas_limit,
            "maxFeePerGas": max_fee_per_gas,
            "maxPriorityFeePerGas": max_priority,
            "nonce": nonce,
            "type": 2,  # EIP-1559
        }

        logger.debug(
            "Built tx nonce=%d gas=%d maxFee=%d priorityFee=%d value=%d",
            nonce,
            gas_limit,
            max_fee_per_gas,
            max_priority,
            value,
        )

        return tx

    # ------------------------------------------------------------------ #
    # Signing and sending
    # ------------------------------------------------------------------ #

    async def _sign_and_send(self, tx_dict: dict) -> str:
        """Sign the transaction and broadcast it to the network.

        Returns the hex-encoded transaction hash.
        """
        loop = asyncio.get_event_loop()

        signed = self._w3.eth.account.sign_transaction(
            tx_dict, private_key=self._wallet.private_key
        )

        tx_hash = await loop.run_in_executor(
            None,
            self._w3.eth.send_raw_transaction,
            signed.raw_transaction,
        )

        return tx_hash.hex()

    # ------------------------------------------------------------------ #
    # Receipt polling
    # ------------------------------------------------------------------ #

    async def _wait_for_receipt(
        self,
        tx_hash: str,
        timeout: int = DEFAULT_RECEIPT_TIMEOUT,
    ) -> Optional[dict]:
        """Poll for a transaction receipt until confirmed or timeout.

        Returns the receipt dict on success, or ``None`` if the timeout
        is reached.
        """
        loop = asyncio.get_event_loop()
        deadline = time.monotonic() + timeout

        while time.monotonic() < deadline:
            try:
                receipt = await loop.run_in_executor(
                    None,
                    self._w3.eth.get_transaction_receipt,
                    tx_hash,
                )
                if receipt is not None:
                    return dict(receipt)
            except TransactionNotFound:
                pass
            except Exception as exc:
                logger.warning(
                    "Receipt poll error tx=%s: %s", tx_hash, exc
                )

            await asyncio.sleep(self._receipt_poll_interval)

        logger.error(
            "Receipt timeout after %ds for tx=%s", timeout, tx_hash
        )
        return None

    # ------------------------------------------------------------------ #
    # Nonce management
    # ------------------------------------------------------------------ #

    async def _get_nonce(self) -> int:
        """Return the next nonce, combining local tracking with on-chain sync.

        The local counter avoids hitting the RPC for every transaction in a
        burst.  If the local value falls behind the on-chain count (e.g.
        after a restart or external wallet usage) we re-sync.
        """
        loop = asyncio.get_event_loop()

        async with self._nonce_lock:
            on_chain_nonce: int = await loop.run_in_executor(
                None,
                self._w3.eth.get_transaction_count,
                self._account_address,
                "pending",
            )

            if self._nonce is None or self._nonce < on_chain_nonce:
                self._nonce = on_chain_nonce
                logger.debug(
                    "Nonce synced from chain: %d", self._nonce
                )
            else:
                logger.debug(
                    "Using local nonce: %d (chain=%d)",
                    self._nonce,
                    on_chain_nonce,
                )

            nonce = self._nonce
            self._nonce += 1
            return nonce

    # ------------------------------------------------------------------ #
    # Token approval helper
    # ------------------------------------------------------------------ #

    async def _ensure_token_approval(
        self,
        token_address: str,
        spender: Optional[str],
        amount: int,
        intent_id: str,
    ) -> None:
        """Check the ERC-20 allowance and send an approval tx if needed.

        If ``spender`` is ``None`` the check is skipped (deferred until the
        0x quote returns the ``allowanceTarget``).
        """
        if spender is None:
            return

        loop = asyncio.get_event_loop()
        spender = Web3.to_checksum_address(spender)

        contract = self._w3.eth.contract(
            address=Web3.to_checksum_address(token_address),
            abi=ERC20_ABI,
        )

        current_allowance: int = await loop.run_in_executor(
            None,
            contract.functions.allowance(
                self._account_address, spender
            ).call,
        )

        if current_allowance >= amount:
            logger.debug(
                "Allowance sufficient token=%s spender=%s allowance=%d",
                token_address,
                spender,
                current_allowance,
            )
            return

        logger.info(
            "Approving token=%s spender=%s intent=%s",
            token_address,
            spender,
            intent_id,
        )

        # Build approval transaction (max approval to avoid repeated approvals)
        nonce = await self._get_nonce()

        latest_block = await loop.run_in_executor(
            None, self._w3.eth.get_block, "latest"
        )
        base_fee = latest_block.get("baseFeePerGas", 0)
        try:
            max_priority = await loop.run_in_executor(
                None, self._w3.eth.max_priority_fee
            )
        except Exception:
            max_priority = Web3.to_wei(1.5, "gwei")
        max_priority = max(max_priority, Web3.to_wei(0.1, "gwei"))
        max_fee_per_gas = 2 * base_fee + max_priority

        approve_tx = contract.functions.approve(
            spender, MAX_UINT256
        ).build_transaction(
            {
                "chainId": self._chain_id,
                "from": self._account_address,
                "nonce": nonce,
                "gas": 60_000,
                "maxFeePerGas": max_fee_per_gas,
                "maxPriorityFeePerGas": max_priority,
                "type": 2,
            }
        )

        tx_hash = await self._sign_and_send(approve_tx)
        logger.info(
            "Approval tx sent token=%s tx=%s intent=%s",
            token_address,
            tx_hash,
            intent_id,
        )

        receipt = await self._wait_for_receipt(tx_hash, timeout=60)
        if receipt is None or receipt.get("status") != 1:
            raise RuntimeError(
                f"Token approval failed for {token_address} "
                f"(tx={tx_hash}, receipt={receipt})"
            )

        logger.info(
            "Approval confirmed token=%s tx=%s", token_address, tx_hash
        )

    # ------------------------------------------------------------------ #
    # Helpers
    # ------------------------------------------------------------------ #

    @staticmethod
    def _to_wei(amount: Any) -> int:
        """Convert an ETH amount (float or string) to wei.

        If the value is already an integer > 1e15 it is assumed to already
        be in wei.
        """
        val = float(amount)
        if val > 1e15:
            # Already in wei
            return int(val)
        return int(Web3.to_wei(val, "ether"))

    @staticmethod
    def _to_token_units(amount: Any) -> int:
        """Convert a raw token amount to integer units.

        Expects the caller to have already resolved decimals -- values are
        passed through as-is (converted to int).
        """
        val = float(amount)
        return int(val)
