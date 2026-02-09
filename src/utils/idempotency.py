"""
MMH v3.1 -- Shared idempotency utilities.

Three deterministic ID / hash helpers that the rest of the system relies on:

* ``generate_intent_id``  -- unique ID for a trade *intent* (buy/sell).
* ``generate_dedup_key``  -- unique key for a blockchain event (tx + log_index).
* ``hash_config``         -- SHA-256 fingerprint of an arbitrary config dict.

All outputs are pure functions of their inputs so that the same logical
operation always produces the same key regardless of which node runs it.
"""

from __future__ import annotations

import hashlib
import json
import struct
import time
import uuid


# --------------------------------------------------------------------------- #
# Intent ID  (UUIDv7-style: timestamp-prefixed, deterministic)
# --------------------------------------------------------------------------- #


def generate_intent_id(
    chain: str,
    token_address: str,
    side: str,
    timestamp: float | None = None,
) -> str:
    """Build a deterministic, time-sortable intent identifier.

    The output is formatted as a UUID string for easy storage in Postgres
    UUID columns, but the bytes are derived from a SHA-256 of the input
    tuple with the top 48 bits replaced by a millisecond Unix timestamp
    (following the UUIDv7 spirit).

    Parameters
    ----------
    chain:
        Chain name, e.g. ``"solana"`` or ``"base"``.
    token_address:
        Canonical token / mint address.
    side:
        ``"buy"`` or ``"sell"``.
    timestamp:
        Unix epoch seconds.  Defaults to ``time.time()``.

    Returns
    -------
    str
        UUID-formatted string, e.g.
        ``"018f3c8a-1b2c-7abc-9def-0123456789ab"``.
    """
    if timestamp is None:
        timestamp = time.time()

    # Deterministic hash of the logical identity
    payload = f"{chain}:{token_address}:{side}:{timestamp:.6f}"
    digest = hashlib.sha256(payload.encode()).digest()

    # Build 16 bytes: 6 bytes timestamp_ms + 10 bytes from digest
    ts_ms = int(timestamp * 1000)
    ts_bytes = struct.pack(">Q", ts_ms)[2:]  # last 6 bytes of 8-byte big-endian

    raw = bytearray(ts_bytes + digest[:10])

    # Set version nibble (bits 48-51) to 0x7 -- UUIDv7
    raw[6] = (raw[6] & 0x0F) | 0x70
    # Set variant bits (bits 64-65) to 0b10 -- RFC 9562
    raw[8] = (raw[8] & 0x3F) | 0x80

    return str(uuid.UUID(bytes=bytes(raw)))


# --------------------------------------------------------------------------- #
# Dedup key  (for on-chain event deduplication)
# --------------------------------------------------------------------------- #


def generate_dedup_key(
    chain: str,
    tx_hash: str,
    log_index: int,
) -> str:
    """Create a deterministic deduplication key for a blockchain event.

    The returned string is a 64-char lowercase hex SHA-256 digest so it
    can double as a Redis key or Postgres index value.

    Parameters
    ----------
    chain:
        Chain name, e.g. ``"solana"`` or ``"base"``.
    tx_hash:
        Transaction hash / signature.
    log_index:
        Log index within the transaction (use 0 for Solana where there
        is no formal log_index).
    """
    payload = f"{chain}:{tx_hash}:{log_index}"
    return hashlib.sha256(payload.encode()).hexdigest()


# --------------------------------------------------------------------------- #
# Config hash  (for versioning / drift detection)
# --------------------------------------------------------------------------- #


def hash_config(config_dict: dict) -> str:
    """Return a stable SHA-256 hex digest of a configuration dictionary.

    Keys are sorted recursively so that insertion order never affects the
    hash.  This is useful for detecting config drift between nodes or
    across deployments.

    Parameters
    ----------
    config_dict:
        Arbitrary JSON-serialisable dictionary.

    Returns
    -------
    str
        64-char lowercase hex SHA-256 digest.
    """
    canonical = json.dumps(config_dict, sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(canonical.encode()).hexdigest()
