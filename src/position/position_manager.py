"""Position Manager - Ledger of Record.

This is THE single source of truth for positions (Option A).
Event-sourced from TradeExecuted events.

Responsibilities:
- Materialize positions from TradeExecuted events
- Track PnL snapshots
- Track exposure per chain/token
- Handle reorg corrections
- Emit PositionUpdated events for dashboards and RiskFabric cache
- Monitor TP/SL/trailing stop/max holding time
- Execute exit strategies

Consumes from: trades:executed:{chain} (for all enabled chains)
Publishes to: positions:updates
Also updates: positions:snapshot in Redis (for RiskFabric cache)
Persists to: PostgreSQL positions + transactions tables
"""

import asyncio
import json
import logging
import time
import uuid
from typing import Optional
from datetime import datetime, timezone
from decimal import Decimal
from enum import Enum

logger = logging.getLogger(__name__)


class PositionStatus(str, Enum):
    OPEN = "OPEN"
    PARTIAL_EXIT = "PARTIAL_EXIT"
    CLOSED = "CLOSED"


class Position:
    """In-memory position representation."""

    def __init__(self, position_id: str, chain: str, token_address: str,
                 token_symbol: str = "", intent_id: str = ""):
        self.id = position_id
        self.chain = chain
        self.token_address = token_address
        self.token_symbol = token_symbol
        self.intent_id = intent_id
        self.status = PositionStatus.OPEN

        # Entry data
        self.entry_price: float = 0.0
        self.entry_amount: float = 0.0
        self.entry_timestamp: float = 0.0
        self.entry_tx_hash: str = ""

        # Current state
        self.current_price: float = 0.0
        self.remaining_amount: float = 0.0

        # Exit data
        self.exit_price: float = 0.0
        self.exit_timestamp: float = 0.0

        # Configuration
        self.take_profit_levels: list[float] = [50.0, 100.0, 200.0]
        self.stop_loss_pct: float = -30.0
        self.trailing_stop_pct: float = 0.0
        self.max_holding_seconds: int = 3600
        self.trailing_stop_high: float = 0.0  # highest price seen

        # PnL
        self.pnl_usd: float = 0.0
        self.pnl_pct: float = 0.0
        self.exposure_usd: float = 0.0

        # Partial exits tracking
        self.exits: list[dict] = []
        self.tp_levels_hit: set[float] = set()

        # Metadata
        self.created_at: float = time.time()
        self.updated_at: float = time.time()
        self.dry_run: bool = False

    def update_price(self, new_price: float):
        """Update current price and recalculate PnL."""
        self.current_price = new_price
        if self.entry_price > 0:
            self.pnl_pct = ((new_price - self.entry_price) / self.entry_price) * 100
            self.pnl_usd = self.remaining_amount * (new_price - self.entry_price)
        self.exposure_usd = self.remaining_amount * new_price

        # Update trailing stop high
        if new_price > self.trailing_stop_high:
            self.trailing_stop_high = new_price

        self.updated_at = time.time()

    def check_exit_conditions(self) -> Optional[str]:
        """Check if any exit condition is met.
        Returns exit reason or None.
        """
        if self.status == PositionStatus.CLOSED:
            return None

        # Stop loss
        if self.pnl_pct <= self.stop_loss_pct:
            return f"STOP_LOSS:{self.pnl_pct:.1f}%"

        # Trailing stop
        if self.trailing_stop_pct != 0 and self.trailing_stop_high > 0:
            trailing_drop = ((self.current_price - self.trailing_stop_high) / self.trailing_stop_high) * 100
            if trailing_drop <= self.trailing_stop_pct:
                return f"TRAILING_STOP:drop={trailing_drop:.1f}%"

        # Take profit levels
        for tp_level in self.take_profit_levels:
            if tp_level not in self.tp_levels_hit and self.pnl_pct >= tp_level:
                return f"TAKE_PROFIT:{tp_level}%"

        # Max holding time
        if time.time() - self.entry_timestamp > self.max_holding_seconds:
            return f"MAX_HOLDING_TIME:{self.max_holding_seconds}s"

        return None

    def to_dict(self) -> dict:
        """Serialize position to dict."""
        return {
            "id": self.id,
            "chain": self.chain,
            "token_address": self.token_address,
            "token_symbol": self.token_symbol,
            "intent_id": self.intent_id,
            "status": self.status.value,
            "entry_price": self.entry_price,
            "entry_amount": self.entry_amount,
            "entry_timestamp": self.entry_timestamp,
            "entry_tx_hash": self.entry_tx_hash,
            "current_price": self.current_price,
            "remaining_amount": self.remaining_amount,
            "exit_price": self.exit_price,
            "pnl_usd": self.pnl_usd,
            "pnl_pct": self.pnl_pct,
            "exposure_usd": self.exposure_usd,
            "take_profit_levels": self.take_profit_levels,
            "stop_loss_pct": self.stop_loss_pct,
            "tp_levels_hit": list(self.tp_levels_hit),
            "exits": self.exits,
            "dry_run": self.dry_run,
            "created_at": self.created_at,
            "updated_at": self.updated_at,
        }


