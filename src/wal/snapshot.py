"""
WAL Snapshot Manager for MMH v3.1.

Manages WAL snapshots and backups:
- Periodic checkpoint creation from the running WAL
- Upload to configured storage (local filesystem / S3-compatible)
- Retention management (keep last N snapshots)
- Restore from a snapshot to rebuild state
"""

from __future__ import annotations

import asyncio
import hashlib
import logging
import os
import shutil
import time
from typing import Any

logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Pydantic model (with dataclass fallback)
# ---------------------------------------------------------------------------

try:
    from pydantic import BaseModel, Field
except ImportError:
    from dataclasses import dataclass as _dataclass, field as _field

    class BaseModel:  # type: ignore[no-redef]
        """Minimal BaseModel shim when pydantic is unavailable."""

        def __init__(self, **kwargs: Any) -> None:
            for cls in type(self).__mro__:
                for key in getattr(cls, "__annotations__", {}):
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
# Data model
# ---------------------------------------------------------------------------


class SnapshotInfo(BaseModel):
    """Metadata about a single WAL snapshot."""
    path: str = Field(default="")
    seq_start: int = Field(default=0)
    seq_end: int = Field(default=-1)
    created_at: float = Field(default=0.0)
    size_bytes: int = Field(default=0)
    checksum: str = Field(default="")
    chain: str = Field(default="")
    event_count: int = Field(default=0)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _dir_size(path: str) -> int:
    """Compute total size of all files under *path* in bytes."""
    total = 0
    for dirpath, _dirnames, filenames in os.walk(path):
        for fname in filenames:
            fpath = os.path.join(dirpath, fname)
            try:
                total += os.path.getsize(fpath)
            except OSError:
                pass
    return total


def _dir_checksum(path: str) -> str:
    """Compute a composite SHA-256 over all files under *path*.

    Files are processed in sorted order for determinism.
    """
    h = hashlib.sha256()
    for dirpath, _dirnames, filenames in os.walk(path):
        for fname in sorted(filenames):
            fpath = os.path.join(dirpath, fname)
            try:
                with open(fpath, "rb") as f:
                    while True:
                        chunk = f.read(1 << 16)  # 64 KiB
                        if not chunk:
                            break
                        h.update(chunk)
            except OSError:
                pass
    return h.hexdigest()


# ---------------------------------------------------------------------------
# SnapshotManager
# ---------------------------------------------------------------------------


