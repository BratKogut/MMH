"""
MMH v3.1 -- L0 Sanitizer: first processing stage after raw collector events.

The L0 Sanitizer sits between the collectors and the core pipeline.  Every raw
event must pass through this gate before it can influence scoring, enrichment,
or execution.  Its job is to reject garbage early so downstream stages never
waste cycles on invalid data.

Formal rules
-------------
1. **Dedup**: keyed per chain --
   - EVM chains: ``{tx_hash}:{log_index}``
   - Solana: ``{signature}``
   Duplicate events are silently dropped with a ``DUPLICATE`` rejection code.

2. **Gap detection**: if sequence numbers are present and a gap is detected
   (expected ``N+1`` but received ``N+k``), emit a ``FREEZE`` event on the
   ``health:freeze`` stream and reject the event with ``GAP_DETECTED``.
   The gap counter is tracked per chain for observability.

3. **Fat-finger filters**: reject obviously invalid data before it can
   pollute the pipeline.  Each rejection carries a specific reason code
   (see :class:`RejectReason`).

4. **Blacklisted creators**: events whose ``creator`` field matches a
   known-bad address are rejected immediately.

5. **All rejections are logged** with structured reason codes and exposed
   via :meth:`L0Sanitizer.get_metrics` for Prometheus scraping.

Streams
-------
Consumes from:  ``tokens:new:{chain}``
Publishes to:   ``tokens:sanitized:{chain}``
Freeze events:  ``health:freeze``

Usage::

    sanitizer = L0Sanitizer(
        bus=redis_event_bus,
        redis_client=redis,
        enabled_chains=["solana", "base"],
        blacklisted_creators={"ScamAddr..."},
    )
    await sanitizer.start()
"""

from __future__ import annotations

import asyncio
import logging
import time
from enum import Enum
from typing import Any, Optional

logger = logging.getLogger(__name__)


# --------------------------------------------------------------------------- #
# Rejection reason codes
# --------------------------------------------------------------------------- #


class RejectReason(str, Enum):
    """Machine-readable codes attached to every rejected event.

    Each rejected event carries one or more of these codes so operators can
    build dashboards and alerts on specific failure modes.
    """

    DUPLICATE = "DUPLICATE"
    GAP_DETECTED = "GAP_DETECTED"
    INVALID_ADDRESS = "INVALID_ADDRESS"
    INVALID_CHAIN = "INVALID_CHAIN"
    MISSING_REQUIRED_FIELDS = "MISSING_REQUIRED_FIELDS"
    FAT_FINGER_LIQUIDITY = "FAT_FINGER_LIQUIDITY"
    FAT_FINGER_PRICE = "FAT_FINGER_PRICE"
    STALE_EVENT = "STALE_EVENT"
    FUTURE_TIMESTAMP = "FUTURE_TIMESTAMP"
    BLACKLISTED_CREATOR = "BLACKLISTED_CREATOR"
    SCHEMA_VALIDATION_ERROR = "SCHEMA_VALIDATION_ERROR"


# --------------------------------------------------------------------------- #
# Sanitization result
# --------------------------------------------------------------------------- #


class SanitizationResult:
    """Outcome of the L0 sanitization pipeline for a single event.

    Attributes:
        passed:
            ``True`` if the event survived all checks and should be forwarded
            downstream.  ``False`` if it was rejected.
        reject_reasons:
            Zero or more :class:`RejectReason` codes explaining *why* the
            event was rejected.  Empty when ``passed`` is ``True``.
        sanitized_data:
            The cleaned event dict with sanitization metadata appended
            (``sanitized_at``, ``sanitizer_version``).  ``None`` when the
            event was rejected.
    """

    __slots__ = ("passed", "reject_reasons", "sanitized_data")

    def __init__(
        self,
        passed: bool,
        reject_reasons: list[RejectReason] | None = None,
        sanitized_data: dict[str, Any] | None = None,
    ) -> None:
        self.passed = passed
        self.reject_reasons: list[RejectReason] = reject_reasons or []
        self.sanitized_data = sanitized_data

    def __repr__(self) -> str:
        if self.passed:
            return "SanitizationResult(passed=True)"
        reasons = ", ".join(r.value for r in self.reject_reasons)
        return f"SanitizationResult(passed=False, reasons=[{reasons}])"


