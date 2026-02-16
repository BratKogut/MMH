"""Decision Journal.

Every decision (scoring + risk + executor) records:
- input refs (event_ids)
- output
- reason codes
- config hash

Provides:
- Instant debugging
- Audit trail
- ML training data (data is the product)
"""

import json
import logging
import time
from typing import Optional

logger = logging.getLogger(__name__)


class DecisionJournal:
    """Centralized Decision Journal service.

    Records all decisions to:
    1. Redis Stream (journal:decisions) for real-time
    2. PostgreSQL (decision_journal table) for long-term
    """

    def __init__(self, bus, db=None):
        self._bus = bus
        self._db = db
        self._running = False

        # Stats
        self._entries_recorded = 0
        self._entries_persisted = 0

    async def start(self):
        """Start journal consumer for persistence."""
        self._running = True
        stream = "journal:decisions"
        group = "journal_persister"
        consumer = "journal-pg"

        await self._bus.ensure_consumer_group(stream, group)

        while self._running:
            try:
                messages = await self._bus.consume(stream, group, consumer, count=20, block_ms=5000)
                for msg_id, data in messages:
                    await self._persist_entry(data)
                    self._entries_recorded += 1
                    await self._bus.ack(stream, group, msg_id)
            except Exception as e:
                logger.error(f"Decision journal error: {e}")
                import asyncio
                await asyncio.sleep(1)

    async def stop(self):
        self._running = False

    async def record(self, decision_type: str, module: str,
                     input_event_ids: list[str], output_event_id: str = "",
                     reason_codes: list[str] = None, config_hash: str = "",
                     decision_data: dict = None):
        """Record a decision to the journal stream."""
        entry = {
            "event_type": "DecisionJournalEntry",
            "decision_type": decision_type,
            "module": module,
            "input_event_ids": json.dumps(input_event_ids),
            "output_event_id": output_event_id,
            "reason_codes": json.dumps(reason_codes or []),
            "config_hash": config_hash,
            "decision_data": json.dumps(decision_data or {}),
            "event_time": str(time.time()),
        }
        await self._bus.publish("journal:decisions", entry)

    async def _persist_entry(self, data: dict):
        """Persist journal entry to PostgreSQL."""
        if not self._db:
            return
        try:
            from src.db.models import DecisionJournal as JournalModel

            async with self._db.get_session() as session:
                entry = JournalModel(
                    decision_type=data.get("decision_type", "UNKNOWN"),
                    module=data.get("module", "unknown"),
                    input_event_ids=json.loads(data.get("input_event_ids", "[]")) if isinstance(data.get("input_event_ids"), str) else data.get("input_event_ids"),
                    output_event_id=data.get("output_event_id", ""),
                    reason_codes=json.loads(data.get("reason_codes", "[]")) if isinstance(data.get("reason_codes"), str) else data.get("reason_codes"),
                    config_hash=data.get("config_hash", ""),
                    decision_data=json.loads(data.get("decision_data", "{}")) if isinstance(data.get("decision_data"), str) else data.get("decision_data"),
                )
                session.add(entry)
                await session.commit()
                self._entries_persisted += 1
        except Exception as e:
            logger.error(f"Failed to persist journal entry: {e}")

    async def query(self, decision_type: str = None, module: str = None,
                    limit: int = 100) -> list[dict]:
        """Query journal entries."""
        if not self._db:
            return []
        try:
            from src.db.models import DecisionJournal as JournalModel
            from sqlalchemy import select

            async with self._db.get_session() as session:
                query = select(JournalModel).order_by(JournalModel.created_at.desc()).limit(limit)
                if decision_type:
                    query = query.where(JournalModel.decision_type == decision_type)
                if module:
                    query = query.where(JournalModel.module == module)

                result = await session.execute(query)
                entries = result.scalars().all()

                return [{
                    "id": str(e.id),
                    "decision_type": e.decision_type,
                    "module": e.module,
                    "input_event_ids": e.input_event_ids,
                    "output_event_id": e.output_event_id,
                    "reason_codes": e.reason_codes,
                    "config_hash": e.config_hash,
                    "decision_data": e.decision_data,
                    "created_at": str(e.created_at),
                } for e in entries]
        except Exception as e:
            logger.error(f"Journal query failed: {e}")
            return []

    def get_metrics(self) -> dict:
        return {
            "entries_recorded": self._entries_recorded,
            "entries_persisted": self._entries_persisted,
        }
