"""
MMH v3.1 -- Reliable Redis Streams consumer with retry, DLQ, and idempotency.

Every microservice that reads from the event bus should use
:class:`ReliableConsumer` instead of calling ``bus.consume()`` directly.
This wrapper provides:

    - **Pending recovery on startup**: Claims and reprocesses orphaned
      messages from crashed consumers.
    - **Automatic retry with exponential backoff**: Transient failures
      are retried up to ``max_retries`` times.
    - **Dead Letter Queue**: Messages that exhaust all retries are moved
      to a DLQ (see :mod:`dlq`) so they can be inspected / replayed.
    - **Idempotency guard**: Optional dedup via a pluggable idempotency
      store (Redis SET with TTL).
    - **Graceful shutdown**: Finishes in-flight processing before stopping.

Contract:
    All streams are at-least-once.  Every consumer handler MUST be
    idempotent -- processing the same message twice must produce the
    same side-effects exactly once.
"""

from __future__ import annotations

import asyncio
import logging
import time
import traceback
from typing import Any, Callable, Optional, Protocol

from src.bus.dlq import DeadLetterQueue
from src.bus.redis_streams import RedisEventBus

logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Idempotency protocol
# ---------------------------------------------------------------------------

class IdempotencyStore(Protocol):
    """Protocol for pluggable idempotency checking.

    Implementations must be async and support ``has_seen`` / ``mark_seen``.
    A Redis-backed default is provided below.
    """

    async def has_seen(self, event_id: str) -> bool:
        """Return True if this event has already been processed."""
        ...

    async def mark_seen(self, event_id: str) -> None:
        """Record that this event has been processed."""
        ...


class RedisIdempotencyStore:
    """Redis SET-based idempotency store with TTL.

    Stores event IDs in Redis with a configurable TTL so that the set
    does not grow unbounded.  Default TTL is 24 hours.

    Args:
        bus: Connected :class:`RedisEventBus` instance.
        ttl_seconds: How long to remember processed event IDs.
        prefix: Key prefix for idempotency keys in Redis.
    """

    def __init__(
        self,
        bus: RedisEventBus,
        ttl_seconds: int = 86400,
        prefix: str = "idem:",
    ) -> None:
        self._bus: RedisEventBus = bus
        self._ttl: int = ttl_seconds
        self._prefix: str = prefix

    def _key(self, event_id: str) -> str:
        return f"{self._prefix}{event_id}"

    async def has_seen(self, event_id: str) -> bool:
        """Check if the event ID is already in the idempotency set."""
        r = self._bus.redis
        result: bool = await r.exists(self._key(event_id)) > 0
        return result

    async def mark_seen(self, event_id: str) -> None:
        """Mark the event ID as processed with a TTL."""
        r = self._bus.redis
        await r.set(self._key(event_id), "1", ex=self._ttl)


# ---------------------------------------------------------------------------
# Reliable consumer
# ---------------------------------------------------------------------------

