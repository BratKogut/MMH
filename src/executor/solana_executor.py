"""Solana chain executor -- Jupiter V6 swap execution with Jito MEV protection.

Implements the actual on-chain swap execution for Solana via the Jupiter V6
aggregator API.  Designed to be registered as a chain executor in the
top-level ``Executor`` class (see ``executor.py``).

Flow:
    1. Receive ApprovedIntent (intent_data dict)
    2. Determine input/output mints based on side (BUY / SELL)
    3. Fetch Jupiter V6 quote (GET /quote)
    4. Request serialized swap transaction (POST /swap)
    5. Sign transaction with wallet
    6. (Optional) Submit as a Jito bundle for MEV protection
    7. Otherwise submit directly to RPC with priority fee
    8. Confirm transaction with polling
    9. Return ExecutionResult

Constants:
    SOL_MINT            -- Native SOL wrapped mint address
    JUPITER_QUOTE_URL   -- Jupiter V6 quote endpoint
    JUPITER_SWAP_URL    -- Jupiter V6 swap endpoint
    JITO_BUNDLE_URL     -- Jito block-engine bundle submission

CRITICAL: This module never retries on its own -- the parent ``Executor``
handles retry logic, circuit breaking, and idempotency.
"""

import asyncio
import base64
import logging
import time
from typing import Optional

import aiohttp

from src.executor.executor import ExecutionResult, ExecutionStatus

logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
SOL_MINT = "So11111111111111111111111111111111"
JUPITER_QUOTE_URL = "https://quote-api.jup.ag/v6/quote"
JUPITER_SWAP_URL = "https://quote-api.jup.ag/v6/swap"
JITO_BUNDLE_URL = "https://mainnet.block-engine.jito.wtf/api/v1/bundles"

# Lamports per SOL
LAMPORTS_PER_SOL = 1_000_000_000

# Default compute-unit price in micro-lamports for priority fees
DEFAULT_PRIORITY_FEE_MICRO_LAMPORTS = 50_000


