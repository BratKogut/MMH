"""
MMH v3.1 Event Schema System - Concrete Event Types

Every event type in the MMH pipeline is defined here as a Pydantic v2 model
inheriting from BaseEvent.  Each class sets ``event_type`` as a class-level
default so serialization/deserialization can discriminate without inspecting
payload contents.

Lifecycle flow (happy path):
    TokenDiscovered -> SecurityCheckResult -> EnrichmentResult -> TokenScored
    -> TradeIntent -> RiskDecision -> ApprovedIntent -> ExecutionAttempt
    -> TradeExecuted -> PositionUpdated

Control plane:
    ControlCommand, HeartbeatEvent, FreezeEvent, DecisionJournalEntry

Chain integrity:
    ReorgNotice (invalidates affected events on reorg)
"""
from __future__ import annotations

from typing import Any, Literal

from pydantic import Field

from src.events.base import BaseEvent


# ---------------------------------------------------------------------------
# 1. Token Discovery
# ---------------------------------------------------------------------------
class TokenDiscovered(BaseEvent):
    """Emitted when a new token is detected on-chain (from pool creation,
    bonding curve deployment, or launchpad event).

    This is the *origin event* for most token lifecycles -- its event_id
    becomes the correlation_id for all downstream processing.
    """

    event_type: str = Field(default="TokenDiscovered", frozen=True)

    # On-chain identifiers
    address: str = Field(..., description="Token contract/mint address.")
    name: str = Field(default="", description="Token name (may be empty on-chain).")
    symbol: str = Field(default="", description="Token ticker symbol.")
    creator: str = Field(default="", description="Deployer/creator wallet address.")
    launchpad: str = Field(
        default="unknown",
        description="Launchpad origin: pump.fun | moonshot | raydium | uniswap | unknown.",
    )
    initial_liquidity_usd: float = Field(
        default=0.0, description="Initial liquidity in USD at pool creation."
    )
    pool_address: str = Field(default="", description="DEX pool/pair address.")
    bonding_curve_progress: float = Field(
        default=0.0,
        description="Bonding curve completion percentage (0.0-100.0). 0 if N/A.",
    )

    # Block-level provenance (for reorg handling)
    block_number: int = Field(..., description="Block number of the discovery tx.")
    block_hash: str = Field(..., description="Block hash (hex).")
    tx_hash: str = Field(..., description="Transaction hash of the creation event.")
    log_index: int = Field(
        default=0, description="Log index within the transaction."
    )
    finality_status: str = Field(
        default="confirmed",
        description="confirmed | finalized | safe.  Drives reorg risk assessment.",
    )


# ---------------------------------------------------------------------------
# 2. Reorg Notice
# ---------------------------------------------------------------------------
class ReorgNotice(BaseEvent):
    """Emitted when a chain reorganization is detected.

    All events whose provenance falls within the reorged block range
    are listed in ``affected_event_ids`` and must be treated as
    potentially invalid by downstream consumers.
    """

    event_type: str = Field(default="ReorgNotice", frozen=True)

    block_number: int = Field(..., description="Block number that was reorged.")
    old_block_hash: str = Field(..., description="Hash of the orphaned block.")
    new_block_hash: str = Field(..., description="Hash of the canonical block.")
    affected_event_ids: list[str] = Field(
        default_factory=list,
        description="event_ids of events that were based on the orphaned block.",
    )


# ---------------------------------------------------------------------------
# 3. Security Check Result
# ---------------------------------------------------------------------------
class SecurityCheckResult(BaseEvent):
    """Result of a token safety scan (honeypot check, contract audit, etc.).

    check_type distinguishes fresh on-demand checks from cache hits so
    downstream scoring can weight staleness appropriately.
    """

    event_type: str = Field(default="SecurityCheckResult", frozen=True)

    token_address: str = Field(..., description="Token address that was checked.")
    is_safe: bool = Field(..., description="True if the token passed all safety checks.")
    risk_level: str = Field(
        default="unknown",
        description="low | medium | high | critical | unknown.",
    )
    flags: list[str] = Field(
        default_factory=list,
        description="List of risk flags, e.g. ['honeypot', 'mint_authority_active'].",
    )
    raw_data: dict[str, Any] = Field(
        default_factory=dict,
        description="Raw response from the security provider.",
    )
    check_type: Literal["cached", "fresh"] = Field(
        default="fresh",
        description="Whether this result is from cache or a live check.",
    )