class ReliableConsumer:
    """Reliable Redis Streams consumer with retry, DLQ, and recovery.

    This is the standard way for any MMH service to consume events.

    The *handler* callback receives ``(event_id: str, event_data: dict)``
    and must return ``True`` on success.  Returning ``False`` or raising
    an exception triggers retry logic.

    Args:
        bus: Connected :class:`RedisEventBus`.
        stream: Stream to consume from.
        group: Consumer group name.
        consumer_name: Unique consumer name within the group.
        handler: Async callable ``(event_id, event_data) -> bool``.
        max_retries: Maximum processing attempts before DLQ.
        retry_base_delay_ms: Base delay for exponential backoff (ms).
        idempotency_store: Optional :class:`IdempotencyStore` implementation.
            If ``None``, idempotency checking is skipped.
        batch_size: Number of messages to fetch per XREADGROUP call.
        block_ms: Block timeout for XREADGROUP in milliseconds.
        pending_claim_interval_ms: How often to scan for stuck pending
            messages (milliseconds).
        pending_min_idle_ms: Minimum idle time before claiming a pending
            message from another consumer.
    """

    def __init__(
        self,
        bus: RedisEventBus,
        stream: str,
        group: str,
        consumer_name: str,
        handler: Callable[..., Any],
        max_retries: int = 3,
        retry_base_delay_ms: int = 1000,
        idempotency_store: Optional[IdempotencyStore] = None,
        batch_size: int = 10,
        block_ms: int = 5000,
        pending_claim_interval_ms: int = 60_000,
        pending_min_idle_ms: int = 30_000,
    ) -> None:
        self._bus: RedisEventBus = bus
        self._stream: str = stream
        self._group: str = group
        self._consumer_name: str = consumer_name
        self._handler: Callable[..., Any] = handler
        self._max_retries: int = max_retries
        self._retry_base_delay_ms: int = retry_base_delay_ms
        self._idempotency_store: Optional[IdempotencyStore] = idempotency_store
        self._batch_size: int = batch_size
        self._block_ms: int = block_ms
        self._pending_claim_interval_ms: int = pending_claim_interval_ms
        self._pending_min_idle_ms: int = pending_min_idle_ms

        self._dlq: DeadLetterQueue = DeadLetterQueue(bus)
        self._running: bool = False
        self._task: Optional[asyncio.Task[None]] = None
        self._pending_task: Optional[asyncio.Task[None]] = None
        self._processing_count: int = 0

        # Metrics (simple counters for observability)
        self.stats: dict[str, int] = {
            "processed": 0,
            "succeeded": 0,
            "retried": 0,
            "dlq": 0,
            "duplicates": 0,
            "errors": 0,
        }

    # -- lifecycle -----------------------------------------------------------

    async def start(self) -> None:
        """Start the consumer.

        1. Ensures the consumer group exists.
        2. Recovers any pending (orphaned) messages.
        3. Enters the main consume loop as a background task.
        """
        if self._running:
            logger.warning(
                "ReliableConsumer '%s' already running on stream '%s'",
                self._consumer_name,
                self._stream,
            )
            return

        # Ensure consumer group
        await self._bus.ensure_consumer_group(self._stream, self._group)

        self._running = True

        # Recover pending messages from previous runs / crashed consumers
        await self._recover_pending()

        # Start main consume loop
        self._task = asyncio.create_task(
            self._consume_loop(),
            name=f"consumer-{self._consumer_name}-{self._stream}",
        )

        # Start periodic pending reclaim loop
        self._pending_task = asyncio.create_task(
            self._periodic_pending_recovery(),
            name=f"pending-recovery-{self._consumer_name}-{self._stream}",
        )

        logger.info(
            "ReliableConsumer started: stream=%s group=%s consumer=%s",
            self._stream,
            self._group,
            self._consumer_name,
        )

    async def stop(self) -> None:
        """Graceful shutdown.

        Sets the running flag to ``False``, waits for in-flight processing
        to complete, then cancels the background tasks.
        """
        if not self._running:
            return

        logger.info(
            "ReliableConsumer stopping: stream=%s consumer=%s "
            "(in-flight=%d)",
            self._stream,
            self._consumer_name,
            self._processing_count,
        )
        self._running = False

        # Wait for in-flight messages to finish (with timeout)
        deadline: float = time.time() + 30.0
        while self._processing_count > 0 and time.time() < deadline:
            await asyncio.sleep(0.1)

        if self._processing_count > 0:
            logger.warning(
                "ReliableConsumer force-stopping with %d in-flight messages",
                self._processing_count,
            )

        # Cancel tasks
        for task in (self._task, self._pending_task):
            if task is not None and not task.done():
                task.cancel()
                try:
                    await task
                except asyncio.CancelledError:
                    pass

        self._task = None
        self._pending_task = None

        logger.info(
            "ReliableConsumer stopped: stream=%s consumer=%s stats=%s",
            self._stream,
            self._consumer_name,
            self.stats,
        )

    # -- main loop -----------------------------------------------------------

    async def _consume_loop(self) -> None:
        """Main consume loop.  Runs until ``stop()`` is called."""
        consecutive_errors: int = 0
        max_consecutive_errors: int = 10
        error_backoff_base: float = 1.0

        while self._running:
            try:
                messages: list[tuple[str, dict[str, str]]] = (
                    await self._bus.consume(
                        stream=self._stream,
                        group=self._group,
                        consumer=self._consumer_name,
                        count=self._batch_size,
                        block_ms=self._block_ms,
                    )
                )
                consecutive_errors = 0

                for msg_id, data in messages:
                    if not self._running:
                        break
                    await self._process_message(msg_id, data, attempt=1)

            except asyncio.CancelledError:
                logger.debug("Consume loop cancelled for %s", self._consumer_name)
                break
            except Exception as exc:
                consecutive_errors += 1
                backoff: float = min(
                    error_backoff_base * (2 ** (consecutive_errors - 1)),
                    30.0,
                )
                logger.error(
                    "Consume loop error #%d on stream=%s consumer=%s: %s "
                    "(backing off %.1fs)",
                    consecutive_errors,
                    self._stream,
                    self._consumer_name,
                    exc,
                    backoff,
                )
                self.stats["errors"] += 1

                if consecutive_errors >= max_consecutive_errors:
                    logger.critical(
                        "ReliableConsumer hit %d consecutive errors on "
                        "stream=%s.  Pausing for 60s.",
                        consecutive_errors,
                        self._stream,
                    )
                    await asyncio.sleep(60.0)
                    consecutive_errors = 0
                else:
                    await asyncio.sleep(backoff)

    # -- message processing --------------------------------------------------

    async def _process_message(
        self,
        msg_id: str,
        data: dict[str, str],
        attempt: int = 1,
    ) -> None:
        """Process a single message with retry and DLQ logic.

        Args:
            msg_id: Redis stream message ID.
            data: Message payload (flat string dict).
            attempt: Current attempt number (1-based).
        """
        self._processing_count += 1
        try:
            # Idempotency check
            event_id: str = data.get("event_id", msg_id)
            if self._idempotency_store is not None:
                if await self._idempotency_store.has_seen(event_id):
                    logger.debug(
                        "Duplicate event skipped: event_id=%s msg_id=%s",
                        event_id,
                        msg_id,
                    )
                    self.stats["duplicates"] += 1
                    # Ack the duplicate so it's removed from PEL
                    await self._bus.ack(self._stream, self._group, msg_id)
                    return

            # Call the handler
            self.stats["processed"] += 1
            try:
                success: bool = await self._handler(event_id, data)
            except Exception as handler_exc:
                success = False
                logger.warning(
                    "Handler exception for msg_id=%s attempt=%d: %s",
                    msg_id,
                    attempt,
                    handler_exc,
                )
                error_str: str = (
                    f"{type(handler_exc).__name__}: {handler_exc}\n"
                    f"{traceback.format_exc()}"
                )

                if attempt < self._max_retries:
                    # Retry with exponential backoff
                    delay_ms: int = self._retry_base_delay_ms * (2 ** (attempt - 1))
                    delay_s: float = delay_ms / 1000.0
                    logger.info(
                        "Retrying msg_id=%s in %.1fs (attempt %d/%d)",
                        msg_id,
                        delay_s,
                        attempt + 1,
                        self._max_retries,
                    )
                    self.stats["retried"] += 1
                    await asyncio.sleep(delay_s)
                    # Recursive retry (decrement processing count since
                    # the recursive call increments it)
                    self._processing_count -= 1
                    await self._process_message(msg_id, data, attempt + 1)
                    return
                else:
                    # Exhausted retries -- send to DLQ
                    logger.error(
                        "Max retries (%d) exhausted for msg_id=%s. "
                        "Sending to DLQ.",
                        self._max_retries,
                        msg_id,
                    )
                    await self._send_to_dlq(
                        msg_id, data, error_str, attempt
                    )
                    # Ack the message so it leaves the PEL
                    await self._bus.ack(self._stream, self._group, msg_id)
                    return

            if success:
                # Success -- ack and mark idempotent
                await self._bus.ack(self._stream, self._group, msg_id)
                if self._idempotency_store is not None:
                    await self._idempotency_store.mark_seen(event_id)
                self.stats["succeeded"] += 1
                logger.debug(
                    "Successfully processed msg_id=%s event_id=%s",
                    msg_id,
                    event_id,
                )
            else:
                # Handler returned False (explicit failure)
                if attempt < self._max_retries:
                    delay_ms = self._retry_base_delay_ms * (2 ** (attempt - 1))
                    delay_s = delay_ms / 1000.0
                    logger.info(
                        "Handler returned False for msg_id=%s. "
                        "Retrying in %.1fs (attempt %d/%d)",
                        msg_id,
                        delay_s,
                        attempt + 1,
                        self._max_retries,
                    )
                    self.stats["retried"] += 1
                    await asyncio.sleep(delay_s)
                    self._processing_count -= 1
                    await self._process_message(msg_id, data, attempt + 1)
                    return
                else:
                    logger.error(
                        "Max retries (%d) exhausted for msg_id=%s "
                        "(handler returned False). Sending to DLQ.",
                        self._max_retries,
                        msg_id,
                    )
                    await self._send_to_dlq(
                        msg_id,
                        data,
                        "Handler returned False after all retries",
                        attempt,
                    )
                    await self._bus.ack(self._stream, self._group, msg_id)

        except Exception as exc:
            logger.error(
                "Unexpected error processing msg_id=%s: %s",
                msg_id,
                exc,
            )
            self.stats["errors"] += 1
            # Do NOT ack -- the message stays pending for recovery
        finally:
            self._processing_count -= 1

    # -- DLQ -----------------------------------------------------------------

    async def _send_to_dlq(
        self,
        msg_id: str,
        data: dict[str, str],
        error: str,
        attempts: int,
    ) -> None:
        """Send a failed message to the Dead Letter Queue.

        Args:
            msg_id: Original Redis message ID.
            data: Original message payload.
            error: Error description.
            attempts: Total number of processing attempts.
        """
        try:
            await self._dlq.push(
                source_stream=self._stream,
                msg_id=msg_id,
                data=data,
                error=error,
                attempts=attempts,
            )
            self.stats["dlq"] += 1
        except Exception as dlq_exc:
            # If even the DLQ push fails, log a critical error.
            # The message remains in the PEL and will be retried
            # on next pending recovery.
            logger.critical(
                "CRITICAL: Failed to push msg_id=%s to DLQ: %s  "
                "Message remains in PEL for manual recovery.",
                msg_id,
                dlq_exc,
            )

    # -- pending recovery ----------------------------------------------------

    async def _recover_pending(self) -> None:
        """On startup, claim and reprocess orphaned pending messages.

        This handles the case where a previous consumer instance crashed
        while processing messages.  The messages are claimed by this
        consumer and reprocessed.
        """
        logger.info(
            "Recovering pending messages for stream=%s group=%s consumer=%s",
            self._stream,
            self._group,
            self._consumer_name,
        )
        try:
            claimed: list[tuple[str, dict[str, str]]] = (
                await self._bus.claim_pending(
                    stream=self._stream,
                    group=self._group,
                    consumer=self._consumer_name,
                    min_idle_ms=self._pending_min_idle_ms,
                    count=100,
                )
            )
            if claimed:
                logger.info(
                    "Recovered %d pending messages from stream=%s",
                    len(claimed),
                    self._stream,
                )
                for msg_id, data in claimed:
                    if not self._running:
                        break
                    await self._process_message(msg_id, data, attempt=1)
            else:
                logger.debug(
                    "No pending messages to recover on stream=%s",
                    self._stream,
                )
        except Exception as exc:
            logger.error(
                "Pending recovery failed for stream=%s: %s",
                self._stream,
                exc,
            )

    async def _periodic_pending_recovery(self) -> None:
        """Periodically scan for stuck pending messages and reclaim them.

        Runs as a background task alongside the main consume loop.
        """
        interval_s: float = self._pending_claim_interval_ms / 1000.0

        while self._running:
            try:
                await asyncio.sleep(interval_s)
                if not self._running:
                    break

                claimed: list[tuple[str, dict[str, str]]] = (
                    await self._bus.claim_pending(
                        stream=self._stream,
                        group=self._group,
                        consumer=self._consumer_name,
                        min_idle_ms=self._pending_min_idle_ms,
                        count=50,
                    )
                )
                if claimed:
                    logger.info(
                        "Periodic recovery: claimed %d pending messages "
                        "from stream=%s",
                        len(claimed),
                        self._stream,
                    )
                    for msg_id, data in claimed:
                        if not self._running:
                            break
                        await self._process_message(msg_id, data, attempt=1)

            except asyncio.CancelledError:
                break
            except Exception as exc:
                logger.error(
                    "Periodic pending recovery error on stream=%s: %s",
                    self._stream,
                    exc,
                )
                # Sleep a bit extra on errors to avoid tight error loops
                await asyncio.sleep(5.0)
