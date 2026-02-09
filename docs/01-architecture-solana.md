# MULTI-CHAIN MEMECOIN HUNTER v2.0 — Part 1/2
## Architecture + Solana Adapter

**Wersja:** 2.0 | **Data:** 2026-02-09 | **SAFETY > PROFIT**

---

## 1. POPRAWKI vs v1

| Obszar | v1 (błędne) | v2 (poprawione) |
|--------|------------|-----------------|
| Język | Python + Node.js | **Python only** (ATLAS = 10k linii Pythona) |
| Redis streams | `token:new` (jeden) | **`tokens:new:{chain}`** (per-chain) |
| pump.fun | Tylko Raydium logs | **PumpPortal WS** (~70% memów, pre-graduation) |
| Base DEX | Tylko Uniswap V3 | **Uniswap V3 + Aerodrome** (#1 TVL na Base) |
| Security | Jeden check | **Dwupoziomowy: pre-filter (cache) + pre-execution (fresh)** |
| WS reconnect | Prosta pętla | **Exponential backoff + circuit breaker** |
| Dead letter | Brak | **Consumer groups + XACK + DLQ** |
| Orca | Brak | **Orca Whirlpool logs subscription** |

---

## 2. HIGH-LEVEL ARCHITECTURE

```
┌───────────────────────────────────────────────────────────────────────────┐
│                    MULTI-CHAIN MEMECOIN HUNTER v2.0                       │
├───────────────────────────────────────────────────────────────────────────┤
│                                                                           │
│  CHAIN ADAPTERS:                                                         │
│  ┌──────────┐ ┌──────────┐ ┌──────┐ ┌──────┐ ┌────────┐ ┌──────┐       │
│  │ SOLANA   │ │  BASE    │ │ BSC  │ │ TON  │ │ARBITRUM│ │ TRON │       │
│  │pump.fun  │ │Uniswap V3│ │ PCS  │ │STON  │ │Camelot │ │SunSw │       │
│  │PumpSwap  │ │Aerodrome │ │      │ │DeDust│ │Uni V3  │ │SunP. │       │
│  │Raydium   │ │          │ │      │ │      │ │        │ │      │       │
│  │Orca      │ │          │ │      │ │      │ │        │ │      │       │
│  └────┬─────┘ └────┬─────┘ └──┬───┘ └──┬───┘ └───┬────┘ └──┬───┘       │
│       └──────┬─────┴─────┬────┴────┬───┴────┬────┴────┬────┘           │
│              ▼                                                           │
│  ┌─────────────────────────────────────────────────────────────┐         │
│  │            REDIS STREAMS (per-chain event bus)              │         │
│  │  tokens:new:{chain} | trades:executed:{chain} | health:*   │         │
│  └──────────────────────────┬──────────────────────────────────┘         │
│                             │                                            │
│       ┌─────────────────────┼────────────────────────┐                  │
│       ▼                     ▼                        ▼                  │
│  ┌──────────┐      ┌──────────────┐         ┌──────────────┐           │
│  │SCORING   │─────▶│ EXECUTION    │────────▶│  POSITION    │           │
│  │SERVICE   │      │ SERVICE      │         │  MANAGER     │           │
│  └────┬─────┘      └──────────────┘         └──────────────┘           │
│       ▼                                                                  │
│  ┌──────────┐     ┌────────────────────────────────────┐                │
│  │TELEGRAM  │     │ PostgreSQL+TimescaleDB | Redis     │                │
│  │ALERTS    │     │ Grafana + Prometheus               │                │
│  └──────────┘     └────────────────────────────────────┘                │
└───────────────────────────────────────────────────────────────────────────┘
```

---

## 3. ZUNIFIKOWANY INTERFACE

```python
from abc import ABC, abstractmethod
from dataclasses import dataclass, field
from enum import Enum
from typing import Optional
import time

class ChainId(str, Enum):
    SOLANA = "solana"
    BASE = "base"
    BSC = "bsc"
    TON = "ton"
    ARBITRUM = "arbitrum"
    TRON = "tron"

@dataclass
class TokenEvent:
    chain: ChainId
    address: str
    name: str
    symbol: str
    creator: str
    timestamp: float
    launchpad: Optional[str] = None
    initial_liquidity_usd: Optional[float] = None
    pool_address: Optional[str] = None
    dex: Optional[str] = None
    tx_hash: Optional[str] = None
    bonding_curve_progress: Optional[float] = None  # pump.fun: 0-100%

@dataclass
class SecurityResult:
    is_safe: bool
    flags: list[str]          # ["HONEYPOT", "MINTABLE", "HIGH_TAX:15%"]
    risk_level: str           # LOW | MEDIUM | HIGH | CRITICAL
    raw_data: dict = field(default_factory=dict)
    checked_at: float = field(default_factory=time.time)
    ttl_seconds: int = 30     # cache TTL

@dataclass
class SwapParams:
    chain: ChainId
    token_in: str
    token_out: str
    amount_raw: int           # lamports / wei
    slippage_bps: int = 300
    use_mev_protection: bool = True
    deadline_seconds: int = 120

@dataclass
class SwapResult:
    success: bool
    tx_hash: str
    amount_out: Optional[int] = None
    price_impact_pct: Optional[float] = None
    gas_cost_usd: Optional[float] = None
    error: Optional[str] = None
    confirmed: bool = False

class ChainAdapter(ABC):
    chain: ChainId
    @abstractmethod
    async def start(self) -> None: ...
    @abstractmethod
    async def stop(self) -> None: ...
    @abstractmethod
    async def health_check(self) -> dict: ...
    @abstractmethod
    async def check_security(self, address: str) -> SecurityResult: ...
    @abstractmethod
    async def get_price_data(self, address: str) -> dict: ...
    @abstractmethod
    async def execute_swap(self, params: SwapParams) -> SwapResult: ...
```

### Shared Base Class (circuit breaker + cache + Redis publish)

```python
class BaseChainAdapter(ChainAdapter):
    def __init__(self, redis_client, config):
        self.redis = redis_client
        self.config = config
        # Circuit breaker
        self._ws_failures = 0
        self._ws_max_failures = 5
        self._ws_backoff_base = 2
        self._ws_backoff_max = 60
        self._circuit_open = False
        # Security cache (two-level)
        self._security_cache: dict[str, SecurityResult] = {}

    def _get_reconnect_delay(self) -> float:
        return min(self._ws_backoff_base ** self._ws_failures, self._ws_backoff_max)

    async def _on_ws_failure(self, error: str):
        self._ws_failures += 1
        if self._ws_failures >= self._ws_max_failures:
            self._circuit_open = True
            await self.redis.xadd("alerts:system", {"data": json.dumps({
                "type": "CIRCUIT_OPEN", "chain": self.chain.value, "error": error
            })}, maxlen=1000)
        await asyncio.sleep(self._get_reconnect_delay())

    def _on_ws_success(self):
        self._ws_failures = 0
        self._circuit_open = False

    async def check_security_cached(self, address: str) -> SecurityResult:
        """Pre-filter: use cache if fresh (TTL-based)"""
        cached = self._security_cache.get(address)
        if cached and (time.time() - cached.checked_at) < cached.ttl_seconds:
            return cached
        result = await self.check_security(address)
        self._security_cache[address] = result
        return result

    async def check_security_fresh(self, address: str) -> SecurityResult:
        """Pre-execution: always fresh, bypasses cache"""
        result = await self.check_security(address)
        self._security_cache[address] = result
        return result
```

---

## 4. SOLANA ADAPTER — PEŁNA SPECYFIKACJA

### 4.1 Endpoints

```
SOLANA:
├── Helius RPC:      https://mainnet.helius-rpc.com/?api-key={KEY}
├── Helius WS:       wss://mainnet.helius-rpc.com/?api-key={KEY}
├── PumpPortal WS:   wss://pumpportal.fun/api/data           (FREE)
├── PumpPortal WS:   wss://pumpportal.fun/api/data?api-key={KEY}  (PumpSwap data)
├── PumpPortal Trade: https://pumpportal.fun/api/trade-local  (0.5% fee)
├── Jupiter Quote:   https://quote-api.jup.ag/v6/quote
├── Jupiter Swap:    https://quote-api.jup.ag/v6/swap
├── Jupiter Price:   https://price.jup.ag/v6/price
├── Birdeye:         https://public-api.birdeye.so
├── DexScreener:     https://api.dexscreener.com/latest
├── Jito MEV:        https://mainnet.block-engine.jito.wtf/api/v1/transactions
└── Programs:
    ├── Raydium V4:  675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8
    ├── Pump.fun:    6EF8rrecthR5Dkzon8Nwu78hRvfCKubJ14M5uBEwF6P
    ├── Orca:        whirLbMiicVdio4qvUfM5KAg6Ct8VwpYzGff3uctyCc
    └── SOL:         So11111111111111111111111111111111111111112
```

### 4.2 Discovery — 3-Layer System

#### Layer 1: PumpPortal WebSocket (fastest, ~200ms, ~70% of new memecoins)

```python
# URL: wss://pumpportal.fun/api/data
# Koszt: FREE (Data API), rate-limited
# CRITICAL: ONE connection, multiple subscriptions

# ─── Subscribe: New token creation ────────────────
→ SEND: {"method": "subscribeNewToken"}

# ─── Subscribe: Migration (graduation → Raydium/PumpSwap) ─
→ SEND: {"method": "subscribeMigration"}

# ─── Subscribe: Trades on specific token ──────────
→ SEND: {"method": "subscribeTokenTrade", "keys": ["<MINT_CA>"]}

# ─── Subscribe: Whale wallet tracking ─────────────
→ SEND: {"method": "subscribeAccountTrade", "keys": ["<WALLET>"]}

# ─── Unsubscribe ──────────────────────────────────
→ SEND: {"method": "unsubscribeNewToken"}
→ SEND: {"method": "unsubscribeTokenTrade", "keys": ["<MINT_CA>"]}

# ─── RESPONSE: New Token Created ──────────────────
← RECEIVE:
{
    "signature": "5K7x...",
    "mint": "ABcd...pump",
    "traderPublicKey": "7Xyz...",
    "txType": "create",
    "initialBuy": 69000000,
    "bondingCurveKey": "BCur...",
    "vTokensInBondingCurve": 1000000000,
    "vSolInBondingCurve": 30000000,
    "marketCapSol": 30.5,
    "name": "DogCoin",
    "symbol": "DOG",
    "uri": "https://..."
}

# ─── RESPONSE: Trade on bonding curve ─────────────
← RECEIVE:
{
    "signature": "3Ab...",
    "mint": "ABcd...pump",
    "traderPublicKey": "8Qwe...",
    "txType": "buy",              # "buy" or "sell"
    "tokenAmount": 50000000,
    "marketCapSol": 33.7,
    "vSolInBondingCurve": 32000000
}

# ─── RESPONSE: Migration (graduation) ────────────
← RECEIVE:
{
    "signature": "9Mn...",
    "mint": "ABcd...pump",
    "txType": "migrate",
    "pool": "RayPool...",
    "bondingCurveKey": "BCur..."
}

# Bonding curve progress calculation:
#   Graduation at ~85 SOL in bonding curve
#   progress = min(100, (vSolInBondingCurve / 85e9) * 100)
```

#### Layer 2: Helius Webhooks (primary reliable, HTTP push with auto-retry)

```python
# POST https://api.helius.xyz/v0/webhooks?api-key={KEY}
{
    "webhookURL": "https://your-server/webhook/solana/new-pool",
    "transactionTypes": ["SWAP"],
    "accountAddresses": [
        "675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8",  # Raydium V4
        "whirLbMiicVdio4qvUfM5KAg6Ct8VwpYzGff3uctyCc"     # Orca Whirlpool
    ],
    "webhookType": "enhanced",
    "encoding": "jsonParsed"
}
# Advantage: no reconnection logic, automatic retries
```

#### Layer 3: Helius WebSocket (fallback if webhooks fail)

```python
# URL: wss://mainnet.helius-rpc.com/?api-key={KEY}

# Subscribe: Raydium V4 logs
→ SEND:
{
    "jsonrpc": "2.0", "id": 1,
    "method": "logsSubscribe",
    "params": [
        {"mentions": ["675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8"]},
        {"commitment": "confirmed"}
    ]
}
# Detect: "initialize2" in logs = NEW RAYDIUM POOL

# Subscribe: Orca Whirlpool logs
→ SEND:
{
    "jsonrpc": "2.0", "id": 2,
    "method": "logsSubscribe",
    "params": [
        {"mentions": ["whirLbMiicVdio4qvUfM5KAg6Ct8VwpYzGff3uctyCc"]},
        {"commitment": "confirmed"}
    ]
}

# Subscribe: Own wallet changes
→ SEND:
{
    "jsonrpc": "2.0", "id": 3,
    "method": "accountSubscribe",
    "params": ["<WALLET>", {"encoding": "jsonParsed", "commitment": "confirmed"}]
}

# Subscribe: TX confirmation
→ SEND:
{
    "jsonrpc": "2.0", "id": 4,
    "method": "signatureSubscribe",
    "params": ["<TX_SIG>", {"commitment": "confirmed"}]
}
```

### 4.3 Token Security API Calls

```python
# ═══ ON-CHAIN: Mint & Freeze Authority ═══════════
# POST {HELIUS_RPC}
{"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
 "params":["<MINT>",{"encoding":"jsonParsed"}]}
# → result.value.data.parsed.info:
#   mintAuthority: null = SAFE, address = DANGER (can mint infinite)
#   freezeAuthority: null = SAFE, address = DANGER (can freeze)

# ═══ HELIUS DAS: Token Metadata ═══════════════════
# POST {HELIUS_RPC}
{"jsonrpc":"2.0","id":1,"method":"getAsset","params":{"id":"<MINT>"}}

# ═══ Top Holders ══════════════════════════════════
# POST {HELIUS_RPC}
{"jsonrpc":"2.0","id":1,"method":"getTokenLargestAccounts","params":["<MINT>"]}

# ═══ BIRDEYE: Token Security ═════════════════════
# GET https://public-api.birdeye.so/defi/token_security?address={MINT}
# Headers: {"X-API-KEY": "{KEY}", "x-chain": "solana"}
# Response.data:
{
    "isMintable": false,          # CRITICAL
    "isFreezable": false,         # CRITICAL
    "top10HolderPercent": 45.2,   # >50% = warning
    "lpBurnedPercent": 95.0       # <80% = warning
}

# ═══ BIRDEYE: Token Overview ═════════════════════
# GET https://public-api.birdeye.so/defi/token_overview?address={MINT}
# Headers: {"X-API-KEY": "{KEY}", "x-chain": "solana"}
# Returns: price, volume, liquidity, priceChange, buys/sells, holders
```

### 4.4 Price Data

```python
# ═══ BIRDEYE OHLCV ═══════════════════════════════
# GET https://public-api.birdeye.so/defi/ohlcv
#   ?address={MINT}&type=1m&time_from={TS}&time_to={TS}
# Intervals: 1m, 3m, 5m, 15m, 30m, 1H, 4H, 1D

# ═══ DEXSCREENER (FREE, no key) ══════════════════
# GET https://api.dexscreener.com/latest/dex/tokens/{MINT}
# Returns: pairs[{priceUsd, volume, liquidity, priceChange, txns}]

# ═══ JUPITER PRICE (FREE, bulk) ══════════════════
# GET https://price.jup.ag/v6/price?ids={MINT1},{MINT2}&vsToken=So111...2
```

### 4.5 Trade Execution

```python
# ═══ JUPITER SWAP — Step 1: Quote ════════════════
# GET https://quote-api.jup.ag/v6/quote
#   ?inputMint=So111...2&outputMint={TOKEN}&amount={LAMPORTS}
#   &slippageBps=300&dynamicSlippage=true

# ═══ JUPITER SWAP — Step 2: Get TX ═══════════════
# POST https://quote-api.jup.ag/v6/swap
{
    "quoteResponse": "<FULL_QUOTE>",
    "userPublicKey": "<WALLET>",
    "wrapAndUnwrapSol": true,
    "dynamicComputeUnitLimit": true,
    "dynamicSlippage": true,
    "prioritizationFeeLamports": {
        "priorityLevelWithMaxLamports": {
            "maxLamports": 5000000,
            "priorityLevel": "veryHigh"
        }
    }
}
# Response: {"swapTransaction": "<BASE64_TX>"}

# ═══ Step 3: Sign & Send ═════════════════════════
# A) Standard RPC (faster, MEV-exposed):
#    POST {HELIUS_RPC} → sendTransaction
# B) Jito MEV Protection (recommended):
#    POST https://mainnet.block-engine.jito.wtf/api/v1/transactions
#    → sendTransaction (private mempool, no sandwich)

# ═══ PUMPPORTAL: Bonding Curve Trade ═════════════
# For tokens STILL ON bonding curve (pre-graduation)
# POST https://pumpportal.fun/api/trade-local
{
    "publicKey": "<WALLET>",
    "action": "buy",
    "mint": "<TOKEN_CA>",
    "denominatedInSol": "true",
    "amount": 0.1,
    "slippage": 15,
    "priorityFee": 0.00001,
    "pool": "pump"
}
# Returns raw TX bytes → sign locally → send via RPC/Jito
# Fee: 0.5% per trade

# ═══ TX Confirmation ═════════════════════════════
# POST {HELIUS_RPC}
{"jsonrpc":"2.0","id":1,"method":"getSignatureStatuses",
 "params":[["<SIG>"],{"searchTransactionHistory":true}]}
# → confirmationStatus: "confirmed" / "finalized", err: null = OK

# ═══ HELIUS Enhanced TX parsing ═══════════════════
# POST https://api.helius.xyz/v0/transactions/?api-key={KEY}
{"transactions": ["<SIG>"]}
# Returns: full parsed swap details, token transfers, fees
```

---

*Continued in Part 2: Base Adapter + Scoring + Execution + Infrastructure*
