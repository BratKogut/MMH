"""RiskFabric - Guardian with Veto Power.

SAFETY > PROFIT

RiskFabric sees:
- TradeIntent from Scoring
- Current ledger (position snapshot + stream delta)
- Pending intents (to prevent over-allocation)

Output: APPROVE | REJECT | FREEZE + reason codes

Risk policies are in versioned config with hash in every decision event.
"""

import asyncio
import json
import logging
import time
import hashlib
from typing import Optional
from enum import Enum

logger = logging.getLogger(__name__)


class RiskDecisionType(str, Enum):
    APPROVE = "APPROVE"
    REJECT = "REJECT"
    FREEZE = "FREEZE"


class RiskPolicy:
    """A single risk policy rule."""
    def __init__(self, name: str, check_fn, severity: str = "REJECT"):
        self.name = name
        self.check_fn = check_fn  # async def check(context) -> (bool, str)
        self.severity = severity  # REJECT or FREEZE


class RiskContext:
    """Context provided to risk policies for evaluation."""
    def __init__(self, intent_data: dict, positions: list, pending_intents: list,
                 daily_pnl: float, config: dict):
        self.intent_data = intent_data
        self.chain = intent_data.get("chain", "")
        self.token_address = intent_data.get("token_address", "")
        self.side = intent_data.get("side", "BUY")
        self.amount_usd = float(intent_data.get("amount_usd", "0") or "0")
        self.positions = positions  # list of open position dicts
        self.pending_intents = pending_intents
        self.daily_pnl = daily_pnl
        self.config = config

    @property
    def total_exposure(self) -> float:
        return sum(float(p.get("exposure_usd", 0) or 0) for p in self.positions)

    @property
    def chain_positions(self) -> list:
        return [p for p in self.positions if p.get("chain") == self.chain]

    @property
    def chain_exposure(self) -> float:
        return sum(float(p.get("exposure_usd", 0) or 0) for p in self.chain_positions)

    @property
    def pending_exposure(self) -> float:
        return sum(float(i.get("amount_usd", 0) or 0) for i in self.pending_intents)

    @property
    def has_existing_position(self) -> bool:
        return any(p.get("token_address") == self.token_address and p.get("status") == "OPEN"
                   for p in self.positions)