class SolanaExecutor:
    """Chain-specific executor for Solana swaps via Jupiter V6.

    Intended to be used as::

        solana_exec = SolanaExecutor(rpc_url, wallet_manager, settings)
        executor = Executor(
            bus, redis, config,
            chain_executors={"solana": solana_exec.execute},
        )

    Parameters
    ----------
    rpc_url:
        Solana JSON-RPC endpoint (e.g. Helius, Triton, QuickNode).
    wallet_manager:
        Object that exposes ``public_key`` (str) and
        ``sign_transaction(tx_bytes: bytes) -> bytes`` async method.
    settings:
        Dict (or MMHSettings-like) with at least:
          - max_slippage_bps (int): maximum slippage in basis points
          - jito_tip_lamports (int): Jito tip; 0 disables Jito bundles
          - priority_fee_micro_lamports (int, optional): compute-unit price
          - tx_confirm_timeout (int, optional): confirmation timeout seconds
    """

    def __init__(self, rpc_url: str, wallet_manager, settings: dict):
        self._rpc_url = rpc_url
        self._wallet = wallet_manager
        self._settings = settings

        # Execution tunables
        self._slippage_bps: int = int(settings.get("max_slippage_bps", 300))
        self._jito_tip_lamports: int = int(settings.get("jito_tip_lamports", 0))
        self._priority_fee: int = int(
            settings.get("priority_fee_micro_lamports", DEFAULT_PRIORITY_FEE_MICRO_LAMPORTS)
        )
        self._confirm_timeout: int = int(settings.get("tx_confirm_timeout", 60))
        self._confirm_poll_interval: float = 1.5

    # ------------------------------------------------------------------
    # Public entry point
    # ------------------------------------------------------------------

    async def execute(self, intent_data: dict) -> ExecutionResult:
        """Execute a swap for the given approved intent.

        Parameters
        ----------
        intent_data:
            Must contain at minimum:
              - intent_id (str)
              - chain (str) -- should be "solana"
              - token_address (str) -- the SPL token mint
              - side (str) -- "BUY" or "SELL"
              - amount_tokens (float, optional) -- required for SELL;
                for BUY the amount is determined by position sizing
                upstream (amount_sol field expected)

        Returns
        -------
        ExecutionResult
        """
        intent_id: str = intent_data.get("intent_id", "")
        token_address: str = intent_data.get("token_address", "")
        side: str = intent_data.get("side", "BUY").upper()

        logger.info(
            "SolanaExecutor.execute | intent=%s side=%s token=%s",
            intent_id, side, token_address,
        )

        try:
            # ---- Determine mints and amount ----
            if side == "BUY":
                input_mint = SOL_MINT
                output_mint = token_address
                # Amount in lamports (SOL -> lamports)
                amount_sol = float(intent_data.get("amount_sol", 0))
                if amount_sol <= 0:
                    return ExecutionResult(
                        intent_id=intent_id,
                        status=ExecutionStatus.FAILED,
                        error="missing_or_zero_amount_sol_for_buy",
                    )
                amount_raw = int(amount_sol * LAMPORTS_PER_SOL)
            elif side == "SELL":
                input_mint = token_address
                output_mint = SOL_MINT
                amount_tokens = float(intent_data.get("amount_tokens", 0))
                decimals = int(intent_data.get("token_decimals", 6))
                if amount_tokens <= 0:
                    return ExecutionResult(
                        intent_id=intent_id,
                        status=ExecutionStatus.FAILED,
                        error="missing_or_zero_amount_tokens_for_sell",
                    )
                amount_raw = int(amount_tokens * (10 ** decimals))
            else:
                return ExecutionResult(
                    intent_id=intent_id,
                    status=ExecutionStatus.FAILED,
                    error=f"unknown_side:{side}",
                )

            # ---- Step 1: Get Jupiter quote ----
            quote = await self._get_quote(input_mint, output_mint, amount_raw, self._slippage_bps)
            if quote is None:
                return ExecutionResult(
                    intent_id=intent_id,
                    status=ExecutionStatus.FAILED,
                    error="jupiter_quote_failed",
                )

            # Extract quote details for the result
            in_amount = int(quote.get("inAmount", 0))
            out_amount = int(quote.get("outAmount", 0))
            price_impact_pct = float(quote.get("priceImpactPct", 0))
            slippage_bps_used = int(quote.get("slippageBps", self._slippage_bps))

            logger.info(
                "Jupiter quote | intent=%s in=%s out=%s impact=%.4f%% slippage=%dbps",
                intent_id, in_amount, out_amount, price_impact_pct, slippage_bps_used,
            )

            # Slippage guard: reject if price impact exceeds configured max
            max_impact_pct = self._slippage_bps / 100.0  # convert bps to %
            if price_impact_pct > max_impact_pct:
                return ExecutionResult(
                    intent_id=intent_id,
                    status=ExecutionStatus.FAILED,
                    error=f"price_impact_too_high:{price_impact_pct:.4f}%>{max_impact_pct:.2f}%",
                    price_impact_pct=price_impact_pct,
                )

            # ---- Step 2: Get serialized swap transaction ----
            user_pubkey = self._wallet.public_key
            tx_hash = await self._execute_swap(quote, user_pubkey)

            if not tx_hash:
                return ExecutionResult(
                    intent_id=intent_id,
                    status=ExecutionStatus.FAILED,
                    error="swap_submission_failed",
                )

            logger.info(
                "Transaction submitted | intent=%s tx=%s",
                intent_id, tx_hash,
            )

            # ---- Step 3: Confirm transaction ----
            confirmed = await self._confirm_transaction(tx_hash, timeout=self._confirm_timeout)

            if not confirmed:
                return ExecutionResult(
                    intent_id=intent_id,
                    status=ExecutionStatus.SUBMITTED,
                    tx_hash=tx_hash,
                    amount_in=in_amount / LAMPORTS_PER_SOL if side == "BUY" else in_amount,
                    amount_out=out_amount / LAMPORTS_PER_SOL if side == "SELL" else out_amount,
                    price_impact_pct=price_impact_pct,
                    error="confirmation_timeout",
                )

            # ---- Build successful result ----
            # Normalize amounts to human-readable
            if side == "BUY":
                amount_in_display = in_amount / LAMPORTS_PER_SOL
                amount_out_display = out_amount  # raw token units (caller has decimals)
                price = amount_in_display / out_amount if out_amount else 0
            else:
                amount_in_display = in_amount  # raw token units
                amount_out_display = out_amount / LAMPORTS_PER_SOL
                price = amount_out_display / in_amount if in_amount else 0

            return ExecutionResult(
                intent_id=intent_id,
                status=ExecutionStatus.CONFIRMED,
                tx_hash=tx_hash,
                amount_in=amount_in_display,
                amount_out=amount_out_display,
                price=price,
                price_impact_pct=price_impact_pct,
            )

        except aiohttp.ClientError as e:
            logger.error("HTTP error during execution | intent=%s error=%s", intent_id, e)
            return ExecutionResult(
                intent_id=intent_id,
                status=ExecutionStatus.FAILED,
                error=f"http_error:{e}",
            )
        except Exception as e:
            logger.exception("Unexpected error during execution | intent=%s", intent_id)
            return ExecutionResult(
                intent_id=intent_id,
                status=ExecutionStatus.FAILED,
                error=f"unexpected:{e}",
            )

    # ------------------------------------------------------------------
    # Jupiter V6 API helpers
    # ------------------------------------------------------------------

    async def _get_quote(
        self,
        input_mint: str,
        output_mint: str,
        amount: int,
        slippage_bps: int,
    ) -> Optional[dict]:
        """Fetch a swap quote from Jupiter V6.

        Parameters
        ----------
        input_mint:  SPL mint address of the token being sold.
        output_mint: SPL mint address of the token being bought.
        amount:      Raw integer amount of ``input_mint`` to swap.
        slippage_bps: Maximum slippage in basis points.

        Returns
        -------
        dict with Jupiter quote response, or ``None`` on failure.
        """
        params = {
            "inputMint": input_mint,
            "outputMint": output_mint,
            "amount": str(amount),
            "slippageBps": str(slippage_bps),
        }

        try:
            async with aiohttp.ClientSession() as session:
                async with session.get(
                    JUPITER_QUOTE_URL,
                    params=params,
                    timeout=aiohttp.ClientTimeout(total=10),
                ) as resp:
                    if resp.status != 200:
                        body = await resp.text()
                        logger.error(
                            "Jupiter quote error | status=%d body=%s",
                            resp.status, body[:500],
                        )
                        return None
                    data = await resp.json()
                    return data
        except Exception as e:
            logger.error("Jupiter quote request failed: %s", e)
            return None

    async def _execute_swap(self, quote: dict, user_pubkey: str) -> str:
        """Request a serialized swap transaction from Jupiter V6, sign it, and
        submit to the network.

        If Jito tips are configured (``jito_tip_lamports > 0``), the signed
        transaction is submitted as a Jito bundle instead of directly to RPC.

        Parameters
        ----------
        quote:        The full Jupiter quote response dict.
        user_pubkey:  The wallet's base-58 public key.

        Returns
        -------
        Transaction signature (base-58 string), or empty string on failure.
        """
        # -- Request serialized transaction from Jupiter --
        swap_payload = {
            "quoteResponse": quote,
            "userPublicKey": user_pubkey,
            "wrapAndUnwrapSol": True,
            "computeUnitPriceMicroLamports": self._priority_fee,
        }

        try:
            async with aiohttp.ClientSession() as session:
                async with session.post(
                    JUPITER_SWAP_URL,
                    json=swap_payload,
                    timeout=aiohttp.ClientTimeout(total=15),
                ) as resp:
                    if resp.status != 200:
                        body = await resp.text()
                        logger.error(
                            "Jupiter swap error | status=%d body=%s",
                            resp.status, body[:500],
                        )
                        return ""
                    swap_data = await resp.json()
        except Exception as e:
            logger.error("Jupiter swap request failed: %s", e)
            return ""

        swap_transaction_b64: str = swap_data.get("swapTransaction", "")
        if not swap_transaction_b64:
            logger.error("Jupiter returned empty swapTransaction")
            return ""

        # -- Decode and sign --
        try:
            tx_bytes = base64.b64decode(swap_transaction_b64)
            signed_tx_bytes = await self._wallet.sign_transaction(tx_bytes)
        except Exception as e:
            logger.error("Transaction signing failed: %s", e)
            return ""

        # -- Submit (Jito bundle or direct RPC) --
        if self._jito_tip_lamports > 0:
            tx_hash = await self._submit_jito_bundle(signed_tx_bytes, self._jito_tip_lamports)
            if tx_hash:
                return tx_hash
            # Fall through to direct submission if Jito fails
            logger.warning("Jito bundle submission failed, falling back to direct RPC")

        # Direct RPC submission
        tx_hash = await self._send_raw_transaction(signed_tx_bytes)
        return tx_hash

    async def _send_raw_transaction(self, signed_tx_bytes: bytes) -> str:
        """Submit a signed transaction directly to the Solana RPC.

        Returns the transaction signature (base-58) or empty string on failure.
        """
        tx_b64 = base64.b64encode(signed_tx_bytes).decode("ascii")
        rpc_payload = {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "sendTransaction",
            "params": [
                tx_b64,
                {
                    "encoding": "base64",
                    "skipPreflight": False,
                    "preflightCommitment": "confirmed",
                    "maxRetries": 3,
                },
            ],
        }

        try:
            async with aiohttp.ClientSession() as session:
                async with session.post(
                    self._rpc_url,
                    json=rpc_payload,
                    timeout=aiohttp.ClientTimeout(total=15),
                ) as resp:
                    result = await resp.json()

                    if "error" in result:
                        err = result["error"]
                        logger.error("RPC sendTransaction error: %s", err)
                        return ""

                    tx_hash = result.get("result", "")
                    return tx_hash
        except Exception as e:
            logger.error("sendTransaction RPC call failed: %s", e)
            return ""

    async def _confirm_transaction(self, tx_hash: str, timeout: int = 60) -> bool:
        """Poll the RPC for transaction confirmation.

        Parameters
        ----------
        tx_hash: The base-58 transaction signature.
        timeout: Maximum seconds to wait for confirmation.

        Returns
        -------
        ``True`` if the transaction reached ``confirmed`` commitment,
        ``False`` if it timed out or encountered an error.
        """
        deadline = time.monotonic() + timeout

        rpc_payload = {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "getSignatureStatuses",
            "params": [
                [tx_hash],
                {"searchTransactionHistory": True},
            ],
        }

        while time.monotonic() < deadline:
            try:
                async with aiohttp.ClientSession() as session:
                    async with session.post(
                        self._rpc_url,
                        json=rpc_payload,
                        timeout=aiohttp.ClientTimeout(total=10),
                    ) as resp:
                        result = await resp.json()

                statuses = result.get("result", {}).get("value", [])
                if statuses and statuses[0] is not None:
                    status = statuses[0]
                    err = status.get("err")
                    if err is not None:
                        logger.error(
                            "Transaction failed on-chain | tx=%s err=%s",
                            tx_hash, err,
                        )
                        return False

                    confirmation = status.get("confirmationStatus", "")
                    if confirmation in ("confirmed", "finalized"):
                        logger.info(
                            "Transaction confirmed | tx=%s status=%s",
                            tx_hash, confirmation,
                        )
                        return True

            except Exception as e:
                logger.warning("Confirmation poll error: %s", e)

            await asyncio.sleep(self._confirm_poll_interval)

        logger.warning("Transaction confirmation timed out | tx=%s timeout=%ds", tx_hash, timeout)
        return False

    async def _submit_jito_bundle(self, tx_bytes: bytes, tip_lamports: int) -> str:
        """Submit a signed transaction as a Jito bundle for MEV protection.

        Jito bundles allow the transaction to be included by a Jito-powered
        validator with a tip, providing frontrunning protection.

        Parameters
        ----------
        tx_bytes:       The fully signed transaction bytes.
        tip_lamports:   Tip amount in lamports for the Jito validator.

        Returns
        -------
        Transaction signature string, or empty string on failure.
        """
        tx_b64 = base64.b64encode(tx_bytes).decode("ascii")

        bundle_payload = {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "sendBundle",
            "params": [
                [tx_b64],
                {
                    "tipLamports": tip_lamports,
                },
            ],
        }

        try:
            async with aiohttp.ClientSession() as session:
                async with session.post(
                    JITO_BUNDLE_URL,
                    json=bundle_payload,
                    timeout=aiohttp.ClientTimeout(total=15),
                ) as resp:
                    if resp.status != 200:
                        body = await resp.text()
                        logger.error(
                            "Jito bundle error | status=%d body=%s",
                            resp.status, body[:500],
                        )
                        return ""

                    result = await resp.json()

                    if "error" in result:
                        logger.error("Jito bundle RPC error: %s", result["error"])
                        return ""

                    bundle_id = result.get("result", "")
                    logger.info(
                        "Jito bundle submitted | bundle_id=%s tip=%d lamports",
                        bundle_id, tip_lamports,
                    )
                    # The bundle ID is not the tx hash; we still need the tx
                    # signature for confirmation.  For single-tx bundles the
                    # first transaction's signature is typically used.  We
                    # extract it from the signed bytes via the wallet manager.
                    tx_hash = getattr(self._wallet, "last_signature", "") or bundle_id
                    return tx_hash

        except Exception as e:
            logger.error("Jito bundle submission failed: %s", e)
            return ""
