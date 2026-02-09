# Multi-Chain Memecoin Hunter v2.0

**SAFETY > PROFIT**

An advanced, production-ready system for automated detection, security analysis, and trading of memecoins across multiple blockchain networks.

## 🎯 Project Overview

Multi-Chain Memecoin Hunter v2.0 is a sophisticated trading bot that:

- **Detects** new memecoins across Solana, Base, BSC, TON, Arbitrum, and Tron
- **Analyzes** security with two-level checks (pre-filter cache + pre-execution fresh)
- **Scores** tokens using chain-specific weighted algorithms
- **Executes** trades with MEV protection and risk management
- **Manages** positions with automated TP/SL and portfolio limits
- **Monitors** health with Prometheus metrics and Telegram alerts

## 🏗️ Architecture

```
┌─────────────────────────────────────────────────────────────┐
│ CHAIN ADAPTERS (Solana, Base, BSC, TON, Arbitrum, Tron)   │
└────────────────────┬────────────────────────────────────────┘
                     │
┌────────────────────▼────────────────────────────────────────┐
│ REDIS STREAMS (per-chain event bus)                         │
│ - tokens:new:{chain}                                        │
│ - trades:executed:{chain}                                   │
│ - health:*                                                  │
└────────────────────┬────────────────────────────────────────┘
                     │
        ┌────────────┼────────────┐
        │            │            │
    ┌───▼────┐  ┌───▼──────┐  ┌──▼──────────┐
    │SCORING │  │EXECUTION │  │ POSITION    │
    │SERVICE │  │SERVICE   │  │ MANAGER     │
    └────────┘  └──────────┘  └─────────────┘
        │            │            │
        └────────────┼────────────┘
                     │
        ┌────────────▼────────────┐
        │ PostgreSQL + TimescaleDB│
        │ Grafana + Prometheus    │
        │ Telegram Alerts         │
        └─────────────────────────┘
```

## 🚀 Key Features

### Security First
- **Two-level security checks**: Pre-filter (cached) + Pre-execution (fresh)
- **Circuit breaker pattern**: Protects against cascading failures
- **Portfolio limits**: MAX_EXPOSURE, MAX_PER_CHAIN, MAX_SINGLE_POSITION
- **Fresh security validation**: Always verified before execution

### Multi-Chain Support
- **Solana**: pump.fun, PumpSwap, Raydium, Orca
- **Base**: Uniswap V3, Aerodrome
- **BSC**: PancakeSwap (Phase 3)
- **TON**: STON.fi, DeDust (Phase 3)
- **Arbitrum**: Camelot, Uniswap V3 (Phase 4)
- **Tron**: SunSwap, SunPump (Phase 4)

### Reliability
- **Consumer groups + DLQ**: Message reliability with dead letter queues
- **Exponential backoff**: Smart retry logic with max delays
- **Health checks**: Real-time monitoring via Prometheus
- **Graceful degradation**: Fallback mechanisms for API failures

### Scalability
- **Redis Streams**: Efficient event processing
- **TimescaleDB**: Time-series data with automatic compression
- **Async/await**: Non-blocking I/O throughout
- **Modular adapters**: Easy to extend with new chains

## 📊 Supported Chains & DEXs

| Chain | DEXs | Status |
|-------|------|--------|
| **Solana** | pump.fun, Raydium, Orca, PumpSwap | ✅ Phase 1 |
| **Base** | Uniswap V3, Aerodrome | ✅ Phase 1 |
| **BSC** | PancakeSwap | 📅 Phase 3 |
| **TON** | STON.fi, DeDust | 📅 Phase 3 |
| **Arbitrum** | Camelot, Uniswap V3 | 📅 Phase 4 |
| **Tron** | SunSwap, SunPump | 📅 Phase 4 |

## 💰 Token Discovery Layers

### Solana (3-Layer System)

**Layer 1: PumpPortal WebSocket** (~200ms, ~70% coverage)
- Real-time new token creation events
- Bonding curve trades
- Migration (graduation) tracking
- Free tier available

**Layer 2: Helius Webhooks** (HTTP push with auto-retry)
- Raydium V4 pool creation
- Orca Whirlpool monitoring
- Reliable delivery with automatic retries

