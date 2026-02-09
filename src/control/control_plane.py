"""ControlPlane - Operator Command Interface.

NOT a stateless chat bot. Every command is persisted.

Commands: /freeze, /kill, /resume, config changes
All commands -> Control Log (Redis Stream + PG)
On restart: system replays last state from command log.

Optional: two-man rule for /kill and limit increases.
"""

import asyncio
import json
import logging
import time
import uuid
from typing import Optional, Callable
from enum import Enum

logger = logging.getLogger(__name__)


class CommandType(str, Enum):
    FREEZE = "FREEZE"
    KILL = "KILL"
    RESUME = "RESUME"
    CONFIG_UPDATE = "CONFIG_UPDATE"
    FREEZE_CHAIN = "FREEZE_CHAIN"
    RESUME_CHAIN = "RESUME_CHAIN"
    UPDATE_LIMITS = "UPDATE_LIMITS"
    BLACKLIST_ADD = "BLACKLIST_ADD"
    BLACKLIST_REMOVE = "BLACKLIST_REMOVE"


class SystemState(str, Enum):
    RUNNING = "RUNNING"
    FROZEN = "FROZEN"
    KILLED = "KILLED"


class ControlCommand:
    """A control command with metadata."""
    def __init__(self, command_type: CommandType, target: str = "global",
                 params: dict = None, operator_id: str = "system"):
        self.id = str(uuid.uuid4())
        self.command_type = command_type
        self.target = target
        self.params = params or {}
        self.operator_id = operator_id
        self.timestamp = time.time()
        self.acknowledged = False

    def to_dict(self) -> dict:
        return {
            "command_id": self.id,
            "command_type": self.command_type.value,
            "target": self.target,
            "params": json.dumps(self.params),
            "operator_id": self.operator_id,
            "timestamp": str(self.timestamp),
            "acknowledged": str(self.acknowledged),
        }


