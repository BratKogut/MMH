"""
WAL Replay Engine for MMH v3.1.

Replays WAL events through the pipeline for:
- Determinism testing (compare output hashes between runs)
- Recovery after crash (re-derive state from the WAL)
- Debugging (step through events with full observability)

Replay is NOT a feature, it is a mode of operation.
Every output of the system must be reproducible from the WAL alone.
"""

from __future__ import annotations

import asyncio
import hashlib
import logging
import time
from enum import Enum
from typing import Any, Callable, Protocol, runtime_checkable

logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Pydantic models (with dataclass fallback)
# ---------------------------------------------------------------------------

try:
    from pydantic import BaseModel, Field
except ImportError:
    from dataclasses import dataclass as _dataclass, field as _field

    class BaseModel:  # type: ignore[no-redef]
        """Minimal BaseModel shim when pydantic is unavailable."""

        def __init__(self, **kwargs: Any) -> None:
            for cls in type(self).__mro__:
                for key, annotation in getattr(cls, "__annotations__", {}).items():
                    if key in kwargs:
                        object.__setattr__(self, key, kwargs[key])
                    elif hasattr(cls, key):
                        object.__setattr__(self, key, getattr(cls, key))

        def model_dump(self) -> dict[str, Any]:
            return {
                k: v for k, v in self.__dict__.items() if not k.startswith("_")
            }

        def __repr__(self) -> str:
            fields = ", ".join(
                f"{k}={v!r}"
                for k, v in self.__dict__.items()
                if not k.startswith("_")
            )
            return f"{type(self).__name__}({fields})"

    def Field(default: Any = None, **kwargs: Any) -> Any:  # type: ignore[misc]
        return default


# ---------------------------------------------------------------------------
# Data models
# ---------------------------------------------------------------------------


class ReplayStatus(str, Enum):
    """Overall status of a replay run."""
    SUCCESS = "success"
    PARTIAL = "partial"
    FAILED = "failed"


class ReplayResult(BaseModel):
    """Result of a WAL replay run.

    Captures enough information to reproduce and compare runs:
    - output_hashes:  event_id -> SHA-256 hex digest of pipeline output
    - decisions:      ordered list of (event_id, decision) pairs
    - counts:         total / processed / skipped / errored
    - timing:         wall-clock duration
    """
    output_hashes: dict[str, str] = Field(default_factory=dict)
    decisions: list[tuple[str, str]] = Field(default_factory=list)
    total_events: int = Field(default=0)
    processed: int = Field(default=0)
    skipped: int = Field(default=0)
    errored: int = Field(default=0)
    seq_start: int = Field(default=0)
    seq_end: int = Field(default=0)
    chain_filter: str | None = Field(default=None)
    status: str = Field(default=ReplayStatus.SUCCESS.value)
    duration_seconds: float = Field(default=0.0)
    started_at: float = Field(default=0.0)
    finished_at: float = Field(default=0.0)


class DivergenceDetail(BaseModel):
    """Details about a single divergent event between two replay runs."""
    event_id: str = Field(default="")
    hash_a: str | None = Field(default=None)
    hash_b: str | None = Field(default=None)
    decision_a: str | None = Field(default=None)
    decision_b: str | None = Field(default=None)
    reason: str = Field(default="")


class ReplayDivergence(BaseModel):
    """Comparison between two replay runs.

    If divergences is empty the runs were deterministic.
    """
    is_deterministic: bool = Field(default=True)
    total_compared: int = Field(default=0)
    divergent_count: int = Field(default=0)
    divergences: list[DivergenceDetail] = Field(default_factory=list)  # type: ignore[assignment]
    events_only_in_a: list[str] = Field(default_factory=list)
    events_only_in_b: list[str] = Field(default_factory=list)


# ---------------------------------------------------------------------------
# OutputCollector protocol
# ---------------------------------------------------------------------------


@runtime_checkable
class OutputCollector(Protocol):
    """Protocol for collecting pipeline outputs during replay.

    Implementations can write to stdout, a file, a database, etc.
    """

    async def collect(self, event_id: str, output: bytes, decision: str) -> None:
        """Called once per processed event with the pipeline output."""
        ...


class InMemoryCollector:
    """Simple in-memory collector for testing and comparison."""

    def __init__(self) -> None:
        self.outputs: dict[str, bytes] = {}
        self.decisions: dict[str, str] = {}

    async def collect(self, event_id: str, output: bytes, decision: str) -> None:
        self.outputs[event_id] = output
        self.decisions[event_id] = decision


# ---------------------------------------------------------------------------
# Pipeline factory protocol
# ---------------------------------------------------------------------------


@runtime_checkable
class PipelineFactory(Protocol):
    """Factory that builds a pipeline instance for replay.

    The returned pipeline must be callable as:
        result_bytes, decision_str = await pipeline.process(event_bytes)
    """

    def create(self) -> Any:
        """Return a fresh pipeline instance."""
        ...


@runtime_checkable
class Pipeline(Protocol):
    """Minimal pipeline interface required by the replay engine."""

    async def process(self, event_bytes: bytes) -> tuple[bytes, str]:
        """Process a single event. Returns (output_bytes, decision_string)."""
        ...


# ---------------------------------------------------------------------------
# ReplayEngine
# ---------------------------------------------------------------------------