**Layer 3: Helius WebSocket** (Fallback)
- Raydium logs subscription
- Orca logs subscription
- Wallet account changes
- TX confirmation tracking

### Base (WebSocket Monitoring)

- **Uniswap V3 PoolCreated** logs
- **Aerodrome V2 PoolCreated** logs
- Automatic new token identification
- Liquidity depth tracking

## 🔒 Security Analysis

### Two-Level Approach

**Pre-Filter (Cached)**
- TTL-based cache (30 seconds)
- Fast initial validation
- Reduces API calls

**Pre-Execution (Fresh)**
- Always fresh check before trade
- Bypasses cache
- Protects against last-minute changes

### Solana Security Checks

1. **On-Chain (Helius)**
   - Mint authority status
   - Freeze authority status
   - Token metadata

2. **Birdeye Security API**
   - `isMintable`: Can infinite tokens be minted?
   - `isFreezable`: Can tokens be frozen?
   - `top10HolderPercent`: Concentration risk
   - `lpBurnedPercent`: LP burn status

3. **Birdeye Overview**
   - Price, volume, liquidity
   - Buy/sell ratio
   - Holder count

### Base Security Checks

1. **GoPlus Security API**
   - Honeypot detection
   - Mintable status
   - Owner drain capability
   - Hidden owner detection
   - Self-destruct flag
   - Transfer pausable status
   - Buy/sell tax levels
   - Proxy upgrade status

2. **Basescan Verification** (optional)
   - Contract source code verification
   - ABI availability

## 📈 Scoring System

### Chain-Specific Weights

**Solana**
- LP Burned: 20%
- Holder Distribution: 20%
- Creator History: 15%
- Pump.fun Graduation: 15%
- Organic Volume: 15%
- Bonding Curve Momentum: 15%

**Base**
- Honeypot Check: 25%
- LP Locked: 20%
- Holder Distribution: 20%
- Contract Verified: 10%
- Liquidity Depth: 15%
- Tax Level: 10%

**BSC**
- Honeypot Check: 30%
- LP Locked: 20%
- Holder Distribution: 15%
- PinkSale Audit: 15%
- Contract Verified: 10%
- Tax Level: 10%

### Output

```python
@dataclass
class TokenScore:
    chain: ChainId
    address: str
    risk_score: int         # 0-100 (higher = safer)
    momentum_score: int     # 0-100 
    overall_score: int
    flags: list[str]
    recommendation: str     # STRONG_BUY | BUY | WATCH | AVOID
```

## ⚙️ Execution Engine

### Trade Flow

```
scoring:results (STRONG_BUY) → EXECUTION SERVICE:
  1. Validate score (still STRONG_BUY?)
  2. Check balance (enough SOL/ETH?)
  3. Check position (not already holding?)
  4. FRESH security check (pre-execution, bypasses cache)
  5. Size position (risk-based)
  6. Execute swap (via chain adapter)
  7. Create position (Position Manager)
  8. Set TP/SL (auto stop-loss)
```

### Position Sizing

```python
SIZING = {
    ("STRONG_BUY", "solana"): {"base_usd": 50, "max_usd": 100},
    ("BUY",        "solana"): {"base_usd": 25, "max_usd": 50},
    ("STRONG_BUY", "base"):   {"base_usd": 30, "max_usd": 80},
    ("BUY",        "base"):   {"base_usd": 15, "max_usd": 40},
}

# Global limits:
MAX_PORTFOLIO_EXPOSURE = 500   # USD total open
MAX_POSITIONS_PER_CHAIN = 5
MAX_SINGLE_POSITION_PCT = 20   # % of portfolio
```

### MEV Protection

| Chain | Method | Detail |
|-------|--------|--------|
| **Solana** | Jito Bundles | Private mempool via `mainnet.block-engine.jito.wtf` |
| **Base** | Low priority | L2 = minimal MEV, standard send |
| **BSC** | None | Public mempool, high slippage + fast exec |
| **Arbitrum** | Flashbots | Flashbots Protect RPC |

## 📊 Position Management

