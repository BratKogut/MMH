"""EVM Collector for Base chain.

Monitors:
- Uniswap V3 PoolCreated events
- Aerodrome V2 PoolCreated events

Tags events with: block_number, block_hash, tx_hash, log_index, finality
On reorg: emits reorg_notice + corrections downstream
"""

import asyncio
import json
import logging
import time
from typing import Optional

from .base import BaseCollector

logger = logging.getLogger(__name__)

# Event topic hashes
UNISWAP_V3_POOL_CREATED = "0x783cca1c0412dd0d695e784568c96da2e9c22ff989357a2e8b1d9b2b4e6b7118"
AERODROME_POOL_CREATED = "0x2128d88d14c80cb081c1252a5acff7a264671bf199ce226b53788fb26065005e"


class EVMCollector(BaseCollector):
    """Base/EVM chain collector monitoring DEX pool creation events."""

    def __init__(self, chain: str, bus, wal, config: dict):
        super().__init__(chain, bus, wal, config)
        self._ws_url = config.get(f"{chain}_ws_url", "")
        self._rpc_url = config.get(f"{chain}_rpc_url", "")
        self._ws = None
        self._subscription_id = None

    async def _connect(self):
        """Connect to EVM WebSocket node."""
        logger.info(f"EVM collector ({self._chain}) connecting...")

    async def _disconnect(self):
        if self._ws:
            try:
                await self._ws.close()
            except Exception:
                pass

    async def _collect_loop(self):
        """Subscribe to logs and process events."""
        import aiohttp

        retry_count = 0

        while self._running and not self._frozen:
            try:
                async with aiohttp.ClientSession() as session:
                    async with session.ws_connect(self._ws_url) as ws:
                        self._ws = ws
                        logger.info(f"Connected to {self._chain} WebSocket")
                        retry_count = 0

                        # Subscribe to pool creation logs
                        await ws.send_json({
                            "jsonrpc": "2.0",
                            "id": 1,
                            "method": "eth_subscribe",
                            "params": ["logs", {
                                "topics": [[UNISWAP_V3_POOL_CREATED, AERODROME_POOL_CREATED]]
                            }]
                        })

                        # Start heartbeat
                        heartbeat_task = asyncio.create_task(self._heartbeat_loop())

                        async for msg in ws:
                            if not self._running:
                                break
                            if msg.type == aiohttp.WSMsgType.TEXT:
                                try:
                                    data = json.loads(msg.data)
                                    event = self._parse_event(data)
                                    if event:
                                        await self._process_event(event)
                                except json.JSONDecodeError:
                                    pass
                            elif msg.type in (aiohttp.WSMsgType.ERROR, aiohttp.WSMsgType.CLOSED):
                                break

                        heartbeat_task.cancel()

            except Exception as e:
                retry_count += 1
                wait = min(60, 2 ** retry_count)
                logger.error(f"EVM WS ({self._chain}) error: {e}, retry in {wait}s")
                await asyncio.sleep(wait)

    async def _heartbeat_loop(self):
        while self._running:
            await self._emit_heartbeat()
            await asyncio.sleep(10)

    def _parse_event(self, raw_data: dict) -> Optional[dict]:
        """Parse EVM log event into TokenDiscovered."""
        params = raw_data.get("params", {})
        result = params.get("result", {})

        if not result.get("topics"):
            return None

        topics = result.get("topics", [])
        if not topics:
            return None

        topic0 = topics[0] if topics else ""

        # Determine DEX
        if topic0 == UNISWAP_V3_POOL_CREATED:
            dex = "uniswap_v3"
        elif topic0 == AERODROME_POOL_CREATED:
            dex = "aerodrome"
        else:
            return None

        # Extract addresses from topics
        token0 = "0x" + topics[1][-40:] if len(topics) > 1 else ""
        token1 = "0x" + topics[2][-40:] if len(topics) > 2 else ""

        block_number = int(result.get("blockNumber", "0x0"), 16)
        log_index = int(result.get("logIndex", "0x0"), 16)

        return {
            "event_type": "TokenDiscovered",
            "event_id": f"{self._chain}:{result.get('transactionHash', '')}:{log_index}",
            "chain": self._chain,
            "address": token0,  # Primary token
            "token1": token1,
            "name": "",
            "symbol": "",
            "creator": "",
            "launchpad": dex,
            "dex": dex,
            "pool_address": result.get("address", ""),
            "tx_hash": result.get("transactionHash", ""),
            "block_number": str(block_number),
            "block_hash": result.get("blockHash", ""),
            "log_index": str(log_index),
            "finality_status": "pending",
            "event_time": str(time.time()),
            "source": f"{self._chain}_ws",
        }