# --------------------------------------------------------------------------- #
# Dedup store (Redis-backed)
# --------------------------------------------------------------------------- #


class DedupStore:
    """Redis-backed deduplication store.

    Uses ``SET NX EX`` for atomic check-and-insert with automatic TTL-based
    expiry.  No background cleanup required.

    Key format::

        dedup:{chain}:{dedup_key}

    Where ``dedup_key`` is chain-specific:

    - **EVM** chains (base, bsc, arbitrum, ...):  ``{tx_hash}:{log_index}``
    - **Solana**:                                  ``{signature}``

    Args:
        redis_client:
            An ``aioredis``-compatible async Redis client.
        ttl_seconds:
            How long to remember seen event keys.  Should be long enough to
            cover redelivery windows but short enough to bound memory usage.
            Default is 3600 s (1 hour); the global setting
            ``MMHSettings.dedup_ttl_seconds`` may override this.
    """

    def __init__(self, redis_client: Any, ttl_seconds: int = 3600) -> None:
        self._redis = redis_client
        self._ttl = ttl_seconds

    async def is_duplicate(self, chain: str, dedup_key: str) -> bool:
        """Check whether *dedup_key* has been seen before on *chain*.

        Atomically sets the key if it does not exist.  Returns ``True`` when
        the key was already present (i.e. this is a duplicate), ``False``
        on first encounter.

        The underlying Redis command is::

            SET dedup:{chain}:{dedup_key} 1 NX EX {ttl}

        ``NX`` ensures the SET only succeeds if the key is absent.
        ``EX`` attaches a TTL so memory is bounded.  The return value is
        ``None`` when the key already existed (duplicate).
        """
        key = f"dedup:{chain}:{dedup_key}"
        result = await self._redis.set(key, "1", nx=True, ex=self._ttl)
        # redis.set(..., nx=True) returns None when the key already existed
        return result is None

    @staticmethod
    def make_dedup_key_evm(tx_hash: str, log_index: int) -> str:
        """Build the dedup key for an EVM-chain event.

        Format: ``{tx_hash}:{log_index}``

        Both components are needed because a single transaction can emit
        multiple logs, each representing a distinct event.
        """
        return f"{tx_hash}:{log_index}"

    @staticmethod
    def make_dedup_key_solana(signature: str) -> str:
        """Build the dedup key for a Solana event.

        On Solana the transaction signature is globally unique, so it serves
        directly as the dedup key.
        """
        return signature


# --------------------------------------------------------------------------- #
# Gap detector
# --------------------------------------------------------------------------- #


class GapDetector:
    """Detects gaps in per-chain event sequence numbers.

    Collectors are expected to emit monotonically increasing sequence numbers
    per chain.  When the detector observes ``seq > last_seq + 1`` it reports
    a gap, which triggers a ``FREEZE`` event upstream.

    Sequence numbers that are *less than or equal to* the last seen value are
    silently ignored -- these indicate redeliveries or reordering, which the
    dedup layer handles separately.

    The first event on any chain always passes because there is no prior
    expectation to compare against.
    """

    def __init__(self) -> None:
        self._last_seq: dict[str, int] = {}
        self._gap_count: dict[str, int] = {}

    def check(self, chain: str, sequence: int) -> int | None:
        """Check for a gap on *chain* at *sequence*.

        Args:
            chain:    The chain identifier (e.g. ``"solana"``).
            sequence: The sequence number from the collector.

        Returns:
            The gap size (number of missing events) if a gap was detected,
            or ``None`` if the sequence is in order (or is the first event).
        """
        if chain not in self._last_seq:
            self._last_seq[chain] = sequence
            self._gap_count[chain] = 0
            return None

        expected = self._last_seq[chain] + 1

        if sequence == expected:
            # Normal case -- in order.
            self._last_seq[chain] = sequence
            return None
        elif sequence > expected:
            # Gap detected.
            gap = sequence - expected
            self._gap_count[chain] = self._gap_count.get(chain, 0) + gap
            self._last_seq[chain] = sequence
            return gap
        else:
            # sequence <= last_seq: reorder or duplicate -- handled by dedup.
            return None

    def get_gap_count(self, chain: str) -> int:
        """Return total cumulative gap count for *chain*."""
        return self._gap_count.get(chain, 0)

    def get_last_sequence(self, chain: str) -> int | None:
        """Return the last seen sequence number for *chain*, or ``None``."""
        return self._last_seq.get(chain)


