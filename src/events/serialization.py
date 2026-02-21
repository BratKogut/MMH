"""
MMH v3.1 Event Schema System - Serialization & Deserialization

Provides msgpack-based serialization (with JSON fallback) and a type registry
for polymorphic deserialization of events.

Wire format:
    Primary:  msgpack (compact, fast, language-agnostic)
    Fallback: JSON (for debugging, logging, and environments without msgpack)

Schema version handling:
    On deserialization, the ``schema_version`` in the payload is compared
    against the current version.  Mismatches produce a logged warning.
    Future versions will support explicit migration functions registered
    per (event_type, old_version -> new_version).

Usage:
    >>> from src.events.schemas import TokenDiscovered
    >>> from src.events.serialization import serialize_event, deserialize_event
    >>> event = TokenDiscovered(chain="solana", address="So1...", block_number=123, block_hash="0xabc", tx_hash="0xdef")
    >>> data = serialize_event(event)
    >>> restored = deserialize_event(data)
    >>> assert restored.event_id == event.event_id
"""
from __future__ import annotations

import json
import logging
from typing import Any

from pydantic import ValidationError

from src.events.base import BaseEvent
from src.events.schemas import ALL_EVENT_CLASSES

logger = logging.getLogger(__name__)

# Current schema version -- must match BaseEvent.schema_version default
CURRENT_SCHEMA_VERSION: str = "3.1.0"


# ---------------------------------------------------------------------------
# Event Type Registry
# ---------------------------------------------------------------------------
# Maps event_type string -> concrete Pydantic model class.
# Built automatically from ALL_EVENT_CLASSES defined in schemas.py.
# ---------------------------------------------------------------------------

EVENT_TYPE_REGISTRY: dict[str, type[BaseEvent]] = {}


def _build_registry() -> None:
    """Populate the event type registry from ALL_EVENT_CLASSES."""
    for cls in ALL_EVENT_CLASSES:
        # Read the default value of event_type from the model fields
        event_type_field = cls.model_fields.get("event_type")
        if event_type_field is not None and event_type_field.default is not None:
            key = event_type_field.default
        else:
            key = cls.__name__
        if key in EVENT_TYPE_REGISTRY:
            raise RuntimeError(
                f"Duplicate event_type registration: {key!r} is claimed by "
                f"both {EVENT_TYPE_REGISTRY[key].__name__} and {cls.__name__}"
            )
        EVENT_TYPE_REGISTRY[key] = cls


_build_registry()


def get_event_class(event_type: str) -> type[BaseEvent]:
    """Look up the concrete class for an event_type string.

    Args:
        event_type: The discriminator string (e.g. ``"TokenDiscovered"``).

    Returns:
        The Pydantic model class.

    Raises:
        KeyError: If the event_type is not registered.
    """
    try:
        return EVENT_TYPE_REGISTRY[event_type]
    except KeyError:
        raise KeyError(
            f"Unknown event_type {event_type!r}. "
            f"Registered types: {sorted(EVENT_TYPE_REGISTRY.keys())}"
        ) from None


# ---------------------------------------------------------------------------
# msgpack helpers
# ---------------------------------------------------------------------------

_HAS_MSGPACK: bool = False
try:
    import msgpack  # type: ignore[import-untyped]
    _HAS_MSGPACK = True
except ImportError:
    logger.warning(
        "msgpack not installed -- falling back to JSON serialization. "
        "Install msgpack for production use: pip install msgpack"
    )


def _encode_msgpack(obj: dict[str, Any]) -> bytes:
    """Encode a dict to msgpack bytes."""
    return msgpack.packb(obj, use_bin_type=True)  # type: ignore[union-attr]


def _decode_msgpack(data: bytes) -> dict[str, Any]:
    """Decode msgpack bytes to a dict."""
    return msgpack.unpackb(data, raw=False)  # type: ignore[union-attr]


def _encode_json(obj: dict[str, Any]) -> bytes:
    """Encode a dict to JSON bytes (UTF-8)."""
    return json.dumps(obj, separators=(",", ":"), default=str).encode("utf-8")


def _decode_json(data: bytes) -> dict[str, Any]:
    """Decode JSON bytes to a dict."""
    return json.loads(data.decode("utf-8"))  # type: ignore[no-any-return]


