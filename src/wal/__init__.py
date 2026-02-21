"""
WAL (Write-Ahead Log) module for MMH v3.1 memecoin trading system.

Provides durable, append-only event storage with CRC32 integrity,
replay for determinism testing and crash recovery, and snapshot/backup
management.

Core components:
    RawWAL            -- Append-only log with CRC32 (RocksDB or file-based)
    ReplayEngine      -- Replays WAL events through pipelines
    SnapshotManager   -- Periodic checkpoints and retention management

Exceptions:
    WALWriteError      -- WAL write failed (triggers FREEZE)
    WALCorruptionError -- CRC integrity check failed

Data models:
    WALEntry           -- Single WAL record with metadata
    ReplayResult       -- Result of a replay run
    ReplayDivergence   -- Comparison between two replay runs
    SnapshotInfo       -- Metadata about a snapshot
"""

from src.wal.raw_wal import (
    HAS_ROCKSDICT,
    RawWAL,
    WALCorruptionError,
    WALEntry,
    WALWriteError,
)
from src.wal.replay import (
    DivergenceDetail,
    InMemoryCollector,
    OutputCollector,
    ReplayDivergence,
    ReplayEngine,
    ReplayResult,
    ReplayStatus,
)
from src.wal.snapshot import (
    SnapshotInfo,
    SnapshotManager,
)

__all__ = [
    # raw_wal
    "RawWAL",
    "WALWriteError",
    "WALCorruptionError",
    "WALEntry",
    "HAS_ROCKSDICT",
    # replay
    "ReplayEngine",
    "ReplayResult",
    "ReplayDivergence",
    "DivergenceDetail",
    "ReplayStatus",
    "OutputCollector",
    "InMemoryCollector",
    # snapshot
    "SnapshotManager",
    "SnapshotInfo",
]
