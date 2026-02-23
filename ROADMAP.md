# NEXUS Memecoin Hunter - Development Roadmap

## Overview

Development roadmap for the NEXUS Memecoin Hunter, a Solana-focused memecoin sniping engine built in Go 1.24. The project follows sprint-based delivery where each sprint ends with a working, tested system.

---

## Sprint Status

| Sprint | Name | Status | LOC | Tests |
|--------|------|--------|-----|-------|
| S0 | Foundation | DONE | ~3,000 | 45 |
| S1 | Market Data Pipeline | DONE | ~6,000 | 95 |
| S2 | Trading Core + Risk | DONE | ~10,000 | 150 |
| S3 | Backtest + Research | DONE | ~14,000 | 200 |
| S4 | Intel Pipeline | DONE | ~18,000 | 250 |
| S5 | Memecoin Hunter Core | DONE | ~22,000 | 350 |
| S5.5 | v3.1 Advanced Modules | DONE | ~26,000 | 420 |
| S6 | Live Solana Integration | DONE | ~30,000 | 470 |
| S6.5 | v3.2 Advanced Analysis | DONE | ~34,000 | 497 |
| **S7** | **Paper Marathon** | **NEXT** | - | - |
| S8 | Live Trading | PENDING | - | - |

---

## S0: Foundation (DONE)

**Goal**: Repo, schemas, CI, infrastructure running.

- [x] Go monorepo layout (cmd/ + internal/)
- [x] Kafka/RedPanda schemas (md/exec/intel/signals)
- [x] ClickHouse table definitions
- [x] Structured JSON logging with trace IDs
- [x] Configuration system (YAML + validation)
- [x] Makefile + build pipeline

---

## S1: Market Data Pipeline (DONE)

**Goal**: Stable data stream from exchanges.

- [x] Kraken adapter (WebSocket v2: ticks, trades, orderbook)
- [x] Kafka producer (franz-go)
- [x] ClickHouse batch writer
- [x] Data quality monitor (feed lag, gaps, jitter)
- [x] Feature engine v1 (VWAP, spread, volatility, momentum, imbalance)
- [x] Prometheus metrics exporter

---

## S2: Trading Core + Risk + Paper (DONE)

**Goal**: Deterministic execution path, paper trading works.

- [x] Order state machine (idempotent)
- [x] Paper broker (simulates fills)
- [x] Risk engine (pre-trade checks, exposure limits, daily loss limits)
- [x] Kill switches (global, per-strategy, in-process atomic)
- [x] Circuit breakers (feed lag, error rate)
- [x] Execution engine (order routing, retry, partial fills)
- [x] Position manager (per-strategy + global netting)
- [x] Audit trail (full trace from signal to fill)

---

## S3: Backtest + Research (DONE)

**Goal**: Same logic in backtest as in paper/live.

- [x] Replay runner (event store -> strategy runtime)
- [x] Backtest engine (ClickHouse -> event replay -> metrics)
- [x] Slippage + fee models (pluggable, per-exchange)
- [x] Metrics: PnL, Sharpe, drawdown, turnover
- [x] Regime detector v1 (HMM, 3 states)
- [x] Binance adapter (second CEX)

---

## S4: Intel Pipeline (DONE)

**Goal**: LLM-powered market intelligence with measurable ROI.

- [x] Intel taxonomy (25 topics with TTL and budgets)
- [x] Query Brain (Go orchestrator)
- [x] Trigger engine (market anomaly + schedule)
- [x] Budget manager (per-topic, per-provider, per-day)
- [x] Circuit breakers per LLM provider
- [x] Intel service with ROI tracking
- [x] ClickHouse intel writer

---

## S5: Memecoin Hunter Core (DONE)

**Goal**: End-to-end pool detection -> scoring -> sniping.

- [x] Token Scanner (WebSocket pool discovery)
- [x] Token Analyzer (safety checks, LP burn, holder concentration)
- [x] Basic scoring (safety + momentum)
- [x] Sniper engine (Jupiter V6 integration)
- [x] Position management (open/close lifecycle)
- [x] Solana RPC client (stub + live)
- [x] Jupiter adapter (quote/swap/price)
- [x] Jito bundle support

---

## S5.5: v3.1 Advanced Modules (DONE)

**Goal**: Advanced analysis modules for deeper risk assessment.