# ---------------------------------------------------------------------------
# Schema version migration
# ---------------------------------------------------------------------------

def _check_schema_version(event_dict: dict[str, Any]) -> None:
    """Warn if the event's schema version doesn't match the current version.

    Future enhancement: run registered migration functions to transform
    the event_dict in-place from old_version to CURRENT_SCHEMA_VERSION.
    """
    version = event_dict.get("schema_version", "unknown")
    if version != CURRENT_SCHEMA_VERSION:
        logger.warning(
            "Schema version mismatch: event has %r, current is %r. "
            "event_type=%s event_id=%s. Attempting best-effort deserialization.",
            version,
            CURRENT_SCHEMA_VERSION,
            event_dict.get("event_type", "?"),
            event_dict.get("event_id", "?"),
        )


# ---------------------------------------------------------------------------
# Public API
# ---------------------------------------------------------------------------

# Wire-format prefix bytes for auto-detection:
#   msgpack dicts start with 0x80-0x8f (fixmap) or 0xde/0xdf (map16/map32)
#   JSON dicts start with 0x7b ('{')
_JSON_BRACE = ord("{")


def serialize_event(event: BaseEvent) -> bytes:
    """Serialize a BaseEvent (or subclass) to bytes.

    Uses msgpack if available, otherwise falls back to JSON.

    The output bytes can be passed directly to ``deserialize_event``.
    The format is auto-detected on deserialization.

    Args:
        event: Any event instance (BaseEvent or subclass).

    Returns:
        Compact binary representation.
    """
    event_dict: dict[str, Any] = event.model_dump()

    if _HAS_MSGPACK:
        return _encode_msgpack(event_dict)
    return _encode_json(event_dict)


def deserialize_event(data: bytes) -> BaseEvent:
    """Deserialize bytes back into a typed event instance.

    Auto-detects msgpack vs JSON format.  Uses ``event_type`` in the
    payload to select the correct Pydantic model class.

    Args:
        data: Bytes produced by ``serialize_event``.

    Returns:
        A fully validated event instance of the appropriate subclass.

    Raises:
        KeyError: If the event_type is unknown.
        pydantic.ValidationError: If the payload fails validation.
        ValueError: If the data cannot be decoded.
    """
    if not data:
        raise ValueError("Cannot deserialize empty data.")

    # Auto-detect format
    event_dict: dict[str, Any]
    first_byte = data[0]

    if first_byte == _JSON_BRACE:
        # JSON format
        event_dict = _decode_json(data)
    elif _HAS_MSGPACK:
        # Attempt msgpack first
        try:
            event_dict = _decode_msgpack(data)
        except Exception:
            # Fallback: maybe it's JSON after all
            event_dict = _decode_json(data)
    else:
        # No msgpack available -- must be JSON
        event_dict = _decode_json(data)

    # Schema version check (logs warning on mismatch)
    _check_schema_version(event_dict)

    # Resolve the concrete class
    event_type = event_dict.get("event_type", "BaseEvent")
    try:
        cls = get_event_class(event_type)
    except KeyError:
        logger.warning(
            "Unknown event_type %r -- deserializing as BaseEvent.", event_type
        )
        cls = BaseEvent

    # Validate and construct
    try:
        return cls.model_validate(event_dict)
    except ValidationError as exc:
        logger.error(
            "Validation failed for event_type=%s event_id=%s: %s",
            event_type,
            event_dict.get("event_id", "?"),
            exc,
        )
        raise


def serialize_event_json(event: BaseEvent) -> bytes:
    """Force JSON serialization regardless of msgpack availability.

    Useful for logging, debugging, or writing to systems that expect JSON.

    Args:
        event: Any event instance.

    Returns:
        UTF-8 encoded JSON bytes.
    """
    return _encode_json(event.model_dump())


def deserialize_event_json(data: bytes) -> BaseEvent:
    """Deserialize from JSON bytes specifically.

    Args:
        data: UTF-8 JSON bytes.

    Returns:
        Typed event instance.
    """
    event_dict = _decode_json(data)
    _check_schema_version(event_dict)
    event_type = event_dict.get("event_type", "BaseEvent")
    try:
        cls = get_event_class(event_type)
    except KeyError:
        cls = BaseEvent
    return cls.model_validate(event_dict)