# ---------------------------------------------------------------------------
# 4. Enrichment Result
# ---------------------------------------------------------------------------
class EnrichmentResult(BaseEvent):
    """Enrichment data attached to a token (social signals, holder stats,
    deployer history, etc.).

    Multiple EnrichmentResult events may be emitted for a single token,
    each with a different ``enrichment_type``.
    """

    event_type: str = Field(default="EnrichmentResult", frozen=True)

    token_address: str = Field(..., description="Token address being enriched.")
    enrichment_type: str = Field(
        ...,
        description=(
            "Type of enrichment: holder_analysis | social_signals | "
            "deployer_history | liquidity_depth | metadata."
        ),
    )
    data: dict[str, Any] = Field(
        default_factory=dict,
        description="Enrichment payload (schema varies by enrichment_type).",
    )
    source: str = Field(
        default="",
        description="Data source identifier, e.g. 'birdeye', 'dexscreener', 'internal'.",
    )


# ---------------------------------------------------------------------------
# 5. Token Scored
# ---------------------------------------------------------------------------
class TokenScored(BaseEvent):
    """Output of the scoring engine.  Combines risk, momentum, and other
    signals into an actionable recommendation.

    ``config_hash`` pins the exact scoring configuration used, enabling
    reproducibility and A/B comparison.
    """

    event_type: str = Field(default="TokenScored", frozen=True)

    token_address: str = Field(..., description="Token address that was scored.")
    risk_score: float = Field(
        ..., description="Risk score (0.0 = safest, 1.0 = most risky)."
    )
    momentum_score: float = Field(
        ..., description="Momentum score (0.0 = no momentum, 1.0 = peak momentum)."
    )
    overall_score: float = Field(
        ..., description="Composite score (higher = more attractive trade)."
    )
    recommendation: str = Field(
        default="SKIP",
        description="STRONG_BUY | BUY | SKIP | SELL | STRONG_SELL.",
    )
    flags: list[str] = Field(
        default_factory=list,
        description="Scoring flags, e.g. ['insider_cluster', 'low_liquidity'].",
    )
    details: dict[str, Any] = Field(
        default_factory=dict,
        description="Breakdown of sub-scores and feature values.",
    )
    config_hash: str = Field(
        default="",
        description="SHA-256 of the scoring config used (for reproducibility).",
    )


# ---------------------------------------------------------------------------
# 6. Trade Intent
# ---------------------------------------------------------------------------
class TradeIntent(BaseEvent):
    """A *desire* to trade, pending risk approval.

    This is NOT an order.  It must pass through RiskDecision before
    becoming an ApprovedIntent that the executor can act on.

    ``intent_id`` is the idempotency key: the executor MUST never execute
    the same intent_id twice, even after crash recovery.
    """

    event_type: str = Field(default="TradeIntent", frozen=True)

    intent_id: str = Field(..., description="Unique intent ID (idempotency key).")
    token_address: str = Field(..., description="Token to trade.")
    side: Literal["BUY", "SELL"] = Field(..., description="Trade direction.")
    amount_usd: float = Field(..., description="Desired trade size in USD.")
    reason_codes: list[str] = Field(
        default_factory=list,
        description="Why this trade was proposed, e.g. ['high_score', 'momentum_spike'].",
    )
    scoring_ref_ids: list[str] = Field(
        default_factory=list,
        description="event_ids of TokenScored events that informed this intent.",
    )
    config_hash: str = Field(
        default="",
        description="SHA-256 of the trading config used.",
    )


# ---------------------------------------------------------------------------
# 7. Risk Decision
# ---------------------------------------------------------------------------
class RiskDecision(BaseEvent):
    """Risk gate output: approve, reject, or freeze a TradeIntent.

    FREEZE means the entire system should halt trading on this chain
    (triggers a FreezeEvent downstream).
    """

    event_type: str = Field(default="RiskDecision", frozen=True)

    intent_id: str = Field(..., description="The TradeIntent.intent_id being decided on.")
    decision: Literal["APPROVE", "REJECT", "FREEZE"] = Field(
        ..., description="Gate decision."
    )
    reason_codes: list[str] = Field(
        default_factory=list,
        description="Reasons for the decision, e.g. ['exposure_limit', 'drawdown_breach'].",
    )
    policy_hash: str = Field(
        default="",
        description="SHA-256 of the risk policy configuration used.",
    )
    exposure_snapshot: dict[str, Any] = Field(
        default_factory=dict,
        description="Snapshot of portfolio exposure at decision time.",
    )


