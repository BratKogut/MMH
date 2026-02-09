# MULTI-CHAIN MEMECOIN HUNTER v2.0 — Part 3/3
## Scoring + Execution + Infrastructure + Roadmap

---

## 7. CONSUMER GROUPS & RELIABILITY

```python
# Setup (run once at startup):
streams_and_groups = {
    "tokens:new:solana": ["scoring_group", "alert_group"],
    "tokens:new:base":   ["scoring_group", "alert_group"],
    "scoring:results":   ["executor_group", "alert_group"],
    "trades:executed:solana": ["position_group"],
    "trades:executed:base":   ["position_group"],
}
# XGROUP CREATE for each

# Consumer pattern: Read → Process → XACK
# If handler fails N times → send to dlq:{stream}
# Pending messages re-processed on restart via XREADGROUP with id="0"
```

---

## 8. SCORING SERVICE

### Chain-Specific Weights

```python
CHAIN_SCORING = {
    "solana": {
        "lp_burned": 0.20,
        "holder_distribution": 0.20,
        "creator_history": 0.15,
        "pump_fun_graduation": 0.15,
        "volume_organic": 0.15,
        "bonding_curve_momentum": 0.15,  # NEW: pre-graduation signal
    },
    "base": {
        "honeypot_check": 0.25,          # GoPlus critical
        "lp_locked": 0.20,
        "holder_distribution": 0.20,
        "contract_verified": 0.10,
        "liquidity_depth": 0.15,
        "tax_level": 0.10,
    },
    "bsc": {
        "honeypot_check": 0.30,          # Highest — BSC has most scams
        "lp_locked": 0.20,
        "holder_distribution": 0.15,
        "pinksale_audit": 0.15,
        "contract_verified": 0.10,
        "tax_level": 0.10,
    },
    "ton": {
        "lp_locked": 0.25,
        "holder_distribution": 0.25,
        "jetton_compliant": 0.20,
        "creator_history": 0.15,
        "telegram_presence": 0.15,
    },
}
```

### Output

```python
@dataclass
class TokenScore:
    chain: ChainId
    address: str
    risk_score: int         # 0-100 (higher = SAFER)
    momentum_score: int     # 0-100 
    overall_score: int
    flags: list[str]
    recommendation: str     # STRONG_BUY | BUY | WATCH | AVOID
```

---

## 9. EXECUTION SERVICE

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

# Global safety:
MAX_PORTFOLIO_EXPOSURE = 500   # USD total open
MAX_POSITIONS_PER_CHAIN = 5
MAX_SINGLE_POSITION_PCT = 20   # % of portfolio
```

### MEV Protection

| Chain | Method | Detail |
|-------|--------|--------|
| Solana | **Jito Bundles** | Private mempool via `mainnet.block-engine.jito.wtf` |
| Base | **Low priority** | L2 = minimal MEV, use standard send |
| BSC | **None** | Public mempool, high slippage + fast exec |
| Arbitrum | **Flashbots** | Flashbots Protect RPC |

---

## 10. POSITION MANAGER

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

    take_profit_levels: list = field(default_factory=lambda: [50, 100, 200])  # %
    stop_loss_pct: float = -30
    trailing_stop_pct: float = 0       # 0 = disabled
    max_holding_seconds: int = 3600    # 1h default
    status: str = "OPEN"               # OPEN | PARTIAL_EXIT | CLOSED
```

---

## 11. INFRASTRUCTURE

### Rate Limiters

```python
RATE_LIMITS = {
    "birdeye":     1.0,   # calls/sec (~60/min)
    "goplus":      0.5,   # 30/min
    "basescan":    5.0,   # 5/sec
    "helius":      10.0,  # plan-based
    "dexscreener": 5.0,   # generous
}
# Token bucket implementation with asyncio lock
```

### Retry Logic

```python
# Exponential backoff decorator for all API calls:
# Attempt 1: immediate
# Attempt 2: wait 1s
# Attempt 3: wait 2s
# Attempt 4: wait 4s (max 30s)
```

---

## 12. DATABASE SCHEMA

```sql
-- PostgreSQL 15 + TimescaleDB

CREATE TABLE tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chain VARCHAR(20) NOT NULL,
    address VARCHAR(100) NOT NULL,
    symbol VARCHAR(50), name VARCHAR(200),
    creator_address VARCHAR(100),
    launchpad VARCHAR(50), dex VARCHAR(50),
    pool_address VARCHAR(100),
    discovered_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(chain, address)
);

CREATE TABLE token_scores (
    token_id UUID REFERENCES tokens(id),
    scored_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    risk_score SMALLINT, momentum_score SMALLINT, overall_score SMALLINT,
    flags JSONB, recommendation VARCHAR(20),
    PRIMARY KEY (token_id, scored_at)
);
SELECT create_hypertable('token_scores', 'scored_at');

CREATE TABLE positions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chain VARCHAR(20) NOT NULL,
    token_address VARCHAR(100) NOT NULL, token_symbol VARCHAR(50),
    entry_price DECIMAL(30,18), entry_amount DECIMAL(30,18),
    entry_timestamp TIMESTAMPTZ, entry_tx_hash VARCHAR(100),
    exit_price DECIMAL(30,18), exit_timestamp TIMESTAMPTZ,
    take_profit_levels JSONB DEFAULT '[50,100,200]',
    stop_loss_pct DECIMAL(10,4) DEFAULT -30,
    status VARCHAR(20) DEFAULT 'OPEN',
    pnl_usd DECIMAL(20,2), pnl_pct DECIMAL(10,4)
);

CREATE TABLE transactions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    position_id UUID REFERENCES positions(id),
    chain VARCHAR(20), tx_hash VARCHAR(100),
    tx_type VARCHAR(20),  -- BUY, SELL, PARTIAL_SELL
    amount_in DECIMAL(30,18), amount_out DECIMAL(30,18),
    price DECIMAL(30,18), gas_cost_usd DECIMAL(10,4),
    executed_at TIMESTAMPTZ DEFAULT NOW()
);
```

