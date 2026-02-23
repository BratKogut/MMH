# NEXUS Memecoin Hunter v3.2 — Scoring & Execution

---

## 1. 5-Dimensional Scoring System

### Overview

The scorer evaluates tokens across 5 dimensions, each capturing a different aspect of risk and opportunity. Weights are adaptive and adjust based on trade outcomes.

### Dimensions

| # | Dimension | Default Weight | What It Measures |
|---|-----------|---------------|------------------|
| 1 | Safety | 30% | Token contract safety (mint/freeze authority, LP burn, holder concentration, sell simulation) |
| 2 | Entity | 15% | Deployer wallet risk (graph analysis, Sybil clusters, hops to known ruggers) |
| 3 | Social | 20% | Narrative momentum (meme lifecycle phase, token count trends, velocity) |
| 4 | OnChain | 20% | On-chain activity (liquidity flow patterns, copy-trade signals) |
| 5 | Timing | 15% | Entry timing (pool age, narrative alignment, copy-trade tier) |

### Scoring Formula

```
Total = (Safety × w_safety) + (Entity × w_entity) + (Social × w_social)
      + (OnChain × w_onchain) + (Timing × w_timing) + CorrelationBonus
```

Where `w_*` are adaptive weights (default: 0.30, 0.15, 0.20, 0.20, 0.15).

### Input Data

```go
type ScoringInput struct {
    Analysis        TokenAnalysis              // from Token Analyzer
    EntityReport    *graph.EntityReport        // from Entity Graph Engine
    SellSim         *SellSimResult             // from Sell Simulator
    LiquidityFlow   *liquidity.LiquidityFlow   // from Liquidity Flow Analyzer [v3.2]
    NarrativeState  *narrative.NarrativeState   // from Narrative Engine [v3.2]
    CrossTokenState *correlation.CrossTokenState // from Correlation Detector [v3.2]
    HoneypotMatch   *honeypot.Signature         // from Honeypot Tracker [v3.2]
    CopyTradeSignal *copytrade.CopySignal       // from Copy-Trade Tracker [v3.2]
}
```

### Output

```go
type TokenScore struct {
    Total            int     // 0-100, weighted composite
    Safety           int     // 0-100
    Entity           int     // 0-100
    Social           int     // 0-100
    OnChain          int     // 0-100
    Timing           int     // 0-100
    CorrelationBonus int     // -20 to +20
    CorrelationType  string  // bonus source
    Recommendation   string  // STRONG_BUY | BUY | WAIT | SKIP | RUG | HONEYPOT
    InstantKill      bool    // if true, score=0
    KillReason       string  // reason for instant kill
    Reasons          []string // human-readable scoring reasons
}
```

### Recommendations

| Score Range | Recommendation | Action |
|-------------|---------------|--------|
| >= 75 | STRONG_BUY | Auto-snipe |
| 60-74 | BUY | Auto-snipe (if enabled) |
| 45-59 | WAIT | Monitor, don't buy |
| < 45 | SKIP | Reject |
| InstantKill | RUG / HONEYPOT | Reject immediately |

---

## 2. v3.2 Instant-Kill Conditions

Before dimension scoring, three instant-kill checks run. If any triggers, score is set to 0 and token is rejected.

### Honeypot High Confidence
```
IF HoneypotMatch != nil AND HoneypotMatch.Confidence >= 0.7:
    InstantKill = true
    KillReason = "HONEYPOT_HIGH_CONFIDENCE"
    Recommendation = "HONEYPOT"
```

### Rug Precursor
```
IF LiquidityFlow != nil AND LiquidityFlow.Pattern == RUG_PRECURSOR:
    InstantKill = true
    KillReason = "RUG_PRECURSOR"
    Recommendation = "RUG"
```

### Cross-Token Critical
```
IF CrossTokenState != nil AND CrossTokenState.RiskLevel == CRITICAL:
    InstantKill = true
    KillReason = "CROSS_TOKEN_CRITICAL"
    Recommendation = "RUG"
```

---

## 3. v3.2 Score Adjustments

After base dimension scoring, v3.2 modules adjust individual dimension scores.

### Liquidity Flow Impact (affects Safety)
- HEALTHY_GROWTH: +10 safety
- SLOW_BLEED: -15 safety
- ARTIFICIAL_PUMP: -10 safety

