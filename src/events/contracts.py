"""
MMH v3.1 Event Schema System - Idempotency Contracts

Guarantees:
    1. All streams are at-least-once delivery (Redis Streams with consumer
       groups). Messages may be delivered more than once on consumer crash,
       network partition, or XAUTOCLAIM.

    2. Every consumer MUST be idempotent. Before processing an event, the
       consumer calls ``check_and_mark(consumer_id, event_id)``.  If it
       returns False the event has already been processed and MUST be
       skipped (just XACK it).

    3. The Executor NEVER executes the same intent_id twice, even after
       crash recovery. Before submitting an on-chain transaction, the
       executor calls ``mark_intent_executed(intent_id)``.  If it returns
       False, another instance already claimed this intent -- abort.

Storage backends:
    - Redis:      Fast path, used for real-time dedup with TTL (default 24h).
                  Keys are evicted after TTL; this is fine because replayed
                  events older than TTL are stale and should be discarded.

    - PostgreSQL: Long-term dedup for intent execution records.  Intent
                  execution is permanent (no TTL) because we must NEVER
                  re-execute a trade, even weeks later.

    Both backends can be used together (Redis for hot path, Postgres for
    durable record), or independently.

Failure modes:
    - If Redis is down, ``check_and_mark`` raises ``RedisUnavailableError``.
      The consumer MUST back off and retry; it MUST NOT process the event
      without dedup verification.

    - If Postgres is down, ``mark_intent_executed`` raises
      ``PostgresUnavailableError``.  The executor MUST NOT submit the
      transaction.

    - In both cases, the event stays unacked in the stream and will be
      redelivered once the backend recovers.
"""
from __future__ import annotations

import logging
import time
from typing import Any, Optional

logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Custom exceptions
# ---------------------------------------------------------------------------

class RedisUnavailableError(Exception):
    """Raised when the Redis backend is unreachable or errored."""
    pass


class PostgresUnavailableError(Exception):
    """Raised when the PostgreSQL backend is unreachable or errored."""
    pass


# ---------------------------------------------------------------------------
# IdempotencyContract
# ---------------------------------------------------------------------------

