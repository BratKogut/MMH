# NEXUS Memecoin Hunter v3.2

**SAFETY > PROFIT > SPEED**

A high-performance Solana memecoin detection, analysis, and sniping engine built on the NEXUS platform. Written in Go 1.24 with an event-driven architecture, 5-dimensional scoring, and self-learning feedback loops.

## Overview

NEXUS Memecoin Hunter is a production-grade trading system that:

- **Detects** new pools on Raydium and pump.fun via WebSocket monitoring
- **Sanitizes** with sub-10ms L0 filter (zero external calls)
- **Analyzes** token safety, sell simulation, deployer entity graph, liquidity flows, narrative momentum, honeypot patterns, cross-token correlations, and copy-trade signals
- **Scores** tokens across 5 dimensions with adaptive weights that learn from outcomes
- **Snipes** via Jupiter V6 with Jito MEV protection and dynamic priority fees
- **Monitors** positions with Continuous Safety Monitor (CSM) per position
- **Exits** with multi-level take profit, trailing stops, and panic sells

## Architecture

```
Pool Discovery (WebSocket)
    |
    v
L0 Sanitizer (<10ms, zero external calls)
    |
    v
Token Analyzer (safety, liquidity, holders)
    |
    v
Sell Simulator (pre-buy honeypot detection)
    |
    v
Entity Graph Engine (deployer risk, Sybil detection)
    |
    v
v3.2 Advanced Analysis
  |- Liquidity Flow Direction (RUG_PRECURSOR detection)
  |- Narrative Momentum (EMERGING -> DEAD lifecycle)
  |- Honeypot Evolution Tracker (pattern learning)
  |- Cross-Token Correlation (rotation/serial/distraction)
  |- Copy-Trade Intelligence (whale/smart-money signals)
    |
    v
5D Scorer (Safety 30% + Entity 15% + Social 20% + OnChain 20% + Timing 15%)
  + Adaptive Weights (recalculated every 30min from trade outcomes)
    |
    v
Sniper Engine (Jupiter V6 + Jito bundles)
    |
    v
CSM + Exit Engine (per-position goroutine)
    |
    v
Position Close -> Outcome Feed -> Adaptive Recalculation
```

## Key Features

### 5-Dimensional Scoring
- **Safety (30%)**: Mint/freeze authority, LP burn, holder concentration
- **Entity (15%)**: Deployer wallet graph, Sybil clusters, hops to known ruggers
- **Social (20%)**: Narrative phase, velocity, token count trends
- **On-Chain (20%)**: Liquidity flow patterns, organic vs artificial volume
- **Timing (15%)**: Pool age, narrative alignment, copy-trade signals

### v3.2 Advanced Analysis Modules
- **Liquidity Flow Direction**: Detects RUG_PRECURSOR, ARTIFICIAL_PUMP, SLOW_BLEED patterns
- **Narrative Momentum**: Tracks meme narrative lifecycle (EMERGING -> GROWING -> PEAK -> DECLINING -> DEAD)
- **Honeypot Evolution**: SHA256 fingerprinting of contract patterns, learns from rugs
- **Cross-Token Correlation**: Detects ROTATION, DISTRACTION, and SERIAL scam patterns
- **Copy-Trade Intelligence**: Whale/smart-money/KOL wallet tracking with signal generation
- **Adaptive Scoring Weights**: Self-learning system that adjusts 5D weights from trade outcomes

### Continuous Safety Monitor (CSM)
Per-position goroutine that runs every 30 seconds:
- Sell simulation re-check (honeypot detection post-buy)
- Holder exodus detection (top-10 holders dumping)
- Liquidity health monitoring
- Entity graph re-check (deployer turned serial rugger)

### Multi-Level Exit Engine
- **Take Profit**: 4 levels with partial sells (1.5x/25%, 2x/25%, 3x/25%, 6x/100%)
- **Stop Loss**: -50% from entry
- **Trailing Stop**: 20% from highest price
- **Timed Exits**: 4h/50%, 12h/75%, 24h/100%
- **Panic Exits**: CSM triggers (sell sim failed, liquidity drop, holder exodus)

### Self-Learning Feedback Loop
On position close:
1. TradeOutcome records PnL + dimension scores at entry
2. AdaptiveWeightEngine adjusts scoring weights based on win/loss correlation
3. If rug detected: honeypot tracker learns new patterns, correlation detector marks cluster

## Tech Stack

- **Language**: Go 1.24
- **Event Bus**: Kafka/RedPanda (franz-go client)
- **Analytics**: ClickHouse (batch writer)
- **Blockchain**: Solana RPC + WebSocket
- **DEX**: Jupiter V6 API (quote/swap/price)
- **MEV Protection**: Jito Bundles
- **Monitoring**: Prometheus metrics + health endpoints
- **Configuration**: YAML with runtime validation