### Narrative Phase Impact (affects Timing)
- EMERGING + ACCELERATING: +15 timing
- GROWING: +5 timing
- DECLINING: -10 timing
- DEAD: -20 timing

### Cross-Token Correlation Impact (affects Entity)
- HIGH risk: -10 entity
- MEDIUM risk: -5 entity
- Also sets CorrelationBonus (negative)

### Honeypot Partial Match (affects Safety)
- Confidence 0.3-0.7: safety penalty proportional to confidence
- Below 0.3: no impact (too uncertain)

### Copy-Trade Signal Impact (affects OnChain)
- FIRST_MOVE by WHALE/SMART_MONEY: +15 onchain
- ACCUMULATE: +10 onchain
- DUMP: -20 onchain

---

## 4. Adaptive Scoring Weights

### How It Works

The AdaptiveWeightEngine learns from trade outcomes:

1. **Record**: On position close, `RecordOutcome(TradeOutcome)` saves the entry dimension scores + PnL
2. **Analyze**: Every 30 minutes, correlate each dimension's score with win/loss outcomes
3. **Adjust**: Increase weight for dimensions that predict winners, decrease for dimensions that don't
4. **Apply**: Update scorer weights via `SetWeights()` (thread-safe)

### Configuration

```go
AdaptiveWeightConfig{
    LearningRate:    0.05,  // max weight change per recalculation
    WindowSize:      50,    // number of recent trades to analyze
    RecalcInterval:  30min, // how often to recalculate
}
```

### TradeOutcome

```go
type TradeOutcome struct {
    Score      TokenScore  // full 5D score at entry
    PnLPct     float64     // profit/loss percentage
    IsWin      bool        // PnL > 0
    IsRug      bool        // detected as rug/scam
    Duration   time.Duration
    RecordedAt time.Time
}
```

### Constraints
- Weights always sum to 1.0
- Minimum weight per dimension: 0.05
- Maximum weight per dimension: 0.50
- Changes capped by LearningRate per cycle

---

## 5. Execution Engine

### Sniper Engine Decision Flow

```
OnDiscovery(ctx, analysis):
    │
    ├── Check: ctrl.paused or ctrl.killed → SKIP
    ├── Check: dailySpentSOL >= MaxDailySpendSOL → SKIP
    ├── Check: dailyLossSOL >= MaxDailyLossSOL → SKIP
    ├── Check: len(positions) >= MaxPositions → SKIP
    ├── Check: analysis.SafetyScore < MinSafetyScore → SKIP
    ├── Check: TokenScore.Recommendation not in (STRONG_BUY, BUY) → SKIP
    └── All pass → ExecuteBuy()
```

### Buy Execution

```
ExecuteBuy(ctx, analysis):
    1. Get Jupiter V6 quote
       - inputMint: SOL (WSOL)
       - outputMint: token
       - amount: MaxBuySOL in lamports
       - slippageBps: from config (default 200)

    2. Get swap TX from Jupiter
       - dynamicComputeUnitLimit: true
       - dynamicSlippage: true
       - priorityFee: dynamic estimation

    3. If UseJito:
       - Add Jito tip instruction to TX
       - Tip amount: dynamic (config JitoTipSOL as base)

    4. Sign TX with wallet private key

    5. Submit:
       - If UseJito: via Jito bundle endpoint
       - Else: via standard RPC sendTransaction

    6. Wait for confirmation (finalized)

    7. Create Position:
       - ID, TokenMint, PoolAddress, DEX
       - EntryPriceUSD, AmountToken, CostSOL
       - SafetyScore, BuySignature
       - Status: OPEN

    8. Start CSM goroutine for this position

    9. Wire CSM with EntityGraph + HoneypotTracker (v3.2)

    10. Cache entry scores for adaptive weights
```

### Sell Execution

```
ExecuteSell(ctx, position, reason):
    1. Get Jupiter V6 quote (reverse: token → SOL)

    2. Build sell TX

    3. Sign + submit (with retry up to SellRetries)

    4. Update position:
       - Status: CLOSED
       - CloseReason: reason
       - PnLPct: calculated from entry/exit
       - SellSignature: TX hash

    5. Trigger onPositionClose callback
```