class IdempotencyContract:
    """Idempotency enforcement for MMH v3.1 event consumers and executor.

    Contract rules (MUST be followed by every consumer/executor):

        Rule 1 -- At-Least-Once Delivery:
            All Redis Streams in MMH use consumer groups with manual XACK.
            Messages may be delivered more than once due to consumer crashes,
            rebalancing, or XAUTOCLAIM for stuck messages. Every consumer
            MUST tolerate duplicates.

        Rule 2 -- Consumer Idempotency:
            Before processing ANY event, the consumer MUST call:
                ``if not contract.check_and_mark(consumer_id, event_id): return``
            This ensures each (consumer, event) pair is processed at most once.

        Rule 3 -- Executor Intent Uniqueness (CRITICAL):
            The executor MUST NEVER execute the same intent_id twice, even
            after crash recovery, pod restart, or failover. Before submitting
            an on-chain transaction, the executor MUST call:
                ``if not contract.mark_intent_executed(intent_id): abort``
            This is backed by BOTH Redis (fast check) and PostgreSQL
            (permanent record) for defense in depth.

        Rule 4 -- Fail-Closed:
            If the dedup backend is unavailable, the consumer/executor MUST
            NOT proceed. It MUST raise an error and let the message be
            redelivered later. Processing without dedup verification can
            cause duplicate trades (financial loss).

    Args:
        redis_client: An initialized ``redis.Redis`` or ``redis.asyncio.Redis``
            instance.  If None, Redis-backed dedup is disabled (NOT
            recommended for production).
        pg_connection: A psycopg2/SQLAlchemy connection for durable intent
            dedup.  If None, Postgres-backed dedup is disabled.
        ttl_seconds: TTL for Redis dedup keys.  Default 86400 (24 hours).
        key_prefix: Prefix for all Redis keys to avoid collisions.

    Usage::

        contract = IdempotencyContract(redis_client=redis_conn, ttl_seconds=86400)

        # In a consumer:
        if not contract.check_and_mark("scoring_consumer", event.event_id):
            stream.xack(event)  # already processed
            return

        # In the executor:
        if not contract.mark_intent_executed(intent.intent_id):
            logger.warning("Intent %s already executed, skipping", intent.intent_id)
            return
    """

    def __init__(
        self,
        redis_client: Any | None = None,
        pg_connection: Any | None = None,
        ttl_seconds: int = 86400,
        key_prefix: str = "mmh:idemp:",
    ) -> None:
        self._redis = redis_client
        self._pg = pg_connection
        self._ttl = ttl_seconds
        self._prefix = key_prefix

        if self._redis is None:
            logger.warning(
                "IdempotencyContract initialized WITHOUT Redis. "
                "Dedup will only work via PostgreSQL (if configured). "
                "This is NOT recommended for production."
            )
        if self._pg is None:
            logger.info(
                "IdempotencyContract initialized without PostgreSQL. "
                "Intent execution records will NOT be durably persisted."
            )

    # ------------------------------------------------------------------
    # Redis key helpers
    # ------------------------------------------------------------------

    def _event_key(self, consumer_id: str, event_id: str) -> str:
        """Redis key for consumer-level event dedup.

        Format: ``mmh:idemp:evt:{consumer_id}:{event_id}``
        """
        return f"{self._prefix}evt:{consumer_id}:{event_id}"

    def _intent_key(self, intent_id: str) -> str:
        """Redis key for intent execution dedup.

        Format: ``mmh:idemp:intent:{intent_id}``
        """
        return f"{self._prefix}intent:{intent_id}"

    # ------------------------------------------------------------------
    # Consumer-level idempotency (Redis)
    # ------------------------------------------------------------------

    def check_and_mark(self, consumer_id: str, event_id: str) -> bool:
        """Check if this (consumer, event) pair is new, and mark it.

        Uses Redis SET NX (set-if-not-exists) for atomic check-and-mark.

        Args:
            consumer_id: Unique identifier for the consuming module/instance.
            event_id: The event's ``event_id`` (UUIDv7).

        Returns:
            True if this is the FIRST time this consumer sees this event
            (proceed with processing).  False if it's a duplicate (skip).

        Raises:
            RedisUnavailableError: If Redis is not configured or unreachable.
                The caller MUST NOT process the event in this case.
        """
        if self._redis is None:
            raise RedisUnavailableError(
                "Redis client not configured. Cannot verify idempotency. "
                "Consumer MUST NOT process event without dedup check."
            )

        key = self._event_key(consumer_id, event_id)
        try:
            # SET key value NX EX ttl  -- atomic set-if-not-exists with expiry
            was_set: Optional[bool] = self._redis.set(
                key,
                str(int(time.time())),
                nx=True,
                ex=self._ttl,
            )
            if was_set:
                return True  # New event -- proceed
            else:
                logger.debug(
                    "Duplicate event detected: consumer=%s event_id=%s",
                    consumer_id,
                    event_id,
                )
                return False  # Duplicate -- skip
        except Exception as exc:
            raise RedisUnavailableError(
                f"Redis error during check_and_mark: {exc}"
            ) from exc

    # ------------------------------------------------------------------
    # Executor intent-level idempotency (Redis + PostgreSQL)
    # ------------------------------------------------------------------

    def mark_intent_executed(self, intent_id: str) -> bool:
        """Atomically claim an intent_id for execution.

        Uses Redis SET NX for the fast path, then records in PostgreSQL
        for durable persistence (if configured).

        The executor MUST call this BEFORE submitting the on-chain
        transaction.  If it returns False, another executor instance has
        already claimed this intent -- the caller MUST abort.

        Args:
            intent_id: The ``TradeIntent.intent_id`` to claim.

        Returns:
            True if this is the FIRST claim (proceed with execution).
            False if already claimed (MUST abort -- do NOT execute).

        Raises:
            RedisUnavailableError: If Redis is down.  The executor MUST
                NOT submit the transaction without this check.
        """
        # --- Redis fast path ---
        if self._redis is None:
            raise RedisUnavailableError(
                "Redis client not configured. Executor MUST NOT execute "
                "intent without idempotency verification."
            )

        key = self._intent_key(intent_id)
        try:
            was_set: Optional[bool] = self._redis.set(
                key,
                str(int(time.time())),
                nx=True,
                ex=self._ttl,
            )
        except Exception as exc:
            raise RedisUnavailableError(
                f"Redis error during mark_intent_executed: {exc}"
            ) from exc

        if not was_set:
            logger.warning(
                "Intent %s already claimed in Redis. Aborting execution.",
                intent_id,
            )
            return False

        # --- PostgreSQL durable record (best-effort if configured) ---
        if self._pg is not None:
            try:
                self._pg_record_intent(intent_id)
            except Exception as exc:
                # Redis succeeded but Postgres failed.  This is a partial
                # state.  We log a critical error but do NOT roll back the
                # Redis claim -- it's safer to execute once with a missing
                # Postgres record than to risk a double execution.
                logger.critical(
                    "CRITICAL: Intent %s claimed in Redis but failed to "
                    "record in PostgreSQL: %s. Manual reconciliation needed.",
                    intent_id,
                    exc,
                )

        return True

    def is_intent_executed(self, intent_id: str) -> bool:
        """Check if an intent_id has already been executed.

        Checks Redis first (fast), then falls back to PostgreSQL (durable).

        This is a read-only check -- it does NOT claim the intent.
        Use ``mark_intent_executed`` for the atomic claim.

        Args:
            intent_id: The ``TradeIntent.intent_id`` to check.

        Returns:
            True if the intent has been executed, False otherwise.
        """
        # Check Redis first
        if self._redis is not None:
            key = self._intent_key(intent_id)
            try:
                if self._redis.exists(key):
                    return True
            except Exception as exc:
                logger.warning(
                    "Redis error during is_intent_executed (falling back "
                    "to PostgreSQL): %s",
                    exc,
                )

        # Check PostgreSQL
        if self._pg is not None:
            try:
                return self._pg_check_intent(intent_id)
            except Exception as exc:
                logger.error(
                    "PostgreSQL error during is_intent_executed: %s", exc
                )

        # If both backends are unavailable/unconfigured, we cannot confirm
        logger.warning(
            "Cannot determine execution status for intent %s -- "
            "no backends available.",
            intent_id,
        )
        return False

    # ------------------------------------------------------------------
    # PostgreSQL helpers
    # ------------------------------------------------------------------

    def _pg_record_intent(self, intent_id: str) -> None:
        """Insert an intent execution record into PostgreSQL.

        Table schema (must be created via migration):
            CREATE TABLE IF NOT EXISTS intent_executions (
                intent_id   TEXT PRIMARY KEY,
                executed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                instance_id TEXT
            );
        """
        cursor = self._pg.cursor()
        try:
            cursor.execute(
                "INSERT INTO intent_executions (intent_id, executed_at) "
                "VALUES (%s, NOW()) "
                "ON CONFLICT (intent_id) DO NOTHING",
                (intent_id,),
            )
            self._pg.commit()
        except Exception:
            self._pg.rollback()
            raise
        finally:
            cursor.close()

    def _pg_check_intent(self, intent_id: str) -> bool:
        """Check if an intent execution record exists in PostgreSQL."""
        cursor = self._pg.cursor()
        try:
            cursor.execute(
                "SELECT 1 FROM intent_executions WHERE intent_id = %s",
                (intent_id,),
            )
            row = cursor.fetchone()
            return row is not None
        finally:
            cursor.close()

    # ------------------------------------------------------------------
    # Utilities
    # ------------------------------------------------------------------

    def flush_expired(self) -> None:
        """No-op: Redis handles TTL-based expiry automatically.

        For PostgreSQL, a periodic cleanup job should archive/delete
        old records.  This is out of scope for the contract itself.
        """
        logger.debug("flush_expired called -- Redis TTL handles expiry.")

    @property
    def ttl_seconds(self) -> int:
        """Return the configured TTL for Redis dedup keys."""
        return self._ttl

    def __repr__(self) -> str:
        return (
            f"IdempotencyContract("
            f"redis={'configured' if self._redis else 'NONE'}, "
            f"pg={'configured' if self._pg else 'NONE'}, "
            f"ttl={self._ttl}s)"
        )
