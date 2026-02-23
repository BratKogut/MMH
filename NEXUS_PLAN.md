# NEXUS — AI-Native Trading & Intelligence Platform
## Plan Architektoniczny v3.2
### Autorzy: Human (spec/wizja) + Claude (architektura/implementacja)

---

## 0. Filozofia — 5 zasad, ktore nie podlegaja negocjacji

```
1. LLM NIE HANDLUJE.
   LLM = ekstrakcja, klasyfikacja, synteza.
   Trading core jest deterministyczny i audytowalny.

2. JEDNA PRAWDA DANYCH.
   Te same eventy dla backtest / paper / live.
   Replay to tryb dzialania, nie feature.

3. KOSZTY SA CZESCIA STRATEGII.
   fees + slippage + infra + LLM — per strategia, per trade.
   Strategia z +5% PnL i +6% kosztow to strata. Zawsze.

4. SAFETY > PROFIT > SPEED.
   Twarde limity strat dzialaja ZAWSZE.
   Zaden modul nie moze ich obejsc.
   Kill switch dziala nawet gdy Kafka lezy.

5. SELEKTYWNA INTELIGENCJA.
   Nie mielimy internetu. Pytamy o konkretne rzeczy,
   z budzetem, TTL i mierzalnym ROI.
```

---

## 1. Architektura — widok z lotu ptaka

```
┌─────────────────────────────────────────────────────────────────┐
│                        NEXUS PLATFORM                           │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐                     │
│  │ Kraken   │  │ Binance  │  │ Jupiter  │                     │
│  │ Adapter  │  │ Adapter  │  │ (Solana) │                     │
│  └────┬─────┘  └────┬─────┘  └────┬─────┘                     │
│       │              │              │                           │
│  ─────┴──────────────┴──────────────┴───────────────────────── │
│                    RedPanda (Kafka API)                          │
│         md.* │ exec.* │ intel.* │ signals.* │ ops.*             │
│  ──────────────────────────────────────────────────────────────  │
│       │              │              │              │             │
│  ┌────┴─────┐  ┌─────┴────┐  ┌─────┴────┐  ┌─────┴────┐       │
│  │ Feature  │  │ Strategy │  │  Intel   │  │  Risk    │       │
│  │ Engine   │  │ Runtime  │  │ Pipeline │  │  Engine  │       │
│  └────┬─────┘  └────┬─────┘  └────┬─────┘  └────┬─────┘       │
│       │              │              │              │             │
│  ┌────┴──────────────┴──────────────┴──────────────┴────┐       │
│  │              Execution Engine (Go)                    │       │
│  │    Order SM │ Position Mgr │ Risk Gates │ Audit      │       │
│  └──────────────────────┬───────────────────────────────┘       │
│                         │                                       │
│  ┌──────────────────────┴───────────────────────────────┐       │
│  │              ClickHouse + Event Store                 │       │
│  │    Ticks │ Trades │ OHLCV │ Fills │ Intel │ Audit    │       │
│  └──────────────────────────────────────────────────────┘       │
│                                                                 │
│  ┌──────────────────────────────────────────────────────┐       │
│  │      MEMECOIN HUNTER v3.2 (nexus-hunter)              │       │
│  │                                                        │       │
│  │  Pool Discovery → L0 Sanitizer → Analyzer → SellSim   │       │
│  │  → EntityGraph → v3.2 Analysis → 5D Scorer → Sniper   │       │
│  │  → CSM + ExitEngine → Outcome Feed → Adaptive Recalc  │       │
│  └──────────────────────────────────────────────────────┘       │
│                                                                 │
│  ┌──────────────────────────────────────────────────────┐       │
│  │         Observability (Prometheus/Grafana)             │       │
│  └──────────────────────────────────────────────────────┘       │
└─────────────────────────────────────────────────────────────────┘
```

---

## 2. Tech Stack

### Warstwa Core (Go 1.24)
```
Jezyk:        Go 1.24
Uzasadnienie: Goroutines idealne dla concurrent exchange connections.
              Kompilacja do single binary. Deploy = scp.
              Silne typowanie, 3x szybszy dev niz Rust.
              Ekosystem: shopspring/decimal, gorilla/websocket, franz-go.

Statystyki:   ~34,000 LOC, 497 testow, 109 plikow Go
```

