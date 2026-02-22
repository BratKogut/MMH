"""MMH v3.1 -- Telegram Bot operator interface.

Provides a command interface for monitoring and controlling the trading
system via Telegram.  All commands are restricted to the configured
``chat_id`` whitelist.

Commands:
    /status     -- Pipeline overview (services, positions, exposure).
    /positions  -- List open positions with PnL.
    /portfolio  -- Summary (total exposure, daily PnL, win rate).
    /sell <id>  -- Manual sell a position by ID.
    /freeze <c> -- Freeze a specific chain.
    /resume <c> -- Resume a frozen chain.
    /kill       -- Emergency stop all trading.

Auto-alerts (consumed from Redis Streams):
    - New position opened
    - TP/SL hit
    - Circuit breaker activated
    - Daily loss limit approaching (>80% of limit)

Integration:
    - python-telegram-bot v20.x (async)
    - ControlPlane via Redis Streams (publishes to control:commands:input)
    - Position data from Redis (positions:snapshot key)
    - Daily PnL from Redis (daily:pnl key)
"""

from __future__ import annotations

import asyncio
import json
import logging
import time
from typing import Optional

from telegram import Update
from telegram.ext import Application, CommandHandler, ContextTypes

logger = logging.getLogger(__name__)


class TelegramBot:
    """Telegram Bot for MMH v3.1 operator interface.

    Provides command handlers for monitoring and controlling the trading
    system, plus an auto-alert consumer that forwards position updates and
    health events to the configured Telegram chat.

    Args:
        bot_token: Telegram Bot API token.
        chat_id: Authorized Telegram chat ID (whitelist).  Only messages
            from this chat will be processed.
        redis_client: An initialized ``redis.asyncio.Redis`` instance for
            reading position snapshots, daily PnL, and system state.
        bus: A ``RedisEventBus`` instance for publishing control commands
            and consuming alert streams.
    """

    def __init__(
        self,
        bot_token: str,
        chat_id: str,
        redis_client,
        bus,
    ) -> None:
        self._bot_token = bot_token
        self._chat_id = str(chat_id)
        self._redis = redis_client
        self._bus = bus

        self._application: Optional[Application] = None
        self._running = False
        self._alert_task: Optional[asyncio.Task] = None

    # ------------------------------------------------------------------
    # Lifecycle
    # ------------------------------------------------------------------

    async def start(self) -> None:
        """Start the Telegram bot polling and alert consumer.

        Builds the ``Application``, registers command handlers, starts
        polling for Telegram updates, and launches the background alert
        consumer task.
        """
        self._running = True

        self._application = (
            Application.builder()
            .token(self._bot_token)
            .build()
        )

        self._register_handlers()

        await self._application.initialize()
        await self._application.start()
        await self._application.updater.start_polling(drop_pending_updates=True)

        logger.info(
            "TelegramBot started -- polling for updates (chat_id=%s)",
            self._chat_id,
        )

        # Launch alert consumer as a background task
        self._alert_task = asyncio.create_task(
            self._consume_alerts(),
            name="telegram-alert-consumer",
        )

        # Keep running until stopped
        try:
            while self._running:
                await asyncio.sleep(1)
        except asyncio.CancelledError:
            pass

    async def stop(self) -> None:
        """Stop the Telegram bot and alert consumer gracefully."""
        self._running = False

        if self._alert_task and not self._alert_task.done():
            self._alert_task.cancel()
            try:
                await self._alert_task
            except asyncio.CancelledError:
                pass

        if self._application:
            try:
                if self._application.updater and self._application.updater.running:
                    await self._application.updater.stop()
                if self._application.running:
                    await self._application.stop()
                await self._application.shutdown()
            except Exception as exc:
                logger.error("Error stopping TelegramBot application: %s", exc)

        logger.info("TelegramBot stopped")

    async def send_alert(self, message: str) -> None:
        """Send an alert message to the configured Telegram chat.

        Args:
            message: The message text to send (supports Markdown).
        """
        if not self._application or not self._application.bot:
            logger.warning(
                "TelegramBot not initialized -- cannot send alert"
            )
            return

        try:
            await self._application.bot.send_message(
                chat_id=self._chat_id,
                text=message,
                parse_mode="Markdown",
            )
        except Exception as exc:
            logger.error("Failed to send Telegram alert: %s", exc)

    # ------------------------------------------------------------------
    # Handler registration
    # ------------------------------------------------------------------

    def _register_handlers(self) -> None:
        """Register all command handlers with the Application."""
        handlers = [
            CommandHandler("status", self._cmd_status),
            CommandHandler("positions", self._cmd_positions),
            CommandHandler("portfolio", self._cmd_portfolio),
            CommandHandler("sell", self._cmd_sell),
            CommandHandler("freeze", self._cmd_freeze),
            CommandHandler("resume", self._cmd_resume),
            CommandHandler("kill", self._cmd_kill),
        ]
        for handler in handlers:
            self._application.add_handler(handler)

    # ------------------------------------------------------------------
    # Authorization
    # ------------------------------------------------------------------

    def _is_authorized(self, update: Update) -> bool:
        """Check whether the incoming message is from the authorized chat.

        Args:
            update: The Telegram ``Update`` object.

        Returns:
            True if the message originates from the whitelisted chat_id.
        """
        if not update.effective_chat:
            return False
        return str(update.effective_chat.id) == self._chat_id

    async def _reject_unauthorized(
        self, update: Update, context: ContextTypes.DEFAULT_TYPE
    ) -> bool:
        """Send an unauthorized response and return True if not authorized.

        Returns:
            True if the request was rejected (unauthorized), False if OK.
        """
        if self._is_authorized(update):
            return False

        logger.warning(
            "Unauthorized Telegram access attempt from chat_id=%s",
            update.effective_chat.id if update.effective_chat else "unknown",
        )
        if update.message:
            await update.message.reply_text("Unauthorized.")
        return True

    # ------------------------------------------------------------------
    # Command handlers
    # ------------------------------------------------------------------

    async def _cmd_status(
        self, update: Update, context: ContextTypes.DEFAULT_TYPE
    ) -> None:
        """/status -- Pipeline overview."""
        if await self._reject_unauthorized(update, context):
            return

        try:
            # System state
            system_state = await self._redis.get("system:state") or "RUNNING"

            # Frozen chains
            frozen_raw = await self._redis.smembers("frozen:chains")
            frozen_chains = sorted(frozen_raw) if frozen_raw else []

            # Open positions count
            positions = await self._load_positions()
            open_count = len(positions)

            # Total exposure
            total_exposure = sum(
                float(p.get("exposure_usd", 0)) for p in positions
            )

            # Daily PnL
            daily_pnl = await self._load_daily_pnl()

            lines = [
                "*MMH v3.1 -- Status*",
                "",
                f"System: `{system_state}`",
                f"Frozen chains: `{', '.join(frozen_chains) if frozen_chains else 'none'}`",
                f"Open positions: `{open_count}`",
                f"Total exposure: `${total_exposure:,.2f}`",
                f"Daily PnL: `${daily_pnl:,.2f}`",
            ]

            await update.message.reply_text("\n".join(lines), parse_mode="Markdown")

        except Exception as exc:
            logger.error("/status error: %s", exc)
            await update.message.reply_text(f"Error fetching status: {exc}")

    async def _cmd_positions(
        self, update: Update, context: ContextTypes.DEFAULT_TYPE
    ) -> None:
        """/positions -- List open positions with PnL."""
        if await self._reject_unauthorized(update, context):
            return

        try:
            positions = await self._load_positions()

            if not positions:
                await update.message.reply_text("No open positions.")
                return

            lines = ["*Open Positions*", ""]
            for i, pos in enumerate(positions, 1):
                pos_id = pos.get("id", "?")[:8]
                symbol = pos.get("token_symbol", "???")
                chain = pos.get("chain", "?")
                pnl_usd = float(pos.get("pnl_usd", 0))
                pnl_pct = float(pos.get("pnl_pct", 0))
                exposure = float(pos.get("exposure_usd", 0))
                status = pos.get("status", "OPEN")

                pnl_emoji = "+" if pnl_usd >= 0 else ""

                lines.append(
                    f"{i}. `{pos_id}` *{symbol}* ({chain})\n"
                    f"   Exposure: ${exposure:,.2f} | "
                    f"PnL: {pnl_emoji}${pnl_usd:,.2f} ({pnl_emoji}{pnl_pct:.1f}%) | "
                    f"Status: {status}"
                )

            await update.message.reply_text("\n".join(lines), parse_mode="Markdown")

        except Exception as exc:
            logger.error("/positions error: %s", exc)
            await update.message.reply_text(f"Error fetching positions: {exc}")

    async def _cmd_portfolio(
        self, update: Update, context: ContextTypes.DEFAULT_TYPE
    ) -> None:
        """/portfolio -- Portfolio summary."""
        if await self._reject_unauthorized(update, context):
            return

        try:
            positions = await self._load_positions()
            daily_pnl = await self._load_daily_pnl()

            total_exposure = sum(
                float(p.get("exposure_usd", 0)) for p in positions
            )

            # Exposure by chain
            chain_exposure: dict[str, float] = {}
            for pos in positions:
                chain = pos.get("chain", "unknown")
                chain_exposure[chain] = chain_exposure.get(chain, 0) + float(
                    pos.get("exposure_usd", 0)
                )

            # Win rate from closed positions (approximate from all positions
            # in snapshot -- the snapshot may include recently closed ones)
            total_closed = 0
            total_wins = 0
            for pos in positions:
                if pos.get("status") == "CLOSED":
                    total_closed += 1
                    if float(pos.get("pnl_usd", 0)) > 0:
                        total_wins += 1

            win_rate = (total_wins / total_closed * 100) if total_closed > 0 else 0.0

            lines = [
                "*MMH v3.1 -- Portfolio*",
                "",
                f"Total exposure: `${total_exposure:,.2f}`",
                f"Daily PnL: `${daily_pnl:,.2f}`",
                f"Open positions: `{len(positions)}`",
                f"Win rate (closed): `{win_rate:.1f}%` ({total_wins}/{total_closed})",
                "",
                "*Exposure by chain:*",
            ]

            for chain, exp in sorted(chain_exposure.items()):
                lines.append(f"  {chain}: `${exp:,.2f}`")

            if not chain_exposure:
                lines.append("  (none)")

            await update.message.reply_text("\n".join(lines), parse_mode="Markdown")

        except Exception as exc:
            logger.error("/portfolio error: %s", exc)
            await update.message.reply_text(f"Error fetching portfolio: {exc}")

    async def _cmd_sell(
        self, update: Update, context: ContextTypes.DEFAULT_TYPE
    ) -> None:
        """/sell <position_id> -- Manual sell a position."""
        if await self._reject_unauthorized(update, context):
            return

        if not context.args:
            await update.message.reply_text(
                "Usage: `/sell <position_id>`", parse_mode="Markdown"
            )
            return

        position_id = context.args[0]

        try:
            # Verify position exists
            positions = await self._load_positions()
            target = None
            for pos in positions:
                if pos.get("id", "").startswith(position_id):
                    target = pos
                    break

            if not target:
                await update.message.reply_text(
                    f"Position `{position_id}` not found.", parse_mode="Markdown"
                )
                return

            # Publish sell command to control:commands:input
            command_data = {
                "command_type": "SELL",
                "target": target["id"],
                "params": json.dumps({
                    "position_id": target["id"],
                    "chain": target.get("chain", ""),
                    "token_address": target.get("token_address", ""),
                    "reason": "manual_telegram",
                }),
                "operator_id": f"telegram:{self._chat_id}",
                "timestamp": str(time.time()),
            }
            await self._bus.publish("control:commands:input", command_data)

            symbol = target.get("token_symbol", "???")
            await update.message.reply_text(
                f"Sell command sent for `{target['id'][:8]}` (*{symbol}*).",
                parse_mode="Markdown",
            )

        except Exception as exc:
            logger.error("/sell error: %s", exc)
            await update.message.reply_text(f"Error executing sell: {exc}")

    async def _cmd_freeze(
        self, update: Update, context: ContextTypes.DEFAULT_TYPE
    ) -> None:
        """/freeze <chain> -- Freeze a specific chain."""
        if await self._reject_unauthorized(update, context):
            return

        if not context.args:
            await update.message.reply_text(
                "Usage: `/freeze <chain>`\nExample: `/freeze solana`",
                parse_mode="Markdown",
            )
            return

        chain = context.args[0].lower()

        try:
            command_data = {
                "command_type": "FREEZE_CHAIN",
                "target": chain,
                "params": json.dumps({"chain": chain}),
                "operator_id": f"telegram:{self._chat_id}",
                "timestamp": str(time.time()),
            }
            await self._bus.publish("control:commands:input", command_data)

            await update.message.reply_text(
                f"Freeze command sent for chain `{chain}`.",
                parse_mode="Markdown",
            )

        except Exception as exc:
            logger.error("/freeze error: %s", exc)
            await update.message.reply_text(f"Error freezing chain: {exc}")

    async def _cmd_resume(
        self, update: Update, context: ContextTypes.DEFAULT_TYPE
    ) -> None:
        """/resume <chain> -- Resume a frozen chain."""
        if await self._reject_unauthorized(update, context):
            return

        if not context.args:
            await update.message.reply_text(
                "Usage: `/resume <chain>`\nExample: `/resume solana`",
                parse_mode="Markdown",
            )
            return

        chain = context.args[0].lower()

        try:
            command_data = {
                "command_type": "RESUME_CHAIN",
                "target": chain,
                "params": json.dumps({"chain": chain}),
                "operator_id": f"telegram:{self._chat_id}",
                "timestamp": str(time.time()),
            }
            await self._bus.publish("control:commands:input", command_data)

            await update.message.reply_text(
                f"Resume command sent for chain `{chain}`.",
                parse_mode="Markdown",
            )

        except Exception as exc:
            logger.error("/resume error: %s", exc)
            await update.message.reply_text(f"Error resuming chain: {exc}")

    async def _cmd_kill(
        self, update: Update, context: ContextTypes.DEFAULT_TYPE
    ) -> None:
        """/kill -- Emergency stop all trading."""
        if await self._reject_unauthorized(update, context):
            return

        try:
            command_data = {
                "command_type": "KILL",
                "target": "global",
                "params": json.dumps({"reason": "operator_kill_via_telegram"}),
                "operator_id": f"telegram:{self._chat_id}",
                "timestamp": str(time.time()),
            }
            await self._bus.publish("control:commands:input", command_data)

            await update.message.reply_text(
                "*KILL command sent.* All trading will stop immediately.",
                parse_mode="Markdown",
            )

        except Exception as exc:
            logger.error("/kill error: %s", exc)
            await update.message.reply_text(f"Error executing kill: {exc}")

    # ------------------------------------------------------------------
    # Alert consumer
    # ------------------------------------------------------------------

    async def _consume_alerts(self) -> None:
        """Consume position:updates and health:freeze streams and forward
        relevant events as Telegram alerts.

        Auto-alerts:
            - New position opened
            - TP/SL hit (position closed)
            - Circuit breaker / freeze activated
            - Daily loss limit approaching (>80% of configured limit)
        """
        streams = {
            "positions:updates": "telegram-bot-pos",
            "health:freeze": "telegram-bot-freeze",
        }
        group = "telegram_bot"

        # Ensure consumer groups exist
        for stream in streams:
            try:
                await self._bus.ensure_consumer_group(stream, group)
            except Exception as exc:
                logger.error(
                    "Failed to create consumer group for %s: %s", stream, exc
                )

        while self._running:
            try:
                # Consume position updates
                await self._consume_position_updates(group, streams["positions:updates"])

                # Consume freeze events
                await self._consume_freeze_events(group, streams["health:freeze"])

                # Check daily loss limit threshold
                await self._check_daily_loss_alert()

            except asyncio.CancelledError:
                break
            except Exception as exc:
                logger.error("Alert consumer error: %s", exc)
                await asyncio.sleep(2)

    async def _consume_position_updates(self, group: str, consumer: str) -> None:
        """Consume position update events and send alerts for key transitions."""
        try:
            messages = await self._bus.consume(
                "positions:updates", group, consumer, count=20, block_ms=1000
            )
            for msg_id, data in messages:
                try:
                    await self._handle_position_alert(data)
                except Exception as exc:
                    logger.error("Error handling position alert: %s", exc)
                await self._bus.ack("positions:updates", group, msg_id)
        except Exception as exc:
            logger.error("Position updates consumer error: %s", exc)
            await asyncio.sleep(1)

    async def _consume_freeze_events(self, group: str, consumer: str) -> None:
        """Consume health:freeze events and send circuit breaker alerts."""
        try:
            messages = await self._bus.consume(
                "health:freeze", group, consumer, count=10, block_ms=1000
            )
            for msg_id, data in messages:
                try:
                    chain = data.get("chain", "unknown")
                    reason = data.get("reason", "unknown")
                    triggered_by = data.get("triggered_by", "unknown")
                    is_auto = data.get("auto", "false") == "true"

                    alert_type = "AUTO-FREEZE" if is_auto else "FREEZE"
                    alert = (
                        f"*{alert_type} ACTIVATED*\n\n"
                        f"Chain: `{chain}`\n"
                        f"Reason: `{reason}`\n"
                        f"Triggered by: `{triggered_by}`"
                    )
                    await self.send_alert(alert)
                except Exception as exc:
                    logger.error("Error handling freeze alert: %s", exc)
                await self._bus.ack("health:freeze", group, msg_id)
        except Exception as exc:
            logger.error("Freeze events consumer error: %s", exc)
            await asyncio.sleep(1)

    async def _handle_position_alert(self, data: dict) -> None:
        """Handle a position update event and send appropriate alert.

        Sends alerts for:
            - New position opened (status == OPEN and no exit data)
            - Position closed (status == CLOSED) with TP/SL info
        """
        status = data.get("status", "")
        position_id = data.get("position_id", "?")[:8]
        chain = data.get("chain", "?")
        token = data.get("token_address", "?")[:12]
        pnl_usd = data.get("pnl_usd", "0")
        pnl_pct = data.get("pnl_pct", "0")
        exposure = data.get("exposure_usd", "0")

        if status == "OPEN":
            alert = (
                f"*New Position Opened*\n\n"
                f"ID: `{position_id}`\n"
                f"Chain: `{chain}`\n"
                f"Token: `{token}`\n"
                f"Exposure: `${float(exposure):,.2f}`"
            )
            await self.send_alert(alert)

        elif status == "CLOSED":
            pnl_f = float(pnl_usd)
            pnl_pct_f = float(pnl_pct)
            result = "WIN" if pnl_f >= 0 else "LOSS"
            pnl_sign = "+" if pnl_f >= 0 else ""

            alert = (
                f"*Position Closed -- {result}*\n\n"
                f"ID: `{position_id}`\n"
                f"Chain: `{chain}`\n"
                f"Token: `{token}`\n"
                f"PnL: `{pnl_sign}${pnl_f:,.2f}` ({pnl_sign}{pnl_pct_f:.1f}%)"
            )
            await self.send_alert(alert)

    async def _check_daily_loss_alert(self) -> None:
        """Check if daily PnL is approaching the loss limit (>80%).

        Reads the configured ``daily_loss_limit_usd`` from Redis
        (``risk:limits`` hash) and compares it against the current
        ``daily:pnl`` value.
        """
        try:
            daily_pnl = await self._load_daily_pnl()

            # Read configured limit from Redis (set by RiskFabric / ControlPlane)
            limit_str = await self._redis.hget("risk:limits", "daily_loss_limit_usd")
            if not limit_str:
                # Fall back: no custom limit in Redis, skip check
                return

            daily_limit = float(limit_str)
            if daily_limit >= 0:
                return  # Limit should be positive; PnL loss is negative

            # daily_pnl is negative when losing money, limit is positive
            # Compare absolute loss against the limit
            if daily_limit > 0 and daily_pnl < 0:
                loss_ratio = abs(daily_pnl) / daily_limit
                if loss_ratio >= 0.8:
                    pct = loss_ratio * 100
                    alert = (
                        f"*DAILY LOSS LIMIT WARNING*\n\n"
                        f"Current daily PnL: `${daily_pnl:,.2f}`\n"
                        f"Daily loss limit: `${daily_limit:,.2f}`\n"
                        f"Usage: `{pct:.0f}%`\n\n"
                        f"Consider reducing exposure or freezing chains."
                    )
                    await self.send_alert(alert)

        except Exception as exc:
            logger.error("Daily loss alert check error: %s", exc)

    # ------------------------------------------------------------------
    # Redis data helpers
    # ------------------------------------------------------------------

    async def _load_positions(self) -> list[dict]:
        """Load open positions from the Redis snapshot.

        Returns:
            List of position dicts.  Empty list on error or if no
            snapshot exists.
        """
        try:
            data = await self._redis.get("positions:snapshot")
            if not data:
                return []
            return json.loads(data)
        except Exception as exc:
            logger.error("Failed to load positions snapshot: %s", exc)
            return []

    async def _load_daily_pnl(self) -> float:
        """Load the current daily PnL from Redis.

        Returns:
            Daily PnL as a float.  Returns 0.0 on error.
        """
        try:
            val = await self._redis.get("daily:pnl")
            if val is None:
                return 0.0
            return float(val)
        except Exception as exc:
            logger.error("Failed to load daily PnL: %s", exc)
            return 0.0