class ReplayEngine:
    """Replays WAL events through the pipeline.

    Usage::

        engine = ReplayEngine(wal, pipeline_factory)

        # Full replay
        result = await engine.replay()

        # Partial replay with chain filter
        result = await engine.replay(seq_start=100, seq_end=200, chain="solana")

        # Determinism check
        result_a = await engine.replay()
        result_b = await engine.replay()
        divergence = await engine.compare_runs(result_a, result_b)
        assert divergence.is_deterministic
    """

    def __init__(self, wal: Any, pipeline_factory: Any) -> None:
        """
        Args:
            wal: A ``RawWAL`` instance to read events from.
            pipeline_factory: Object with a ``create()`` method that returns
                a pipeline with an ``async process(event_bytes) -> (output, decision)``
                method.
        """
        self._wal = wal
        self._pipeline_factory = pipeline_factory

    async def replay(
        self,
        seq_start: int = 0,
        seq_end: int | None = None,
        chain: str | None = None,
        output_collector: OutputCollector | None = None,
    ) -> ReplayResult:
        """Replay events through the pipeline.

        Args:
            seq_start: First sequence number to replay (inclusive).
            seq_end: Last sequence number to replay (inclusive). ``None`` = latest.
            chain: If set, only replay events for this chain.
            output_collector: Optional collector for pipeline outputs.

        Returns:
            A ``ReplayResult`` with output hashes for determinism comparison.
        """
        t_start = time.time()

        # Read entries from WAL (CRC verified by RawWAL)
        entries = await self._wal.read_entries(seq_start, seq_end)

        # Apply chain filter
        if chain is not None:
            entries = [e for e in entries if e.chain == chain]

        total = len(entries)
        if seq_end is None and entries:
            seq_end = entries[-1].sequence

        # Build a fresh pipeline for this replay run
        pipeline = self._pipeline_factory.create()

        output_hashes: dict[str, str] = {}
        decisions: list[tuple[str, str]] = []
        processed = 0
        skipped = 0
        errored = 0

        for entry in entries:
            try:
                output_bytes, decision = await pipeline.process(entry.data)

                # Hash the output for determinism comparison
                h = hashlib.sha256(output_bytes).hexdigest()
                output_hashes[entry.event_id] = h
                decisions.append((entry.event_id, decision))

                if output_collector is not None:
                    await output_collector.collect(entry.event_id, output_bytes, decision)

                processed += 1
                logger.debug(
                    "Replayed seq=%d event_id=%s decision=%s hash=%s",
                    entry.sequence, entry.event_id, decision, h[:16],
                )

            except Exception as exc:
                errored += 1
                logger.error(
                    "Replay error at seq=%d event_id=%s: %s",
                    entry.sequence, entry.event_id, exc,
                )
                decisions.append((entry.event_id, f"ERROR:{exc}"))

        t_end = time.time()

        status = ReplayStatus.SUCCESS.value
        if errored > 0 and processed > 0:
            status = ReplayStatus.PARTIAL.value
        elif errored > 0 and processed == 0:
            status = ReplayStatus.FAILED.value

        return ReplayResult(
            output_hashes=output_hashes,
            decisions=decisions,
            total_events=total,
            processed=processed,
            skipped=skipped,
            errored=errored,
            seq_start=seq_start,
            seq_end=seq_end if seq_end is not None else -1,
            chain_filter=chain,
            status=status,
            duration_seconds=round(t_end - t_start, 6),
            started_at=t_start,
            finished_at=t_end,
        )

    async def compare_runs(
        self,
        result_a: ReplayResult,
        result_b: ReplayResult,
    ) -> ReplayDivergence:
        """Compare two replay runs and detect divergences.

        Compares output hashes and decisions for every event that appears
        in either run.  Events present in only one run are flagged separately.
        """
        all_ids_a = set(result_a.output_hashes.keys())
        all_ids_b = set(result_b.output_hashes.keys())
        common = all_ids_a & all_ids_b
        only_a = sorted(all_ids_a - all_ids_b)
        only_b = sorted(all_ids_b - all_ids_a)

        # Build decision lookup for fast access
        decisions_a = dict(result_a.decisions)
        decisions_b = dict(result_b.decisions)

        divergences: list[DivergenceDetail] = []

        for eid in sorted(common):
            h_a = result_a.output_hashes[eid]
            h_b = result_b.output_hashes[eid]
            d_a = decisions_a.get(eid)
            d_b = decisions_b.get(eid)

            reasons: list[str] = []
            if h_a != h_b:
                reasons.append("output_hash_mismatch")
            if d_a != d_b:
                reasons.append("decision_mismatch")

            if reasons:
                divergences.append(
                    DivergenceDetail(
                        event_id=eid,
                        hash_a=h_a,
                        hash_b=h_b,
                        decision_a=d_a,
                        decision_b=d_b,
                        reason="; ".join(reasons),
                    )
                )

        total_compared = len(common)
        divergent = len(divergences)
        is_deterministic = divergent == 0 and len(only_a) == 0 and len(only_b) == 0

        return ReplayDivergence(
            is_deterministic=is_deterministic,
            total_compared=total_compared,
            divergent_count=divergent,
            divergences=divergences,
            events_only_in_a=only_a,
            events_only_in_b=only_b,
        )

    async def replay_and_verify(
        self,
        seq_start: int = 0,
        seq_end: int | None = None,
        chain: str | None = None,
        runs: int = 2,
    ) -> tuple[ReplayResult, ReplayDivergence | None]:
        """Convenience: run replay *runs* times and compare.

        Returns the first ``ReplayResult`` and the ``ReplayDivergence``
        between the first and last runs (``None`` if only 1 run).
        """
        results: list[ReplayResult] = []
        for i in range(runs):
            logger.info("Replay run %d/%d", i + 1, runs)
            r = await self.replay(seq_start, seq_end, chain)
            results.append(r)

        if len(results) >= 2:
            divergence = await self.compare_runs(results[0], results[-1])
            return results[0], divergence

        return results[0], None