# ---------------------------------------------------------------------------
# 8. Approved Intent
# ---------------------------------------------------------------------------
class ApprovedIntent(BaseEvent):
    """A trade intent that has passed the risk gate and is cleared for
    execution.  The executor consumes these events.

    This is a separate event (not just a flag on TradeIntent) so that
    the executor stream contains ONLY actionable items.
    """

    event_type: str = Field(default="ApprovedIntent", frozen=True)

    intent_id: str = Field(..., description="Original intent ID (idempotency key).")
    token_address: str = Field(..., description="Token to trade.")
    side: Literal["BUY", "SELL"] = Field(..., description="Trade direction.")
    amount_usd: float = Field(..., description="Approved trade size in USD.")
    risk_decision_ref: str = Field(
        default="",
        description="event_id of the RiskDecision that approved this intent.",
    )


# ---------------------------------------------------------------------------
# 9. Execution Attempt
# ---------------------------------------------------------------------------
class ExecutionAttempt(BaseEvent):
    """Logged for every on-chain submission attempt (including retries).

    ``dry_run`` allows the executor to simulate without submitting.
    Multiple ExecutionAttempt events may exist for a single intent_id
    if retries are needed (e.g. gas bumps, nonce collisions).
    """

    event_type: str = Field(default="ExecutionAttempt", frozen=True)

    intent_id: str = Field(..., description="The intent being executed.")
    attempt_number: int = Field(
        default=1, description="Attempt sequence (1-indexed)."
    )
    nonce: int = Field(default=-1, description="On-chain nonce used (-1 if N/A).")
    gas_price: float = Field(
        default=0.0, description="Gas price / priority fee in native units."
    )
    route: str = Field(
        default="",
        description="DEX route description, e.g. 'jupiter_v6' or 'uniswap_v3'.",
    )
    dry_run: bool = Field(
        default=False,
        description="True if this is a simulation, not a real submission.",
    )


# ---------------------------------------------------------------------------
# 10. Trade Executed
# ---------------------------------------------------------------------------
class TradeExecuted(BaseEvent):
    """Emitted after an on-chain trade is confirmed (or known to have failed).

    ``confirmed`` is False if the tx was submitted but not yet confirmed,
    or if it reverted.
    """

    event_type: str = Field(default="TradeExecuted", frozen=True)

    intent_id: str = Field(..., description="The intent that was executed.")
    token_address: str = Field(..., description="Token that was traded.")
    side: Literal["BUY", "SELL"] = Field(..., description="Trade direction.")
    tx_hash: str = Field(..., description="On-chain transaction hash.")
    amount_in: float = Field(
        default=0.0, description="Amount of input token spent."
    )
    amount_out: float = Field(
        default=0.0, description="Amount of output token received."
    )
    price: float = Field(default=0.0, description="Effective execution price.")
    price_impact_pct: float = Field(
        default=0.0, description="Price impact as a percentage."
    )
    gas_cost_usd: float = Field(
        default=0.0, description="Gas/fees cost in USD."
    )
    confirmed: bool = Field(
        default=False,
        description="True if the transaction is confirmed on-chain.",
    )


# ---------------------------------------------------------------------------
# 11. Position Updated
# ---------------------------------------------------------------------------
class PositionUpdated(BaseEvent):
    """Snapshot of a position's current state.

    Emitted on entry, on every meaningful price change, and on exit.
    ``status`` tracks the position lifecycle.
    """

    event_type: str = Field(default="PositionUpdated", frozen=True)

    position_id: str = Field(..., description="Unique position identifier.")
    token_address: str = Field(..., description="Token address of the position.")
    status: str = Field(
        default="open",
        description="open | closing | closed | liquidated.",
    )
    entry_price: float = Field(default=0.0, description="Entry price in USD.")
    current_price: float = Field(default=0.0, description="Current price in USD.")
    pnl_usd: float = Field(default=0.0, description="Unrealized P&L in USD.")
    pnl_pct: float = Field(default=0.0, description="Unrealized P&L as percentage.")
    exposure_usd: float = Field(
        default=0.0, description="Current exposure in USD."
    )