## Project Structure

```
nexus/
├── cmd/
│   ├── nexus-core/      # Market data + Kafka producer + ClickHouse writer
│   ├── nexus-intel/     # LLM intelligence pipeline
│   └── nexus-hunter/    # Memecoin hunter (main binary)
│
├── internal/
│   ├── scanner/         # Token Scanner + Analyzer + 5D Scoring + Sell Sim + Adaptive Weights
│   ├── sniper/          # Sniper Engine + CSM + Exit Engine
│   ├── graph/           # Entity Graph (wallet clustering, Sybil detection)
│   ├── liquidity/       # Liquidity Flow Direction Analysis
│   ├── narrative/       # Narrative Momentum Engine
│   ├── honeypot/        # Honeypot Evolution Tracker
│   ├── correlation/     # Cross-Token Correlation Detector
│   ├── copytrade/       # Copy-Trade Intelligence
│   ├── solana/          # RPC client + WebSocket monitor + Jito + priority fees
│   ├── adapters/        # Exchange adapters (Jupiter, Kraken, Binance)
│   ├── bus/             # Kafka/RedPanda event streaming
│   ├── clickhouse/      # Analytics database writer
│   ├── execution/       # Order engine + state machine + position manager
│   ├── risk/            # Risk veto guardian
│   ├── intel/           # Brain + TriggerEngine + IntelService
│   ├── config/          # YAML configuration
│   └── observability/   # Prometheus metrics + health
│
├── go.mod
└── Makefile
```

## Binaries

| Binary | Purpose |
|--------|---------|
| `nexus-hunter` | Memecoin sniping engine (main focus) |
| `nexus-core` | Market data aggregation (Kraken/Binance -> Kafka -> ClickHouse) |
| `nexus-intel` | LLM intelligence pipeline |

## HTTP API (nexus-hunter, port 9092)

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/health` | GET | Health status (dry_run, paused, killed) |
| `/stats` | GET | Combined stats from all modules |
| `/positions` | GET | All positions (open + closed) |
| `/positions/open` | GET | Currently open positions |
| `/control/pause` | POST | Soft pause (stop new entries) |
| `/control/resume` | POST | Resume from pause |
| `/control/kill` | POST | Hard kill (force close all) |
| `/control/status` | GET | Current state |
| `/copytrade/wallets` | GET | List tracked wallets |
| `/copytrade/wallets` | POST | Add tracked wallet |
| `/copytrade/wallets` | DELETE | Remove tracked wallet |

## Configuration

```yaml
general:
  instance_id: "nexus-1"
  environment: "development"
  dry_run: true

solana:
  rpc_endpoint: "https://mainnet.helius-rpc.com/?api-key=YOUR_KEY"
  ws_endpoint: "wss://mainnet.helius-rpc.com/?api-key=YOUR_KEY"
  private_key: "base58_private_key"
  rate_limit_rps: 10

hunter:
  enabled: true
  dry_run: true
  monitor_dexes: ["raydium", "pumpfun"]
  max_buy_sol: 0.1
  slippage_bps: 200
  max_positions: 5
  min_safety_score: 40
  min_liquidity_usd: 500
  use_jito: true
  jito_tip_sol: 0.001
  tracked_wallets:
    - address: "WHALE_WALLET_ADDRESS"
      tier: "whale"
      label: "Known whale"
```

## Quick Start

### Prerequisites

- Go 1.24+
- Solana RPC endpoint (Helius recommended)
- Solana wallet with SOL

### Build

```bash
cd nexus
go build -o bin/ ./cmd/...
```

### Run (dry-run mode)

```bash
./bin/nexus-hunter --config config.yaml
```

### Run Tests

```bash
cd nexus
go test -race -count=1 ./...
```

**Current test status**: 497 tests passing, ~34,000 LOC

## Important Notes

### Safety First
- All trades subject to daily spend/loss limits
- Two-level security: pre-filter (L0 sanitizer) + pre-buy (analyzer + sell sim)
- CSM monitors every open position continuously
- Kill switch stops everything immediately (in-process, no Kafka dependency)

### Dry-Run Required
- Always start in dry-run mode (`dry_run: true`)
- Monitor scoring accuracy and signal quality before live trading
- Paper Marathon (S7) recommended before going live

### Not Financial Advice
- This is a trading bot, not investment advice
- Memecoins are highly speculative and risky
- You can lose your entire investment
- Use at your own risk

## Documentation

- [Architecture & Pipeline](docs/01-architecture-solana.md)
- [Scoring & Execution](docs/03-scoring-execution.md)
- [External API Reference](docs/API_REFERENCE.md)
- [Development Roadmap](ROADMAP.md)
- [NEXUS Platform Plan](NEXUS_PLAN.md)

## License

MIT License - See LICENSE file for details

---

*Last Updated: 2026-02-23*