### Warstwa Event Bus
```
RedPanda:     Kafka-compatible API, zero JVM, single binary
              Klient: franz-go (pure Go, zero-alloc)
              Topic naming: <domain>.<category>.<entity>.<variant>
              Retencja: od 72h (orderbook) do 8760h (audit)
```

### Warstwa Analytics
```
ClickHouse:   OLAP, kolumnowy, partycjonowanie po dniu/symbolu.
              ReplacingMergeTree dla dedup.
              Materialized views dla OHLCV aggregation.
              Batch writer + Intel writer.
```

### Warstwa Blockchain (Solana)
```
RPC:          Helius mainnet (Business plan) — getAccountInfo, getTokenLargestAccounts
WebSocket:    Helius WS — logsSubscribe (Raydium/PumpFun pool creation)
DEX:          Jupiter V6 API (quote/swap/price)
MEV:          Jito Bundles (private mempool, dynamic tips)
Priority:     Dynamic priority fees (percentile-based estimation)
```

---

## 3. Moduly — stan implementacji v3.2

### 3.1 Exchange Adapters (Go) — DONE
```
Implementacje:
  - KrakenAdapter (CEX, fiat pairs, WS v2) [S1]
  - BinanceAdapter (CEX, crypto pairs, WS) [S3]
  - JupiterAdapter (DEX, Solana, Jupiter V6 API) [S5]
  - Unified ExchangeAdapter interface
```

### 3.2 Feature Engine (Go) — DONE
```
Features:
  - VWAP, TWAP
  - Orderbook imbalance (bid/ask ratio)
  - Volatility (realized, Parkinson)
  - Spread (quoted, effective)
  - Momentum (ROC, RSI na eventach)
```

### 3.3 Regime Detector (Go) — DONE
```
Rezimy: TRENDING_UP/DOWN, MEAN_REVERTING, HIGH/LOW_VOLATILITY, BREAKOUT
Metoda: HMM (2-3 stany) + threshold-based fallback
```

### 3.4 Strategy Runtime (Go) — DONE
```
Interface: OnEvent, OnTimer, OnIntel, SnapshotState, RestoreState
Conflict Resolution: priority = capital_allocation_weight x confidence
```

### 3.5 Risk Engine (Go) — DONE
```
Pre-trade checks, position limits, daily/weekly loss limits
Kill switches: global + per-strategy + per-exchange (in-process atomic)
Circuit breakers: feed lag, error rate, reject spike
Cooldown: N consecutive losses → cooldown T minut
```

### 3.6 Execution Engine (Go) — DONE
```
Order State Machine: CREATED → SUBMITTED → ACKED → PARTIAL → FILLED
Paper Broker for dry-run mode
Slippage tracking: expected_price vs actual_fill_price
```

### 3.7 Intel Pipeline (Go) — DONE
```
Query Brain (orchestrator) + Trigger Engine + Budget Manager
Circuit breakers per LLM provider
ROI tracking: intel event → trade linkage
ClickHouse intel writer
```

### 3.8 Memecoin Hunter Pipeline — DONE (v3.2)

