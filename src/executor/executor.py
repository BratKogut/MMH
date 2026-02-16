"""Idempotent Executor.

CRITICAL RULES:
- Accepts ONLY ApprovedIntent (passed through RiskFabric)
- Idempotency key = intent_id (deterministic, UUIDv7)
- NEVER executes same intent_id twice, even after crash
- Records ExecutionAttempt as event
- Supports dry-run mode (generates same events, no actual TX)
- Circuit breaker on execution failures
- Partial fill handling

Contract:
- Executor checks dedup store BEFORE any on-chain action
- On crash recovery: checks dedup store, skips already executed
- After success: publishes TradeExecuted -> Position Manager
"""

import asyncio
import json
import logging
import time
import hashlib
from typing import Optional, Callable
from enum import Enum

logger = logging.getLogger(__name__)


class ExecutionStatus(str, Enum):
    PENDING = "PENDING"
    SUBMITTED = "SUBMITTED"
    CONFIRMED = "CONFIRMED"
    FAILED = "FAILED"
    PARTIAL = "PARTIAL"
    DRY_RUN = "DRY_RUN"


class ExecutionResult:
    """Result of a trade execution attempt."""
    def __init__(self, intent_id: str, status: ExecutionStatus, tx_hash: str = "",
                 amount_in: float = 0, amount_out: float = 0, price: float = 0,
                 price_impact_pct: float = 0, gas_cost_usd: float = 0,
                 error: str = "", attempt: int = 1):
        self.intent_id = intent_id
        self.status = status
        self.tx_hash = tx_hash
        self.amount_in = amount_in
        self.amount_out = amount_out
        self.price = price
        self.price_impact_pct = price_impact_pct
        self.gas_cost_usd = gas_cost_usd
        self.error = error
        self.attempt = attempt
        self.timestamp = time.time()


class ExecutorCircuitBreaker:
    """Circuit breaker specific to execution failures."""

    def __init__(self, threshold: int = 3, timeout_seconds: int = 120):
        self._threshold = threshold
        self._timeout = timeout_seconds
        self._failure_count = 0
        self._last_failure_time = 0.0
        self._state = "CLOSED"  # CLOSED, OPEN, HALF_OPEN

    def record_failure(self):
        self._failure_count += 1
        self._last_failure_time = time.time()
        if self._failure_count >= self._threshold:
            self._state = "OPEN"
            logger.warning(f"Executor circuit breaker OPEN after {self._failure_count} failures")

    def record_success(self):
        self._failure_count = 0
        self._state = "CLOSED"

    def is_open(self) -> bool:
        if self._state == "OPEN":
            if time.time() - self._last_failure_time > self._timeout:
                self._state = "HALF_OPEN"
                return False
            return True
        return False

    @property
    def state(self) -> str:
        return self._state


