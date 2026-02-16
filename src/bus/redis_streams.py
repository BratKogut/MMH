"""
MMH v3.1 -- Redis Streams durable event bus.

This module is the SINGLE durable event bus between all MMH services.
ZMQ (see zmq_fastlane.py) is an optional accelerator only -- never the
sole transport for any event.

Design principles:
    - At-least-once delivery via Redis Streams consumer groups.
    - Backpressure via MAXLEN on every XADD.
    - Consumer groups for reliable, partitioned processing.
    - Dead-letter queue for messages that exceed max retries.
    - Idempotent consumers are REQUIRED (see consumer.py / contracts).

Usage:
    bus = RedisEventBus("redis://localhost:6379")
    await bus.connect()
    msg_id = await bus.publish("tokens:new:solana", {"mint": "abc..."})
    await bus.disconnect()
"""

from __future__ import annotations

import json
import logging
import time
from typing import Any, Optional

import redis.asyncio as redis
from redis.exceptions import ConnectionError as RedisConnectionError
from redis.exceptions import ResponseError, TimeoutError

logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Stream name constants -- single source of truth for stream topology
# ---------------------------------------------------------------------------

class StreamNames:
    """Canonical stream names.  Single Source of Truth for stream topology.

    Templates containing ``{chain}`` must be resolved via ``for_chain()``
    before use with Redis commands.
    """

    # Discovery
    TOKENS_NEW: str = "tokens:new:{chain}"
    TOKENS_GRADUATED: str = "tokens:graduated:{chain}"

    # Sanitized
    TOKENS_SANITIZED: str = "tokens:sanitized:{chain}"

    # Enriched
    TOKENS_ENRICHED: str = "tokens:enriched:{chain}"

    # Scoring
    SCORING_RESULTS: str = "scoring:results:{chain}"

    # Risk
    RISK_DECISIONS: str = "risk:decisions"

    # Execution
    EXECUTION_APPROVED: str = "execution:approved"
    EXECUTION_ATTEMPTS: str = "execution:attempts"
    TRADES_EXECUTED: str = "trades:executed:{chain}"

    # Position
    POSITION_UPDATES: str = "positions:updates"

    # Control
    CONTROL_COMMANDS: str = "control:commands"
    CONTROL_LOG: str = "control:log"

    # Health
    HEARTBEAT: str = "health:heartbeat"
    FREEZE_EVENTS: str = "health:freeze"

    # Decision Journal
    DECISION_JOURNAL: str = "journal:decisions"

    @classmethod
    def for_chain(cls, template: str, chain: str) -> str:
        """Resolve a ``{chain}`` placeholder in a stream name template.

        Args:
            template: Stream name template, e.g. ``"tokens:new:{chain}"``.
            chain: Chain identifier, e.g. ``"solana"``.

        Returns:
            Resolved stream name, e.g. ``"tokens:new:solana"``.

        Raises:
            ValueError: If the template does not contain ``{chain}``.
        """
        if "{chain}" not in template:
            raise ValueError(
                f"Template '{template}' does not contain '{{chain}}' placeholder"
            )
        return template.format(chain=chain)


# ---------------------------------------------------------------------------
# Core event bus
# ---------------------------------------------------------------------------