```
Pelny pipeline (nexus-hunter):

Pool Discovery (WebSocket)
    │
    ▼
L0 Sanitizer (<10ms, zero external calls)
├── MinLiquidityUSD (500)
├── MinQuoteReserveSOL (1.0)
├── Deployer spam filter
├── DEX whitelist
└── PoolAge check
    │
    ▼
Token Analyzer
├── Mint/freeze authority check
├── LP burn/lock verification
├── Holder concentration (top-10)
└── Liquidity depth check
    │
    ▼
Sell Simulator
├── Pre-buy sell simulation
├── Tax estimation
└── Honeypot detection
    │
    ▼
Entity Graph Engine
├── BFS wallet clustering (depth 2)
├── Label propagation (SERIAL_RUGGER, CLEAN, CEX)
├── Sybil detection (cluster size)
├── HopsToRugger/Insider
├── SeedFunder identification
└── CEX hot wallet exclusion
    │
    ▼
v3.2 Advanced Analysis
├── Liquidity Flow Direction
│   └── Patterns: HEALTHY_GROWTH, ARTIFICIAL_PUMP, RUG_PRECURSOR, SLOW_BLEED
├── Narrative Momentum Engine
│   └── Phases: EMERGING → GROWING → PEAK → DECLINING → DEAD
├── Honeypot Evolution Tracker
│   └── SHA256 fingerprinting, learns from rug samples
├── Cross-Token Correlation Detector
│   └── Patterns: ROTATION, DISTRACTION, SERIAL (risk: LOW→CRITICAL)
└── Copy-Trade Intelligence
    └── Signals: BUY, SELL, ACCUMULATE, DUMP, FIRST_MOVE
    │
    ▼
5-Dimensional Scorer
├── Safety (30%): analyzer score + flags
├── Entity (15%): graph risk + cluster + hops
├── Social (20%): narrative phase + velocity
├── OnChain (20%): liquidity flow pattern
├── Timing (15%): pool age + narrative + copytrade
├── v3.2 Instant-Kill: honeypot>=0.7, rug_precursor, cross-token CRITICAL
├── v3.2 Score Adjustments: all module outputs
├── Adaptive Weights: recalculated every 30min from outcomes
└── Recommendations: STRONG_BUY | BUY | WAIT | SKIP | RUG | HONEYPOT
    │
    ▼
Sniper Engine
├── Budget checks (MaxDailySpendSOL, MaxDailyLossSOL)
├── Position limit check (MaxPositions)
├── Jupiter V6 quote + swap
├── Jito bundles (dynamic tips)
├── Dynamic priority fees
└── TX confirmation tracking
    │
    ▼
CSM (per-position goroutine, every 30s)
├── Sell simulation re-check
├── Holder exodus detection (top-10 dumping)
├── Liquidity health monitoring
├── Entity graph re-check (deployer risk change)
└── [Panic sell on any trigger]
    │
    ▼
Exit Engine (multi-level)
├── TP: 1.5x→25%, 2x→25%, 3x→25%, 6x→100%
├── SL: -50% from entry
├── Trailing: 20% from highest
├── Timed: 4h→50%, 12h→75%, 24h→100%
└── Panic: CSM triggers
    │
    ▼
Position Close → Outcome Feed
├── AdaptiveWeights.RecordOutcome(score + PnL)
├── If rug: honeypotTracker.RecordRug()
├── If rug: correlationDetector.MarkRugged()
└── Weight recalculation (30min cycle)
```

---

## 4. Delivery Plan — stan realizacji

### S0: Fundament — DONE
```
✅ Monorepo layout (Go modules)
✅ Kafka schemas (md/exec/intel/signals)
✅ ClickHouse tabele
✅ CI: lint + test + build
✅ Structured logging (JSON, trace_id)
✅ Configuration system (YAML + validation)
```

### S1: Market Data Pipeline — DONE
```
✅ KrakenAdapter (WS v2: ticks, trades, orderbook)
✅ Kafka producer (franz-go)
✅ ClickHouse ingest (batch writer)
✅ Data Quality Monitor (lag, gaps, jitter)
✅ Feature Engine v1 (VWAP, spread, volatility, momentum, imbalance)
✅ Prometheus metrics exporter
```

### S2: Trading Core + Risk + Paper — DONE
```
✅ Order State Machine (idempotent)
✅ Paper Broker
✅ Risk Engine v1 (pre-trade, exposure, daily loss, kill switches)
✅ Circuit breakers (feed lag, error rate)
✅ Execution Engine (routing, retry, partial fills)
✅ Position Manager
✅ Audit trail
```

### S3: Backtest + Research — DONE
```
✅ Replay Runner
✅ Backtest engine (ClickHouse → replay → metrics)
✅ Slippage + fee models
✅ Metrics: PnL, Sharpe, drawdown, turnover
✅ Regime Detector v1 (HMM, 3 stany)
✅ BinanceAdapter
```

### S4: Intel Pipeline — DONE
```
✅ Taxonomia topicow v1
✅ Query Brain (Go orchestrator)
✅ Trigger engine + Budget manager
✅ Circuit breakers per provider
✅ Intel service + ROI tracking
✅ ClickHouse intel writer
```

### S5: Memecoin Hunter Core — DONE
```
✅ Token Scanner + Analyzer
✅ Basic scoring
✅ Sniper engine + Jupiter V6
✅ Solana RPC client (stub + live)
✅ Jito bundle support
```

### S5.5: v3.1 Advanced Modules — DONE
```
✅ Entity Graph Engine (BFS, labels, Sybil, persistence)
✅ CEX hot wallet database
✅ 5D Scoring (Safety+Entity+Social+OnChain+Timing)
✅ Sell Simulator
✅ CSM (per-position goroutine)
✅ Exit Engine (4-level TP + trailing + timed)
✅ L0 Sanitizer
✅ Dynamic priority fees + Jito tips
✅ Control plane HTTP API
```