# --------------------------------------------------------------------------- #
# Fat-finger filter
# --------------------------------------------------------------------------- #


class FatFingerFilter:
    """Rejects obviously invalid event data.

    Each check targets a specific class of bad data that should never reach
    the scoring or execution layers.  Thresholds are intentionally generous
    to avoid false positives -- the goal is to catch garbage, not borderline
    data.

    Configurable via class-level constants.  Subclass or monkey-patch in
    tests to override thresholds.
    """

    # -- Liquidity bounds ---------------------------------------------------
    MAX_LIQUIDITY_USD: float = 100_000_000  # $100 M -- above this is bad data
    MIN_LIQUIDITY_USD: float = 0            # 0 is valid for bonding-curve tokens

    # -- Timing bounds ------------------------------------------------------
    MAX_EVENT_AGE_SECONDS: float = 300      # 5 minutes stale threshold
    MAX_FUTURE_SECONDS: float = 30          # allow 30 s of clock skew

    # -- Address length bounds -----------------------------------------------
    MIN_ADDRESS_LENGTH: int = 20            # shortest plausible on-chain address
    MAX_ADDRESS_LENGTH: int = 100           # longest plausible on-chain address

    # -- Valid chains --------------------------------------------------------
    VALID_CHAINS: frozenset[str] = frozenset(
        {"solana", "base", "bsc", "ton", "arbitrum", "tron"}
    )

    def check(self, event_data: dict[str, Any]) -> list[RejectReason]:
        """Run all fat-finger checks on *event_data*.

        Returns a (possibly empty) list of :class:`RejectReason` codes.
        An empty list means all checks passed.

        The checks are executed in a fixed order but *all* checks run
        regardless of earlier failures so that the caller receives the
        complete set of problems in one pass.

        Check order:
            1. Chain validity
            2. Address format
            3. Required fields presence
            4. Liquidity bounds
            5. Timestamp staleness and future-drift
        """
        reasons: list[RejectReason] = []

        # 1. Chain validation
        chain = event_data.get("chain", "")
        if chain not in self.VALID_CHAINS:
            reasons.append(RejectReason.INVALID_CHAIN)

        # 2. Address validation -- check whichever address field is present
        address = (
            event_data.get("address", "")
            or event_data.get("token_address", "")
        )
        if address and (
            len(address) < self.MIN_ADDRESS_LENGTH
            or len(address) > self.MAX_ADDRESS_LENGTH
        ):
            reasons.append(RejectReason.INVALID_ADDRESS)

        # 3. Required fields
        required_fields = ("chain", "event_type", "event_id")
        for field in required_fields:
            if not event_data.get(field):
                reasons.append(RejectReason.MISSING_REQUIRED_FIELDS)
                break  # one reason code is sufficient

        # 4. Liquidity bounds
        liquidity = event_data.get("initial_liquidity_usd")
        if liquidity is not None:
            try:
                liq = float(liquidity)
                if liq > self.MAX_LIQUIDITY_USD:
                    reasons.append(RejectReason.FAT_FINGER_LIQUIDITY)
            except (ValueError, TypeError):
                reasons.append(RejectReason.FAT_FINGER_LIQUIDITY)

        # 5. Timestamp checks
        event_time = event_data.get("event_time")
        if event_time is not None:
            try:
                et = float(event_time)
                now = time.time()
                if now - et > self.MAX_EVENT_AGE_SECONDS:
                    reasons.append(RejectReason.STALE_EVENT)
                if et - now > self.MAX_FUTURE_SECONDS:
                    reasons.append(RejectReason.FUTURE_TIMESTAMP)
            except (ValueError, TypeError):
                pass  # non-numeric event_time is not rejected here

        return reasons


