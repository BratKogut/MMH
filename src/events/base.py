"""
MMH v3.1 Event Schema System - Base Event

Provides the foundational BaseEvent model used by all events in the system.
Includes a manual UUIDv7 generator for time-ordered unique identifiers.

All events in MMH are immutable, time-ordered, and carry full lineage
(correlation_id for lifecycle tracking, causation_id for causal chains).

Architecture note:
    Events flow through Redis Streams with at-least-once delivery.
    Every consumer MUST be idempotent (see contracts.py).
    event_id uniqueness is guaranteed by UUIDv7 (timestamp + random).
"""
from __future__ import annotations

import os
import struct
import time
from typing import Any, Literal

from pydantic import BaseModel, Field, model_validator


# ---------------------------------------------------------------------------
# UUIDv7 generator (RFC 9562-compliant, manual implementation)
# ---------------------------------------------------------------------------
# UUIDv7 layout (128 bits):
#   48 bits - unix_ts_ms (milliseconds since epoch)
#    4 bits - version (0b0111 = 7)
#   12 bits - rand_a
#    2 bits - variant (0b10)
#   62 bits - rand_b
# This gives time-ordered, globally-unique, k-sortable identifiers.
# ---------------------------------------------------------------------------

def generate_uuid7() -> str:
    """Generate a UUIDv7 string (time-ordered, random-suffix).

    Returns a lowercase hex UUID string with dashes, e.g.
    ``018f6b1c-a3b0-7def-8123-456789abcdef``.

    The first 48 bits encode the current Unix timestamp in milliseconds,
    giving natural chronological ordering when sorted lexicographically.
    The remaining 80 bits are cryptographically random (via ``os.urandom``),
    with version and variant bits set per RFC 9562.
    """
    # Current time in milliseconds
    timestamp_ms: int = int(time.time() * 1000)

    # 48 bits of timestamp
    ts_bytes: bytes = struct.pack(">Q", timestamp_ms)[2:]  # last 6 bytes = 48 bits

    # 10 bytes (80 bits) of randomness
    rand_bytes: bytes = os.urandom(10)

    # Combine into 16-byte UUID
    uuid_bytes: bytearray = bytearray(ts_bytes + rand_bytes)

    # Set version to 7 (bits 48-51): clear top 4 bits of byte 6, set 0111
    uuid_bytes[6] = (uuid_bytes[6] & 0x0F) | 0x70

    # Set variant to RFC 9562 (bits 64-65): clear top 2 bits of byte 8, set 10
    uuid_bytes[8] = (uuid_bytes[8] & 0x3F) | 0x80

    # Format as standard UUID string
    hex_str: str = uuid_bytes.hex()
    return (
        f"{hex_str[0:8]}-{hex_str[8:12]}-{hex_str[12:16]}"
        f"-{hex_str[16:20]}-{hex_str[20:32]}"
    )


def unix_timestamp_us() -> float:
    """Return current Unix timestamp in seconds with microsecond precision."""
    return time.time()


# ---------------------------------------------------------------------------
# Supported chains
# ---------------------------------------------------------------------------
ChainType = Literal["solana", "base", "bsc", "ton"]


# ---------------------------------------------------------------------------
# BaseEvent - the envelope every event in MMH must conform to
# ---------------------------------------------------------------------------
class BaseEvent(BaseModel):
    """Fintech-grade base event envelope for the MMH v3.1 event bus.

    Every event that flows through the system -- from token discovery through
    trade execution and position management -- inherits from this model.

    Field semantics:
        event_id:
            UUIDv7 guaranteeing global uniqueness *and* time-ordering.
            Generated at event-creation time; never mutated.

        event_type:
            Discriminator string (e.g. ``"TokenDiscovered"``).  Set
            automatically by each concrete subclass via ``model_post_init``.

        schema_version:
            Semantic version of the event schema.  Consumers MUST check
            this and handle migrations (see ``serialization.py``).

        correlation_id:
            Groups all events belonging to the same logical lifecycle
            (e.g. one token from discovery through exit).  Set once at the
            origin event and propagated downstream.

        causation_id:
            The ``event_id`` of the event that *directly* caused this one.
            Enables full causal-chain reconstruction for post-mortems.

        source_instance_id:
            Identifies the specific process / container that emitted the
            event.  Critical for multi-writer debugging.

        event_time:
            The *market* time -- when the thing actually happened on-chain
            or at the data source.  Unix seconds, microsecond precision.

        ingest_time:
            When our system first received / observed the raw data.
            Set by the ingest layer.

        process_time:
            When a consumer actually processed this event.  Set on
            consumption, NOT on production.  ``0.0`` means not yet consumed.

        chain:
            Which blockchain this event pertains to.

        payload:
            Arbitrary extra data.  Concrete subclasses add typed fields;
            this dict is a catch-all for forward-compatible extensibility.
    """

    # --- Identity & lineage ---
    event_id: str = Field(
        default_factory=generate_uuid7,
        description="UUIDv7 -- globally unique, time-sortable identifier.",
    )
    event_type: str = Field(
        default="BaseEvent",
        description="Discriminator for polymorphic deserialization.",
    )
    schema_version: str = Field(
        default="3.1.0",
        description="Semantic version of the event schema.",
    )
    correlation_id: str = Field(
        default_factory=generate_uuid7,
        description="Lifecycle correlation ID (e.g. per-token lifecycle).",
    )
    causation_id: str = Field(
        default="",
        description="event_id of the direct cause; empty for root events.",
    )
    source_instance_id: str = Field(
        default="",
        description="Process/container ID of the emitter.",
    )

    # --- Timing (all Unix seconds, microsecond precision) ---
    event_time: float = Field(
        default_factory=unix_timestamp_us,
        description="Market / source time (when it actually happened).",
    )
    ingest_time: float = Field(
        default_factory=unix_timestamp_us,
        description="When our ingest layer received the raw data.",
    )
    process_time: float = Field(
        default=0.0,
        description="When consumer processed this event (set on consumption).",
    )

    # --- Routing ---
    chain: str = Field(
        default="solana",
        description="Blockchain: solana | base | bsc | ton.",
    )

    # --- Extensibility ---
    payload: dict[str, Any] = Field(
        default_factory=dict,
        description="Catch-all for forward-compatible extra data.",
    )

    model_config = {
        "frozen": False,  # process_time is set on consumption
        "extra": "forbid",
        "json_schema_extra": {
            "title": "MMH v3.1 BaseEvent",
        },
    }

    @model_validator(mode="after")
    def _set_causation_default(self) -> BaseEvent:
        """If no causation_id was provided, default to event_id (root event)."""
        if not self.causation_id:
            self.causation_id = self.event_id
        return self

    def mark_processed(self) -> None:
        """Stamp process_time to current time.  Called by consumers."""
        self.process_time = unix_timestamp_us()

    def derive_ids(self, new_correlation: bool = False) -> dict[str, str]:
        """Return kwargs for creating a child event caused by this one.

        Args:
            new_correlation: If True, generate a new correlation_id instead
                of propagating this event's correlation_id.

        Returns:
            Dict with ``correlation_id`` and ``causation_id`` ready to
            be unpacked into a child event constructor.
        """
        return {
            "correlation_id": (
                generate_uuid7() if new_correlation else self.correlation_id
            ),
            "causation_id": self.event_id,
        }
