"""
MMH v3.1 -- Dead Letter Queue for failed event-bus messages.

When a message exhausts its retry budget in :class:`ReliableConsumer`,
it lands here.  DLQ entries include the original payload, the error
that caused the final failure, and the number of processing attempts.

DLQ streams are named ``dlq:{source_stream}`` and capped at
:pyattr:`MAX_DLQ_SIZE` entries per source stream.

Operators can inspect, replay (re-inject into the source stream),
or purge the DLQ via the methods on :class:`DeadLetterQueue`.
"""

from __future__ import annotations

import json
import logging
import time
from typing import Any

from src.bus.redis_streams import RedisEventBus

logger = logging.getLogger(__name__)


class DeadLetterQueue:
    """Dead Letter Queue for failed messages.

    Capabilities:
        - Store failed messages with full error context.
        - Cap at ``MAX_DLQ_SIZE`` entries per source stream.
        - Inspect DLQ contents for debugging / alerting.
        - Replay individual messages back to their source stream.
        - Purge a DLQ entirely.
        - Aggregate stats across all DLQ streams.

    Args:
        bus: The connected :class:`RedisEventBus` instance.
    """

    DLQ_PREFIX: str = "dlq:"
    MAX_DLQ_SIZE: int = 1000

    def __init__(self, bus: RedisEventBus) -> None:
        self._bus: RedisEventBus = bus

    def _dlq_stream(self, source_stream: str) -> str:
        """Derive the DLQ stream name from the source stream."""
        return f"{self.DLQ_PREFIX}{source_stream}"

    # -- core operations -----------------------------------------------------

    async def push(
        self,
        source_stream: str,
        msg_id: str,
        data: dict[str, Any],
        error: str,
        attempts: int,
    ) -> str:
        """Push a failed message into the DLQ.

        The original data is preserved alongside failure metadata so that
        operators have full context when investigating.

        Args:
            source_stream: The stream the message originally came from.
            msg_id: Original Redis message ID.
            data: Original message payload (flat dict).
            error: String description of the final error.
            attempts: Number of processing attempts before giving up.

        Returns:
            The DLQ message ID assigned by Redis.
        """
        dlq_name: str = self._dlq_stream(source_stream)

        dlq_entry: dict[str, str] = {
            "_dlq_source_stream": source_stream,
            "_dlq_original_msg_id": msg_id,
            "_dlq_error": str(error),
            "_dlq_attempts": str(attempts),
            "_dlq_dead_at": str(time.time()),
            "_dlq_original_data": json.dumps(data),
        }

        r = self._bus.redis
        try:
            dlq_msg_id: str = await r.xadd(
                name=dlq_name,
                fields=dlq_entry,
                maxlen=self.MAX_DLQ_SIZE,
                approximate=True,
            )
            logger.warning(
                "DLQ push: stream=%s original_msg=%s error=%s attempts=%d "
                "dlq_msg=%s",
                source_stream,
                msg_id,
                error[:200],
                attempts,
                dlq_msg_id,
            )
            return dlq_msg_id
        except Exception as exc:
            logger.error(
                "Failed to push to DLQ %s: %s (original msg_id=%s)",
                dlq_name,
                exc,
                msg_id,
            )
            raise

    async def inspect(
        self,
        source_stream: str,
        count: int = 10,
    ) -> list[dict[str, Any]]:
        """View the most recent DLQ entries for a given source stream.

        Args:
            source_stream: The source stream whose DLQ to inspect.
            count: Maximum number of entries to return (most recent first).

        Returns:
            List of dicts, each containing:
            ``dlq_msg_id``, ``source_stream``, ``original_msg_id``,
            ``error``, ``attempts``, ``dead_at``, ``original_data``.
        """
        dlq_name: str = self._dlq_stream(source_stream)
        r = self._bus.redis

        try:
            # XREVRANGE gives newest-first
            entries = await r.xrevrange(
                name=dlq_name,
                count=count,
            )
        except Exception as exc:
            logger.error("Failed to inspect DLQ %s: %s", dlq_name, exc)
            return []

        result: list[dict[str, Any]] = []
        for msg_id, data in entries:
            original_data_raw: str = data.get("_dlq_original_data", "{}")
            try:
                original_data = json.loads(original_data_raw)
            except (json.JSONDecodeError, TypeError):
                original_data = {"_raw": original_data_raw}

            result.append({
                "dlq_msg_id": msg_id,
                "source_stream": data.get("_dlq_source_stream", source_stream),
                "original_msg_id": data.get("_dlq_original_msg_id", ""),
                "error": data.get("_dlq_error", ""),
                "attempts": int(data.get("_dlq_attempts", "0")),
                "dead_at": float(data.get("_dlq_dead_at", "0")),
                "original_data": original_data,
            })

        logger.debug(
            "DLQ inspect: %s returned %d entries", dlq_name, len(result)
        )
        return result

    async def replay(self, source_stream: str, msg_id: str) -> bool:
        """Replay a single DLQ message back to its source stream.

        The original payload is extracted from the DLQ entry and re-published
        to the source stream.  A ``_dlq_replayed`` flag is added so
        downstream consumers can detect replayed events.

        After successful re-publish, the DLQ entry is removed.

        Args:
            source_stream: The source stream to replay into.
            msg_id: The *DLQ* message ID to replay (not the original msg ID).

        Returns:
            ``True`` if the message was successfully replayed, ``False`` if
            the DLQ entry was not found or replay failed.
        """
        dlq_name: str = self._dlq_stream(source_stream)
        r = self._bus.redis

        try:
            # Fetch the specific DLQ entry
            entries = await r.xrange(
                name=dlq_name,
                min=msg_id,
                max=msg_id,
                count=1,
            )
            if not entries:
                logger.warning(
                    "DLQ replay: msg_id %s not found in %s",
                    msg_id,
                    dlq_name,
                )
                return False

            _dlq_id, dlq_data = entries[0]
            original_data_raw: str = dlq_data.get("_dlq_original_data", "{}")
            try:
                original_data: dict[str, Any] = json.loads(original_data_raw)
            except (json.JSONDecodeError, TypeError):
                logger.error(
                    "DLQ replay: could not parse original_data for %s", msg_id
                )
                return False

            # Mark as replayed
            original_data["_dlq_replayed"] = "true"
            original_data["_dlq_replayed_at"] = str(time.time())

            # Re-publish to source stream
            new_msg_id: str = await self._bus.publish(
                stream=source_stream,
                event_data=original_data,
            )

            # Remove from DLQ
            await r.xdel(dlq_name, msg_id)

            logger.info(
                "DLQ replay: %s -> %s new_msg_id=%s",
                msg_id,
                source_stream,
                new_msg_id,
            )
            return True

        except Exception as exc:
            logger.error(
                "DLQ replay failed for %s in %s: %s",
                msg_id,
                dlq_name,
                exc,
            )
            return False

    async def purge(self, source_stream: str) -> int:
        """Delete all entries in the DLQ for a given source stream.

        Args:
            source_stream: The source stream whose DLQ to purge.

        Returns:
            Number of entries that were in the DLQ before purge.
        """
        dlq_name: str = self._dlq_stream(source_stream)
        r = self._bus.redis

        try:
            length: int = await r.xlen(dlq_name)
            if length > 0:
                await r.delete(dlq_name)
                logger.info(
                    "DLQ purged: %s (%d entries removed)", dlq_name, length
                )
            else:
                logger.debug("DLQ purge: %s is already empty", dlq_name)
            return length

        except Exception as exc:
            logger.error("DLQ purge failed for %s: %s", dlq_name, exc)
            raise

    async def stats(self) -> dict[str, int]:
        """Get DLQ sizes for all source streams that have DLQ entries.

        Scans Redis for keys matching ``dlq:*`` and returns their lengths.

        Returns:
            Dict mapping source stream names to their DLQ entry counts.
            E.g. ``{"tokens:new:solana": 3, "scoring:results:base": 1}``.
        """
        r = self._bus.redis
        result: dict[str, int] = {}

        try:
            cursor: int = 0
            while True:
                cursor, keys = await r.scan(
                    cursor=cursor,
                    match=f"{self.DLQ_PREFIX}*",
                    count=100,
                )
                for key in keys:
                    # key is e.g. "dlq:tokens:new:solana"
                    source_stream: str = key[len(self.DLQ_PREFIX):]
                    length: int = await r.xlen(key)
                    if length > 0:
                        result[source_stream] = length

                if cursor == 0:
                    break

        except Exception as exc:
            logger.error("DLQ stats scan failed: %s", exc)
            raise

        logger.debug("DLQ stats: %s", result)
        return result