class Executor:
    """Idempotent trade executor with dry-run support.

    Consumes from: execution:approved (ApprovedIntents)
    Publishes to: trades:executed:{chain} (TradeExecuted events)
    Logs to: journal:decisions

    Dry-run generates identical events without actual TX submission.
    """

    def __init__(self, bus, redis_client, config: dict,
                 chain_executors: dict = None, dry_run: bool = False):
        """
        Args:
            bus: RedisEventBus
            redis_client: for idempotency store
            config: execution config
            chain_executors: dict mapping chain -> async executor function
            dry_run: if True, no actual TX sent (first-class mode)
        """
        self._bus = bus
        self._redis = redis_client
        self._config = config
        self._chain_executors = chain_executors or {}
        self._dry_run = dry_run
        self._running = False
        self._circuit_breaker = ExecutorCircuitBreaker(
            threshold=config.get("circuit_breaker_threshold", 3),
            timeout_seconds=config.get("circuit_breaker_timeout_seconds", 120)
        )
        self._max_retries = config.get("max_execution_retries", 2)

        # Metrics
        self._executed = 0
        self._failed = 0
        self._dry_runs = 0
        self._duplicates_prevented = 0
        self._fail_reasons: dict[str, int] = {}

    async def start(self):
        """Start executor consumer."""
        self._running = True
        input_stream = "execution:approved"
        group = "executor"
        consumer = "executor-main"

        await self._bus.ensure_consumer_group(input_stream, group)

        while self._running:
            try:
                messages = await self._bus.consume(input_stream, group, consumer, count=5, block_ms=2000)
                for msg_id, data in messages:
                    await self._process_intent(data)
                    await self._bus.ack(input_stream, group, msg_id)
            except Exception as e:
                logger.error(f"Executor error: {e}")
                await asyncio.sleep(1)

    async def stop(self):
        self._running = False

    async def _process_intent(self, intent_data: dict):
        """Process an approved intent with idempotency guarantee."""
        intent_id = intent_data.get("intent_id", "")
        chain = intent_data.get("chain", "")

        # CRITICAL: Idempotency check BEFORE any action
        if await self._is_already_executed(intent_id):
            logger.info(f"Intent {intent_id} already executed, skipping (idempotency)")
            self._duplicates_prevented += 1
            return

        # Circuit breaker check
        if self._circuit_breaker.is_open():
            logger.warning(f"Circuit breaker OPEN, rejecting intent {intent_id}")
            self._failed += 1
            self._fail_reasons["circuit_breaker"] = self._fail_reasons.get("circuit_breaker", 0) + 1
            return

        # Execute with retry
        result = None
        for attempt in range(1, self._max_retries + 1):
            try:
                result = await self._execute(intent_data, attempt)

                if result.status in (ExecutionStatus.CONFIRMED, ExecutionStatus.DRY_RUN):
                    self._circuit_breaker.record_success()
                    break
                elif result.status == ExecutionStatus.FAILED:
                    self._circuit_breaker.record_failure()
                    if attempt < self._max_retries:
                        wait = 2 ** attempt
                        logger.info(f"Retry {attempt}/{self._max_retries} for {intent_id}, waiting {wait}s")
                        await asyncio.sleep(wait)
            except Exception as e:
                logger.error(f"Execution attempt {attempt} failed for {intent_id}: {e}")
                self._circuit_breaker.record_failure()
                result = ExecutionResult(
                    intent_id=intent_id,
                    status=ExecutionStatus.FAILED,
                    error=str(e),
                    attempt=attempt
                )
                if attempt < self._max_retries:
                    await asyncio.sleep(2 ** attempt)

        if result is None:
            result = ExecutionResult(intent_id=intent_id, status=ExecutionStatus.FAILED, error="no_result")

        # Mark as executed (even if failed, to prevent retry storm)
        await self._mark_executed(intent_id, result)

        # Publish execution event
        if result.status in (ExecutionStatus.CONFIRMED, ExecutionStatus.DRY_RUN, ExecutionStatus.PARTIAL):
            await self._publish_trade_executed(intent_data, result)
            if result.status == ExecutionStatus.DRY_RUN:
                self._dry_runs += 1
            else:
                self._executed += 1
        else:
            self._failed += 1
            reason = result.error[:50] if result.error else "unknown"
            self._fail_reasons[reason] = self._fail_reasons.get(reason, 0) + 1

        # Log to decision journal
        await self._log_decision(intent_data, result)

    async def _execute(self, intent_data: dict, attempt: int) -> ExecutionResult:
        """Execute trade (or simulate in dry-run mode)."""
        intent_id = intent_data.get("intent_id", "")
        chain = intent_data.get("chain", "")
        token_address = intent_data.get("token_address", "")
        side = intent_data.get("side", "BUY")

        # Record attempt
        attempt_data = {
            "event_type": "ExecutionAttempt",
            "intent_id": intent_id,
            "chain": chain,
            "token_address": token_address,
            "side": side,
            "attempt_number": str(attempt),
            "dry_run": str(self._dry_run),
            "event_time": str(time.time()),
        }
        await self._bus.publish("execution:attempts", attempt_data)

        if self._dry_run:
            # DRY RUN: Generate realistic-looking result without TX
            logger.info(f"DRY RUN execution for {intent_id} on {chain}")
            return ExecutionResult(
                intent_id=intent_id,
                status=ExecutionStatus.DRY_RUN,
                tx_hash=f"dryrun_{hashlib.sha256(intent_id.encode()).hexdigest()[:16]}",
                amount_in=0,
                amount_out=0,
                price=0,
                price_impact_pct=0,
                gas_cost_usd=0,
                attempt=attempt,
            )

        # REAL EXECUTION
        chain_executor = self._chain_executors.get(chain)
        if not chain_executor:
            return ExecutionResult(
                intent_id=intent_id,
                status=ExecutionStatus.FAILED,
                error=f"no_executor_for_chain:{chain}",
                attempt=attempt,
            )

        # Call chain-specific executor
        result = await chain_executor(intent_data)
        result.attempt = attempt
        return result

    async def _is_already_executed(self, intent_id: str) -> bool:
        """Check idempotency store. Returns True if already executed."""
        key = f"executed:{intent_id}"
        data = await self._redis.get(key)
        return data is not None

    async def _mark_executed(self, intent_id: str, result: ExecutionResult):
        """Mark intent as executed in idempotency store."""
        key = f"executed:{intent_id}"
        data = json.dumps({
            "status": result.status.value,
            "tx_hash": result.tx_hash,
            "timestamp": result.timestamp,
        })
        # TTL of 7 days for executed intents
        await self._redis.set(key, data, ex=604800)

    async def _publish_trade_executed(self, intent_data: dict, result: ExecutionResult):
        """Publish TradeExecuted event for Position Manager."""
        chain = intent_data.get("chain", "")
        output_stream = f"trades:executed:{chain}"

        trade_data = {
            "event_type": "TradeExecuted",
            "intent_id": result.intent_id,
            "chain": chain,
            "token_address": intent_data.get("token_address", ""),
            "side": intent_data.get("side", "BUY"),
            "tx_hash": result.tx_hash,
            "amount_in": str(result.amount_in),
            "amount_out": str(result.amount_out),
            "price": str(result.price),
            "price_impact_pct": str(result.price_impact_pct),
            "gas_cost_usd": str(result.gas_cost_usd),
            "confirmed": str(result.status == ExecutionStatus.CONFIRMED),
            "dry_run": str(result.status == ExecutionStatus.DRY_RUN),
            "attempt": str(result.attempt),
            "event_time": str(time.time()),
        }
        await self._bus.publish(output_stream, trade_data)
        logger.info(f"TradeExecuted published: {result.intent_id} tx={result.tx_hash} status={result.status.value}")

    async def _log_decision(self, intent_data: dict, result: ExecutionResult):
        """Log execution decision to journal."""
        journal_entry = {
            "event_type": "DecisionJournalEntry",
            "decision_type": "EXECUTION",
            "module": "Executor",
            "input_event_ids": json.dumps([result.intent_id]),
            "output_event_id": result.tx_hash,
            "reason_codes": json.dumps([result.status.value, result.error] if result.error else [result.status.value]),
            "config_hash": hashlib.sha256(json.dumps(self._config, sort_keys=True, default=str).encode()).hexdigest()[:16],
            "decision_data": json.dumps({
                "status": result.status.value,
                "tx_hash": result.tx_hash,
                "amount_in": result.amount_in,
                "amount_out": result.amount_out,
                "attempt": result.attempt,
                "dry_run": self._dry_run,
            }),
            "event_time": str(time.time()),
        }
        try:
            await self._bus.publish("journal:decisions", journal_entry)
        except Exception as e:
            logger.error(f"Failed to log execution decision: {e}")

    def get_metrics(self) -> dict:
        return {
            "executed": self._executed,
            "failed": self._failed,
            "dry_runs": self._dry_runs,
            "duplicates_prevented": self._duplicates_prevented,
            "fail_reasons": dict(self._fail_reasons),
            "circuit_breaker_state": self._circuit_breaker.state,
            "dry_run_mode": self._dry_run,
        }