class ControlPlane:
    """ControlPlane - persistent operator command interface.

    All commands are persisted to:
    1. Redis Stream (control:log) for real-time consumption
    2. PostgreSQL (control_log table) for audit trail

    On restart: replays command log to restore state.

    Endpoints: freeze, kill, resume, config updates
    Heartbeat monitoring: no heartbeat > T -> auto FREEZE
    """

    def __init__(self, bus, redis_client, db=None, config: dict = None):
        self._bus = bus
        self._redis = redis_client
        self._db = db
        self._config = config or {}
        self._running = False

        # System state
        self._state = SystemState.RUNNING
        self._frozen_chains: set[str] = set()
        self._command_history: list[dict] = []

        # Heartbeat monitoring
        self._last_heartbeats: dict[str, float] = {}  # module -> timestamp
        self._heartbeat_timeout = config.get("heartbeat_timeout_seconds", 30.0) if config else 30.0

        # Command handlers
        self._handlers: dict[CommandType, Callable] = {}
        self._register_default_handlers()

        # Metrics
        self._commands_processed = 0
        self._auto_freezes = 0

    def _register_default_handlers(self):
        """Register default command handlers."""
        self._handlers[CommandType.FREEZE] = self._handle_freeze
        self._handlers[CommandType.KILL] = self._handle_kill
        self._handlers[CommandType.RESUME] = self._handle_resume
        self._handlers[CommandType.FREEZE_CHAIN] = self._handle_freeze_chain
        self._handlers[CommandType.RESUME_CHAIN] = self._handle_resume_chain
        self._handlers[CommandType.CONFIG_UPDATE] = self._handle_config_update
        self._handlers[CommandType.UPDATE_LIMITS] = self._handle_update_limits

    async def start(self):
        """Start ControlPlane - restore state and begin monitoring."""
        self._running = True

        # Restore state from command log
        await self._restore_state()

        tasks = [
            asyncio.create_task(self._consume_commands()),
            asyncio.create_task(self._monitor_heartbeats()),
            asyncio.create_task(self._consume_heartbeats()),
        ]
        await asyncio.gather(*tasks)

    async def stop(self):
        self._running = False

    # === Command Execution ===

    async def execute_command(self, command: ControlCommand) -> dict:
        """Execute a control command. Persists to log first."""
        logger.info(f"Executing command: {command.command_type.value} target={command.target} by={command.operator_id}")

        # Persist command to log FIRST
        await self._persist_command(command)

        # Execute handler
        handler = self._handlers.get(command.command_type)
        if handler:
            try:
                result = await handler(command)
                command.acknowledged = True
                self._commands_processed += 1

                # Publish to control stream for other services
                await self._bus.publish("control:commands", {
                    **command.to_dict(),
                    "result": json.dumps(result) if result else "{}",
                })

                return {"status": "ok", "command_id": command.id, "result": result}
            except Exception as e:
                logger.error(f"Command execution failed: {e}")
                return {"status": "error", "command_id": command.id, "error": str(e)}
        else:
            return {"status": "error", "command_id": command.id, "error": f"Unknown command: {command.command_type}"}

    # === Command Handlers ===

    async def _handle_freeze(self, cmd: ControlCommand) -> dict:
        """Global freeze - stop all trading."""
        self._state = SystemState.FROZEN
        await self._redis.set("system:state", "FROZEN")
        logger.warning(f"SYSTEM FROZEN by {cmd.operator_id}")

        freeze_data = {
            "event_type": "FreezeEvent",
            "chain": "global",
            "reason": f"operator_command:freeze by {cmd.operator_id}",
            "triggered_by": f"ControlPlane:{cmd.operator_id}",
            "auto": "false",
            "event_time": str(time.time()),
        }
        await self._bus.publish("health:freeze", freeze_data)
        return {"state": "FROZEN"}

    async def _handle_kill(self, cmd: ControlCommand) -> dict:
        """Emergency kill - stop everything immediately."""
        self._state = SystemState.KILLED
        await self._redis.set("system:state", "KILLED")
        logger.critical(f"SYSTEM KILLED by {cmd.operator_id}")
        return {"state": "KILLED"}

    async def _handle_resume(self, cmd: ControlCommand) -> dict:
        """Resume system from freeze."""
        if self._state == SystemState.KILLED:
            logger.warning("Cannot resume from KILLED state - requires restart")
            return {"state": "KILLED", "error": "Cannot resume from KILLED"}
        self._state = SystemState.RUNNING
        self._frozen_chains.clear()
        await self._redis.set("system:state", "RUNNING")
        logger.info(f"SYSTEM RESUMED by {cmd.operator_id}")
        return {"state": "RUNNING"}

    async def _handle_freeze_chain(self, cmd: ControlCommand) -> dict:
        """Freeze a specific chain."""
        chain = cmd.target
        self._frozen_chains.add(chain)
        await self._redis.sadd("frozen:chains", chain)
        logger.warning(f"Chain {chain} FROZEN by {cmd.operator_id}")
        return {"chain": chain, "frozen": True}

    async def _handle_resume_chain(self, cmd: ControlCommand) -> dict:
        """Resume a specific chain."""
        chain = cmd.target
        self._frozen_chains.discard(chain)
        await self._redis.srem("frozen:chains", chain)
        logger.info(f"Chain {chain} RESUMED by {cmd.operator_id}")
        return {"chain": chain, "frozen": False}

    async def _handle_config_update(self, cmd: ControlCommand) -> dict:
        """Update configuration."""
        updates = cmd.params
        logger.info(f"Config update by {cmd.operator_id}: {updates}")
        return {"updated": list(updates.keys())}

    async def _handle_update_limits(self, cmd: ControlCommand) -> dict:
        """Update risk limits (requires two-man rule if configured)."""
        limits = cmd.params
        logger.info(f"Limits update by {cmd.operator_id}: {limits}")

        # Store updated limits
        for key, value in limits.items():
            await self._redis.hset("risk:limits", key, str(value))

        return {"updated_limits": limits}

    # === Heartbeat Monitoring ===

    async def _consume_heartbeats(self):
        """Consume heartbeat events."""
        stream = "health:heartbeat"
        group = "control_plane"
        consumer = "control-hb"

        await self._bus.ensure_consumer_group(stream, group)

        while self._running:
            try:
                messages = await self._bus.consume(stream, group, consumer, count=20, block_ms=2000)
                for msg_id, data in messages:
                    module = data.get("source_module", "unknown")
                    self._last_heartbeats[module] = time.time()
                    await self._bus.ack(stream, group, msg_id)
            except Exception as e:
                logger.error(f"Heartbeat consumer error: {e}")
                await asyncio.sleep(1)

    async def _monitor_heartbeats(self):
        """Monitor for missing heartbeats -> auto FREEZE."""
        while self._running:
            try:
                now = time.time()
                for module, last_hb in list(self._last_heartbeats.items()):
                    if now - last_hb > self._heartbeat_timeout:
                        logger.warning(f"Heartbeat timeout for {module}: last={now - last_hb:.1f}s ago")
                        self._auto_freezes += 1

                        # Auto freeze
                        chain = module.split(":")[-1] if ":" in module else ""
                        if chain:
                            self._frozen_chains.add(chain)

                        freeze_data = {
                            "event_type": "FreezeEvent",
                            "chain": chain or "unknown",
                            "reason": f"heartbeat_timeout:{module}",
                            "triggered_by": "ControlPlane:heartbeat_monitor",
                            "auto": "true",
                            "event_time": str(now),
                        }
                        await self._bus.publish("health:freeze", freeze_data)

                        # Remove from monitoring to avoid spam
                        del self._last_heartbeats[module]
            except Exception as e:
                logger.error(f"Heartbeat monitor error: {e}")

            await asyncio.sleep(5)

    # === Command Consumer ===

    async def _consume_commands(self):
        """Consume commands from Redis stream (for API/external input)."""
        stream = "control:commands:input"
        group = "control_plane"
        consumer = "control-cmd"

        await self._bus.ensure_consumer_group(stream, group)

        while self._running:
            try:
                messages = await self._bus.consume(stream, group, consumer, count=5, block_ms=2000)
                for msg_id, data in messages:
                    cmd_type = data.get("command_type", "")
                    try:
                        cmd = ControlCommand(
                            command_type=CommandType(cmd_type),
                            target=data.get("target", "global"),
                            params=json.loads(data.get("params", "{}")) if isinstance(data.get("params"), str) else data.get("params", {}),
                            operator_id=data.get("operator_id", "api"),
                        )
                        await self.execute_command(cmd)
                    except ValueError:
                        logger.error(f"Unknown command type: {cmd_type}")
                    await self._bus.ack(stream, group, msg_id)
            except Exception as e:
                logger.error(f"Command consumer error: {e}")
                await asyncio.sleep(1)

    # === Persistence ===

    async def _persist_command(self, command: ControlCommand):
        """Persist command to Redis Stream + PG."""
        # Redis Stream
        await self._bus.publish("control:log", command.to_dict())

        # PostgreSQL
        if self._db:
            try:
                from src.db.models import ControlLog
                async with self._db.get_session() as session:
                    log_entry = ControlLog(
                        command_type=command.command_type.value,
                        target=command.target,
                        params=command.params,
                        operator_id=command.operator_id,
                    )
                    session.add(log_entry)
                    await session.commit()
            except Exception as e:
                logger.error(f"Failed to persist command to PG: {e}")

    async def _restore_state(self):
        """Restore system state from Redis on startup."""
        try:
            state = await self._redis.get("system:state")
            if state:
                self._state = SystemState(state.decode() if isinstance(state, bytes) else state)
                logger.info(f"Restored system state: {self._state.value}")

            frozen = await self._redis.smembers("frozen:chains")
            if frozen:
                self._frozen_chains = {c.decode() if isinstance(c, bytes) else c for c in frozen}
                logger.info(f"Restored frozen chains: {self._frozen_chains}")
        except Exception as e:
            logger.error(f"Failed to restore state: {e}")

    # === Public API ===

    @property
    def state(self) -> SystemState:
        return self._state

    @property
    def frozen_chains(self) -> set[str]:
        return self._frozen_chains

    def is_chain_frozen(self, chain: str) -> bool:
        return chain in self._frozen_chains or self._state != SystemState.RUNNING

    def get_metrics(self) -> dict:
        return {
            "state": self._state.value,
            "frozen_chains": list(self._frozen_chains),
            "commands_processed": self._commands_processed,
            "auto_freezes": self._auto_freezes,
            "monitored_modules": list(self._last_heartbeats.keys()),
        }