### Position Sizing

Currently fixed at `MaxBuySOL` per trade (configured in YAML, default 0.1 SOL).

Global safety limits:
- `MaxDailySpendSOL` — total SOL spent per day
- `MaxDailyLossSOL` — max cumulative loss per day
- `MaxPositions` — max concurrent open positions (default 5)

---

## 6. CSM — Continuous Safety Monitor

### Purpose

Post-buy protection. A goroutine runs for each open position, continuously re-evaluating safety.

### Checks

| Check | Interval | Panic Condition |
|-------|----------|-----------------|
| Sell Simulation | 30s | CanSell = false OR tax > MaxSellTaxPct |
| Holder Exodus | 30s | Top-N holders lost > HolderExodusPanicPct |
| Liquidity Health | 30s | Liquidity dropped > LiquidityDropPanicPct |
| Entity Re-Check | 60s | Deployer risk >= EntityRiskPanicScore |

### Panic Sell

When any check triggers panic:
1. Force sell entire position immediately
2. Set CloseReason to specific CSM reason
3. Position Close triggers rug feedback loop

### CSM Close Reasons

| Reason | Description |
|--------|-------------|
| CSM_SELL_SIM_FAILED | Sell simulation detected honeypot post-buy |
| CSM_LIQUIDITY_PANIC | Liquidity dropped below panic threshold |
| CSM_HOLDER_EXODUS | Top holders dumping massively |
| CSM_ENTITY_SERIAL_RUGGER | Deployer flagged as serial rugger |

---

## 7. Exit Engine

### Multi-Level Take Profit

The exit engine supports 4 configurable TP levels with partial sells:

```go
ExitConfig{
    TPLevels: []TPLevel{
        {Multiplier: 1.5, SellPct: 25},  // sell 25% at 1.5x
        {Multiplier: 2.0, SellPct: 25},  // sell 25% at 2x
        {Multiplier: 3.0, SellPct: 25},  // sell 25% at 3x
        {Multiplier: 6.0, SellPct: 100}, // sell remaining at 6x
    },
    StopLossPct:    50,  // -50% from entry
    TrailingStop: TrailingStopConfig{
        Enabled: true,
        Pct:     20,     // 20% from highest
    },
    TimedExits: []TimedExit{
        {AfterMinutes: 240,  SellPct: 50},  // 4h: sell 50%
        {AfterMinutes: 720,  SellPct: 75},  // 12h: sell 75%
        {AfterMinutes: 1440, SellPct: 100}, // 24h: sell all
    },
    PanicLiqDropPct:   50,
    PanicWhaleSellSOL: 100,
}
```

### Exit Priority (highest to lowest)

1. **Panic** — CSM triggers (immediate)
2. **Stop Loss** — price below threshold (immediate)
3. **Take Profit** — price above TP level (partial sell)
4. **Trailing Stop** — price dropped from high (close all)
5. **Timed Exit** — holding too long (partial/full sell)

---

## 8. Rug Feedback Loop

When a position closes as a rug, the system learns:

### Honeypot Learning
```go
honeypotTracker.RecordRug(honeypot.RugSample{
    TokenAddress:  mint,
    Chain:         "solana",
    DeployerAddr:  deployer,
    ContractData:  contractBytes,
    RugType:       "RUG_DETECTED",
    DetectedBy:    closeReason,
    Timestamp:     time.Now(),
})
```
- Extracts new honeypot signatures from contract data
- Updates pattern database
- Triggers retroactive scan of open positions

### Correlation Learning
```go
correlationDetector.MarkRugged(clusterID, mint)
```
- Updates cluster risk level (may promote to CRITICAL)
- Future tokens from same cluster get penalized or instant-killed

### Adaptive Weight Update
```go
adaptiveWeights.RecordOutcome(TradeOutcome{
    Score:  entryScore,   // full 5D scores at time of entry
    PnLPct: position.PnLPct,
    IsWin:  false,
    IsRug:  true,
})
```
- Feeds into weight recalculation
- If safety dimension was high but token rugged: safety weight increases
- If entity dimension was low and token rugged: entity weight increases

---

*Last Updated: 2026-02-23*