# ---------------------------------------------------------------------------
# 12. Control Command
# ---------------------------------------------------------------------------
class ControlCommand(BaseEvent):
    """Operator-initiated control plane command.

    FREEZE: halt all trading on target chain/module.
    KILL:   emergency shutdown of a specific component.
    RESUME: lift a freeze.
    CONFIG_UPDATE: hot-reload configuration.
    """

    event_type: str = Field(default="ControlCommand", frozen=True)

    command_type: Literal["FREEZE", "KILL", "RESUME", "CONFIG_UPDATE"] = Field(
        ..., description="Type of control command."
    )
    target: str = Field(
        default="*",
        description="Target module or chain ('*' for all).",
    )
    params: dict[str, Any] = Field(
        default_factory=dict,
        description="Command-specific parameters.",
    )
    operator_id: str = Field(
        default="system",
        description="Operator or system component that issued the command.",
    )


# ---------------------------------------------------------------------------
# 13. Heartbeat Event
# ---------------------------------------------------------------------------
class HeartbeatEvent(BaseEvent):
    """Periodic liveness signal from each module.

    If a module's heartbeat is missing for > configured threshold,
    the control plane emits a FreezeEvent.
    """

    event_type: str = Field(default="HeartbeatEvent", frozen=True)

    source_module: str = Field(
        ..., description="Module name emitting the heartbeat."
    )
    config_hash: str = Field(
        default="",
        description="SHA-256 of the module's current config (drift detection).",
    )
    uptime_seconds: float = Field(
        default=0.0, description="Seconds since module startup."
    )
    status: str = Field(
        default="healthy",
        description="healthy | degraded | unhealthy.",
    )


# ---------------------------------------------------------------------------
# 14. Freeze Event
# ---------------------------------------------------------------------------
class FreezeEvent(BaseEvent):
    """System-wide or chain-specific trading freeze.

    Triggered by risk breaches, missing heartbeats, operator command, or
    anomaly detection.  ``auto`` distinguishes automated from manual freezes.
    """

    event_type: str = Field(default="FreezeEvent", frozen=True)

    reason: str = Field(
        ..., description="Human-readable reason for the freeze."
    )
    triggered_by: str = Field(
        default="",
        description="event_id or operator_id that caused the freeze.",
    )
    auto: bool = Field(
        default=True,
        description="True if triggered automatically, False if manual.",
    )


# ---------------------------------------------------------------------------
# 15. Decision Journal Entry
# ---------------------------------------------------------------------------
class DecisionJournalEntry(BaseEvent):
    """Audit trail entry capturing a decision point in the pipeline.

    Connects input events to output events with the reasoning and
    configuration that produced the decision.  Essential for post-mortem
    analysis and strategy backtesting.
    """

    event_type: str = Field(default="DecisionJournalEntry", frozen=True)

    decision_type: str = Field(
        ...,
        description=(
            "Type of decision: scoring | risk_gate | execution | exit | freeze."
        ),
    )
    input_event_ids: list[str] = Field(
        default_factory=list,
        description="event_ids of events that were inputs to this decision.",
    )
    output_event_id: str = Field(
        default="",
        description="event_id of the event produced by this decision.",
    )
    reason_codes: list[str] = Field(
        default_factory=list,
        description="Machine-readable reason codes for the decision.",
    )
    config_hash: str = Field(
        default="",
        description="SHA-256 of the config used for this decision.",
    )
    module: str = Field(
        default="",
        description="Module that made the decision.",
    )


# ---------------------------------------------------------------------------
# All event classes for registry building (see serialization.py)
# ---------------------------------------------------------------------------
ALL_EVENT_CLASSES: list[type[BaseEvent]] = [
    TokenDiscovered,
    ReorgNotice,
    SecurityCheckResult,
    EnrichmentResult,
    TokenScored,
    TradeIntent,
    RiskDecision,
    ApprovedIntent,
    ExecutionAttempt,
    TradeExecuted,
    PositionUpdated,
    ControlCommand,
    HeartbeatEvent,
    FreezeEvent,
    DecisionJournalEntry,
]