# --------------------------------------------------------------------------- #
# L0 Sanitizer (main orchestrator)
# --------------------------------------------------------------------------- #


class L0Sanitizer:
    """L0 Sanitizer -- the first processing stage in the MMH v3.1 pipeline.

    Consumes raw events from ``tokens:new:{chain}`` Redis streams, runs them
    through validation / dedup / fat-finger checks, and publishes surviving
    events to ``tokens:sanitized:{chain}``.

    Processing pipeline (per event)
    --------------------------------
    1. Fat-finger filters  -- reject obviously invalid data.
    2. Blacklisted creator -- reject known-bad deployer addresses.
    3. Dedup check         -- reject events already seen (Redis SET NX).
    4. Gap detection       -- detect missing sequence numbers and emit FREEZE.

    If *any* check fails the event is rejected, the reason codes are counted
    in metrics, and the event is **not** forwarded downstream.

    On gap detection the sanitizer publishes a ``FreezeEvent`` to the
    ``health:freeze`` stream so the control plane can halt trading on the
    affected chain.

    Args:
        bus:
            A ``RedisEventBus`` (or compatible) instance providing
            ``ensure_consumer_group``, ``consume``, ``publish``, and ``ack``
            async methods.
        redis_client:
            An ``aioredis``-compatible async Redis client used by the
            :class:`DedupStore`.
        enabled_chains:
            List of chain identifiers to consume from (e.g.
            ``["solana", "base"]``).
        blacklisted_creators:
            Optional set of creator/deployer addresses that should always be
            rejected.

    Example::

        sanitizer = L0Sanitizer(
            bus=event_bus,
            redis_client=redis,
            enabled_chains=settings.enabled_chains,
            blacklisted_creators={"ScamDeploy..."},
        )
        await sanitizer.start()   # runs until stop() is called
        metrics = sanitizer.get_metrics()
    """

    #: Semantic version stamped onto every sanitized event.
    SANITIZER_VERSION: str = "3.1.0"

    def __init__(
        self,
        bus: Any,
        redis_client: Any,
        enabled_chains: list[str],
        blacklisted_creators: set[str] | None = None,
    ) -> None:
        self._bus = bus
        self._redis = redis_client
        self._chains = list(enabled_chains)
        self._dedup = DedupStore(redis_client)
        self._gap_detector = GapDetector()
        self._fat_finger = FatFingerFilter()
        self._blacklisted_creators: set[str] = blacklisted_creators or set()
        self._running: bool = False

        # ---- Metrics counters ----
        self._processed: int = 0
        self._passed: int = 0
        self._rejected: int = 0
        self._reject_reasons: dict[str, int] = {}
        self._gaps_detected: int = 0

    # -- lifecycle ---------------------------------------------------------- #

    async def start(self) -> None:
        """Start sanitizer consumers for all enabled chains.

        Creates one :func:`asyncio.create_task` per chain and ``await``s them
        concurrently via :func:`asyncio.gather`.  The method blocks until
        :meth:`stop` is called (typically from a signal handler or the
        control plane).
        """
        self._running = True
        tasks = [
            asyncio.create_task(
                self._consume_chain(chain),
                name=f"l0-sanitizer-{chain}",
            )
            for chain in self._chains
        ]
        logger.info(
            "L0Sanitizer started",
            extra={"chains": self._chains},
        )
        await asyncio.gather(*tasks)

    async def stop(self) -> None:
        """Signal all consumer loops to exit after their current iteration."""
        self._running = False
        logger.info("L0Sanitizer stop requested")

    # -- per-chain consumer loop -------------------------------------------- #

    async def _consume_chain(self, chain: str) -> None:
        """Consume and sanitize events for a single chain.

        Reads from ``tokens:new:{chain}`` in batches of 10, processes each
        event through :meth:`sanitize`, and either forwards the result to
        ``tokens:sanitized:{chain}`` or logs the rejection.

        The consumer uses Redis consumer groups for at-least-once delivery
        semantics.  Messages are ACK'd only after successful processing
        (including publish on pass or metric-recording on reject).

        On transient errors the loop sleeps 1 s before retrying -- the
        circuit breaker at the bus level handles sustained outages.
        """
        input_stream = f"tokens:new:{chain}"
        output_stream = f"tokens:sanitized:{chain}"
        group = "sanitizer"
        consumer = f"sanitizer-{chain}"

        await self._bus.ensure_consumer_group(input_stream, group)

        while self._running:
            try:
                messages = await self._bus.consume(
                    input_stream,
                    group,
                    consumer,
                    count=10,
                    block_ms=2000,
                )

                for msg_id, data in messages:
                    result = await self.sanitize(data, chain)
                    self._processed += 1

                    if result.passed:
                        await self._bus.publish(output_stream, result.sanitized_data)
                        self._passed += 1
                    else:
                        for reason in result.reject_reasons:
                            self._reject_reasons[reason.value] = (
                                self._reject_reasons.get(reason.value, 0) + 1
                            )
                        self._rejected += 1
                        logger.info(
                            "Event rejected by L0 Sanitizer",
                            extra={
                                "event_id": data.get("event_id", "unknown"),
                                "chain": chain,
                                "reasons": [r.value for r in result.reject_reasons],
                            },
                        )

                    await self._bus.ack(input_stream, group, msg_id)

            except Exception:
                logger.exception("L0 Sanitizer error on chain %s", chain)
                await asyncio.sleep(1)

    # -- core sanitization pipeline ----------------------------------------- #

    async def sanitize(
        self,
        event_data: dict[str, Any],
        chain: str,
    ) -> SanitizationResult:
        """Run the full sanitization pipeline on a single event.

        This method is the public entry point for sanitization and can be
        called directly in tests without starting the consumer loop.

        Pipeline order:
            1. Fat-finger filters (stateless, fast)
            2. Blacklisted creator check (set lookup)
            3. Dedup check (Redis round-trip, skipped if earlier checks failed)
            4. Gap detection (in-memory per-chain state)

        Args:
            event_data: Raw event dict from the collector.
            chain:      Chain identifier (e.g. ``"solana"``).

        Returns:
            A :class:`SanitizationResult` indicating pass/reject with reason
            codes and, on pass, the cleaned event dict.
        """
        reject_reasons: list[RejectReason] = []

        # -- 1. Fat-finger filters ------------------------------------------
        ff_reasons = self._fat_finger.check(event_data)
        reject_reasons.extend(ff_reasons)

        # -- 2. Blacklisted creator -----------------------------------------
        creator = event_data.get("creator", "")
        if creator and creator in self._blacklisted_creators:
            reject_reasons.append(RejectReason.BLACKLISTED_CREATOR)

        # -- 3. Dedup check (skip if basic validation already failed) --------
        if not reject_reasons:
            dedup_key = self._make_dedup_key(event_data, chain)
            if dedup_key and await self._dedup.is_duplicate(chain, dedup_key):
                reject_reasons.append(RejectReason.DUPLICATE)

        # -- 4. Gap detection -----------------------------------------------
        seq = event_data.get("sequence")
        if seq is not None:
            gap = self._gap_detector.check(chain, int(seq))
            if gap is not None:
                self._gaps_detected += 1
                logger.warning(
                    "Sequence gap detected",
                    extra={"chain": chain, "gap_size": gap},
                )
                await self._publish_freeze(chain, f"gap_detected:size={gap}")
                reject_reasons.append(RejectReason.GAP_DETECTED)

        # -- Result ---------------------------------------------------------
        if reject_reasons:
            return SanitizationResult(passed=False, reject_reasons=reject_reasons)

        # All checks passed -- stamp sanitization metadata.
        sanitized = dict(event_data)
        sanitized["sanitized_at"] = str(time.time())
        sanitized["sanitizer_version"] = self.SANITIZER_VERSION

        return SanitizationResult(passed=True, sanitized_data=sanitized)

    # -- dedup key construction --------------------------------------------- #

    def _make_dedup_key(
        self,
        event_data: dict[str, Any],
        chain: str,
    ) -> str | None:
        """Build a chain-appropriate dedup key from *event_data*.

        EVM chains use ``tx_hash:log_index`` because a single transaction can
        emit multiple log events.  Solana uses the transaction signature
        directly since it is globally unique.

        Returns ``None`` if the event lacks the fields needed to build a key,
        in which case dedup is skipped (the event is treated as unique).
        """
        if chain == "solana":
            sig = event_data.get("tx_hash") or event_data.get("signature")
            if sig:
                return self._dedup.make_dedup_key_solana(sig)
            return None

        # EVM chains (base, bsc, arbitrum, tron, ...)
        tx_hash = event_data.get("tx_hash")
        log_index = event_data.get("log_index")
        if tx_hash and log_index is not None:
            return self._dedup.make_dedup_key_evm(tx_hash, int(log_index))
        elif tx_hash:
            # Fallback: tx_hash alone (no log_index available).
            return tx_hash
        return None

    # -- freeze event ------------------------------------------------------- #

    async def _publish_freeze(self, chain: str, reason: str) -> None:
        """Publish a FREEZE event to the ``health:freeze`` stream.

        The control plane monitors this stream and can halt trading on the
        affected chain until the gap is investigated and resolved.

        Args:
            chain:  The chain where the gap was detected.
            reason: Human-readable reason string (e.g.
                    ``"gap_detected:size=3"``).
        """
        freeze_data: dict[str, str] = {
            "event_type": "FreezeEvent",
            "chain": chain,
            "reason": reason,
            "triggered_by": "L0Sanitizer",
            "auto": "true",
            "event_time": str(time.time()),
        }
        await self._bus.publish("health:freeze", freeze_data)
        logger.warning(
            "FREEZE event published",
            extra={"chain": chain, "reason": reason},
        )

    # -- observability ------------------------------------------------------ #

    def get_metrics(self) -> dict[str, Any]:
        """Return a snapshot of sanitizer metrics for Prometheus / dashboards.

        Returns:
            A dict with the following keys:

            - ``processed``           -- total events received
            - ``passed``              -- events forwarded downstream
            - ``rejected``            -- events dropped
            - ``pass_rate``           -- ``passed / processed`` (0.0 if none)
            - ``reject_reasons``      -- per-reason counters
            - ``gaps_detected``       -- total gap events
            - ``gap_counts_per_chain``-- cumulative gap count per chain
        """
        processed = self._processed
        return {
            "processed": processed,
            "passed": self._passed,
            "rejected": self._rejected,
            "pass_rate": (
                round(self._passed / processed, 4) if processed > 0 else 0.0
            ),
            "reject_reasons": dict(self._reject_reasons),
            "gaps_detected": self._gaps_detected,
            "gap_counts_per_chain": {
                chain: self._gap_detector.get_gap_count(chain)
                for chain in self._chains
            },
        }

    def __repr__(self) -> str:
        return (
            f"L0Sanitizer(chains={self._chains!r}, "
            f"processed={self._processed}, "
            f"passed={self._passed}, "
            f"rejected={self._rejected})"
        )
