"""Base Collector - single-writer per chain.

Rules:
- Single-writer on chain (if HA: leader election)
- WAL append -> THEN publish to Redis
- WAL fail -> FREEZE chain immediately
- Publish fail -> retry (event safe in WAL)

Finality/reorg handling for EVM:
- Tag events: block_number, block_hash, tx_hash, log_index, finality
- On reorg: emit reorg_notice + corrections downstream
"""

import asyncio
import logging
import time
from abc import ABC, abstractmethod
from typing import Optional

logger = logging.getLogger(__name__)


class BaseCollector(ABC):
    """Base collector class - single-writer pattern per chain.

    Subclasses implement chain-specific connection and event parsing.
    Base class handles: WAL writing, Redis publishing, reorg detection.
    """

    def __init__(self, chain: str, bus, wal, config: dict):
        self._chain = chain
        self._bus = bus  # RedisEventBus
        self._wal = wal  # RawWAL (can be None for testing)
        self._config = config
        self._running = False
        self._frozen = False
        self._sequence = 0
        self._last_block_number: Optional[int] = None
        self._last_block_hash: Optional[str] = None

        # Metrics
        self._events_collected = 0
        self._wal_writes = 0
        self._publish_failures = 0
        self._reorgs_detected = 0

    @property
    def chain(self) -> str:
        return self._chain

    @property
    def is_frozen(self) -> bool:
        return self._frozen

    async def start(self):
        """Start collector. Connects to data source and begins collecting."""
        self._running = True
        logger.info(f"Collector starting for chain {self._chain}")

        try:
            await self._connect()
            await self._collect_loop()
        except Exception as e:
            logger.error(f"Collector {self._chain} fatal error: {e}")
            await self._freeze(f"fatal_error:{e}")

    async def stop(self):
        """Stop collector gracefully."""
        self._running = False
        await self._disconnect()
        logger.info(f"Collector stopped for chain {self._chain}")

    @abstractmethod
    async def _connect(self):
        """Connect to chain data source (WS, RPC, etc.)."""
        pass

    @abstractmethod
    async def _disconnect(self):
        """Disconnect from data source."""
        pass

    @abstractmethod
    async def _collect_loop(self):
        """Main collection loop. Must call self._process_event() for each event."""
        pass

    @abstractmethod
    def _parse_event(self, raw_data: dict) -> Optional[dict]:
        """Parse raw data into standardized event dict. Returns None to skip."""
        pass

    async def _process_event(self, event_data: dict):
        """Process a single event: WAL write -> Redis publish.

        This is THE critical path. WAL must succeed before publish.
        """
        if self._frozen:
            logger.warning(f"Collector {self._chain} is FROZEN, dropping event")
            return

        # Add collector metadata
        event_data["chain"] = self._chain
        event_data["ingest_time"] = str(time.time())
        event_data["sequence"] = str(self._sequence)
        self._sequence += 1

        # Check for reorg (EVM chains)
        if event_data.get("block_number") and self._last_block_number:
            new_block = int(event_data["block_number"])
            if new_block <= self._last_block_number:
                await self._handle_reorg(event_data)

        # Step 1: WAL append (MUST succeed)
        if self._wal:
            try:
                import json
                event_bytes = json.dumps(event_data).encode()
                event_id = event_data.get("event_id", f"{self._chain}:{self._sequence}")
                event_type = event_data.get("event_type", "unknown")
                await self._wal.append(event_bytes, event_id, event_type, self._chain)
                self._wal_writes += 1
            except Exception as e:
                # WAL FAIL -> FREEZE IMMEDIATELY
                logger.critical(f"WAL write failed for {self._chain}: {e}")
                await self._freeze(f"wal_write_failed:{e}")
                return

        # Step 2: Publish to Redis (retry on failure, event safe in WAL)
        output_stream = f"tokens:new:{self._chain}"
        max_retries = 3
        for attempt in range(1, max_retries + 1):
            try:
                await self._bus.publish(output_stream, event_data)
                self._events_collected += 1
                break
            except Exception as e:
                self._publish_failures += 1
                logger.error(f"Redis publish failed for {self._chain} (attempt {attempt}): {e}")
                if attempt < max_retries:
                    await asyncio.sleep(2 ** attempt)
                else:
                    logger.error(f"Redis publish failed after {max_retries} retries, event saved in WAL")

        # Update block tracking
        if event_data.get("block_number"):
            self._last_block_number = int(event_data["block_number"])
        if event_data.get("block_hash"):
            self._last_block_hash = event_data["block_hash"]

    async def _handle_reorg(self, event_data: dict):
        """Handle blockchain reorganization."""
        self._reorgs_detected += 1
        new_block = int(event_data["block_number"])
        logger.warning(
            f"Reorg detected on {self._chain}: "
            f"expected block > {self._last_block_number}, got {new_block}"
        )

        # Publish reorg notice
        reorg_data = {
            "event_type": "ReorgNotice",
            "chain": self._chain,
            "block_number": str(new_block),
            "old_block_hash": self._last_block_hash or "",
            "new_block_hash": event_data.get("block_hash", ""),
            "event_time": str(time.time()),
        }
        await self._bus.publish("health:freeze", reorg_data)

    async def _freeze(self, reason: str):
        """Freeze this collector."""
        self._frozen = True
        logger.critical(f"Collector {self._chain} FROZEN: {reason}")

        freeze_data = {
            "event_type": "FreezeEvent",
            "chain": self._chain,
            "reason": reason,
            "triggered_by": f"Collector:{self._chain}",
            "auto": "true",
            "event_time": str(time.time()),
        }
        try:
            await self._bus.publish("health:freeze", freeze_data)
        except Exception:
            logger.error("Failed to publish freeze event (Redis may be down)")

    async def _emit_heartbeat(self):
        """Emit heartbeat event."""
        import hashlib, json
        heartbeat = {
            "event_type": "HeartbeatEvent",
            "source_module": f"collector:{self._chain}",
            "config_hash": hashlib.sha256(json.dumps(self._config, sort_keys=True, default=str).encode()).hexdigest()[:16],
            "uptime_seconds": "0",
            "status": "frozen" if self._frozen else "running",
            "events_collected": str(self._events_collected),
            "event_time": str(time.time()),
        }
        try:
            await self._bus.publish("health:heartbeat", heartbeat)
        except Exception:
            pass

    def get_metrics(self) -> dict:
        return {
            "chain": self._chain,
            "events_collected": self._events_collected,
            "wal_writes": self._wal_writes,
            "publish_failures": self._publish_failures,
            "reorgs_detected": self._reorgs_detected,
            "frozen": self._frozen,
            "sequence": self._sequence,
        }