---

## 13. KOSZTY — REALISTYCZNY BUDŻET

### MVP (Solana + Base)

| Provider | Plan | $/mo | Użycie |
|----------|------|------|--------|
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

### Per additional chain

| Chain | Provider | Extra $/mo |
|-------|----------|-----------|
| BSC | NodeReal/Ankr | $0-50 |
| TON | TON Center | $0-30 |
| Arbitrum | Alchemy (included) | $0 |
| Tron | TronGrid | $0-30 |

---

## 14. API QUICK REFERENCE

| Service | URL | Auth | Limit |
|---------|-----|------|-------|
| Helius RPC | `https://mainnet.helius-rpc.com/?api-key={K}` | URL | plan |
| Helius WS | `wss://mainnet.helius-rpc.com/?api-key={K}` | URL | plan |
| Helius Webhooks | `https://api.helius.xyz/v0/webhooks?api-key={K}` | URL | plan |
| PumpPortal WS | `wss://pumpportal.fun/api/data` | none | rate-limited |
| PumpPortal Trade | `https://pumpportal.fun/api/trade-local` | none | 0.5% fee |
| Jupiter Quote | `https://quote-api.jup.ag/v6/quote` | none | unlimited |
| Jupiter Swap | `https://quote-api.jup.ag/v6/swap` | none | unlimited |
| Jupiter Price | `https://price.jup.ag/v6/price` | none | unlimited |
| Birdeye | `https://public-api.birdeye.so/defi/*` | X-API-KEY | plan |
| Jito | `https://mainnet.block-engine.jito.wtf/api/v1/transactions` | none | unlimited |
| Alchemy RPC | `https://base-mainnet.g.alchemy.com/v2/{K}` | URL | plan |
| Alchemy WS | `wss://base-mainnet.g.alchemy.com/v2/{K}` | URL | plan |
| GoPlus | `https://api.gopluslabs.io/api/v1/token_security/{CID}` | none | 30/min |
| Basescan | `https://api.basescan.org/api` | apikey | 5/sec |
| 0x | `https://base.api.0x.org/swap/v1/*` | 0x-api-key | 100k/mo |
| DexScreener | `https://api.dexscreener.com/latest/dex/tokens/{A}` | none | unlimited |

---

## 15. ROADMAP

### Phase 1 — MVP (2-3 tygodnie)

```
WEEK 1:
  ☐ SolanaAdapter — PumpPortal WS (new tokens + migrations)
  ☐ SolanaAdapter — Helius WS fallback (Raydium + Orca)
  ☐ SolanaAdapter — Security (on-chain + Birdeye)
  ☐ Redis Streams + consumer groups

WEEK 2:
  ☐ BaseAdapter — WS (Uniswap V3 + Aerodrome PoolCreated)
  ☐ BaseAdapter — Security (GoPlus)
  ☐ Basic Scoring (risk + momentum)
  ☐ Telegram alerts

WEEK 3:
  ☐ Jupiter swap execution (Solana)
  ☐ Uniswap V3 swap execution (Base)
  ☐ Two-level security (pre-filter + pre-execution)
  ☐ Circuit breaker + exponential backoff
  ☐ PostgreSQL + position tracking
```

### Phase 2 — Semi-Auto (2 tygodnie)

```
  ☐ Telegram buy/sell commands
  ☐ Position Manager (TP/SL monitoring)
  ☐ Auto stop-loss
  ☐ Portfolio summary (Telegram)
  ☐ Dead letter queue
  ☐ Grafana dashboard
```

### Phase 3 — Expansion (2-3 tygodnie)

```
  ☐ BSC adapter (PancakeSwap)
  ☐ TON adapter (STON.fi / DeDust)
  ☐ Cross-chain portfolio
  ☐ Advanced scoring (wallet analysis, social)
  ☐ Bonding curve sniper (buy on curve, sell on graduation)
```

### Phase 4 — Hardening (ongoing)

```
  ☐ Arbitrum + Tron adapters
  ☐ Full auto mode + risk limits
  ☐ React web dashboard
  ☐ Backtesting engine
  ☐ ML-based scoring
```