- [x] Entity Graph Engine (wallet clustering, BFS traversal, label propagation)
- [x] Sybil detection (cluster size thresholds)
- [x] CEX hot wallet database (exclude known exchanges)
- [x] 5-Dimensional Scoring (Safety + Entity + Social + OnChain + Timing)
- [x] Sell Simulator (pre-buy honeypot detection)
- [x] Continuous Safety Monitor (CSM) per position
- [x] Advanced Exit Engine (4-level TP + trailing stop + timed exits)
- [x] L0 Sanitizer (sub-10ms, zero external calls)
- [x] Dynamic priority fees
- [x] Dynamic Jito tips
- [x] Entity graph persistence (snapshot/restore)
- [x] Control plane HTTP API (/health, /stats, /positions, /control/*)

---

## S6: Live Solana Integration (DONE)

**Goal**: Working connection to Solana mainnet RPC/WebSocket.

- [x] LiveRPCClient (getAccountInfo, getTokenLargestAccounts, sendTransaction)
- [x] WebSocket monitor (logsSubscribe for Raydium/PumpFun)
- [x] Transaction builder (Jupiter swap TX)
- [x] Jito bundle submission
- [x] TX confirmation tracking
- [x] Priority fee estimation (percentile-based)

---

## S6.5: v3.2 Advanced Analysis (DONE)

**Goal**: Self-learning analysis modules, full pipeline integration.

### Modules
- [x] Liquidity Flow Direction Analysis
  - Net flow tracking (5min/30min windows)
  - Pattern detection: HEALTHY_GROWTH, ARTIFICIAL_PUMP, RUG_PRECURSOR, SLOW_BLEED
  - Velocity: ACCELERATING / DECELERATING / STABLE
- [x] Narrative Momentum Engine
  - 7 default narratives (AI_AGENTS, POLITICAL, ANIMAL_MEME, etc.)
  - Phase lifecycle: EMERGING -> GROWING -> PEAK -> DECLINING -> DEAD
  - Token count trends (1h/6h/24h)
- [x] Honeypot Evolution Tracker
  - SHA256 contract fingerprinting
  - Pattern learning from rug samples
  - Retroactive scan on new signature detection
- [x] Cross-Token Correlation Detector
  - Cluster-based tracking (same deployer/funder)
  - Pattern detection: ROTATION, DISTRACTION, SERIAL
  - Risk levels: LOW -> MEDIUM -> HIGH -> CRITICAL
- [x] Copy-Trade Intelligence
  - Wallet tiers: WHALE, SMART_MONEY, KOL, INSIDER, FRESH
  - Signal generation: BUY, SELL, ACCUMULATE, DUMP, FIRST_MOVE
  - Config-based wallet loading + HTTP API
- [x] Adaptive Scoring Weights
  - Learns from TradeOutcome (PnL + dimension scores)
  - 30-minute recalculation cycle
  - Configurable learning rate and window size

### Integration
- [x] v3.2 instant-kill checks (honeypot >= 0.7, rug precursor, cross-token CRITICAL)
- [x] v3.2 score adjustments (liquidity flow, narrative phase, correlation, honeypot partial, copytrade)
- [x] CSM enhanced (holder exodus, entity re-check, honeypot retro scan)
- [x] Rug feedback loop (honeypot.RecordRug + correlation.MarkRugged on position close)
- [x] Entry score cache for adaptive weight training
- [x] 12 v3.2 scoring integration tests
- [x] All 497 tests passing with race detector

---

## S7: Paper Marathon (NEXT)

**Goal**: 2-4 weeks dry-run on Solana mainnet, measuring everything.

### Planned
- [ ] Connect to mainnet in dry-run mode
- [ ] Monitor scoring accuracy vs actual token outcomes
- [ ] Track signal-to-noise ratio (how many STRONG_BUY vs actual winners)
- [ ] Measure detection latency (pool creation -> score ready)
- [ ] Validate CSM triggers (false positive rate)
- [ ] Tune scoring weights with real data
- [ ] Tune exit engine parameters
- [ ] Weekly performance reports
- [ ] Alerting pipeline (system health, missed opportunities)
- [ ] Runbooks for top 10 failure modes

### Success Criteria
- 2 weeks continuous operation without critical failures
- Detection latency < 5s from pool creation to score
- False positive rate < 30% on STRONG_BUY signals
- CSM catches > 80% of actual rugs before -50% loss
- Adaptive weights show convergence

---

## S8: Live Trading (PENDING)

**Goal**: Live trading with small capital, hard safety limits.

### Planned
- [ ] Enable live execution (remove dry-run flag)
- [ ] Start with minimal capital (0.05 SOL max per trade)
- [ ] Hard limits (not configurable):
  - max_daily_loss: X SOL
  - max_daily_spend: Y SOL
  - max_positions: 3
- [ ] Auto-stop on limit breach
- [ ] Daily P&L tracking
- [ ] Weekly review: keep / kill / modify parameters
- [ ] Gradual capital increase based on performance

### Success Criteria
- Limits enforced 100% of the time
- Full audit trail for every trade
- Positive expected value after 2 weeks
- Stability > PnL (prefer $0 over bugs)

---

## Future Considerations

### Near-term
- Telegram alerts for STRONG_BUY signals
- Grafana dashboard for real-time monitoring
- PumpPortal WebSocket integration (pre-graduation trades)
- Bonding curve sniper (buy on curve, sell on graduation)

### Medium-term
- Base chain adapter (Uniswap V3 + Aerodrome)
- BSC adapter (PancakeSwap)
- Multi-chain portfolio tracking
- ML-based scoring model

### Long-term
- Web dashboard (React)
- Social sentiment analysis
- Cross-chain correlation analysis
- Full automation with dynamic risk management

---

*Last Updated: 2026-02-23*