```python
@dataclass
class Position:
    id: str
    chain: ChainId
    token_address: str
    token_symbol: str
    entry_price: float
    entry_timestamp: float
    entry_tx_hash: str
    amount_tokens: int
    
    take_profit_levels: list = [50, 100, 200]  # %
    stop_loss_pct: float = -30
    trailing_stop_pct: float = 0       # 0 = disabled
    max_holding_seconds: int = 3600    # 1h default
    status: str = "OPEN"               # OPEN | PARTIAL_EXIT | CLOSED
```

## 💾 Database Schema

### PostgreSQL 15 + TimescaleDB

**Tokens Table**
- Chain, address, symbol, name
- Creator, launchpad, DEX, pool
- Discovery timestamp

**Token Scores Hypertable**
- Risk score, momentum score, overall score
- Flags, recommendation
- Scored timestamp (for time-series)

**Positions Table**
- Entry/exit price, amount, timestamp
- TX hash, TP/SL levels
- Status, P&L tracking

**Transactions Table**
- Position reference
- Buy/sell/partial-sell
- Amount in/out, price, gas cost
- Execution timestamp

## 💰 Cost Breakdown (MVP)

### Solana + Base

| Provider | Plan | $/mo | Usage |
|----------|------|------|-------|
| Helius | Business | $199 | RPC+WS+Webhooks+DAS |
| Alchemy (Base) | Growth | $49 | RPC+WS |
| Birdeye | Standard | $99 | Security+OHLCV+Overview |
| PumpPortal | Free | $0 | pump.fun WS data |
| GoPlus | Free | $0 | Base/BSC security |
| DexScreener | Free | $0 | Pair data |
| Jupiter | Free | $0 | Solana swaps |
| 0x | Free tier | $0 | Base quotes |
| Basescan | Free | $0 | Contract verification |
| Jito | Free | $0 | MEV protection (tip in TX) |
| VPS (Hetzner AX41) | 64GB RAM | $45 | All services |
| **TOTAL** | | **~$392/mo** | |

### Additional Chains

| Chain | Provider | Extra $/mo |
|-------|----------|-----------|
| BSC | NodeReal/Ankr | $0-50 |
| TON | TON Center | $0-30 |
| Arbitrum | Alchemy (included) | $0 |
| Tron | TronGrid | $0-30 |

## 📅 Roadmap

### Phase 1 — MVP (2-3 weeks)

**Week 1**
- ☐ SolanaAdapter — PumpPortal WS (new tokens + migrations)
- ☐ SolanaAdapter — Helius WS fallback (Raydium + Orca)
- ☐ SolanaAdapter — Security (on-chain + Birdeye)
- ☐ Redis Streams + consumer groups

**Week 2**
- ☐ BaseAdapter — WS (Uniswap V3 + Aerodrome PoolCreated)
- ☐ BaseAdapter — Security (GoPlus)
- ☐ Basic Scoring (risk + momentum)
- ☐ Telegram alerts

**Week 3**
- ☐ Jupiter swap execution (Solana)
- ☐ Uniswap V3 swap execution (Base)
- ☐ Two-level security (pre-filter + pre-execution)
- ☐ Circuit breaker + exponential backoff
- ☐ PostgreSQL + position tracking

### Phase 2 — Semi-Auto (2 weeks)

- ☐ Telegram buy/sell commands
- ☐ Position Manager (TP/SL monitoring)
- ☐ Auto stop-loss
- ☐ Portfolio summary (Telegram)
- ☐ Dead letter queue
- ☐ Grafana dashboard

### Phase 3 — Expansion (2-3 weeks)

- ☐ BSC adapter (PancakeSwap)
- ☐ TON adapter (STON.fi / DeDust)
- ☐ Cross-chain portfolio
- ☐ Advanced scoring (wallet analysis, social)
- ☐ Bonding curve sniper

### Phase 4 — Hardening (ongoing)

- ☐ Arbitrum + Tron adapters
- ☐ Full auto mode + risk limits
- ☐ React web dashboard
- ☐ Backtesting engine
- ☐ ML-based scoring

## 🛠️ Tech Stack

