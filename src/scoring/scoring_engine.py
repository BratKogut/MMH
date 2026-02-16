"""Deterministic Scoring Engine.

CRITICAL RULES:
- ZERO I/O to network during scoring
- Receives only "clean + enriched" events
- Output: TradeIntent (not "order")
- All scoring is deterministic: same input -> same output
- Config hash emitted with every decision for versioning

Chain-specific scoring weights:
| Factor              | Solana | Base | BSC | TON |
|---------------------|--------|------|-----|-----|
| Honeypot Check      |   -    | 25%  | 30% |  -  |
| LP Burned/Locked    | 20%    | 20%  | 20% | 25% |
| Holder Distribution | 20%    | 20%  | 15% | 25% |
| Creator History     | 15%    |  -   |  -  | 15% |
| Graduation          | 15%    |  -   |  -  |  -  |
| Contract Verified   |  -     | 10%  | 10% |  -  |
| Organic Volume      | 15%    |  -   |  -  |  -  |
| Bonding Momentum    | 15%    |  -   |  -  |  -  |
| Tax Level           |  -     | 10%  | 10% |  -  |
| Liquidity Depth     |  -     | 15%  |  -  |  -  |
| PinkSale Audit      |  -     |  -   | 15% |  -  |
| Jetton Compliance   |  -     |  -   |  -  | 20% |
| Telegram Presence   |  -     |  -   |  -  | 15% |
"""

import hashlib
import json
import logging
import time
from typing import Optional
from enum import Enum

logger = logging.getLogger(__name__)


class Recommendation(str, Enum):
    STRONG_BUY = "STRONG_BUY"
    BUY = "BUY"
    WATCH = "WATCH"
    AVOID = "AVOID"


# Default scoring weights per chain
DEFAULT_WEIGHTS = {
    "solana": {
        "lp_burned": 0.20,
        "holder_distribution": 0.20,
        "creator_history": 0.15,
        "graduation": 0.15,
        "organic_volume": 0.15,
        "bonding_momentum": 0.15,
    },
    "base": {
        "honeypot_check": 0.25,
        "lp_burned": 0.20,
        "holder_distribution": 0.20,
        "liquidity_depth": 0.15,
        "contract_verified": 0.10,
        "tax_level": 0.10,
    },
    "bsc": {
        "honeypot_check": 0.30,
        "lp_burned": 0.20,
        "holder_distribution": 0.15,
        "pinksale_audit": 0.15,
        "contract_verified": 0.10,
        "tax_level": 0.10,
    },
    "ton": {
        "lp_burned": 0.25,
        "holder_distribution": 0.25,
        "jetton_compliance": 0.20,
        "creator_history": 0.15,
        "telegram_presence": 0.15,
    },
}


class ScoringConfig:
    """Scoring configuration with version hash."""

    def __init__(self, weights: dict = None,
                 min_score_strong_buy: int = 80,
                 min_score_buy: int = 60):
        self.weights = weights or DEFAULT_WEIGHTS
        self.min_score_strong_buy = min_score_strong_buy
        self.min_score_buy = min_score_buy
        self._config_hash = self._compute_hash()

    def _compute_hash(self) -> str:
        config_str = json.dumps({
            "weights": self.weights,
            "min_strong_buy": self.min_score_strong_buy,
            "min_buy": self.min_score_buy,
        }, sort_keys=True)
        return hashlib.sha256(config_str.encode()).hexdigest()[:16]

    @property
    def config_hash(self) -> str:
        return self._config_hash


class ScoringResult:
    """Result of scoring a token."""
    def __init__(self, chain: str, token_address: str, risk_score: int,
                 momentum_score: int, overall_score: int, recommendation: Recommendation,
                 flags: list[str], details: dict, config_hash: str):
        self.chain = chain
        self.token_address = token_address
        self.risk_score = risk_score  # 0-100, higher = safer
        self.momentum_score = momentum_score  # 0-100
        self.overall_score = overall_score  # 0-100
        self.recommendation = recommendation
        self.flags = flags
        self.details = details
        self.config_hash = config_hash