class SnapshotManager:
    """Manages WAL snapshots and backups.

    Usage::

        mgr = SnapshotManager(wal, config={
            "snapshot_dir": "/data/wal_snapshots",
            "chain": "solana",
        })

        info = await mgr.create_snapshot()
        await mgr.cleanup_old_snapshots(keep_count=5)

        # Periodic background snapshots
        task = asyncio.create_task(
            mgr.schedule_periodic_snapshots(interval_seconds=3600)
        )

    Config keys (all optional, sensible defaults provided):
        snapshot_dir : str   -- root directory for snapshots
        chain        : str   -- chain identifier
    """

    def __init__(self, wal: Any, config: dict[str, Any] | None = None) -> None:
        """
        Args:
            wal: A ``RawWAL`` instance.
            config: Optional configuration dict.
        """
        self._wal = wal
        config = config or {}

        self._chain: str = config.get("chain", getattr(wal, "_chain", "default"))
        self._snapshot_dir: str = config.get(
            "snapshot_dir",
            os.path.join(
                getattr(wal, "_data_dir", "/tmp/mmh_wal"),
                self._chain,
                "snapshots",
            ),
        )

        os.makedirs(self._snapshot_dir, exist_ok=True)

        self._lock = asyncio.Lock()
        self._periodic_task: asyncio.Task[None] | None = None

    # -- snapshot creation ---------------------------------------------------

    async def create_snapshot(self) -> SnapshotInfo:
        """Create a snapshot from the current WAL state.

        Uses ``RawWAL.checkpoint()`` to get a consistent copy, then
        computes size and checksum metadata.

        Returns:
            ``SnapshotInfo`` describing the snapshot.
        """
        async with self._lock:
            ts = time.time()
            latest_seq = await self._wal.get_latest_sequence()
            total_events = await self._wal.get_total_events()

            # Ask the WAL to create a checkpoint (backend-specific)
            checkpoint_path = await self._wal.checkpoint()

            # Move the checkpoint into our managed snapshot directory
            # Use nanosecond-precision timestamp to avoid name collisions
            snap_name = f"snap_{int(ts * 1_000_000)}_{latest_seq}"
            snap_path = os.path.join(self._snapshot_dir, snap_name)

            if os.path.exists(snap_path):
                shutil.rmtree(snap_path)

            await asyncio.to_thread(shutil.move, checkpoint_path, snap_path)

            # Compute metadata
            size = await asyncio.to_thread(_dir_size, snap_path)
            checksum = await asyncio.to_thread(_dir_checksum, snap_path)

            # Determine sequence range from entries if possible
            seq_start = 0
            seq_end = latest_seq

            info = SnapshotInfo(
                path=snap_path,
                seq_start=seq_start,
                seq_end=seq_end,
                created_at=ts,
                size_bytes=size,
                checksum=checksum,
                chain=self._chain,
                event_count=total_events,
            )

            # Persist snapshot metadata alongside the snapshot
            meta_path = os.path.join(snap_path, "snapshot.meta")
            await asyncio.to_thread(self._write_meta, meta_path, info)

            logger.info(
                "Created snapshot %s (seq=%d..%d, %d events, %d bytes)",
                snap_name, seq_start, seq_end, total_events, size,
            )
            return info

    @staticmethod
    def _write_meta(path: str, info: SnapshotInfo) -> None:
        """Write snapshot metadata to a JSON file."""
        import json
        data = info.model_dump()
        tmp = path + ".tmp"
        with open(tmp, "w") as f:
            json.dump(data, f, indent=2, default=str)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)

    @staticmethod
    def _read_meta(path: str) -> SnapshotInfo | None:
        """Read snapshot metadata from a JSON file."""
        import json
        if not os.path.exists(path):
            return None
        try:
            with open(path, "r") as f:
                data = json.load(f)
            return SnapshotInfo(**data)
        except (json.JSONDecodeError, TypeError, KeyError) as exc:
            logger.warning("Failed to read snapshot metadata %s: %s", path, exc)
            return None

    # -- listing snapshots ---------------------------------------------------

    async def list_snapshots(self) -> list[SnapshotInfo]:
        """List all snapshots, sorted by creation time (newest first)."""
        snapshots: list[SnapshotInfo] = []

        if not os.path.isdir(self._snapshot_dir):
            return snapshots

        for name in os.listdir(self._snapshot_dir):
            snap_path = os.path.join(self._snapshot_dir, name)
            if not os.path.isdir(snap_path):
                continue

            meta_path = os.path.join(snap_path, "snapshot.meta")
            info = self._read_meta(meta_path)
            if info is not None:
                snapshots.append(info)
            else:
                # Reconstruct minimal info from directory
                try:
                    stat = os.stat(snap_path)
                    snapshots.append(
                        SnapshotInfo(
                            path=snap_path,
                            created_at=stat.st_mtime,
                            size_bytes=await asyncio.to_thread(_dir_size, snap_path),
                            chain=self._chain,
                        )
                    )
                except OSError:
                    pass

        snapshots.sort(key=lambda s: s.created_at, reverse=True)
        return snapshots

    # -- periodic snapshots --------------------------------------------------

    async def schedule_periodic_snapshots(self, interval_seconds: int) -> None:
        """Run periodic snapshot creation in the background.

        This is a long-running coroutine meant to be wrapped in
        ``asyncio.create_task()``.  It runs indefinitely until cancelled.

        Args:
            interval_seconds: Time between snapshots.
        """
        logger.info(
            "Starting periodic WAL snapshots every %ds for chain=%s",
            interval_seconds, self._chain,
        )
        while True:
            try:
                await asyncio.sleep(interval_seconds)
                info = await self.create_snapshot()
                logger.info("Periodic snapshot created: %s", info.path)
            except asyncio.CancelledError:
                logger.info("Periodic snapshot task cancelled for chain=%s", self._chain)
                raise
            except Exception as exc:
                logger.error(
                    "Periodic snapshot failed for chain=%s: %s",
                    self._chain, exc,
                )

    def start_periodic(self, interval_seconds: int) -> asyncio.Task[None]:
        """Start periodic snapshots as a background task.

        Returns the ``asyncio.Task`` so the caller can cancel it later.
        """
        if self._periodic_task is not None and not self._periodic_task.done():
            logger.warning("Periodic snapshot task already running; not starting another")
            return self._periodic_task

        self._periodic_task = asyncio.create_task(
            self.schedule_periodic_snapshots(interval_seconds),
            name=f"wal-snapshot-{self._chain}",
        )
        return self._periodic_task

    def stop_periodic(self) -> None:
        """Cancel the periodic snapshot background task."""
        if self._periodic_task is not None and not self._periodic_task.done():
            self._periodic_task.cancel()
            self._periodic_task = None

    # -- cleanup / retention -------------------------------------------------

    async def cleanup_old_snapshots(self, keep_count: int = 5) -> int:
        """Remove old snapshots beyond retention.

        Keeps the *keep_count* most recent snapshots and deletes the rest.

        Returns:
            Number of snapshots removed.
        """
        snapshots = await self.list_snapshots()  # sorted newest-first

        if len(snapshots) <= keep_count:
            return 0

        to_remove = snapshots[keep_count:]
        removed = 0

        for snap in to_remove:
            try:
                if os.path.isdir(snap.path):
                    await asyncio.to_thread(shutil.rmtree, snap.path)
                    logger.info("Removed old snapshot: %s", snap.path)
                    removed += 1
            except OSError as exc:
                logger.error("Failed to remove snapshot %s: %s", snap.path, exc)

        return removed

    # -- restore -------------------------------------------------------------

    async def restore_from_snapshot(self, snapshot_path: str) -> int:
        """Restore the WAL from a snapshot.

        The current WAL is replaced with the snapshot contents.
        The caller must ensure the WAL is not actively being written to
        during restore (i.e., freeze the chain first).

        Args:
            snapshot_path: Path to the snapshot directory.

        Returns:
            Number of events restored (based on snapshot metadata or
            a scan of the restored WAL).
        """
        if not os.path.isdir(snapshot_path):
            raise FileNotFoundError(f"Snapshot path does not exist: {snapshot_path}")

        async with self._lock:
            # Read snapshot metadata if available
            meta_path = os.path.join(snapshot_path, "snapshot.meta")
            meta = self._read_meta(meta_path)

            # Determine the WAL data directory from the RawWAL instance
            wal_data_dir = os.path.join(
                getattr(self._wal, "_data_dir", "/tmp/mmh_wal"),
                self._chain,
            )

            # Close the current WAL backend
            self._wal.close()

            # Replace WAL files with snapshot contents
            # We copy individual WAL files (not the snapshot.meta)
            wal_files = ("wal.bin", "wal.idx", "wal.meta")
            rocksdb_dir = "wal.rocksdb"

            has_file_wal = os.path.exists(os.path.join(snapshot_path, "wal.bin"))
            has_rocksdb = os.path.isdir(os.path.join(snapshot_path, rocksdb_dir))

            if has_file_wal:
                for name in wal_files:
                    src = os.path.join(snapshot_path, name)
                    dst = os.path.join(wal_data_dir, name)
                    if os.path.exists(src):
                        await asyncio.to_thread(shutil.copy2, src, dst)
                        logger.info("Restored %s -> %s", src, dst)

            elif has_rocksdb:
                src = os.path.join(snapshot_path, rocksdb_dir)
                dst = os.path.join(wal_data_dir, rocksdb_dir)
                if os.path.exists(dst):
                    await asyncio.to_thread(shutil.rmtree, dst)
                await asyncio.to_thread(shutil.copytree, src, dst)
                logger.info("Restored RocksDB %s -> %s", src, dst)

            else:
                raise FileNotFoundError(
                    f"Snapshot at {snapshot_path} contains no recognisable WAL data"
                )

            # Re-initialise the WAL so it picks up the restored data.
            # We create a new backend by re-constructing the WAL internals.
            from src.wal.raw_wal import RawWAL, HAS_ROCKSDICT, _FileBackend, _RocksDBBackend

            if HAS_ROCKSDICT:
                self._wal._backend = _RocksDBBackend(
                    self._wal._data_dir, self._wal._chain,
                )
            else:
                self._wal._backend = _FileBackend(
                    self._wal._data_dir, self._wal._chain,
                )

            # Determine event count
            if meta is not None:
                event_count = meta.event_count
            else:
                event_count = await self._wal.get_total_events()

            logger.info(
                "WAL restored from snapshot %s (%d events)",
                snapshot_path, event_count,
            )
            return event_count