### S6: Live Solana Integration — DONE
```
✅ LiveRPCClient
✅ WebSocket monitor (logsSubscribe)
✅ Transaction builder (Jupiter swap TX)
✅ Jito bundle submission
✅ TX confirmation + priority fee estimation
```

### S6.5: v3.2 Advanced Analysis — DONE
```
✅ Liquidity Flow Direction Analysis (RUG_PRECURSOR)
✅ Narrative Momentum Engine (EMERGING→DEAD)
✅ Honeypot Evolution Tracker (SHA256, retro scan)
✅ Cross-Token Correlation (ROTATION/DISTRACTION/SERIAL)
✅ Copy-Trade Intelligence (whale/KOL tracking)
✅ Adaptive Scoring Weights (self-learning)
✅ Full pipeline integration (scoring + CSM + feedback loop)
✅ 12 v3.2 integration tests
✅ 497 tests passing, ~34,000 LOC
```

### S7: Paper Marathon — NASTEPNY
```
☐ Mainnet dry-run (2-4 tygodnie)
☐ Scoring accuracy measurement
☐ CSM false positive rate
☐ Adaptive weight convergence
☐ Weekly performance reports
☐ Runbooks + alerting
```

### S8: Live Trading — OCZEKUJE
```
☐ Live execution z malym kapitalem
☐ Twarde limity strat
☐ Tygodniowe review
☐ Stopniowe zwiekszanie kapitalu
```

---

## 5. Monorepo Structure (aktualna)

```
nexus/
├── go.mod
├── go.sum
├── Makefile
│
├── cmd/
│   ├── nexus-core/main.go      # Market data + Kafka + ClickHouse
│   ├── nexus-intel/main.go     # LLM intel pipeline
│   ├── nexus-hunter/main.go    # Memecoin hunter (28KB, pelny pipeline)
│   └── nexus-tools/            # CLI tools (placeholder)
│
├── internal/
│   ├── adapters/               # Exchange adapters (Kraken, Binance, Jupiter)
│   ├── audit/                  # Transaction audit trail
│   ├── backtest/               # Backtesting engine
│   ├── bus/                    # Kafka/RedPanda (franz-go)
│   ├── clickhouse/             # Analytics writer
│   ├── config/                 # YAML configuration
│   ├── copytrade/              # Copy-trade intelligence [v3.2]
│   ├── correlation/            # Cross-token correlation [v3.2]
│   ├── execution/              # Order engine + state machine
│   ├── features/               # Feature engineering
│   ├── graph/                  # Entity graph engine [v3.1]
│   ├── honeypot/               # Honeypot evolution tracker [v3.2]
│   ├── intel/                  # LLM intel pipeline
│   ├── liquidity/              # Liquidity flow analysis [v3.2]
│   ├── market/                 # Market data service
│   ├── narrative/              # Narrative momentum [v3.2]
│   ├── observability/          # Prometheus + health
│   ├── quality/                # Data quality monitor
│   ├── regime/                 # Market regime detector
│   ├── risk/                   # Risk engine
│   ├── scanner/                # Scanner + Analyzer + 5D Scoring + SellSim + Adaptive [v3.1+v3.2]
│   ├── sniper/                 # Sniper + CSM + ExitEngine [v3.1]
│   ├── solana/                 # RPC + WS + Jito + priority fees [v3.1]
│   └── strategy/               # Strategy runtime
│
└── docs/
    ├── 01-architecture-solana.md
    ├── 02-base-adapter.md       # (legacy spec, not implemented)
    ├── 03-scoring-execution.md
    └── API_REFERENCE.md
```

---

## 6. Co NIE JEST w tym planie (i dlaczego)

```
1. Multi-chain (Base, BSC, TON)
   → Solana-first. Inne chainy po udowodnieniu profitowosci na Solanie.

2. ML model training pipeline
   → Za wczesnie. Najpierw zbierz dane z Paper Marathon (S7).

3. Multi-region deployment
   → Overengineering na start. Jeden serwer blisko Solana validators.

4. HFT / sub-millisecond latency
   → Ten system targetuje sekundy, nie mikrosekundy.
   → Go daje < 1ms per decision.

5. Frontend / Dashboard UI
   → Grafana + HTTP API wystarczaja na start.
   → Custom UI po udowodnieniu profitowosci.
```

---

*NEXUS v3.2 — aktualizacja 2026-02-23*