- **Language**: Python 3.11+
- **Async**: asyncio, aiohttp
- **Blockchain**: web3.py, solders (Solana)
- **Database**: PostgreSQL 15 + TimescaleDB
- **Cache/Streams**: Redis
- **Monitoring**: Prometheus + Grafana
- **Notifications**: Telegram Bot API
- **APIs**: Helius, Alchemy, Birdeye, GoPlus, Jupiter, 0x, DexScreener

## 📁 Project Structure

```
multichain-memecoin-hunter/
├── README.md
├── docs/
│   ├── 01-architecture-solana.md
│   ├── 02-base-adapter.md
│   ├── 03-scoring-execution.md
│   ├── API_REFERENCE.md
│   ├── DEPLOYMENT.md
│   └── TROUBLESHOOTING.md
├── src/
│   ├── __init__.py
│   ├── main.py
│   ├── config.py
│   ├── adapters/
│   │   ├── __init__.py
│   │   ├── base.py
│   │   ├── solana_adapter.py
│   │   ├── base_adapter.py
│   │   └── [other chains]
│   ├── services/
│   │   ├── __init__.py
│   │   ├── scoring_service.py
│   │   ├── execution_service.py
│   │   ├── position_manager.py
│   │   └── alerts_service.py
│   └── models/
│       ├── __init__.py
│       ├── types.py
│       ├── security.py
│       └── position.py
├── tests/
│   ├── __init__.py
│   ├── test_adapters.py
│   ├── test_scoring.py
│   ├── test_execution.py
│   └── test_security.py
├── config/
│   ├── config.example.yaml
│   ├── scoring_weights.yaml
│   └── position_sizing.yaml
├── scripts/
│   ├── setup_redis.sh
│   ├── setup_postgres.sh
│   ├── setup_monitoring.sh
│   └── backtest.py
├── requirements.txt
├── docker-compose.yml
├── .gitignore
└── LICENSE
```

## 🚀 Quick Start

### Prerequisites

- Python 3.11+
- Redis 7.0+
- PostgreSQL 15+
- API keys for: Helius, Alchemy, Birdeye, GoPlus

### Installation

```bash
# Clone repository
git clone https://github.com/yourusername/multichain-memecoin-hunter.git
cd multichain-memecoin-hunter

# Create virtual environment
python3.11 -m venv venv
source venv/bin/activate

# Install dependencies
pip install -r requirements.txt

# Copy and configure
cp config/config.example.yaml config/config.yaml
# Edit config.yaml with your API keys

# Setup database
./scripts/setup_postgres.sh

# Setup Redis
./scripts/setup_redis.sh

# Run
python src/main.py
```

### Docker Compose

```bash
docker-compose up -d
```

## 📖 Documentation

- [Architecture & Solana Adapter](docs/01-architecture-solana.md)
- [Base Adapter Specification](docs/02-base-adapter.md)
- [Scoring, Execution & Infrastructure](docs/03-scoring-execution.md)
- [API Reference](docs/API_REFERENCE.md)
- [Deployment Guide](docs/DEPLOYMENT.md)
- [Troubleshooting](docs/TROUBLESHOOTING.md)

## ⚠️ Important Notes

### Safety First
- This system is designed with **SAFETY > PROFIT** philosophy
- All trades are subject to portfolio limits and risk checks
- Two-level security validation prevents most honeypots
- Circuit breaker protects against cascading failures

### Backtesting Required
- Always backtest scoring weights on historical data
- Empirically validate assumptions before live trading
- Monitor P&L and adjust parameters continuously

### MEV & Slippage
- Solana: Jito Bundles protect against sandwich attacks
- Base: Minimal MEV, but still present
- BSC: High slippage expected, MEV protection recommended
- Always simulate slippage before execution

### Not Financial Advice
- This is a trading bot, not investment advice
- Memecoins are highly speculative and risky
- You can lose your entire investment
- Use at your own risk

## 📝 License

MIT License - See LICENSE file for details

## 🤝 Contributing

Contributions are welcome! Please see CONTRIBUTING.md for guidelines.

## 📧 Support

For issues, questions, or suggestions:
- Open an issue on GitHub
- Check TROUBLESHOOTING.md
- Review documentation

---

**Built with ❤️ for memecoin traders who value safety and reliability.**

*Last Updated: 2026-02-09*