class ScoringEngine:
    """Deterministic scoring engine.

    Consumes from: tokens:enriched:{chain}
    Publishes to: scoring:results:{chain}
    Also publishes TradeIntent for STRONG_BUY/BUY to risk:decisions stream
    Logs to: journal:decisions

    ZERO network I/O during scoring.
    """

    def __init__(self, bus, config: ScoringConfig = None, enabled_chains: list[str] = None):
        self._bus = bus
        self._config = config or ScoringConfig()
        self._chains = enabled_chains or ["solana", "base"]
        self._running = False

        # Metrics
        self._scored = 0
        self._intents_generated = 0
        self._scores_by_recommendation: dict[str, int] = {}

    async def start(self):
        """Start scoring consumers."""
        import asyncio
        self._running = True
        tasks = []
        for chain in self._chains:
            tasks.append(asyncio.create_task(self._consume_chain(chain)))
        await asyncio.gather(*tasks)

    async def stop(self):
        self._running = False

    async def _consume_chain(self, chain: str):
        """Consume enriched events and score them."""
        import asyncio
        input_stream = f"tokens:enriched:{chain}"
        output_stream = f"scoring:results:{chain}"
        group = "scoring"
        consumer = f"scoring-{chain}"

        await self._bus.ensure_consumer_group(input_stream, group)

        while self._running:
            try:
                messages = await self._bus.consume(input_stream, group, consumer, count=10, block_ms=2000)
                for msg_id, data in messages:
                    result = self.score(data, chain)
                    self._scored += 1
                    self._scores_by_recommendation[result.recommendation.value] = \
                        self._scores_by_recommendation.get(result.recommendation.value, 0) + 1

                    # Publish scoring result
                    score_data = {
                        "event_type": "TokenScored",
                        "chain": chain,
                        "token_address": result.token_address,
                        "risk_score": str(result.risk_score),
                        "momentum_score": str(result.momentum_score),
                        "overall_score": str(result.overall_score),
                        "recommendation": result.recommendation.value,
                        "flags": json.dumps(result.flags),
                        "details": json.dumps(result.details),
                        "config_hash": result.config_hash,
                        "event_time": str(time.time()),
                        "source_event_id": data.get("event_id", ""),
                    }
                    await self._bus.publish(output_stream, score_data)

                    # Generate TradeIntent for actionable scores
                    if result.recommendation in (Recommendation.STRONG_BUY, Recommendation.BUY):
                        await self._generate_trade_intent(result, data)

                    # Log to decision journal
                    await self._log_decision(result, data.get("event_id", ""))

                    await self._bus.ack(input_stream, group, msg_id)
            except Exception as e:
                logger.error(f"Scoring error on {chain}: {e}")
                import asyncio
                await asyncio.sleep(1)

    def score(self, event_data: dict, chain: str) -> ScoringResult:
        """Score a token. PURE FUNCTION - no I/O.

        Args:
            event_data: enriched event data dict
            chain: blockchain name

        Returns:
            ScoringResult with scores and recommendation
        """
        token_address = event_data.get("address", "") or event_data.get("token_address", "")
        security_data = {}
        security_str = event_data.get("security_result", "{}")
        try:
            security_data = json.loads(security_str) if isinstance(security_str, str) else security_str
        except (json.JSONDecodeError, TypeError):
            pass

        weights = self._config.weights.get(chain, {})
        flags = []
        details = {}

        # Calculate risk score (0-100, higher = safer)
        risk_score = self._calculate_risk_score(event_data, security_data, chain, weights, flags, details)

        # Calculate momentum score (0-100)
        momentum_score = self._calculate_momentum_score(event_data, chain, weights, flags, details)

        # Overall score (weighted average: 60% risk, 40% momentum)
        overall_score = int(risk_score * 0.6 + momentum_score * 0.4)

        # Determine recommendation
        if overall_score >= self._config.min_score_strong_buy and risk_score >= 70:
            recommendation = Recommendation.STRONG_BUY
        elif overall_score >= self._config.min_score_buy and risk_score >= 50:
            recommendation = Recommendation.BUY
        elif overall_score >= 40:
            recommendation = Recommendation.WATCH
        else:
            recommendation = Recommendation.AVOID

        # Override: any CRITICAL security flag -> AVOID
        if security_data.get("risk_level") == "CRITICAL":
            recommendation = Recommendation.AVOID
            flags.append("CRITICAL_SECURITY_OVERRIDE")

        return ScoringResult(
            chain=chain,
            token_address=token_address,
            risk_score=risk_score,
            momentum_score=momentum_score,
            overall_score=overall_score,
            recommendation=recommendation,
            flags=flags,
            details=details,
            config_hash=self._config.config_hash,
        )

    def _calculate_risk_score(self, event_data: dict, security_data: dict,
                              chain: str, weights: dict, flags: list, details: dict) -> int:
        """Calculate risk score based on security data and chain-specific weights."""
        score = 0.0
        max_score = 0.0

        # Parse security flags
        sec_flags = security_data.get("flags", [])
        is_safe = security_data.get("is_safe", None)

        # Honeypot check (Base, BSC)
        w = weights.get("honeypot_check", 0)
        if w > 0:
            max_score += w
            honeypot_safe = "HONEYPOT" not in sec_flags
            factor_score = 100 if honeypot_safe else 0
            score += w * factor_score
            details["honeypot_check"] = {"score": factor_score, "weight": w, "safe": honeypot_safe}

        # LP Burned/Locked
        w = weights.get("lp_burned", 0)
        if w > 0:
            max_score += w
            lp_score = 50  # default neutral
            # Check if LP burned info is available
            providers_data = security_data.get("providers_data", {})
            for prov_data in providers_data.values():
                if isinstance(prov_data, dict):
                    sec_inner = prov_data.get("security", prov_data)
                    lp_burned = sec_inner.get("lpBurnedPercent") or sec_inner.get("lp_burned_percent")
                    if lp_burned is not None:
                        try:
                            lp_pct = float(lp_burned)
                            lp_score = min(100, int(lp_pct))
                        except (ValueError, TypeError):
                            pass
            score += w * lp_score
            details["lp_burned"] = {"score": lp_score, "weight": w}

        # Holder distribution
        w = weights.get("holder_distribution", 0)
        if w > 0:
            max_score += w
            holder_score = 50  # default
            for prov_data in security_data.get("providers_data", {}).values():
                if isinstance(prov_data, dict):
                    sec_inner = prov_data.get("security", prov_data)
                    top10 = sec_inner.get("top10HolderPercent") or sec_inner.get("holder_count")
                    if top10 is not None:
                        try:
                            top10_pct = float(top10)
                            # Lower concentration = better
                            holder_score = max(0, min(100, int(100 - top10_pct)))
                        except (ValueError, TypeError):
                            pass
            score += w * holder_score
            details["holder_distribution"] = {"score": holder_score, "weight": w}

        # Creator history (Solana, TON)
        w = weights.get("creator_history", 0)
        if w > 0:
            max_score += w
            creator_score = 50  # neutral default
            score += w * creator_score
            details["creator_history"] = {"score": creator_score, "weight": w}

        # Graduation (Solana - pump.fun)
        w = weights.get("graduation", 0)
        if w > 0:
            max_score += w
            grad_score = 0
            bonding_progress = event_data.get("bonding_curve_progress")
            if bonding_progress is not None:
                try:
                    progress = float(bonding_progress)
                    grad_score = min(100, int(progress))
                except (ValueError, TypeError):
                    pass
            launchpad = event_data.get("launchpad", "")
            if launchpad and "pump" in str(launchpad).lower():
                grad_score = max(grad_score, 30)  # bonus for pump.fun origin
            score += w * grad_score
            details["graduation"] = {"score": grad_score, "weight": w}

        # Contract verified (Base, BSC)
        w = weights.get("contract_verified", 0)
        if w > 0:
            max_score += w
            verified_score = 50
            score += w * verified_score
            details["contract_verified"] = {"score": verified_score, "weight": w}

        # Tax level (Base, BSC)
        w = weights.get("tax_level", 0)
        if w > 0:
            max_score += w
            tax_score = 100  # perfect by default
            for flag in sec_flags:
                if "HIGH_BUY_TAX" in flag or "HIGH_SELL_TAX" in flag:
                    tax_score = 20
                    break
            score += w * tax_score
            details["tax_level"] = {"score": tax_score, "weight": w}

        # Organic volume (Solana)
        w = weights.get("organic_volume", 0)
        if w > 0:
            max_score += w
            vol_score = 50
            score += w * vol_score
            details["organic_volume"] = {"score": vol_score, "weight": w}

        # Bonding momentum (Solana)
        w = weights.get("bonding_momentum", 0)
        if w > 0:
            max_score += w
            momentum = 50
            score += w * momentum
            details["bonding_momentum"] = {"score": momentum, "weight": w}

        # Liquidity depth (Base)
        w = weights.get("liquidity_depth", 0)
        if w > 0:
            max_score += w
            liq_score = 50
            liq_usd = event_data.get("initial_liquidity_usd")
            if liq_usd is not None:
                try:
                    liq = float(liq_usd)
                    if liq > 50000:
                        liq_score = 90
                    elif liq > 10000:
                        liq_score = 70
                    elif liq > 1000:
                        liq_score = 50
                    else:
                        liq_score = 20
                except (ValueError, TypeError):
                    pass
            score += w * liq_score
            details["liquidity_depth"] = {"score": liq_score, "weight": w}

        # PinkSale audit (BSC)
        w = weights.get("pinksale_audit", 0)
        if w > 0:
            max_score += w
            audit_score = 50
            score += w * audit_score
            details["pinksale_audit"] = {"score": audit_score, "weight": w}

        # Jetton compliance (TON)
        w = weights.get("jetton_compliance", 0)
        if w > 0:
            max_score += w
            jetton_score = 50
            score += w * jetton_score
            details["jetton_compliance"] = {"score": jetton_score, "weight": w}

        # Telegram presence (TON)
        w = weights.get("telegram_presence", 0)
        if w > 0:
            max_score += w
            tg_score = 50
            score += w * tg_score
            details["telegram_presence"] = {"score": tg_score, "weight": w}

        # Normalize
        if max_score > 0:
            final_risk_score = int(score / max_score)
        else:
            final_risk_score = 50

        return max(0, min(100, final_risk_score))

    def _calculate_momentum_score(self, event_data: dict, chain: str,
                                  weights: dict, flags: list, details: dict) -> int:
        """Calculate momentum score from market data."""
        score = 50  # neutral default

        # Liquidity-based momentum
        liq_usd = event_data.get("initial_liquidity_usd")
        if liq_usd:
            try:
                liq = float(liq_usd)
                if liq > 100000:
                    score += 20
                elif liq > 10000:
                    score += 10
            except (ValueError, TypeError):
                pass

        # Bonding curve momentum (Solana pump.fun)
        bonding = event_data.get("bonding_curve_progress")
        if bonding:
            try:
                bp = float(bonding)
                if 60 <= bp <= 90:  # sweet spot
                    score += 15
                elif bp > 90:  # near graduation
                    score += 25
            except (ValueError, TypeError):
                pass

        return max(0, min(100, score))

    async def _generate_trade_intent(self, result: ScoringResult, event_data: dict):
        """Generate TradeIntent for actionable scoring results."""
        import hashlib as _hashlib

        # Deterministic intent_id
        intent_seed = f"{result.chain}:{result.token_address}:BUY:{int(time.time())}"
        intent_id = _hashlib.sha256(intent_seed.encode()).hexdigest()[:32]

        intent_data = {
            "event_type": "TradeIntent",
            "intent_id": intent_id,
            "chain": result.chain,
            "token_address": result.token_address,
            "side": "BUY",
            "recommendation": result.recommendation.value,
            "overall_score": str(result.overall_score),
            "risk_score": str(result.risk_score),
            "reason_codes": json.dumps(result.flags),
            "scoring_ref_id": event_data.get("event_id", ""),
            "config_hash": result.config_hash,
            "event_time": str(time.time()),
        }

        await self._bus.publish("risk:decisions", intent_data)
        self._intents_generated += 1
        logger.info(f"TradeIntent generated: {intent_id} for {result.token_address} on {result.chain} ({result.recommendation.value})")

    async def _log_decision(self, result: ScoringResult, source_event_id: str):
        """Log scoring decision to journal."""
        journal_entry = {
            "event_type": "DecisionJournalEntry",
            "decision_type": "SCORING",
            "module": "ScoringEngine",
            "input_event_ids": json.dumps([source_event_id]),
            "reason_codes": json.dumps(result.flags),
            "config_hash": result.config_hash,
            "decision_data": json.dumps({
                "risk_score": result.risk_score,
                "momentum_score": result.momentum_score,
                "overall_score": result.overall_score,
                "recommendation": result.recommendation.value,
            }),
            "event_time": str(time.time()),
        }
        try:
            await self._bus.publish("journal:decisions", journal_entry)
        except Exception as e:
            logger.error(f"Failed to log scoring decision: {e}")

    def get_metrics(self) -> dict:
        return {
            "scored": self._scored,
            "intents_generated": self._intents_generated,
            "by_recommendation": dict(self._scores_by_recommendation),
            "config_hash": self._config.config_hash,
        }
