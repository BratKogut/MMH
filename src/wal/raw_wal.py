"""
WAL (Write-Ahead Log) module for MMH v3.1 memecoin trading system.

Single source of truth for all input events. Append-only with CRC32 integrity.

Rules:
- WAL append MUST succeed before any publish to Redis
- WAL fail -> FREEZE chain immediately
- Publish fail -> retry (event is safe in WAL)

Supports two backends:
- RocksDB via rocksdict (preferred)
- File-based binary WAL (fallback, always available)

Both backends guarantee:
- Fsync after every write (power-cut safe)
- CRC32 integrity check on every read
- Sequence-ordered, append-only semantics
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import shutil
import struct
import time
import zlib
from dataclasses import dataclass
from typing import Optional

logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Backend availability
# ---------------------------------------------------------------------------
try:
    from rocksdict import Rdict, Options  # type: ignore[import-untyped]
    HAS_ROCKSDICT = True
except ImportError:
    HAS_ROCKSDICT = False

# ---------------------------------------------------------------------------
# Exceptions
# ---------------------------------------------------------------------------


class WALWriteError(Exception):
    """Raised when a WAL write fails. This MUST trigger a FREEZE on the chain."""


class WALCorruptionError(Exception):
    """Raised when WAL data fails a CRC32 integrity check."""


# ---------------------------------------------------------------------------
# WALEntry - internal record representation
# ---------------------------------------------------------------------------


@dataclass(frozen=True, slots=True)
class WALEntry:
    """Single WAL record with full metadata."""
    sequence: int
    event_id: str
    event_type: str
    chain: str
    timestamp: float
    crc32: int
    data: bytes


# ---------------------------------------------------------------------------
# Binary record format (shared by both backends)
#
# On-disk record (file backend - after 8-byte file magic header):
#   [4B record_len: >I]   total bytes of record body (everything below)
#   --- record body ---
#   [8B sequence:    >Q]
#   [4B crc32:       >I]   CRC32 of event data bytes only
#   [8B timestamp:   >d]   time.time() at write
#   [2B eid_len:     >H]   + [eid_len bytes: event_id UTF-8]
#   [2B etype_len:   >H]   + [etype_len bytes: event_type UTF-8]
#   [2B chain_len:   >H]   + [chain_len bytes: chain UTF-8]
#   [4B data_len:    >I]   + [data_len bytes: raw event data]
#
# Fixed-size portion inside the body: 8 + 4 + 8 = 20 bytes
# ---------------------------------------------------------------------------

FILE_MAGIC = b"MMHWAL01"
_FIXED_BODY_SIZE = 20  # seq(8) + crc(4) + ts(8)


def _pack_record_body(
    seq: int,
    event_bytes: bytes,
    event_id: str,
    event_type: str,
    chain: str,
    timestamp: float,
    crc: int,
) -> bytes:
    """Pack a WAL record body (without the 4-byte length prefix)."""
    eid = event_id.encode("utf-8")
    etype = event_type.encode("utf-8")
    ch = chain.encode("utf-8")

    return (
        struct.pack(">QId", seq, crc, timestamp)
        + struct.pack(">H", len(eid)) + eid
        + struct.pack(">H", len(etype)) + etype
        + struct.pack(">H", len(ch)) + ch
        + struct.pack(">I", len(event_bytes)) + event_bytes
    )


def _unpack_record_body(data: bytes) -> WALEntry:
    """Unpack a WAL record body into a WALEntry."""
    pos = 0

    seq = struct.unpack_from(">Q", data, pos)[0]; pos += 8
    crc = struct.unpack_from(">I", data, pos)[0]; pos += 4
    ts  = struct.unpack_from(">d", data, pos)[0]; pos += 8

    eid_len = struct.unpack_from(">H", data, pos)[0]; pos += 2
    event_id = data[pos:pos + eid_len].decode("utf-8"); pos += eid_len

    etype_len = struct.unpack_from(">H", data, pos)[0]; pos += 2
    event_type = data[pos:pos + etype_len].decode("utf-8"); pos += etype_len

    ch_len = struct.unpack_from(">H", data, pos)[0]; pos += 2
    chain = data[pos:pos + ch_len].decode("utf-8"); pos += ch_len

    data_len = struct.unpack_from(">I", data, pos)[0]; pos += 4
    event_data = data[pos:pos + data_len]

    return WALEntry(
        sequence=seq,
        event_id=event_id,
        event_type=event_type,
        chain=chain,
        timestamp=ts,
        crc32=crc,
        data=event_data,
    )


# ---------------------------------------------------------------------------
# File-based backend
# ---------------------------------------------------------------------------


class _FileBackend:
    """File-based WAL backend.

    Provides append-only binary storage with fsync durability guarantee.
    Maintains an in-memory index (flushed to disk periodically) for fast
    lookups by event_id and sequence number.
    """

    # How often (in appends) to flush the index/meta to disk.
    _INDEX_FLUSH_INTERVAL = 100

    def __init__(self, data_dir: str, chain: str):
        self._dir = os.path.join(data_dir, chain)
        os.makedirs(self._dir, exist_ok=True)

        self._wal_path = os.path.join(self._dir, "wal.bin")
        self._index_path = os.path.join(self._dir, "wal.idx")
        self._meta_path = os.path.join(self._dir, "wal.meta")

        # In-memory indices --------------------------------------------------
        self._id_to_offset: dict[str, int] = {}    # event_id -> file offset
        self._seq_to_offset: dict[int, int] = {}   # sequence  -> file offset
        self._latest_seq: int = -1
        self._total_events: int = 0

        # Bootstrap ----------------------------------------------------------
        self._ensure_wal_file()
        self._rebuild_index()

    # -- initialisation helpers ----------------------------------------------

    def _ensure_wal_file(self) -> None:
        """Create WAL file with magic header if absent, or verify existing."""
        if not os.path.exists(self._wal_path):
            with open(self._wal_path, "wb") as f:
                f.write(FILE_MAGIC)
                f.flush()
                os.fsync(f.fileno())
        else:
            with open(self._wal_path, "rb") as f:
                magic = f.read(len(FILE_MAGIC))
                if len(magic) < len(FILE_MAGIC) or magic != FILE_MAGIC:
                    raise WALCorruptionError(
                        f"WAL file has invalid magic header: {magic!r}"
                    )

    def _rebuild_index(self) -> None:
        """Scan the entire WAL file and rebuild in-memory indices."""
        self._id_to_offset.clear()
        self._seq_to_offset.clear()
        self._latest_seq = -1
        self._total_events = 0

        with open(self._wal_path, "rb") as f:
            magic = f.read(len(FILE_MAGIC))
            if magic != FILE_MAGIC:
                return

            while True:
                record_offset = f.tell()
                rl_bytes = f.read(4)
                if len(rl_bytes) < 4:
                    break

                record_len = struct.unpack(">I", rl_bytes)[0]
                record_body = f.read(record_len)
                if len(record_body) < record_len:
                    # Truncated record at tail - WAL was interrupted mid-write.
                    # Truncate the file to the last good record boundary.
                    logger.warning(
                        "Truncated WAL record at offset %d; truncating file",
                        record_offset,
                    )
                    f.close()
                    with open(self._wal_path, "r+b") as fw:
                        fw.truncate(record_offset)
                        fw.flush()
                        os.fsync(fw.fileno())
                    break

                # Parse just seq + event_id for the index
                seq = struct.unpack_from(">Q", record_body, 0)[0]
                # skip crc(4) + timestamp(8) = 12 bytes -> pos 20
                pos = _FIXED_BODY_SIZE
                eid_len = struct.unpack_from(">H", record_body, pos)[0]
                pos += 2
                event_id = record_body[pos:pos + eid_len].decode("utf-8")

                self._id_to_offset[event_id] = record_offset
                self._seq_to_offset[seq] = record_offset
                if seq > self._latest_seq:
                    self._latest_seq = seq
                self._total_events += 1

        self._flush_index()

    # -- index persistence ---------------------------------------------------

    def _flush_index(self) -> None:
        """Atomically persist index and metadata to disk."""
        # Index file (event_id -> offset, seq -> offset)
        idx_payload = {
            "by_id": self._id_to_offset,
            "by_seq": {str(k): v for k, v in self._seq_to_offset.items()},
        }
        self._atomic_write_json(self._index_path, idx_payload)

        # Metadata file
        meta_payload = {
            "latest_seq": self._latest_seq,
            "total_events": self._total_events,
        }
        self._atomic_write_json(self._meta_path, meta_payload)

    @staticmethod
    def _atomic_write_json(path: str, data: dict) -> None:
        """Write JSON atomically via tmp + rename."""
        tmp = path + ".tmp"
        with open(tmp, "w") as f:
            json.dump(data, f)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)

    # -- core operations -----------------------------------------------------

    def append(
        self,
        seq: int,
        event_bytes: bytes,
        event_id: str,
        event_type: str,
        chain_name: str,
        timestamp: float,
        crc: int,
    ) -> None:
        body = _pack_record_body(seq, event_bytes, event_id, event_type, chain_name, timestamp, crc)
        framed = struct.pack(">I", len(body)) + body

        with open(self._wal_path, "ab") as f:
            offset = f.tell()
            f.write(framed)
            f.flush()
            os.fsync(f.fileno())

        self._id_to_offset[event_id] = offset
        self._seq_to_offset[seq] = offset
        self._latest_seq = seq
        self._total_events += 1

        if self._total_events % self._INDEX_FLUSH_INTERVAL == 0:
            self._flush_index()

    def read_range(self, seq_start: int, seq_end: int | None = None) -> list[WALEntry]:
        if self._latest_seq < 0:
            return []
        if seq_end is None:
            seq_end = self._latest_seq

        results: list[WALEntry] = []
        with open(self._wal_path, "rb") as f:
            for seq in sorted(self._seq_to_offset):
                if seq < seq_start:
                    continue
                if seq > seq_end:
                    break
                offset = self._seq_to_offset[seq]
                f.seek(offset)
                rl_bytes = f.read(4)
                if len(rl_bytes) < 4:
                    continue
                record_len = struct.unpack(">I", rl_bytes)[0]
                body = f.read(record_len)
                if len(body) < record_len:
                    continue
                results.append(_unpack_record_body(body))
        return results

    def read_by_id(self, event_id: str) -> WALEntry | None:
        offset = self._id_to_offset.get(event_id)
        if offset is None:
            return None
        return self._read_at_offset(offset)

    def _read_at_offset(self, offset: int) -> WALEntry | None:
        with open(self._wal_path, "rb") as f:
            f.seek(offset)
            rl_bytes = f.read(4)
            if len(rl_bytes) < 4:
                return None
            record_len = struct.unpack(">I", rl_bytes)[0]
            body = f.read(record_len)
            if len(body) < record_len:
                return None
            return _unpack_record_body(body)

    # -- accessors -----------------------------------------------------------

    def get_latest_seq(self) -> int:
        return self._latest_seq

    def get_total_events(self) -> int:
        return self._total_events

    def get_all_entries(self) -> list[WALEntry]:
        return self.read_range(0)

    # -- compaction ----------------------------------------------------------

    def rewrite(self, entries: list[WALEntry]) -> None:
        """Rewrite the WAL file with only the given entries (for compaction)."""
        tmp_path = self._wal_path + ".compact"

        with open(tmp_path, "wb") as f:
            f.write(FILE_MAGIC)
            for entry in entries:
                body = _pack_record_body(
                    entry.sequence,
                    entry.data,
                    entry.event_id,
                    entry.event_type,
                    entry.chain,
                    entry.timestamp,
                    entry.crc32,
                )
                f.write(struct.pack(">I", len(body)) + body)
            f.flush()
            os.fsync(f.fileno())

        os.replace(tmp_path, self._wal_path)
        self._rebuild_index()

    # -- checkpointing -------------------------------------------------------

    def checkpoint(self, checkpoint_dir: str) -> str:
        os.makedirs(checkpoint_dir, exist_ok=True)
        self._flush_index()
        for name in ("wal.bin", "wal.idx", "wal.meta"):
            src = os.path.join(self._dir, name)
            if os.path.exists(src):
                shutil.copy2(src, os.path.join(checkpoint_dir, name))
        return checkpoint_dir

    # -- lifecycle -----------------------------------------------------------

    def close(self) -> None:
        self._flush_index()


# ---------------------------------------------------------------------------
# RocksDB backend (requires rocksdict)
# ---------------------------------------------------------------------------


class _RocksDBBackend:
    """RocksDB-backed WAL using rocksdict.

    Uses key prefixes to simulate column families:
      e:{seq:020d}  -> packed record body
      i:{event_id}  -> packed sequence (>Q)
      m:{key}       -> metadata value
    """

    _PFX_EVENT = b"e:"
    _PFX_INDEX = b"i:"
    _PFX_META  = b"m:"

    def __init__(self, data_dir: str, chain: str):
        from rocksdict import Rdict, Options  # type: ignore[import-untyped]

        self._dir = os.path.join(data_dir, chain)
        os.makedirs(self._dir, exist_ok=True)

        db_path = os.path.join(self._dir, "wal.rocksdb")
        opts = Options()
        opts.create_if_missing(True)
        self._db: Rdict = Rdict(db_path, options=opts)

        # Recover metadata
        raw = self._db.get(self._PFX_META + b"latest_seq")
        self._latest_seq: int = struct.unpack(">q", raw)[0] if raw is not None else -1

        raw2 = self._db.get(self._PFX_META + b"total_events")
        self._total_events: int = struct.unpack(">Q", raw2)[0] if raw2 is not None else 0

        # Build in-memory set of known sequences for efficient range queries
        self._known_seqs: set[int] = set()
        self._scan_known_sequences()

    def _scan_known_sequences(self) -> None:
        """Populate _known_seqs from the database."""
        pfx = self._PFX_EVENT
        for key in self._db.keys():
            if isinstance(key, bytes) and key.startswith(pfx):
                seq_str = key[len(pfx):].decode("utf-8")
                self._known_seqs.add(int(seq_str))

    # -- key helpers ---------------------------------------------------------

    def _event_key(self, seq: int) -> bytes:
        return self._PFX_EVENT + f"{seq:020d}".encode()

    def _index_key(self, event_id: str) -> bytes:
        return self._PFX_INDEX + event_id.encode("utf-8")

    # -- core operations -----------------------------------------------------

    def append(
        self,
        seq: int,
        event_bytes: bytes,
        event_id: str,
        event_type: str,
        chain_name: str,
        timestamp: float,
        crc: int,
    ) -> None:
        body = _pack_record_body(seq, event_bytes, event_id, event_type, chain_name, timestamp, crc)

        self._db[self._event_key(seq)] = body
        self._db[self._index_key(event_id)] = struct.pack(">Q", seq)
        self._db[self._PFX_META + b"latest_seq"] = struct.pack(">q", seq)
        self._total_events += 1
        self._db[self._PFX_META + b"total_events"] = struct.pack(">Q", self._total_events)
        self._latest_seq = seq
        self._known_seqs.add(seq)

    def read_range(self, seq_start: int, seq_end: int | None = None) -> list[WALEntry]:
        if self._latest_seq < 0:
            return []
        if seq_end is None:
            seq_end = self._latest_seq

        results: list[WALEntry] = []
        for seq in sorted(self._known_seqs):
            if seq < seq_start:
                continue
            if seq > seq_end:
                break
            raw = self._db.get(self._event_key(seq))
            if raw is not None:
                results.append(_unpack_record_body(raw))
        return results

    def read_by_id(self, event_id: str) -> WALEntry | None:
        raw_seq = self._db.get(self._index_key(event_id))
        if raw_seq is None:
            return None
        seq = struct.unpack(">Q", raw_seq)[0]
        raw = self._db.get(self._event_key(seq))
        if raw is None:
            return None
        return _unpack_record_body(raw)

    # -- accessors -----------------------------------------------------------

    def get_latest_seq(self) -> int:
        return self._latest_seq

    def get_total_events(self) -> int:
        return self._total_events

    def get_all_entries(self) -> list[WALEntry]:
        return self.read_range(0)

    # -- compaction ----------------------------------------------------------

    def rewrite(self, entries: list[WALEntry]) -> None:
        """Remove entries not in *entries*. Re-key if needed."""
        surviving_seqs = {e.sequence for e in entries}
        to_delete_seqs = self._known_seqs - surviving_seqs

        for seq in to_delete_seqs:
            # Look up event_id so we can remove the index entry too
            raw = self._db.get(self._event_key(seq))
            if raw is not None:
                entry = _unpack_record_body(raw)
                idx_key = self._index_key(entry.event_id)
                if self._db.get(idx_key) is not None:
                    del self._db[idx_key]
            del self._db[self._event_key(seq)]

        self._known_seqs = surviving_seqs.copy()
        self._total_events = len(surviving_seqs)
        self._db[self._PFX_META + b"total_events"] = struct.pack(">Q", self._total_events)

    # -- checkpointing -------------------------------------------------------

    def checkpoint(self, checkpoint_dir: str) -> str:
        os.makedirs(checkpoint_dir, exist_ok=True)
        src = os.path.join(self._dir, "wal.rocksdb")
        dst = os.path.join(checkpoint_dir, "wal.rocksdb")
        if os.path.exists(dst):
            shutil.rmtree(dst)
        shutil.copytree(src, dst)
        return checkpoint_dir

    # -- lifecycle -----------------------------------------------------------

    def close(self) -> None:
        self._db.close()


# ---------------------------------------------------------------------------
# RawWAL - async public API
# ---------------------------------------------------------------------------


class RawWAL:
    """Write-Ahead Log backed by RocksDB (preferred) or file-based fallback.

    Single source of truth for all input events.
    Append-only with CRC32 integrity checks.

    Rules:
    - WAL append MUST succeed before any publish to Redis
    - WAL fail -> FREEZE chain immediately
    - Publish fail -> retry (event is safe in WAL)

    Thread-safety: all public methods are protected by an asyncio.Lock so
    only one operation runs at a time per RawWAL instance.  Only one
    instance should be created per (data_dir, chain) pair.
    """

    def __init__(self, data_dir: str, chain: str):
        self._data_dir = data_dir
        self._chain = chain
        self._lock = asyncio.Lock()

        if HAS_ROCKSDICT:
            logger.info("Using RocksDB backend for WAL (chain=%s)", chain)
            self._backend: _FileBackend | _RocksDBBackend = _RocksDBBackend(data_dir, chain)
        else:
            logger.info("Using file-based backend for WAL (chain=%s)", chain)
            self._backend = _FileBackend(data_dir, chain)

    # -- writes --------------------------------------------------------------

    async def append(
        self,
        event_bytes: bytes,
        event_id: str,
        event_type: str,
        chain: str,
    ) -> int:
        """Append event to WAL. Returns the assigned sequence number.

        Computes CRC32 and stores it alongside the data.
        Raises ``WALWriteError`` on failure (which MUST trigger FREEZE).
        """
        async with self._lock:
            try:
                seq = self._backend.get_latest_seq() + 1
                crc = zlib.crc32(event_bytes) & 0xFFFFFFFF
                ts = time.time()

                await asyncio.to_thread(
                    self._backend.append,
                    seq,
                    event_bytes,
                    event_id,
                    event_type,
                    chain,
                    ts,
                    crc,
                )

                logger.debug(
                    "WAL append seq=%d event_id=%s crc=0x%08x",
                    seq, event_id, crc,
                )
                return seq

            except Exception as exc:
                logger.critical(
                    "WAL WRITE FAILED: %s -- FREEZE chain=%s", exc, chain,
                )
                raise WALWriteError(
                    f"WAL write failed for chain={chain}: {exc}"
                ) from exc

    # -- reads ---------------------------------------------------------------

    async def read(
        self,
        seq_start: int,
        seq_end: int | None = None,
    ) -> list[tuple[int, bytes]]:
        """Read events by sequence range.

        Returns ``[(seq, event_bytes), ...]``.
        Every entry has its CRC32 verified on read.
        """
        async with self._lock:
            entries = await asyncio.to_thread(
                self._backend.read_range, seq_start, seq_end,
            )
            return self._verify_entries(entries)

    async def read_by_id(self, event_id: str) -> Optional[bytes]:
        """Read a single event by *event_id* (uses index).

        Returns the raw event bytes, or ``None`` if not found.
        CRC32 verified on read.
        """
        async with self._lock:
            entry = await asyncio.to_thread(
                self._backend.read_by_id, event_id,
            )
            if entry is None:
                return None
            self._verify_crc(entry)
            return entry.data

    async def read_entries(
        self,
        seq_start: int,
        seq_end: int | None = None,
    ) -> list[WALEntry]:
        """Read full ``WALEntry`` objects (with metadata). CRC verified."""
        async with self._lock:
            entries = await asyncio.to_thread(
                self._backend.read_range, seq_start, seq_end,
            )
            for entry in entries:
                self._verify_crc(entry)
            return entries

    # -- integrity -----------------------------------------------------------

    async def verify_integrity(
        self,
        seq_start: int = 0,
        seq_end: int | None = None,
    ) -> tuple[bool, list[int]]:
        """Verify CRC integrity of stored events.

        Returns ``(all_ok, [corrupted_sequence_numbers])``.
        """
        async with self._lock:
            entries = await asyncio.to_thread(
                self._backend.read_range, seq_start, seq_end,
            )
            corrupted: list[int] = []
            for entry in entries:
                actual = zlib.crc32(entry.data) & 0xFFFFFFFF
                if actual != entry.crc32:
                    corrupted.append(entry.sequence)
            return (len(corrupted) == 0, corrupted)

    # -- metadata ------------------------------------------------------------

    async def get_latest_sequence(self) -> int:
        """Return the latest sequence number (-1 if WAL is empty)."""
        return self._backend.get_latest_seq()

    async def get_total_events(self) -> int:
        """Return total number of events currently in the WAL."""
        return self._backend.get_total_events()

    # -- checkpointing / compaction ------------------------------------------

    async def checkpoint(self) -> str:
        """Create a RocksDB-style checkpoint (or file copy). Returns path."""
        async with self._lock:
            ts = int(time.time() * 1_000_000)  # microsecond precision
            checkpoint_dir = os.path.join(
                self._data_dir, self._chain, "checkpoints", f"cp_{ts}",
            )
            path = await asyncio.to_thread(
                self._backend.checkpoint, checkpoint_dir,
            )
            logger.info("Created WAL checkpoint at %s", path)
            return path

    async def compact(self, retention_hours: int) -> int:
        """Remove entries older than *retention_hours*.

        Returns the number of entries removed.
        """
        async with self._lock:
            cutoff = time.time() - (retention_hours * 3600)
            entries = await asyncio.to_thread(self._backend.get_all_entries)

            surviving = [e for e in entries if e.timestamp >= cutoff]
            removed = len(entries) - len(surviving)

            if removed > 0:
                await asyncio.to_thread(self._backend.rewrite, surviving)
                logger.info(
                    "WAL compaction: removed %d entries older than %dh, "
                    "%d remaining",
                    removed, retention_hours, len(surviving),
                )
            return removed

    # -- lifecycle -----------------------------------------------------------

    def close(self) -> None:
        """Close the underlying backend (flushes index for file backend)."""
        self._backend.close()

    # -- CRC helpers (private) -----------------------------------------------

    @staticmethod
    def _verify_crc(entry: WALEntry) -> None:
        actual = zlib.crc32(entry.data) & 0xFFFFFFFF
        if actual != entry.crc32:
            raise WALCorruptionError(
                f"CRC mismatch at seq={entry.sequence}: "
                f"stored=0x{entry.crc32:08x} computed=0x{actual:08x}"
            )

    @classmethod
    def _verify_entries(cls, entries: list[WALEntry]) -> list[tuple[int, bytes]]:
        results: list[tuple[int, bytes]] = []
        for entry in entries:
            cls._verify_crc(entry)
            results.append((entry.sequence, entry.data))
        return results
