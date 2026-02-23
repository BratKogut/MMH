# NEXUS Memecoin Hunter v3.2 — Architecture & Pipeline

**Language:** Go 1.24 | **Chain:** Solana | **SAFETY > PROFIT > SPEED**

---

## 1. High-Level Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                    NEXUS MEMECOIN HUNTER v3.2                    │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  POOL DISCOVERY (WebSocket)                                     │
│  ┌──────────────────────────────────────────────────┐           │
│  │ Raydium V4 (675kPX9...)  │  PumpFun (6EF8rr...)  │           │
│  │ logsSubscribe            │  logsSubscribe         │           │
│  └────────────┬─────────────┴────────────┬──────────┘           │
│               └──────────┬───────────────┘                      │
│                          ▼                                      │
│  ┌──────────────────────────────────────────────────┐           │
│  │ L0 SANITIZER (<10ms, zero external calls)         │           │
│  └──────────────────────┬───────────────────────────┘           │
│                          ▼                                      │
│  ┌──────────────────────────────────────────────────┐           │
│  │ ANALYSIS PIPELINE                                 │           │
│  │ TokenAnalyzer → SellSim → EntityGraph → v3.2     │           │
│  └──────────────────────┬───────────────────────────┘           │
│                          ▼                                      │
│  ┌──────────────────────────────────────────────────┐           │
│  │ 5D SCORER (adaptive weights)                      │           │
│  │ Safety(30) + Entity(15) + Social(20)              │           │
│  │ + OnChain(20) + Timing(15) + CorrelationBonus     │           │
│  └──────────────────────┬───────────────────────────┘           │
│                          ▼                                      │
│  ┌──────────────────────────────────────────────────┐           │
│  │ SNIPER ENGINE                                     │           │
│  │ Budget check → Jupiter V6 → Jito bundle → Confirm │           │
│  └──────────────────────┬───────────────────────────┘           │
│                          ▼                                      │
│  ┌──────────────┬───────────────────────────────────┐           │
│  │ CSM          │ EXIT ENGINE                        │           │
│  │ (per-pos     │ TP: 1.5x/2x/3x/6x                │           │
│  │  goroutine)  │ SL: -50% | Trail: 20%             │           │
│  │ SellSim      │ Timed: 4h/12h/24h                 │           │
│  │ Holders      │ Panic: CSM triggers                │           │
│  │ Liquidity    │                                    │           │
│  │ Entity       │                                    │           │
│  └──────────────┴──────────┬────────────────────────┘           │
│                             ▼                                   │
│  ┌──────────────────────────────────────────────────┐           │
│  │ OUTCOME FEED → Adaptive Weights + Rug Learning   │           │
│  └──────────────────────────────────────────────────┘           │
│                                                                 │
│  HTTP API (:9092)                                               │
│  /health │ /stats │ /positions │ /control/* │ /copytrade/*     │
│                                                                 │
│  Prometheus Metrics (:9092/metrics)                             │
└─────────────────────────────────────────────────────────────────┘
```

---

## 2. Solana Integration

### 2.1 Endpoints

```
Solana:
├── RPC:         Helius mainnet (https://mainnet.helius-rpc.com/?api-key={KEY})
├── WebSocket:   Helius WS (wss://mainnet.helius-rpc.com/?api-key={KEY})
├── Jupiter V6:
│   ├── Quote:   https://quote-api.jup.ag/v6/quote
│   ├── Swap:    https://quote-api.jup.ag/v6/swap
│   └── Price:   https://price.jup.ag/v6/price
├── Jito MEV:    https://mainnet.block-engine.jito.wtf/api/v1/transactions
└── Programs:
    ├── Raydium V4:  675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8
    ├── PumpFun:     6EF8rrecthR5Dkzon8Nwu78hRvfCKubJ14M5uBEwF6P
    └── SOL (WSOL):  So11111111111111111111111111111111111111112
```

### 2.2 RPC Client (internal/solana/)

**StubRPCClient** — for tests and dry-run:
- Returns mock data for all RPC calls
- Configurable safety scores and holder data
- Simulates TX submission (always succeeds)

**LiveRPCClient** — for mainnet:
- `GetAccountInfo(mint)` — fetch mint/freeze authority
- `GetTokenLargestAccounts(mint)` — top holders
- `SendTransaction(rawTx)` — submit signed TX
- Configurable rate limiting (RateLimitRPS from config)

### 2.3 WebSocket Monitor (internal/solana/)

Subscribes to Raydium and PumpFun program logs:

```go
// logsSubscribe for Raydium V4 pool creation
{
    "jsonrpc": "2.0", "id": 1,
    "method": "logsSubscribe",
    "params": [
        {"mentions": ["675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8"]},
        {"commitment": "confirmed"}
    ]
}

// Detect: "initialize2" in logs = NEW RAYDIUM POOL
```

Emits `PoolDiscovery` events with:
- PoolAddress, DEX, LiquidityUSD
- TokenMint, QuoteReserveSOL
- LatencyMs from detection

### 2.4 Jupiter Adapter (internal/adapters/jupiter/)

**V6 API Client:**
- `GetQuote(inputMint, outputMint, amount, slippageBps)` — get swap quote
- `GetSwapTx(quote, userPubkey)` — get serialized swap TX
- `GetPrice(mints)` — bulk price lookup

**Swap flow:**
1. Get quote from Jupiter V6
2. Set dynamic slippage (from config SlippageBps)
3. Set priority fees (dynamic or configured)
4. Build swap TX with Jito tip (if UseJito=true)
5. Sign with wallet private key
6. Submit via Jito bundle or standard RPC
7. Wait for confirmation (finalized)

### 2.5 Jito Bundles (internal/solana/)

```go
// Jito bundle submission
POST https://mainnet.block-engine.jito.wtf/api/v1/transactions
{
    "jsonrpc": "2.0", "id": 1,
    "method": "sendTransaction",
    "params": ["<BASE64_TX>"]
}
```

Benefits: Private mempool, no sandwich attacks.
Dynamic tips: estimated from recent block data.

### 2.6 Priority Fees (internal/solana/)

```go
// Percentile-based priority fee estimation
// Fetches recent priority fees from RPC
// Returns fee at configured percentile (default: p75)
// Capped at configured maximum
```

---

## 3. Pipeline Stages

### 3.1 L0 Sanitizer (scanner/sanitizer.go)

**Purpose:** Instant reject of obviously bad tokens. < 10ms, zero external calls.

**Checks:**
- MinLiquidityUSD (default 500 USD)
- MinQuoteReserveSOL (default 1.0 SOL)
- Deployer history (spam filter — N launches in M minutes)
- DEX whitelist (only configured DEXes)
- MaxTokenAgeMinutes (reject old pools)

**Result:** PASS or DROP with reason.

### 3.2 Token Analyzer (scanner/analyzer.go)

**Purpose:** Deep safety analysis with external API calls.

**Checks:**
1. **Mint Authority** — null = safe, present = can mint infinite tokens
2. **Freeze Authority** — null = safe, present = can freeze wallets
3. **LP Burn/Lock** — percentage of LP tokens burned (higher = safer)
4. **Holder Concentration** — top-10 holder percentage (lower = safer)
5. **Single Holder Max** — largest single holder percentage
6. **Liquidity Depth** — USD value of pool liquidity

**Output:** `TokenAnalysis` with SafetyScore (0-100), Verdict, Flags[], HolderAnalysis.

### 3.3 Sell Simulator (scanner/sellsim.go)

**Purpose:** Pre-buy honeypot detection by simulating a sell.

**Process:**
1. Simulate selling MaxBuySOL worth of tokens
2. Check if sell would succeed (CanSell)
3. Estimate sell tax percentage
4. Detect slippage anomalies

**Output:** `SellSimResult` with CanSell, EstimatedTaxPct, SlippagePct.

### 3.4 Entity Graph Engine (graph/engine.go)

**Purpose:** Deployer risk assessment through wallet relationship analysis.

**Process:**
1. BFS traversal from deployer wallet (depth 2)
2. Build adjacency graph (nodes + edges)
3. Label propagation (SERIAL_RUGGER, CLEAN, CEX_HOT_WALLET, etc.)
4. Sybil detection (cluster size threshold)
5. Measure hops to known ruggers/insiders
6. Identify seed funder

**Features:**
- Max 500K nodes in memory
- CEX hot wallet database (Binance, Coinbase, Kraken, etc.)
- Persistence: periodic snapshots to disk + restore on startup
- Thread-safe concurrent access

**Output:** `EntityReport` with RiskScore (0-100), Labels[], ClusterSize, HopsToRugger, SeedFunder.

### 3.5 v3.2 Analysis Modules

#### Liquidity Flow Direction (liquidity/flow.go)

Tracks net liquidity flow in 5min and 30min windows.

**Patterns:**
- HEALTHY_GROWTH — steady organic inflow
- ARTIFICIAL_PUMP — spike from single source
- RUG_PRECURSOR — outflow acceleration (instant kill)
- SLOW_BLEED — gradual outflow
- STABLE — balanced flow

**Velocity:** ACCELERATING, DECELERATING, STABLE

#### Narrative Momentum (narrative/engine.go)

Tracks meme narrative lifecycle across tokens.

**Default narratives:** AI_AGENTS, POLITICAL, ANIMAL_MEME, CELEBRITY, DEFI_META, GAMING, RWA

**Phases:** EMERGING -> GROWING -> PEAK -> DECLINING -> DEAD

**Metrics:** TokenCount1h/6h/24h, AvgPerformance, BestPerformer, Velocity

#### Honeypot Evolution Tracker (honeypot/evolution.go)

Learns honeypot patterns from rug samples.

**Process:**
1. SHA256 fingerprint contract data
2. Match against known signatures
3. Learn from rug samples (RecordRug)
4. Retroactive scan on new signature detection

**Signature types:** bytecode, instruction, behavior

#### Cross-Token Correlation (correlation/crosstoken.go)

Detects coordinated token launches by same deployer/funder.

**Patterns:**
- ROTATION — dump token A, launch token B
- DISTRACTION — launch decoy while rugpulling main
- SERIAL — repeated launch-and-rug pattern
- NONE — independent tokens

**Risk levels:** LOW -> MEDIUM -> HIGH -> CRITICAL (instant kill)

#### Copy-Trade Intelligence (copytrade/tracker.go)

Tracks whale and smart money wallets.

**Wallet tiers:** WHALE, SMART_MONEY, KOL, INSIDER, FRESH

**Signals:** BUY, SELL, ACCUMULATE, DUMP, FIRST_MOVE

**Management:** Config-based loading + HTTP API (GET/POST/DELETE /copytrade/wallets)

### 3.6 5-Dimensional Scorer (scanner/scoring.go)

**Dimensions:**
| Dimension | Weight | Source |
|-----------|--------|--------|
| Safety | 30% | TokenAnalyzer + SellSim |
| Entity | 15% | EntityGraph |
| Social | 20% | NarrativeMomentum |
| OnChain | 20% | LiquidityFlow + CopyTrade |
| Timing | 15% | PoolAge + Narrative + CopyTrade |

**v3.2 Instant-Kill Conditions (score = 0):**
- Honeypot match confidence >= 0.7
- Liquidity flow pattern = RUG_PRECURSOR
- Cross-token risk level = CRITICAL

**v3.2 Score Adjustments:**
- Liquidity flow: HEALTHY_GROWTH boosts safety, SLOW_BLEED penalizes
- Narrative: EMERGING boosts timing, DEAD penalizes
- Correlation: penalty proportional to risk level
- Honeypot: partial penalty for low-confidence matches
- Copy-trade: FIRST_MOVE boosts onchain, DUMP penalizes

**Adaptive Weights:**
- Recalculated every 30 minutes from TradeOutcome data
- Configurable learning rate (default 0.05)
- Window size (default 50 recent trades)
- Thread-safe via sync.RWMutex

**Recommendations:** STRONG_BUY | BUY | WAIT | SKIP | RUG | HONEYPOT

### 3.7 Sniper Engine (sniper/sniper.go)

**Pre-buy checks:**
1. Daily spend budget (MaxDailySpendSOL)
2. Daily loss limit (MaxDailyLossSOL)
3. Position count limit (MaxPositions)
4. SafetyScore >= MinSafetyScore
5. Recommendation check

**Buy execution:**
1. Jupiter V6 quote
2. Slippage calculation
3. Priority fee estimation (dynamic)
4. Jito tip calculation (dynamic, if enabled)
5. TX build + sign
6. Submit via Jito or standard RPC
7. Wait for confirmation
8. Create Position record

### 3.8 CSM — Continuous Safety Monitor (sniper/csm.go)

Per-position goroutine, runs until position closes.

**Checks (every 30 seconds):**
1. **Sell Simulation** — re-simulate sell, check for honeypot changes
2. **Holder Exodus** — monitor top-10 holders, panic on mass dump
3. **Liquidity Health** — check for sudden liquidity drops
4. **Entity Re-Check** — re-query deployer, catch new rug labels

**Config:**
```go
CSMConfig{
    SellSimIntervalS:       30,
    PriceCheckIntervalS:    5,
    LiquidityDropPanicPct:  50,
    LiquidityDropWarnPct:   30,
    MaxSellTaxPct:          10,
    HolderExodusEnabled:    true,
    HolderCheckTopN:        10,
    HolderExodusPanicPct:   30,
    EntityReCheckEnabled:   true,
    EntityReCheckIntervalS: 60,
    EntityRiskPanicScore:   85,
}
```

### 3.9 Exit Engine (sniper/exits.go)

**Take Profit (4 levels, partial sells):**
| Level | Multiplier | Sell % |
|-------|-----------|--------|
| 1 | 1.5x | 25% |
| 2 | 2.0x | 25% |
| 3 | 3.0x | 25% |
| 4 | 6.0x | 100% |

**Stop Loss:** -50% from entry price

**Trailing Stop:** 20% from highest observed price (if enabled)

**Timed Exits:**
| After | Sell % |
|-------|--------|
| 4 hours | 50% |
| 12 hours | 75% |
| 24 hours | 100% |

**Panic Exits:** Triggered by CSM (sell sim failed, liquidity drop, holder exodus, entity re-check)

---

## 4. Self-Learning Feedback Loop

On every position close:

```
Position Close
    │
    ▼
Determine if RUG:
  CloseReason ∈ {RUG, PANIC_SELL, CSM_SELL_SIM_FAILED,
                  CSM_LIQUIDITY_PANIC, CSM_HOLDER_EXODUS,
                  CSM_ENTITY_SERIAL_RUGGER}
    │
    ▼
Create TradeOutcome:
  - Score (full TokenScore at entry, from entryInfo cache)
  - PnLPct
  - IsWin (PnL > 0)
  - IsRug
    │
    ├──▶ AdaptiveWeights.RecordOutcome(outcome)
    │    └── Adjusts 5D weights based on dimension-PnL correlation
    │
    └──▶ If isRug:
         ├── honeypotTracker.RecordRug(sample)
         │   └── Learn new honeypot signatures
         │
         └── correlationDetector.MarkRugged(clusterID, mint)
             └── Update cluster risk level
```

---

## 5. HTTP Control Plane

**Port:** PrometheusPort + 2 (default 9092)

### Health & Stats
- `GET /health` — `{status, dry_run, paused, killed}`
- `GET /stats` — combined stats from all modules
- `GET /positions` — all positions (open + closed)
- `GET /positions/open` — currently open positions

### Control
- `POST /control/pause` — soft pause (stop new entries, manage existing)
- `POST /control/resume` — resume from pause
- `POST /control/kill` — hard kill (force close all, halt)
- `GET /control/status` — `{paused, killed, dry_run, instance_id, open_positions}`

### Copy-Trade Management
- `GET /copytrade/wallets` — list tracked wallets
- `POST /copytrade/wallets` — add wallet `{address, tier, label}`
- `DELETE /copytrade/wallets` — remove wallet `{address}`

---

## 6. Periodic Maintenance

| Interval | Task |
|----------|------|
| 30 seconds | Log stats |
| 5 minutes | NarrativeEngine.Refresh() |
| 5 minutes | EntityGraph.SnapshotLoop() |
| 10 minutes | Sanitizer.CleanupDeployerHistory() |
| 15 minutes | FlowAnalyzer.Cleanup(1h) |
| 15 minutes | CorrelationDetector.Cleanup(6h) |
| 15 minutes | CopytradeTracker.Cleanup(30m) |
| 30 minutes | AdaptiveWeights.Recalculate() → Scorer.SetWeights() |

---

*Last Updated: 2026-02-23*