class RiskFabric:
    """RiskFabric - Guardian with veto power over all trade intents.

    Consumes from: risk:decisions (TradeIntents from Scoring)
    Publishes to: execution:approved (ApprovedIntents)
    Also publishes: health:freeze (on FREEZE decisions)
    Logs to: journal:decisions

    All policies are versioned (config hash in every decision).
    """

    def __init__(self, bus, redis_client, config: dict):
        self._bus = bus
        self._redis = redis_client
        self._config = config
        self._policies: list[RiskPolicy] = []
        self._running = False
        self._pending_intents: dict[str, dict] = {}  # intent_id -> data
        self._frozen_chains: set[str] = set()
        self._policy_hash = self._compute_policy_hash()

        # Metrics
        self._approved = 0
        self._rejected = 0
        self._frozen = 0
        self._reject_reasons: dict[str, int] = {}

        # Register default policies
        self._register_default_policies()

    def _compute_policy_hash(self) -> str:
        config_str = json.dumps(self._config, sort_keys=True, default=str)
        return hashlib.sha256(config_str.encode()).hexdigest()[:16]

    def _register_default_policies(self):
        """Register all default risk policies."""

        # Policy 1: Max portfolio exposure
        async def check_max_exposure(ctx: RiskContext) -> tuple[bool, str]:
            max_exp = ctx.config.get("max_portfolio_exposure_usd", 500)
            total = ctx.total_exposure + ctx.pending_exposure + ctx.amount_usd
            if total > max_exp:
                return False, f"OVER_MAX_EXPOSURE:total={total:.2f},max={max_exp}"
            return True, ""
        self._policies.append(RiskPolicy("max_portfolio_exposure", check_max_exposure, "REJECT"))

        # Policy 2: Max positions per chain
        async def check_max_positions_chain(ctx: RiskContext) -> tuple[bool, str]:
            max_pos = ctx.config.get("max_positions_per_chain", 5)
            current = len([p for p in ctx.chain_positions if p.get("status") == "OPEN"])
            if current >= max_pos:
                return False, f"MAX_POSITIONS_CHAIN:{ctx.chain}:current={current},max={max_pos}"
            return True, ""
        self._policies.append(RiskPolicy("max_positions_per_chain", check_max_positions_chain, "REJECT"))

        # Policy 3: Max single position size
        async def check_max_single_position(ctx: RiskContext) -> tuple[bool, str]:
            max_pct = ctx.config.get("max_single_position_pct", 20)
            max_exp = ctx.config.get("max_portfolio_exposure_usd", 500)
            max_single = max_exp * (max_pct / 100)
            if ctx.amount_usd > max_single:
                return False, f"OVER_SINGLE_POSITION_MAX:amount={ctx.amount_usd:.2f},max={max_single:.2f}"
            return True, ""
        self._policies.append(RiskPolicy("max_single_position", check_max_single_position, "REJECT"))

        # Policy 4: Daily loss limit -> FREEZE
        async def check_daily_loss(ctx: RiskContext) -> tuple[bool, str]:
            limit = ctx.config.get("daily_loss_limit_usd", 200)
            if ctx.daily_pnl < -limit:
                return False, f"DAILY_LOSS_LIMIT:pnl={ctx.daily_pnl:.2f},limit=-{limit}"
            return True, ""
        self._policies.append(RiskPolicy("daily_loss_limit", check_daily_loss, "FREEZE"))

        # Policy 5: No duplicate positions
        async def check_no_duplicate(ctx: RiskContext) -> tuple[bool, str]:
            if ctx.side == "BUY" and ctx.has_existing_position:
                return False, f"DUPLICATE_POSITION:{ctx.token_address}"
            return True, ""
        self._policies.append(RiskPolicy("no_duplicate_position", check_no_duplicate, "REJECT"))

        # Policy 6: Chain not frozen
        async def check_chain_not_frozen(ctx: RiskContext) -> tuple[bool, str]:
            if ctx.chain in self._frozen_chains:
                return False, f"CHAIN_FROZEN:{ctx.chain}"
            return True, ""
        self._policies.append(RiskPolicy("chain_not_frozen", check_chain_not_frozen, "REJECT"))

    async def start(self):
        """Start RiskFabric consumer."""
        self._running = True
        input_stream = "risk:decisions"
        group = "risk_fabric"
        consumer = "risk-main"

        await self._bus.ensure_consumer_group(input_stream, group)

        while self._running:
            try:
                messages = await self._bus.consume(input_stream, group, consumer, count=10, block_ms=2000)
                for msg_id, data in messages:
                    decision = await self.evaluate(data)
                    await self._bus.ack(input_stream, group, msg_id)
            except Exception as e:
                logger.error(f"RiskFabric error: {e}")
                await asyncio.sleep(1)

    async def stop(self):
        self._running = False

    async def evaluate(self, intent_data: dict) -> dict:
        """Evaluate a TradeIntent against all risk policies.

        Returns decision dict with APPROVE/REJECT/FREEZE.
        """
        intent_id = intent_data.get("intent_id", "unknown")

        # Build context
        positions = await self._get_positions()
        daily_pnl = await self._get_daily_pnl()

        context = RiskContext(
            intent_data=intent_data,
            positions=positions,
            pending_intents=list(self._pending_intents.values()),
            daily_pnl=daily_pnl,
            config=self._config,
        )

        # Evaluate all policies
        reject_reasons = []
        freeze_reasons = []

        for policy in self._policies:
            try:
                passed, reason = await policy.check_fn(context)
                if not passed:
                    if policy.severity == "FREEZE":
                        freeze_reasons.append(f"{policy.name}:{reason}")
                    else:
                        reject_reasons.append(f"{policy.name}:{reason}")
            except Exception as e:
                logger.error(f"Policy {policy.name} error: {e}")
                reject_reasons.append(f"{policy.name}:ERROR:{e}")

        # Determine decision
        if freeze_reasons:
            decision_type = RiskDecisionType.FREEZE
            all_reasons = freeze_reasons + reject_reasons
            self._frozen += 1
            # Publish freeze event
            await self._publish_freeze(intent_data.get("chain", ""), freeze_reasons)
        elif reject_reasons:
            decision_type = RiskDecisionType.REJECT
            all_reasons = reject_reasons
            self._rejected += 1
            for reason in reject_reasons:
                policy_name = reason.split(":")[0]
                self._reject_reasons[policy_name] = self._reject_reasons.get(policy_name, 0) + 1
        else:
            decision_type = RiskDecisionType.APPROVE
            all_reasons = []
            self._approved += 1

            # Track as pending intent
            self._pending_intents[intent_id] = intent_data

            # Publish approved intent
            approved_data = dict(intent_data)
            approved_data["event_type"] = "ApprovedIntent"
            approved_data["risk_decision"] = "APPROVE"
            approved_data["policy_hash"] = self._policy_hash
            approved_data["event_time"] = str(time.time())
            await self._bus.publish("execution:approved", approved_data)

        # Build decision record
        decision = {
            "event_type": "RiskDecision",
            "intent_id": intent_id,
            "decision": decision_type.value,
            "reason_codes": json.dumps(all_reasons),
            "policy_hash": self._policy_hash,
            "exposure_snapshot": json.dumps({
                "total_exposure": context.total_exposure,
                "pending_exposure": context.pending_exposure,
                "chain_exposure": context.chain_exposure,
                "daily_pnl": daily_pnl,
                "open_positions": len([p for p in positions if p.get("status") == "OPEN"]),
            }),
            "event_time": str(time.time()),
        }

        # Log to journal
        await self._log_decision(decision, intent_id)

        logger.info(f"Risk decision for {intent_id}: {decision_type.value} reasons={all_reasons}")
        return decision

    async def freeze_chain(self, chain: str, reason: str):
        """Freeze a chain (no new trades)."""
        self._frozen_chains.add(chain)
        logger.warning(f"Chain {chain} FROZEN: {reason}")
        await self._publish_freeze(chain, [reason])

    async def resume_chain(self, chain: str):
        """Resume a frozen chain."""
        self._frozen_chains.discard(chain)
        logger.info(f"Chain {chain} RESUMED")

    def clear_pending_intent(self, intent_id: str):
        """Remove pending intent after execution/rejection."""
        self._pending_intents.pop(intent_id, None)

    async def _get_positions(self) -> list[dict]:
        """Get current open positions from Redis cache."""
        try:
            data = await self._redis.get("positions:snapshot")
            if data:
                return json.loads(data)
        except Exception as e:
            logger.error(f"Failed to get positions: {e}")
        return []

    async def _get_daily_pnl(self) -> float:
        """Get today's PnL from Redis."""
        try:
            data = await self._redis.get("daily:pnl")
            if data:
                return float(data)
        except Exception as e:
            logger.error(f"Failed to get daily PnL: {e}")
        return 0.0

    async def _publish_freeze(self, chain: str, reasons: list[str]):
        """Publish freeze event."""
        freeze_data = {
            "event_type": "FreezeEvent",
            "chain": chain,
            "reason": json.dumps(reasons),
            "triggered_by": "RiskFabric",
            "auto": "true",
            "event_time": str(time.time()),
        }
        await self._bus.publish("health:freeze", freeze_data)

    async def _log_decision(self, decision: dict, intent_id: str):
        """Log risk decision to journal."""
        journal_entry = {
            "event_type": "DecisionJournalEntry",
            "decision_type": "RISK",
            "module": "RiskFabric",
            "input_event_ids": json.dumps([intent_id]),
            "output_event_id": intent_id,
            "reason_codes": decision.get("reason_codes", "[]"),
            "config_hash": self._policy_hash,
            "decision_data": json.dumps({
                "decision": decision.get("decision"),
                "exposure": decision.get("exposure_snapshot"),
            }),
            "event_time": str(time.time()),
        }
        try:
            await self._bus.publish("journal:decisions", journal_entry)
        except Exception as e:
            logger.error(f"Failed to log risk decision: {e}")

    def get_metrics(self) -> dict:
        return {
            "approved": self._approved,
            "rejected": self._rejected,
            "frozen": self._frozen,
            "reject_reasons": dict(self._reject_reasons),
            "pending_intents": len(self._pending_intents),
            "frozen_chains": list(self._frozen_chains),
            "policy_hash": self._policy_hash,
        }