class PositionManager:
    """Position Manager - Event-sourced Ledger of Record.

    THE only module that materializes positions.
    """

    def __init__(self, bus, redis_client, db=None, config: dict = None):
        """
        Args:
            bus: RedisEventBus
            redis_client: for position snapshot cache
            db: Database instance (optional, for persistence)
            config: position management config
        """
        self._bus = bus
        self._redis = redis_client
        self._db = db
        self._config = config or {}
        self._running = False

        # In-memory position state
        self._positions: dict[str, Position] = {}  # position_id -> Position
        self._intent_to_position: dict[str, str] = {}  # intent_id -> position_id

        # Configuration defaults
        self._default_tp = config.get("default_take_profit_levels", [50.0, 100.0, 200.0]) if config else [50.0, 100.0, 200.0]
        self._default_sl = config.get("default_stop_loss_pct", -30.0) if config else -30.0
        self._default_trailing = config.get("default_trailing_stop_pct", 0.0) if config else 0.0
        self._max_holding = config.get("max_holding_seconds", 3600) if config else 3600
        self._enabled_chains = config.get("enabled_chains", ["solana", "base"]) if config else ["solana", "base"]

        # Metrics
        self._positions_opened = 0
        self._positions_closed = 0
        self._total_pnl_usd = 0.0
        self._trades_processed = 0

    async def start(self):
        """Start position manager - consume TradeExecuted events and monitor positions."""
        self._running = True

        # Restore state from DB if available
        await self._restore_state()

        tasks = []
        # Consumer per chain
        for chain in self._enabled_chains:
            tasks.append(asyncio.create_task(self._consume_chain(chain)))
        # Position monitor (TP/SL/trailing/max time)
        tasks.append(asyncio.create_task(self._monitor_positions()))
        # Snapshot updater
        tasks.append(asyncio.create_task(self._snapshot_updater()))

        await asyncio.gather(*tasks)

    async def stop(self):
        self._running = False
        await self._save_snapshot()

    async def _consume_chain(self, chain: str):
        """Consume TradeExecuted events for a chain."""
        input_stream = f"trades:executed:{chain}"
        group = "position_manager"
        consumer = f"posmgr-{chain}"

        await self._bus.ensure_consumer_group(input_stream, group)

        while self._running:
            try:
                messages = await self._bus.consume(input_stream, group, consumer, count=10, block_ms=2000)
                for msg_id, data in messages:
                    await self._handle_trade_executed(data)
                    await self._bus.ack(input_stream, group, msg_id)
            except Exception as e:
                logger.error(f"Position Manager error on {chain}: {e}")
                await asyncio.sleep(1)

    async def _handle_trade_executed(self, trade_data: dict):
        """Handle a TradeExecuted event - create or update position."""
        self._trades_processed += 1
        intent_id = trade_data.get("intent_id", "")
        chain = trade_data.get("chain", "")
        token_address = trade_data.get("token_address", "")
        side = trade_data.get("side", "BUY")
        dry_run = trade_data.get("dry_run", "false").lower() == "true"

        if side == "BUY":
            # Create new position
            position_id = str(uuid.uuid4())
            pos = Position(
                position_id=position_id,
                chain=chain,
                token_address=token_address,
                token_symbol=trade_data.get("token_symbol", ""),
                intent_id=intent_id,
            )

            price = float(trade_data.get("price", "0") or "0")
            amount_out = float(trade_data.get("amount_out", "0") or "0")

            pos.entry_price = price
            pos.entry_amount = amount_out
            pos.remaining_amount = amount_out
            pos.entry_timestamp = time.time()
            pos.entry_tx_hash = trade_data.get("tx_hash", "")
            pos.current_price = price
            pos.exposure_usd = amount_out * price if price > 0 else float(trade_data.get("amount_in", "0") or "0")
            pos.dry_run = dry_run

            # Apply defaults
            pos.take_profit_levels = list(self._default_tp)
            pos.stop_loss_pct = self._default_sl
            pos.trailing_stop_pct = self._default_trailing
            pos.max_holding_seconds = self._max_holding
            pos.trailing_stop_high = price

            self._positions[position_id] = pos
            self._intent_to_position[intent_id] = position_id
            self._positions_opened += 1

            logger.info(f"Position opened: {position_id} for {token_address} on {chain} (dry_run={dry_run})")

            # Persist to DB
            await self._persist_position(pos)

            # Emit update
            await self._emit_position_update(pos)

        elif side == "SELL":
            # Close/partial close existing position
            position_id = self._intent_to_position.get(intent_id) or self._find_position(chain, token_address)
            if position_id and position_id in self._positions:
                pos = self._positions[position_id]
                exit_price = float(trade_data.get("price", "0") or "0")
                amount_sold = float(trade_data.get("amount_in", "0") or "0")

                pos.exits.append({
                    "price": exit_price,
                    "amount": amount_sold,
                    "tx_hash": trade_data.get("tx_hash", ""),
                    "timestamp": time.time(),
                    "reason": trade_data.get("exit_reason", "manual"),
                })

                pos.remaining_amount -= amount_sold
                pos.exit_price = exit_price
                pos.exit_timestamp = time.time()

                if pos.remaining_amount <= 0:
                    pos.status = PositionStatus.CLOSED
                    pos.remaining_amount = 0
                    self._positions_closed += 1

                    # Calculate final PnL
                    if pos.entry_price > 0:
                        pos.pnl_pct = ((exit_price - pos.entry_price) / pos.entry_price) * 100
                        pos.pnl_usd = pos.entry_amount * (exit_price - pos.entry_price)
                    self._total_pnl_usd += pos.pnl_usd

                    logger.info(f"Position closed: {position_id} PnL={pos.pnl_usd:.2f} USD ({pos.pnl_pct:.1f}%)")
                else:
                    pos.status = PositionStatus.PARTIAL_EXIT

                pos.update_price(exit_price)
                await self._persist_position(pos)
                await self._emit_position_update(pos)

    def _find_position(self, chain: str, token_address: str) -> Optional[str]:
        """Find open position by chain and token."""
        for pid, pos in self._positions.items():
            if pos.chain == chain and pos.token_address == token_address and pos.status == PositionStatus.OPEN:
                return pid
        return None

    async def _monitor_positions(self):
        """Monitor open positions for exit conditions.
        Runs periodically to check TP/SL/trailing/max time.
        """
        while self._running:
            try:
                open_positions = [p for p in self._positions.values() if p.status in (PositionStatus.OPEN, PositionStatus.PARTIAL_EXIT)]

                for pos in open_positions:
                    exit_reason = pos.check_exit_conditions()
                    if exit_reason:
                        logger.info(f"Exit condition met for {pos.id}: {exit_reason}")
                        await self._trigger_exit(pos, exit_reason)

            except Exception as e:
                logger.error(f"Position monitor error: {e}")

            await asyncio.sleep(5)  # Check every 5 seconds

    async def _trigger_exit(self, pos: Position, reason: str):
        """Trigger position exit by publishing a sell intent."""
        # Determine sell amount based on reason
        sell_amount = pos.remaining_amount

        if reason.startswith("TAKE_PROFIT"):
            # Partial sell at TP levels
            tp_level = float(reason.split(":")[1].replace("%", ""))
            pos.tp_levels_hit.add(tp_level)

            if tp_level == self._default_tp[0] if self._default_tp else 50:
                sell_amount = pos.remaining_amount * 0.33  # Sell 33% at first TP
            elif tp_level == self._default_tp[1] if len(self._default_tp) > 1 else 100:
                sell_amount = pos.remaining_amount * 0.5   # Sell 50% at second TP
            else:
                sell_amount = pos.remaining_amount  # Sell all at last TP

        # Publish sell intent
        import hashlib
        intent_seed = f"{pos.chain}:{pos.token_address}:SELL:{int(time.time())}:{reason}"
        intent_id = hashlib.sha256(intent_seed.encode()).hexdigest()[:32]

        sell_intent = {
            "event_type": "TradeIntent",
            "intent_id": intent_id,
            "chain": pos.chain,
            "token_address": pos.token_address,
            "side": "SELL",
            "amount_tokens": str(sell_amount),
            "position_id": pos.id,
            "exit_reason": reason,
            "event_time": str(time.time()),
        }

        await self._bus.publish("risk:decisions", sell_intent)
        logger.info(f"Sell intent published for position {pos.id}: {reason}")

    async def _emit_position_update(self, pos: Position):
        """Emit PositionUpdated event."""
        update_data = {
            "event_type": "PositionUpdated",
            "position_id": pos.id,
            "chain": pos.chain,
            "token_address": pos.token_address,
            "status": pos.status.value,
            "entry_price": str(pos.entry_price),
            "current_price": str(pos.current_price),
            "pnl_usd": str(pos.pnl_usd),
            "pnl_pct": str(pos.pnl_pct),
            "exposure_usd": str(pos.exposure_usd),
            "dry_run": str(pos.dry_run),
            "event_time": str(time.time()),
        }
        await self._bus.publish("positions:updates", update_data)

    async def _save_snapshot(self):
        """Save positions snapshot to Redis for RiskFabric cache."""
        open_positions = [
            pos.to_dict() for pos in self._positions.values()
            if pos.status != PositionStatus.CLOSED
        ]
        try:
            await self._redis.set("positions:snapshot", json.dumps(open_positions))
        except Exception as e:
            logger.error(f"Failed to save position snapshot: {e}")

    async def _snapshot_updater(self):
        """Periodically update position snapshot in Redis."""
        while self._running:
            await self._save_snapshot()
            # Also update daily PnL
            await self._update_daily_pnl()
            await asyncio.sleep(10)

    async def _update_daily_pnl(self):
        """Update daily PnL in Redis for RiskFabric."""
        try:
            daily_pnl = sum(
                pos.pnl_usd for pos in self._positions.values()
                if pos.created_at > time.time() - 86400
            )
            await self._redis.set("daily:pnl", str(daily_pnl))
        except Exception as e:
            logger.error(f"Failed to update daily PnL: {e}")

    async def _persist_position(self, pos: Position):
        """Persist position to PostgreSQL."""
        if not self._db:
            return
        try:
            from src.db.models import Position as PositionModel, Transaction
            from sqlalchemy import select

            async with self._db.get_session() as session:
                # Check if exists
                result = await session.execute(
                    select(PositionModel).where(PositionModel.id == uuid.UUID(pos.id) if len(pos.id) == 36 else PositionModel.intent_id == pos.intent_id)
                )
                existing = result.scalar_one_or_none()

                if existing:
                    existing.status = pos.status.value
                    existing.exit_price = pos.exit_price if pos.exit_price else None
                    existing.pnl_usd = pos.pnl_usd
                    existing.pnl_pct = pos.pnl_pct
                    existing.exposure_usd = pos.exposure_usd
                else:
                    new_pos = PositionModel(
                        chain=pos.chain,
                        token_address=pos.token_address,
                        token_symbol=pos.token_symbol,
                        entry_price=pos.entry_price,
                        entry_amount=pos.entry_amount,
                        entry_tx_hash=pos.entry_tx_hash,
                        status=pos.status.value,
                        intent_id=pos.intent_id,
                        exposure_usd=pos.exposure_usd,
                    )
                    session.add(new_pos)

                await session.commit()
        except Exception as e:
            logger.error(f"Failed to persist position {pos.id}: {e}")

    async def _restore_state(self):
        """Restore position state from Redis snapshot on startup."""
        try:
            data = await self._redis.get("positions:snapshot")
            if data:
                positions = json.loads(data)
                for pd in positions:
                    pos = Position(
                        position_id=pd["id"],
                        chain=pd["chain"],
                        token_address=pd["token_address"],
                        token_symbol=pd.get("token_symbol", ""),
                        intent_id=pd.get("intent_id", ""),
                    )
                    pos.status = PositionStatus(pd.get("status", "OPEN"))
                    pos.entry_price = pd.get("entry_price", 0)
                    pos.entry_amount = pd.get("entry_amount", 0)
                    pos.remaining_amount = pd.get("remaining_amount", pd.get("entry_amount", 0))
                    pos.entry_timestamp = pd.get("entry_timestamp", 0)
                    pos.entry_tx_hash = pd.get("entry_tx_hash", "")
                    pos.current_price = pd.get("current_price", 0)
                    pos.pnl_usd = pd.get("pnl_usd", 0)
                    pos.pnl_pct = pd.get("pnl_pct", 0)
                    pos.exposure_usd = pd.get("exposure_usd", 0)
                    pos.dry_run = pd.get("dry_run", False)
                    pos.trailing_stop_high = pd.get("trailing_stop_high", pos.entry_price)
                    pos.tp_levels_hit = set(pd.get("tp_levels_hit", []))

                    self._positions[pos.id] = pos
                    if pos.intent_id:
                        self._intent_to_position[pos.intent_id] = pos.id

                logger.info(f"Restored {len(self._positions)} positions from snapshot")
        except Exception as e:
            logger.error(f"Failed to restore positions: {e}")

    # Public API
    def get_open_positions(self) -> list[dict]:
        """Get all open positions."""
        return [pos.to_dict() for pos in self._positions.values() if pos.status != PositionStatus.CLOSED]

    def get_position(self, position_id: str) -> Optional[dict]:
        """Get specific position."""
        pos = self._positions.get(position_id)
        return pos.to_dict() if pos else None

    def get_exposure(self) -> dict:
        """Get current exposure summary."""
        total = 0.0
        by_chain: dict[str, float] = {}
        by_token: dict[str, float] = {}

        for pos in self._positions.values():
            if pos.status != PositionStatus.CLOSED:
                total += pos.exposure_usd
                by_chain[pos.chain] = by_chain.get(pos.chain, 0) + pos.exposure_usd
                key = f"{pos.chain}:{pos.token_address}"
                by_token[key] = by_token.get(key, 0) + pos.exposure_usd

        return {"total_usd": total, "by_chain": by_chain, "by_token": by_token}

    def get_metrics(self) -> dict:
        return {
            "positions_opened": self._positions_opened,
            "positions_closed": self._positions_closed,
            "open_count": len([p for p in self._positions.values() if p.status != PositionStatus.CLOSED]),
            "total_pnl_usd": self._total_pnl_usd,
            "trades_processed": self._trades_processed,
            "exposure": self.get_exposure(),
        }