class RedisEventBus:
    """Redis Streams based durable event bus.

    This is the SINGLE durable event bus between services.
    ZMQ is optional accelerator only (feature flag).

    Guarantees:
        - At-least-once delivery via consumer groups.
        - Backpressure via MAXLEN on every XADD.
        - Consumer groups with XREADGROUP for reliable processing.
        - Dead-letter queue support for failed messages (see dlq.py).

    Args:
        redis_url: Redis connection URL, e.g. ``"redis://localhost:6379"``.
        maxlen: Maximum stream length (approximate) for backpressure.
        instance_id: Unique identifier for this MMH instance (used in logs).
    """

    def __init__(
        self,
        redis_url: str,
        maxlen: int = 5000,
        instance_id: str = "mmh-1",
    ) -> None:
        self._redis: Optional[redis.Redis] = None
        self._redis_url: str = redis_url
        self._maxlen: int = maxlen
        self._instance_id: str = instance_id

    # -- lifecycle -----------------------------------------------------------

    async def connect(self) -> None:
        """Establish connection to Redis.

        Creates a connection pool with ``decode_responses=True`` so all
        values come back as Python ``str`` (not ``bytes``).

        Raises:
            RedisConnectionError: If the initial connection attempt fails.
        """
        if self._redis is not None:
            logger.debug("RedisEventBus already connected, skipping")
            return

        try:
            self._redis = redis.from_url(
                self._redis_url,
                decode_responses=True,
                socket_connect_timeout=5.0,
                socket_keepalive=True,
                retry_on_timeout=True,
                health_check_interval=30,
            )
            # Verify connectivity with a PING
            await self._redis.ping()
            logger.info(
                "RedisEventBus connected to %s [instance=%s]",
                self._redis_url,
                self._instance_id,
            )
        except (RedisConnectionError, TimeoutError, OSError) as exc:
            logger.error(
                "RedisEventBus failed to connect to %s: %s",
                self._redis_url,
                exc,
            )
            self._redis = None
            raise

    async def disconnect(self) -> None:
        """Close the Redis connection pool gracefully."""
        if self._redis is not None:
            try:
                await self._redis.aclose()
                logger.info(
                    "RedisEventBus disconnected [instance=%s]",
                    self._instance_id,
                )
            except Exception as exc:
                logger.warning("Error during RedisEventBus disconnect: %s", exc)
            finally:
                self._redis = None

    @property
    def redis(self) -> redis.Redis:
        """Return the underlying ``redis.asyncio.Redis`` client.

        Raises:
            RuntimeError: If :meth:`connect` has not been called.
        """
        if self._redis is None:
            raise RuntimeError(
                "RedisEventBus is not connected. Call connect() first."
            )
        return self._redis

    # -- publish -------------------------------------------------------------

    async def publish(
        self,
        stream: str,
        event_data: dict[str, Any],
        event_id: str | None = None,
    ) -> str:
        """Publish an event to a Redis Stream.

        Uses ``XADD`` with approximate ``MAXLEN`` for backpressure so
        streams never grow unbounded.

        Args:
            stream: Target stream name (already resolved, no ``{chain}``).
            event_data: Flat dict of string keys to string-serialisable
                values.  Nested dicts are JSON-encoded automatically.
            event_id: Optional explicit stream message ID.  If ``None``
                (the default), Redis auto-generates a time-based ID (``*``).

        Returns:
            The Redis-assigned message ID (e.g. ``"1704067200000-0"``).

        Raises:
            RuntimeError: If the bus is not connected.
            RedisConnectionError: On connection failure.
        """
        r = self.redis

        # Flatten nested values to JSON strings for Redis hash compliance
        flat_data: dict[str, str] = {}
        for key, value in event_data.items():
            if isinstance(value, (dict, list)):
                flat_data[key] = json.dumps(value)
            elif isinstance(value, bool):
                flat_data[key] = "true" if value else "false"
            elif value is None:
                flat_data[key] = ""
            else:
                flat_data[key] = str(value)

        # Add bus metadata
        flat_data["_bus_instance"] = self._instance_id
        flat_data["_bus_publish_ts"] = str(time.time())

        try:
            msg_id: str = await r.xadd(
                name=stream,
                fields=flat_data,
                maxlen=self._maxlen,
                approximate=True,
                id=event_id if event_id else "*",
            )
            logger.debug(
                "Published to %s: msg_id=%s keys=%s",
                stream,
                msg_id,
                list(flat_data.keys()),
            )
            return msg_id
        except (RedisConnectionError, TimeoutError) as exc:
            logger.error("Publish to %s failed: %s", stream, exc)
            raise
        except ResponseError as exc:
            logger.error(
                "Redis protocol error publishing to %s: %s", stream, exc
            )
            raise

    # -- consumer groups -----------------------------------------------------

    async def ensure_consumer_group(
        self,
        stream: str,
        group: str,
        start_id: str = "0",
    ) -> None:
        """Create a consumer group on *stream* if it does not already exist.

        This is fully idempotent -- calling it multiple times with the same
        arguments is safe.

        Args:
            stream: Stream name.
            group: Consumer group name.
            start_id: ID from which the group should start reading.
                ``"0"`` = from the very beginning, ``"$"`` = only new msgs.
        """
        r = self.redis
        try:
            await r.xgroup_create(
                name=stream,
                groupname=group,
                id=start_id,
                mkstream=True,
            )
            logger.info(
                "Created consumer group '%s' on stream '%s' (start=%s)",
                group,
                stream,
                start_id,
            )
        except ResponseError as exc:
            # "BUSYGROUP Consumer Group name already exists"
            if "BUSYGROUP" in str(exc):
                logger.debug(
                    "Consumer group '%s' already exists on '%s'", group, stream
                )
            else:
                logger.error(
                    "Failed to create consumer group '%s' on '%s': %s",
                    group,
                    stream,
                    exc,
                )
                raise

    async def consume(
        self,
        stream: str,
        group: str,
        consumer: str,
        count: int = 10,
        block_ms: int = 5000,
    ) -> list[tuple[str, dict[str, str]]]:
        """Read messages from a consumer group via ``XREADGROUP``.

        Messages returned here are in *pending* state until
        :meth:`ack` is called for their IDs.

        Args:
            stream: Stream name.
            group: Consumer group name.
            consumer: Consumer name within the group.
            count: Maximum number of messages to fetch.
            block_ms: Block for up to this many milliseconds if no messages
                are available.  ``0`` = block forever; ``None`` = no block.

        Returns:
            List of ``(message_id, data_dict)`` tuples.  Empty list if
            no messages arrived within the block timeout.
        """
        r = self.redis
        try:
            response = await r.xreadgroup(
                groupname=group,
                consumername=consumer,
                streams={stream: ">"},
                count=count,
                block=block_ms,
            )
            if not response:
                return []

            # response format: [[stream_name, [(msg_id, data), ...]]]
            messages: list[tuple[str, dict[str, str]]] = []
            for _stream_name, stream_messages in response:
                for msg_id, data in stream_messages:
                    messages.append((msg_id, data))

            if messages:
                logger.debug(
                    "Consumed %d messages from %s (group=%s, consumer=%s)",
                    len(messages),
                    stream,
                    group,
                    consumer,
                )
            return messages

        except (RedisConnectionError, TimeoutError) as exc:
            logger.error(
                "Consume from %s (group=%s) failed: %s", stream, group, exc
            )
            raise
        except ResponseError as exc:
            # Handle group/stream not existing
            if "NOGROUP" in str(exc):
                logger.warning(
                    "Consumer group '%s' does not exist on '%s'. "
                    "Creating it now.",
                    group,
                    stream,
                )
                await self.ensure_consumer_group(stream, group)
                return []
            raise

    async def ack(self, stream: str, group: str, *msg_ids: str) -> int:
        """Acknowledge one or more messages in a consumer group.

        Acknowledged messages are removed from the Pending Entries List
        (PEL) and will not be re-delivered.

        Args:
            stream: Stream name.
            group: Consumer group name.
            *msg_ids: One or more message IDs to acknowledge.

        Returns:
            Number of messages successfully acknowledged.
        """
        if not msg_ids:
            return 0

        r = self.redis
        try:
            acked: int = await r.xack(stream, group, *msg_ids)
            logger.debug(
                "Acked %d/%d messages on %s (group=%s)",
                acked,
                len(msg_ids),
                stream,
                group,
            )
            return acked
        except (RedisConnectionError, TimeoutError) as exc:
            logger.error(
                "Ack on %s (group=%s) failed: %s", stream, group, exc
            )
            raise

    # -- pending / crash recovery --------------------------------------------

    async def get_pending(
        self,
        stream: str,
        group: str,
        min_idle_ms: int = 30_000,
        count: int = 10,
    ) -> list[dict[str, Any]]:
        """Get pending (unacknowledged) messages older than *min_idle_ms*.

        Useful for detecting messages stuck with a crashed consumer.

        Args:
            stream: Stream name.
            group: Consumer group name.
            min_idle_ms: Minimum idle time in milliseconds.
            count: Maximum number of entries to return.

        Returns:
            List of dicts with keys: ``msg_id``, ``consumer``,
            ``idle_ms``, ``delivery_count``.
        """
        r = self.redis
        try:
            # XPENDING stream group IDLE min_idle_ms - + count
            pending_entries = await r.xpending_range(
                name=stream,
                groupname=group,
                min="-",
                max="+",
                count=count,
                idle=min_idle_ms,
            )
            result: list[dict[str, Any]] = []
            for entry in pending_entries:
                result.append({
                    "msg_id": entry.get("message_id", ""),
                    "consumer": entry.get("consumer", ""),
                    "idle_ms": entry.get("time_since_delivered", 0),
                    "delivery_count": entry.get("times_delivered", 0),
                })
            logger.debug(
                "Found %d pending messages on %s (group=%s, min_idle=%dms)",
                len(result),
                stream,
                group,
                min_idle_ms,
            )
            return result

        except (RedisConnectionError, TimeoutError) as exc:
            logger.error(
                "get_pending on %s (group=%s) failed: %s",
                stream,
                group,
                exc,
            )
            raise
        except ResponseError as exc:
            if "NOGROUP" in str(exc):
                logger.warning(
                    "Consumer group '%s' does not exist on '%s' for "
                    "get_pending.",
                    group,
                    stream,
                )
                return []
            raise

    async def claim_pending(
        self,
        stream: str,
        group: str,
        consumer: str,
        min_idle_ms: int = 30_000,
        count: int = 10,
    ) -> list[tuple[str, dict[str, str]]]:
        """Claim pending messages via ``XCLAIM`` for crash recovery.

        Transfers ownership of idle pending messages to *consumer* so
        they can be reprocessed.

        Args:
            stream: Stream name.
            group: Consumer group name.
            consumer: Consumer that should take ownership.
            min_idle_ms: Only claim messages idle for at least this long.
            count: Maximum number of messages to claim.

        Returns:
            List of ``(message_id, data_dict)`` tuples for claimed messages.
        """
        r = self.redis

        # Step 1: find pending message IDs
        pending = await self.get_pending(
            stream, group, min_idle_ms=min_idle_ms, count=count
        )
        if not pending:
            return []

        msg_ids: list[str] = [entry["msg_id"] for entry in pending]

        try:
            # XCLAIM stream group consumer min-idle-time id [id ...]
            claimed_raw = await r.xclaim(
                name=stream,
                groupname=group,
                consumername=consumer,
                min_idle_time=min_idle_ms,
                message_ids=msg_ids,
            )

            claimed: list[tuple[str, dict[str, str]]] = []
            for msg_id, data in claimed_raw:
                if data is not None:
                    claimed.append((msg_id, data))

            logger.info(
                "Claimed %d/%d pending messages on %s (group=%s -> %s)",
                len(claimed),
                len(msg_ids),
                stream,
                group,
                consumer,
            )
            return claimed

        except (RedisConnectionError, TimeoutError) as exc:
            logger.error(
                "claim_pending on %s (group=%s) failed: %s",
                stream,
                group,
                exc,
            )
            raise
        except ResponseError as exc:
            logger.error(
                "XCLAIM protocol error on %s (group=%s): %s",
                stream,
                group,
                exc,
            )
            raise

    # -- introspection -------------------------------------------------------

    async def stream_info(self, stream: str) -> dict[str, Any]:
        """Get detailed information about a stream.

        Returns:
            Dict with keys like ``length``, ``first_entry``,
            ``last_entry``, ``groups``, ``radix_tree_keys``, etc.
            Returns empty dict if the stream does not exist.
        """
        r = self.redis
        try:
            info = await r.xinfo_stream(name=stream)
            # Also fetch group info
            try:
                groups = await r.xinfo_groups(name=stream)
            except ResponseError:
                groups = []

            return {
                "length": info.get("length", 0),
                "first_entry": info.get("first-entry"),
                "last_entry": info.get("last-entry"),
                "radix_tree_keys": info.get("radix-tree-keys", 0),
                "radix_tree_nodes": info.get("radix-tree-nodes", 0),
                "groups_count": len(groups),
                "groups": [
                    {
                        "name": g.get("name", ""),
                        "consumers": g.get("consumers", 0),
                        "pending": g.get("pending", 0),
                        "last_delivered_id": g.get("last-delivered-id", ""),
                    }
                    for g in groups
                ],
            }
        except ResponseError as exc:
            if "no such key" in str(exc).lower() or "ERR" in str(exc):
                logger.debug("Stream '%s' does not exist.", stream)
                return {}
            raise
        except (RedisConnectionError, TimeoutError) as exc:
            logger.error("stream_info for '%s' failed: %s", stream, exc)
            raise

    async def stream_lag(self, stream: str, group: str) -> int:
        """Calculate consumer group lag (number of pending + undelivered).

        This returns the total number of messages in the stream that have
        not yet been acknowledged by the consumer group -- i.e. the sum of
        pending entries and entries not yet delivered.

        Args:
            stream: Stream name.
            group: Consumer group name.

        Returns:
            Lag count.  ``0`` if stream or group does not exist.
        """
        r = self.redis
        try:
            pending_summary = await r.xpending(
                name=stream,
                groupname=group,
            )
            # pending_summary: { "pending": int, "min": str, "max": str,
            #                    "consumers": [...] }
            pending_count: int = pending_summary.get("pending", 0)

            # Also compute undelivered messages (stream length beyond
            # last-delivered-id).
            groups_info = await r.xinfo_groups(name=stream)
            last_delivered_id: str = "0-0"
            for g in groups_info:
                if g.get("name") == group:
                    last_delivered_id = g.get("last-delivered-id", "0-0")
                    break

            # Count entries after last-delivered-id
            if last_delivered_id and last_delivered_id != "0-0":
                # XRANGE stream (last_delivered_id + EXCLUSIVE range
                # Unfortunately XRANGE does not support exclusive; we use
                # XLEN - delivered - acked as approximation, or count via
                # xrange with count limit.
                undelivered = await r.xrange(
                    name=stream,
                    min=f"({last_delivered_id}",  # exclusive via '('
                    max="+",
                    count=None,
                )
                undelivered_count: int = len(undelivered)
            else:
                # Group hasn't delivered anything yet, all messages are lag
                stream_len = await r.xlen(name=stream)
                undelivered_count = stream_len

            total_lag: int = pending_count + undelivered_count
            logger.debug(
                "Lag for %s (group=%s): pending=%d undelivered=%d total=%d",
                stream,
                group,
                pending_count,
                undelivered_count,
                total_lag,
            )
            return total_lag

        except ResponseError as exc:
            if "NOGROUP" in str(exc) or "no such key" in str(exc).lower():
                logger.debug(
                    "stream_lag: stream '%s' or group '%s' does not exist.",
                    stream,
                    group,
                )
                return 0
            raise
        except (RedisConnectionError, TimeoutError) as exc:
            logger.error(
                "stream_lag for %s (group=%s) failed: %s",
                stream,
                group,
                exc,
            )
            raise

    # -- utilities -----------------------------------------------------------

    async def trim(self, stream: str, maxlen: int | None = None) -> int:
        """Manually trim a stream to *maxlen* entries.

        Args:
            stream: Stream name.
            maxlen: Maximum entries to keep.  Uses the bus default if None.

        Returns:
            Number of entries trimmed.
        """
        r = self.redis
        target: int = maxlen if maxlen is not None else self._maxlen
        try:
            trimmed: int = await r.xtrim(
                name=stream, maxlen=target, approximate=True
            )
            if trimmed > 0:
                logger.info(
                    "Trimmed %d entries from %s (maxlen=%d)",
                    trimmed,
                    stream,
                    target,
                )
            return trimmed
        except (RedisConnectionError, TimeoutError) as exc:
            logger.error("trim on %s failed: %s", stream, exc)
            raise

    async def health_check(self) -> bool:
        """Return True if the Redis connection is healthy."""
        try:
            await self.redis.ping()
            return True
        except Exception as exc:
            logger.warning("RedisEventBus health check failed: %s", exc)
            return False
