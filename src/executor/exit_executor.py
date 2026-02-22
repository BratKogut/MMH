"""Exit Executor — handles closing positions (TP/SL/timeout/manual sell).

Determines which chain executor to use based on the position chain,
builds a SELL intent, and delegates to the appropriate executor.
"""

import logging
import time
from typing import Optional

logger = logging.getLogger(__name__)


class ExitExecutor:
    """Manages position exits by delegating to chain-specific executors.

    Listens on: positions:exit_signals
    Uses: SolanaExecutor / EVMExecutor for actual swap
    Updates: positions table + publishes position:updates
    """

    def __init__(self, solana_executor, evm_executors: dict, bus, position_manager, dry_run: bool = True):
        self._solana = solana_executor
        self._evm = evm_executors  # chain_name -> EVMExecutor
        self._bus = bus
        self._position_manager = position_manager
        self._dry_run = dry_run
        self._running = False

    async def start(self):
        """Start consuming exit signals."""
        import asyncio

        self._running = True
        stream = "positions:exit_signals"
        group = "exit_executor"
        consumer = "exit-main"

        await self._bus.ensure_consumer_group(stream, group)

        while self._running:
            try:
                messages = await self._bus.consume(stream, group, consumer, count=5, block_ms=2000)
                for msg_id, data in messages:
                    await self._handle_exit(data)
                    await self._bus.ack(stream, group, msg_id)
            except Exception as e:
                logger.error(f"ExitExecutor error: {e}")
                await asyncio.sleep(1)

    async def stop(self):
        self._running = False

    async def _handle_exit(self, signal_data: dict):
        """Handle a single exit signal."""
        position_id = signal_data.get("position_id", "")
        chain = signal_data.get("chain", "")
        token_address = signal_data.get("token_address", "")
        reason = signal_data.get("reason", "manual")
        amount_tokens = signal_data.get("amount_tokens", "0")

        logger.info(f"Exit signal: position={position_id} chain={chain} reason={reason}")

        if self._dry_run:
            logger.info(f"[DRY-RUN] Would sell {amount_tokens} of {token_address} on {chain}")
            await self._publish_exit_result(position_id, chain, token_address, reason, "DRY_RUN", "")
            return

        sell_intent = {
            "intent_id": f"exit:{position_id}:{int(time.time())}",
            "chain": chain,
            "token_address": token_address,
            "side": "SELL",
            "amount_tokens": str(amount_tokens),
            "position_id": position_id,
            "reason": reason,
        }

        executor = self._get_executor(chain)
        if not executor:
            logger.error(f"No executor for chain {chain}")
            await self._publish_exit_result(position_id, chain, token_address, reason, "ERROR", "no_executor")
            return

        try:
            from src.executor.executor import ExecutionStatus
            result = await executor.execute(sell_intent)

            if result.status == ExecutionStatus.SUCCESS:
                await self._publish_exit_result(
                    position_id, chain, token_address, reason, "SUCCESS", result.tx_hash
                )
            else:
                logger.error(f"Exit execution failed: {result.error}")
                await self._publish_exit_result(
                    position_id, chain, token_address, reason, "FAILED", str(result.error)
                )
        except Exception as e:
            logger.error(f"Exit execution error: {e}")
            await self._publish_exit_result(position_id, chain, token_address, reason, "ERROR", str(e))

    def _get_executor(self, chain: str):
        """Get the appropriate chain executor."""
        if chain == "solana":
            return self._solana
        return self._evm.get(chain)

    async def _publish_exit_result(self, position_id: str, chain: str,
                                   token_address: str, reason: str,
                                   status: str, tx_hash_or_error: str):
        """Publish exit result to position:updates stream."""
        update_data = {
            "event_type": "PositionExitResult",
            "position_id": position_id,
            "chain": chain,
            "token_address": token_address,
            "exit_reason": reason,
            "exit_status": status,
            "tx_hash": tx_hash_or_error if status == "SUCCESS" else "",
            "error": tx_hash_or_error if status != "SUCCESS" else "",
            "event_time": str(time.time()),
        }
        try:
            await self._bus.publish("position:updates", update_data)
        except Exception as e:
            logger.error(f"Failed to publish exit result: {e}")
