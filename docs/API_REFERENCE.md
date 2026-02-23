# API Reference

API endpoints for all services used by NEXUS Memecoin Hunter v3.2.

## Table of Contents

1. [Hunter HTTP API](#hunter-http-api)
2. [Solana APIs](#solana-apis)
3. [DEX APIs](#dex-apis)
4. [Security APIs](#security-apis)
5. [Price & Market Data](#price--market-data)
6. [Rate Limits](#rate-limits)

---

## Hunter HTTP API

The nexus-hunter binary exposes an HTTP API on port 9092 (PrometheusPort + 2).

### Health

**GET /health**

```json
{
    "status": "ok",
    "dry_run": true,
    "paused": false,
    "killed": false
}
```

### Stats

**GET /stats**

Returns combined statistics from all modules:

```json
{
    "sniper": {
        "total_snipes": 42,
        "total_sells": 38,
        "win_count": 25,
        "loss_count": 13,
        "open_positions": 3,
        "daily_spent_sol": "1.5",
        "daily_loss_sol": "0.3"
    },
    "scanner": {
        "pools_discovered": 1250,
        "l0_passed": 340,
        "l0_dropped": 910,
        "analyzed": 340
    },
    "graph": {
        "node_count": 15000,
        "edge_count": 28000,
        "query_count": 340
    },
    "liquidity": { "tracked_tokens": 50 },
    "narrative": { "active_narratives": 7 },
    "honeypot": { "known_signatures": 12 },
    "correlation": { "active_clusters": 8 },
    "copytrade": { "tracked_wallets": 5, "active_signals": 3 },
    "adaptive_weights": {
        "outcomes_recorded": 38,
        "last_recalc": "2026-02-23T10:30:00Z",
        "current_weights": {
            "safety": 0.32, "entity": 0.14,
            "social": 0.19, "onchain": 0.21, "timing": 0.14
        }
    }
}
```

### Positions

**GET /positions**

Returns all positions (open and closed).

**GET /positions/open**

Returns only currently open positions:

```json
[
    {
        "id": "uuid-1234",
        "token_mint": "ABcd...pump",
        "pool_address": "Pool...",
        "dex": "raydium",
        "entry_price_usd": 0.0001234,
        "amount_token": 1000000,
        "cost_sol": 0.1,
        "current_price": 0.0002468,
        "highest_price": 0.0003000,
        "pnl_pct": 100.0,
        "safety_score": 72,
        "status": "OPEN",
        "opened_at": "2026-02-23T09:15:00Z"
    }
]
```

### Control Plane

**POST /control/pause**

Soft pause: stops opening new positions, continues managing existing ones.

**POST /control/resume**

Resume from paused state.

**POST /control/kill**

Hard kill: force closes all open positions, halts system.

**GET /control/status**

```json
{
    "paused": false,
    "killed": false,
    "dry_run": true,
    "instance_id": "nexus-1",
    "open_positions": 3
}
```

### Copy-Trade Wallet Management

**GET /copytrade/wallets**

```json
[
    {
        "address": "7Xyz...",
        "tier": "whale",
        "label": "Known whale",
        "win_rate": 0.65,
        "avg_roi": 120.5,
        "trade_count": 48
    }
]
```

**POST /copytrade/wallets**

```json
{
    "address": "8Abc...",
    "tier": "smart_money",
    "label": "Smart money wallet"
}
```

**DELETE /copytrade/wallets**

```json
{
    "address": "8Abc..."
}
```

---

## Solana APIs

### Helius RPC

**Endpoints:**
- RPC: `https://mainnet.helius-rpc.com/?api-key={KEY}`
- WebSocket: `wss://mainnet.helius-rpc.com/?api-key={KEY}`

**Authentication:** URL parameter `api-key`

**Methods used by nexus-hunter:**
- `getAccountInfo` — Get mint/freeze authority
- `getTokenLargestAccounts` — Top token holders
- `logsSubscribe` — Subscribe to program logs (pool detection)
- `sendTransaction` — Send signed TX

**Rate Limits:** Plan-dependent (10 calls/sec for Business plan)

**Cost:** $199/mo (Business plan recommended)

---

### Jito MEV Protection

**Endpoint:** `https://mainnet.block-engine.jito.wtf/api/v1/transactions`

**Method:** POST

**Request:**

```json
{
    "jsonrpc": "2.0",
    "id": 1,
    "method": "sendTransaction",
    "params": ["<BASE64_TX>"]
}
```

**Benefit:** Private mempool, no sandwich attacks

**Cost:** FREE (tip included in TX)

---

## DEX APIs

### Jupiter V6

**Quote:** `GET https://quote-api.jup.ag/v6/quote`

```
?inputMint=So11111111111111111111111111111111111111112
&outputMint={TOKEN}
&amount={LAMPORTS}
&slippageBps=300
&dynamicSlippage=true
```

**Response:**

```json
{
    "inputMint": "So11111111111111111111111111111111111111112",
    "inAmount": "1000000",
    "outputMint": "...",
    "outAmount": "1234000",
    "otherAmountThreshold": "1200000",
    "swapMode": "ExactIn",
    "priceImpactPct": "0.5",
    "routePlan": [...]
}
```

**Swap:** `POST https://quote-api.jup.ag/v6/swap`

```json
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
```

**Response:**

```json
{
    "swapTransaction": "<BASE64_TX>",
    "lastValidBlockHeight": 123456789
}
```

**Price:** `GET https://price.jup.ag/v6/price?ids={MINT1},{MINT2}&vsToken=So111...2`

**Rate Limits:** Unlimited | **Cost:** FREE

---

## Security APIs

### Birdeye Security (Solana)

**Endpoint:** `https://public-api.birdeye.so/defi/token_security`

**Request:**

```
GET /defi/token_security?address={MINT}
Headers: {"X-API-KEY": "{KEY}", "x-chain": "solana"}
```

**Response:**

```json
{
    "data": {
        "isMintable": false,
        "isFreezable": false,
        "top10HolderPercent": 45.2,
        "lpBurnedPercent": 95.0,
        "holderCount": 1234,
        "lpHolderCount": 56
    }
}
```

**Rate Limits:** Plan-dependent (~60/min for Standard)

**Cost:** $99/mo (Standard plan)

---

### Birdeye Token Overview

**Endpoint:** `https://public-api.birdeye.so/defi/token_overview`

**Request:**

```
GET /defi/token_overview?address={MINT}
Headers: {"X-API-KEY": "{KEY}", "x-chain": "solana"}
```

**Response:**

```json
{
    "data": {
        "address": "...",
        "symbol": "DOG",
        "name": "DogCoin",
        "decimals": 6,
        "price": 0.0001234,
        "priceChange": { "h1": 5.2, "h24": 12.3 },
        "volume": { "h1": 50000, "h24": 500000 },
        "liquidity": 100000,
        "holderCount": 1234,
        "buys": 567,
        "sells": 234
    }
}
```

---

### DexScreener

**Endpoint:** `https://api.dexscreener.com/latest/dex/tokens/{TOKEN_ADDRESS}`

**Response:**

```json
{
    "pairs": [
        {
            "chainId": "solana",
            "dexId": "raydium",
            "pairAddress": "...",
            "baseToken": { "address": "...", "name": "DogCoin", "symbol": "DOG" },
            "quoteToken": { "address": "So11...2", "symbol": "SOL" },
            "priceUsd": "0.0001234",
            "volume": { "h1": 500000, "h24": 5000000 },
            "liquidity": { "usd": 100000 },
            "txns": {
                "h1": { "buys": 100, "sells": 50 },
                "h24": { "buys": 1000, "sells": 500 }
            }
        }
    ]
}
```

**Rate Limits:** Unlimited | **Cost:** FREE

---

## Rate Limits

### Summary

| Service | Limit | Auth |
|---------|-------|------|
| Helius RPC | Plan-dependent (10/sec Business) | URL param |
| Jupiter V6 | Unlimited | None |
| Birdeye | Plan-dependent (~60/min Standard) | Header: X-API-KEY |
| DexScreener | Unlimited | None |
| Jito | Unlimited | None |

### Cost Summary (Solana MVP)

| Provider | Plan | $/mo |
|----------|------|------|
| Helius | Business | $199 |
| Birdeye | Standard | $99 |
| Jupiter | Free | $0 |
| DexScreener | Free | $0 |
| Jito | Free (tip in TX) | $0 |
| VPS (Hetzner) | 64GB RAM | $45 |
| **TOTAL** | | **~$343/mo** |

---

## Authentication

### API Key Management

Store all API keys in environment variables or YAML config:

```yaml
solana:
  rpc_endpoint: "https://mainnet.helius-rpc.com/?api-key=YOUR_HELIUS_KEY"
  ws_endpoint: "wss://mainnet.helius-rpc.com/?api-key=YOUR_HELIUS_KEY"
  private_key: "YOUR_WALLET_PRIVATE_KEY_BASE58"
```

### Security Best Practices

1. Never commit API keys to version control
2. Use .gitignore for config files with keys
3. Rotate keys regularly
4. Use read-only keys where possible
5. Monitor usage for unusual activity

---

## Error Handling

### Common HTTP Status Codes

| Code | Meaning | Action |
|------|---------|--------|
| 200 | OK | Process response |
| 400 | Bad Request | Check parameters |
| 429 | Rate Limited | Backoff and retry |
| 500 | Server Error | Retry with backoff |
| 503 | Service Unavailable | Fallback |

### Retry Strategy (Go implementation)

Exponential backoff with max retries:
- Attempt 1: immediate
- Attempt 2: wait 1s
- Attempt 3: wait 2s
- Attempt 4: wait 4s (max 30s)

---

*Last Updated: 2026-02-23*
