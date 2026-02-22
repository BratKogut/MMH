"""Tests for WAL (Write-Ahead Log) module."""

import asyncio
import json
import os
import tempfile
import pytest

from src.wal.raw_wal import RawWAL, WALWriteError, WALCorruptionError


class TestRawWAL:
    """Tests for RawWAL file-based backend."""

    @pytest.fixture
    def wal_dir(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            yield tmpdir

    @pytest.fixture
    def wal(self, wal_dir):
        w = RawWAL(wal_dir, "test_chain")
        yield w
        w.close()

    @pytest.mark.asyncio
    async def test_append_and_read(self, wal):
        data = json.dumps({"msg": "hello"}).encode()
        seq = await wal.append(data, "evt:1", "TestEvent", "test_chain")
        assert seq == 0

        entries = await wal.read(0)
        assert len(entries) == 1
        assert entries[0][0] == 0
        assert json.loads(entries[0][1]) == {"msg": "hello"}

    @pytest.mark.asyncio
    async def test_multiple_appends(self, wal):
        for i in range(5):
            await wal.append(f"event_{i}".encode(), f"evt:{i}", "Test", "test")

        entries = await wal.read(0)
        assert len(entries) == 5

    @pytest.mark.asyncio
    async def test_read_by_id(self, wal):
        data = b"specific_event"
        await wal.append(data, "evt:special", "Test", "test")

        result = await wal.read_by_id("evt:special")
        assert result == data

    @pytest.mark.asyncio
    async def test_read_nonexistent_id(self, wal):
        result = await wal.read_by_id("evt:nonexistent")
        assert result is None

    @pytest.mark.asyncio
    async def test_read_range(self, wal):
        for i in range(10):
            await wal.append(f"e{i}".encode(), f"evt:{i}", "Test", "test")

        entries = await wal.read(3, 6)
        assert len(entries) == 4  # seq 3, 4, 5, 6

    @pytest.mark.asyncio
    async def test_crc_integrity(self, wal):
        data = b"integrity_test"
        await wal.append(data, "evt:crc", "Test", "test")

        ok, corrupted = await wal.verify_integrity()
        assert ok is True
        assert len(corrupted) == 0

    @pytest.mark.asyncio
    async def test_get_latest_sequence(self, wal):
        assert await wal.get_latest_sequence() == -1
        await wal.append(b"first", "evt:0", "Test", "test")
        assert await wal.get_latest_sequence() == 0
        await wal.append(b"second", "evt:1", "Test", "test")
        assert await wal.get_latest_sequence() == 1

    @pytest.mark.asyncio
    async def test_get_total_events(self, wal):
        assert await wal.get_total_events() == 0
        await wal.append(b"data", "evt:0", "Test", "test")
        assert await wal.get_total_events() == 1

    @pytest.mark.asyncio
    async def test_compact_removes_old_entries(self, wal):
        # Append with very short retention
        for i in range(5):
            await wal.append(f"e{i}".encode(), f"evt:{i}", "Test", "test")

        # Compact with 0 hours retention should remove everything
        removed = await wal.compact(retention_hours=0)
        assert removed == 5

    @pytest.mark.asyncio
    async def test_checkpoint(self, wal_dir, wal):
        await wal.append(b"checkpoint_test", "evt:0", "Test", "test")
        path = await wal.checkpoint()
        assert os.path.exists(path)

    @pytest.mark.asyncio
    async def test_empty_wal_read(self, wal):
        entries = await wal.read(0)
        assert entries == []

    @pytest.mark.asyncio
    async def test_persistence_across_reopens(self, wal_dir):
        """WAL data survives close and reopen."""
        wal1 = RawWAL(wal_dir, "persist_test")
        await wal1.append(b"persistent", "evt:1", "Test", "persist_test")
        wal1.close()

        wal2 = RawWAL(wal_dir, "persist_test")
        entries = await wal2.read(0)
        assert len(entries) == 1
        assert entries[0][1] == b"persistent"
        wal2.close()
