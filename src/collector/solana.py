"""Solana Collector - multi-source token discovery.

3-layer discovery:
1. PumpPortal WebSocket (bonding curve tokens, ~200ms)
2. Helius WebSocket (Raydium/Orca pool creation)
3. Helius Webhooks (fallback)
"""

import asyncio
import aiohttp
import json
import logging
import time
from typing import Optional

from .base import BaseCollector

logger = logging.getLogger(__name__)


class SolanaCollector(BaseCollector):
    """Solana chain collector with multi-source discovery."""

    def __init__(self, bus, wal, config: dict):
        super().__init__("solana", bus, wal, config)
        self._pumpportal_ws_url = config.get("pumpportal_ws_url", "wss://pumpportal.fun/api/data")
        self._helius_ws_url = config.get("helius_ws_url", "")
        self._ws_connections = []

    async def _connect(self):
        """Connect to PumpPortal and Helius WebSockets."""
        logger.info("Solana collector connecting to data sources...")

    async def _disconnect(self):
        """Close all WebSocket connections."""
        for ws in self._ws_connections:
            try:
                await ws.close()
            except Exception:
                pass
        self._ws_connections.clear()

    async def _collect_loop(self):
        """Run multiple WS listeners concurrently."""
        tasks = [
            asyncio.create_task(self._pumpportal_listener()),
        ]
        if self._helius_ws_url:
            tasks.append(asyncio.create_task(self._helius_listener()))

        # Heartbeat task
        tasks.append(asyncio.create_task(self._heartbeat_loop()))

        await asyncio.gather(*tasks, return_exceptions=True)

    async def _pumpportal_listener(self):
        """Listen to PumpPortal WebSocket for new tokens."""
        import aiohttp

        retry_count = 0
        max_retry = 10

        while self._running and not self._frozen:
            try:
                async with aiohttp.ClientSession() as session:
                    async with session.ws_connect(self._pumpportal_ws_url) as ws:
                        logger.info("Connected to PumpPortal WebSocket")
                        retry_count = 0

                        # Subscribe to new tokens
                        await ws.send_json({"method": "subscribeNewToken"})

                        async for msg in ws:
                            if not self._running:
                                break
                            if msg.type == aiohttp.WSMsgType.TEXT:
                                try:
                                    data = json.loads(msg.data)
                                    event = self._parse_pumpportal_event(data)
                                    if event:
                                        await self._process_event(event)
                                except json.JSONDecodeError:
                                    logger.warning("Invalid JSON from PumpPortal")
                            elif msg.type in (aiohttp.WSMsgType.ERROR, aiohttp.WSMsgType.CLOSED):
                                break

            except Exception as e:
                retry_count += 1
                if retry_count > max_retry:
                    logger.error(f"PumpPortal max retries exceeded, freezing")
                    await self._freeze(f"pumpportal_connection_failed:retries={retry_count}")
                    return
                wait = min(60, 2 ** retry_count)
                logger.error(f"PumpPortal connection error: {e}, retry in {wait}s")
                await asyncio.sleep(wait)

    async def _helius_listener(self):
        """Listen to Helius WebSocket for Raydium/Orca pool creation."""
        import aiohttp

        while self._running and not self._frozen:
            try:
                async with aiohttp.ClientSession() as session:
                    async with session.ws_connect(self._helius_ws_url) as ws:
                        logger.info("Connected to Helius WebSocket")

                        # Subscribe to Raydium program logs
                        await ws.send_json({
                            "jsonrpc": "2.0",
                            "id": 1,
                            "method": "logsSubscribe",
                            "params": [
                                {"mentions": ["675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8"]},
                                {"commitment": "confirmed"}
                            ]
                        })

                        async for msg in ws:
                            if not self._running:
                                break
                            if msg.type == aiohttp.WSMsgType.TEXT:
                                try:
                                    data = json.loads(msg.data)
                                    event = self._parse_helius_event(data)
                                    if event:
                                        await self._process_event(event)
                                except json.JSONDecodeError:
                                    pass
                            elif msg.type in (aiohttp.WSMsgType.ERROR, aiohttp.WSMsgType.CLOSED):
                                break
            except Exception as e:
                logger.error(f"Helius WS error: {e}")
                await asyncio.sleep(5)

    async def _heartbeat_loop(self):
        """Emit periodic heartbeats."""
        while self._running:
            await self._emit_heartbeat()
            await asyncio.sleep(10)

    def _parse_event(self, raw_data: dict) -> Optional[dict]:
        """Generic parse (dispatch to specific parsers)."""
        return raw_data

    def _parse_pumpportal_event(self, data: dict) -> Optional[dict]:
        """Parse PumpPortal new token event."""
        if not data.get("mint"):
            return None

        return {
            "event_type": "TokenDiscovered",
            "event_id": f"sol:{data.get('signature', '')}",
            "chain": "solana",
            "address": data.get("mint", ""),
            "name": data.get("name", ""),
            "symbol": data.get("symbol", ""),
            "creator": data.get("traderPublicKey", ""),
            "launchpad": "pump.fun",
            "tx_hash": data.get("signature", ""),
            "initial_liquidity_usd": str(data.get("initialBuy", 0)),
            "bonding_curve_progress": "0",
            "pool_address": data.get("bondingCurveKey", ""),
            "finality_status": "confirmed",
            "event_time": str(time.time()),
            "source": "pumpportal",
        }

    def _parse_helius_event(self, data: dict) -> Optional[dict]:
        """Parse Helius WebSocket log event."""
        result = data.get("result") or data.get("params", {}).get("result")
        if not result:
            return None

        value = result.get("value", {})
        signature = value.get("signature", "")
        logs = value.get("logs", [])

        # Check for pool initialization
        is_pool_init = any("InitializeInstruction" in log or "initialize2" in log for log in logs)
        if not is_pool_init:
            return None

        return {
            "event_type": "TokenDiscovered",
            "event_id": f"sol:{signature}",
            "chain": "solana",
            "address": "",  # Needs parsing from TX
            "name": "",
            "symbol": "",
            "creator": "",
            "launchpad": "raydium",
            "tx_hash": signature,
            "finality_status": "confirmed",
            "event_time": str(time.time()),
            "source": "helius",
        }
