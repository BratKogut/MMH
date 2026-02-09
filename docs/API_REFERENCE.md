# API Reference

Complete API endpoints and authentication details for all services used by Multi-Chain Memecoin Hunter v2.0.

## Table of Contents

1. [Solana APIs](#solana-apis)
2. [Base APIs](#base-apis)
3. [Security APIs](#security-apis)
4. [Price & Market Data](#price--market-data)
5. [DEX APIs](#dex-apis)
6. [Rate Limits](#rate-limits)

---

## Solana APIs

### Helius RPC

**Endpoints:**
- RPC: `https://mainnet.helius-rpc.com/?api-key={KEY}`
- WebSocket: `wss://mainnet.helius-rpc.com/?api-key={KEY}`
- Webhooks: `https://api.helius.xyz/v0/webhooks?api-key={KEY}`

**Authentication:** URL parameter `api-key`

**Plans:**
- Free: Limited
- Business: $199/mo (recommended for production)

**Methods:**
- `getAccountInfo` — Get mint/freeze authority
- `getAsset` — Token metadata (DAS)
- `getTokenLargestAccounts` — Top token holders
- `logsSubscribe` — Subscribe to program logs
- `accountSubscribe` — Monitor wallet changes
- `signatureSubscribe` — Track TX confirmation
- `sendTransaction` — Send signed TX

**Rate Limits:** Plan-dependent (typically 10 calls/sec for Business)

---

### PumpPortal WebSocket

**Endpoint:** `wss://pumpportal.fun/api/data`

**Authentication:** None (free tier)

**Subscriptions:**

```json
// New token creation
{"method": "subscribeNewToken"}

// Migration (graduation)
{"method": "subscribeMigration"}

// Token trades
{"method": "subscribeTokenTrade", "keys": ["<MINT_CA>"]}

// Whale wallet tracking
{"method": "subscribeAccountTrade", "keys": ["<WALLET>"]}

// Unsubscribe
{"method": "unsubscribeNewToken"}
{"method": "unsubscribeTokenTrade", "keys": ["<MINT_CA>"]}
```

**Response Format:**

```json
{
    "signature": "5K7x...",
    "mint": "ABcd...pump",
    "traderPublicKey": "7Xyz...",
    "txType": "create|buy|sell|migrate",
    "initialBuy": 69000000,
    "bondingCurveKey": "BCur...",
    "vTokensInBondingCurve": 1000000000,
    "vSolInBondingCurve": 30000000,
    "marketCapSol": 30.5,
    "name": "DogCoin",
    "symbol": "DOG",
    "uri": "https://..."
}
```

**Rate Limits:** Rate-limited (no exact limit published)

**Cost:** FREE

---

### PumpPortal Trade API

**Endpoint:** `https://pumpportal.fun/api/trade-local`

**Method:** POST

**Request:**

```json
{
    "publicKey": "<WALLET>",
    "action": "buy|sell",
    "mint": "<TOKEN_CA>",
    "denominatedInSol": "true",
    "amount": 0.1,
    "slippage": 15,
    "priorityFee": 0.00001,
    "pool": "pump"
}
```

**Response:** Raw TX bytes (sign locally, send via RPC)

**Fee:** 0.5% per trade

**Cost:** FREE (fee-based)

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

**Cost:** FREE (tip-based)

---

## Base APIs

### Alchemy RPC (Base)

**Endpoints:**
- RPC: `https://base-mainnet.g.alchemy.com/v2/{KEY}`
- WebSocket: `wss://base-mainnet.g.alchemy.com/v2/{KEY}`

**Authentication:** URL parameter `v2/{KEY}`

**Plans:**
- Free: Limited
- Growth: $49/mo (recommended)

**Methods:**
- `eth_subscribe` — Subscribe to logs
- `eth_blockNumber` — Get latest block
- `eth_call` — Read contract state
- `eth_sendRawTransaction` — Send signed TX
- `eth_getTransactionReceipt` — Get TX receipt

**Rate Limits:** Plan-dependent (typically 5 calls/sec for Growth)

---

### 0x API (Base)

**Endpoints:**
- Price: `https://base.api.0x.org/swap/v1/price`
- Quote: `https://base.api.0x.org/swap/v1/quote`
- Swap: `https://base.api.0x.org/swap/v1/swap`

**Authentication:** Header `0x-api-key` (optional for free tier)

**Price Request:**

```
GET /swap/v1/price?sellToken=0x...&buyToken=0x...&sellAmount=1000000
```

**Response:**

```json
{
    "chainId": 8453,
    "price": "1.234",
    "estimatedPriceImpact": "0.01",
    "value": "0",
    "gasPrice": "1000000000",
    "gas": "150000",
    "estimatedGas": "150000",
    "protocolFees": [],
    "buyAmount": "1234000",
    "sellTokenAddress": "0x...",
    "buyTokenAddress": "0x...",
    "sources": [...]
}
```

**Rate Limits:** 100k calls/month (free tier)

**Cost:** FREE

---

## Security APIs

### Birdeye Security

**Endpoint:** `https://public-api.birdeye.so/defi/token_security`

**Authentication:** Header `X-API-KEY`

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

**Plans:**
- Free: Limited
- Standard: $99/mo (recommended)

**Rate Limits:** Plan-dependent

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
        "priceChange": {
            "h1": 5.2,
            "h24": 12.3,
            "h7d": 45.6
        },
        "volume": {
            "h1": 50000,
            "h24": 500000,
            "h7d": 5000000
        },
        "liquidity": 100000,
        "holderCount": 1234,
        "buys": 567,
        "sells": 234
    }
}
```

**Rate Limits:** Plan-dependent

---

### GoPlus Security (Base/BSC)

**Endpoint:** `https://api.gopluslabs.io/api/v1/token_security/{CHAIN_ID}`

**Parameters:**
- `contract_addresses` — Token address (comma-separated)

**Chain IDs:**
- Base: `8453`
- BSC: `56`

**Request:**

```
GET /api/v1/token_security/8453?contract_addresses=0x...
```

**Response:**

```json
{
    "code": "1",
    "message": "OK",
    "result": {
        "0x...": {
            "is_honeypot": "0",
            "is_mintable": "0",
            "owner_change_balance": "0",
            "hidden_owner": "0",
            "selfdestruct": "0",
            "transfer_pausable": "0",
            "is_proxy": "0",
            "buy_tax": "0.01",
            "sell_tax": "0.01",
            "holder_count": "1234",
            "lp_holder_count": "56",
            "holders": [...]
        }
    }
}
```

**Rate Limits:** 30 calls/minute

**Cost:** FREE

---

### Basescan API

**Endpoint:** `https://api.basescan.org/api`

**Authentication:** Query parameter `apikey`

**Get Contract Source Code:**

```
GET /api?module=contract&action=getsourcecode&address=0x...&apikey={KEY}
```

**Response:**

```json
{
    "status": "1",
    "message": "OK",
    "result": [
        {
            "SourceCode": "...",
            "ABI": "...",
            "ContractName": "Token",
            "CompilerVersion": "v0.8.0",
            "Verified": "1"
        }
    ]
}
```

**Rate Limits:** 5 calls/second

**Cost:** FREE

---

## Price & Market Data

### Jupiter Price API

**Endpoint:** `https://price.jup.ag/v6/price`

**Request:**

```
GET /v6/price?ids={MINT1},{MINT2}&vsToken=So11111111111111111111111111111111111111112
```

**Response:**

```json
{
    "data": {
        "So11111111111111111111111111111111111111112": {
            "id": "So11111111111111111111111111111111111111112",
            "mintSymbol": "SOL",
            "vsToken": "So11111111111111111111111111111111111111112",
            "vsTokenSymbol": "SOL",
            "price": "1.0"
        },
        "EPjFWaLb3odccjf2cj6ipjsPSEtQDDUSqualEhUWQjv": {
            "id": "EPjFWaLb3odccjf2cj6ipjsPSEtQDDUSqualEhUWQjv",
            "mintSymbol": "USDC",
            "vsToken": "So11111111111111111111111111111111111111112",
            "vsTokenSymbol": "SOL",
            "price": "0.0001234"
        }
    }
}
```

**Rate Limits:** Unlimited

**Cost:** FREE

---

### DexScreener API

**Endpoint:** `https://api.dexscreener.com/latest/dex/tokens/{TOKEN_ADDRESS}`

**Request:**

```
GET /latest/dex/tokens/{TOKEN_ADDRESS}
```

**Response:**

```json
{
    "schemaVersion": "1.0.0",
    "pairs": [
        {
            "chainId": "solana",
            "dexId": "raydium",
            "url": "https://dexscreener.com/solana/...",
            "pairAddress": "...",
            "baseToken": {
                "address": "...",
                "name": "DogCoin",
                "symbol": "DOG"
            },
            "quoteToken": {
                "address": "So11111111111111111111111111111111111111112",
                "name": "Wrapped SOL",
                "symbol": "SOL"
            },
            "priceUsd": "0.0001234",
            "priceNative": "0.00123",
            "priceChange": {
                "m5": 5.2,
                "h1": 12.3,
                "h24": 45.6
            },
            "volume": {
                "m5": 50000,
                "h1": 500000,
                "h24": 5000000
            },
            "liquidity": {
                "usd": 100000,
                "base": 1000000000,
                "quote": 123000
            },
            "txns": {
                "m5": {"buys": 10, "sells": 5},
                "h1": {"buys": 100, "sells": 50},
                "h24": {"buys": 1000, "sells": 500}
            }
        }
    ]
}
```

**Rate Limits:** Unlimited

**Cost:** FREE

---

## DEX APIs

### Jupiter Swap API

**Quote Endpoint:** `https://quote-api.jup.ag/v6/quote`

**Request:**

```
GET /v6/quote
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
    "marketInfos": [...],
    "routePlan": [...]
}
```

**Swap Endpoint:** `https://quote-api.jup.ag/v6/swap`

**Request:**

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

**Rate Limits:** Unlimited

**Cost:** FREE

---

### Uniswap V3 (Base)

**Router Address:** `0xE592427A0AEce92De3Edee1F18E0157C05861564`

**Methods:**
- `exactInputSingle` — Swap exact input amount
- `exactOutputSingle` — Swap for exact output amount

**Fee Tiers:**
- 0.01% (100 bps)
- 0.05% (500 bps)
- 0.30% (3000 bps)
- 1.00% (10000 bps) — Most common for memecoins

**Cost:** Gas fees only

---

### Aerodrome (Base)

**Router Address:** `0xcF77a3Ba9A5CA922335EaC26B0A7DB85d5E7e907`

**Methods:**
- `swapExactTokensForTokens` — Swap exact input
- `swapTokensForExactTokens` — Swap for exact output

**Pool Types:**
- Stable pools (for stablecoins)
- Volatile pools (for memecoins)

**Cost:** Gas fees only

---

## Rate Limits

### Summary Table

| Service | Limit | Notes |
|---------|-------|-------|
| Helius RPC | Plan-dependent | 10/sec for Business |
| PumpPortal WS | Rate-limited | No exact limit |
| Birdeye | Plan-dependent | 60/min for Standard |
| GoPlus | 30/min | Free tier |
| Basescan | 5/sec | Free tier |
| Jupiter | Unlimited | Free tier |
| DexScreener | Unlimited | Free tier |
| Alchemy (Base) | Plan-dependent | 5/sec for Growth |
| 0x | 100k/month | Free tier |

### Rate Limiting Strategy

**Token Bucket Implementation:**

```python
RATE_LIMITS = {
    "birdeye":     1.0,   # calls/sec (~60/min)
    "goplus":      0.5,   # 30/min
    "basescan":    5.0,   # 5/sec
    "helius":      10.0,  # plan-based
    "dexscreener": 5.0,   # generous
}
```

**Exponential Backoff:**
- Attempt 1: immediate
- Attempt 2: wait 1s
- Attempt 3: wait 2s
- Attempt 4: wait 4s (max 30s)

---

## Authentication

### API Key Management

Store all API keys in environment variables:

```bash
export HELIUS_API_KEY="your_key_here"
export ALCHEMY_API_KEY="your_key_here"
export BIRDEYE_API_KEY="your_key_here"
export BASESCAN_API_KEY="your_key_here"
export ZERO_X_API_KEY="your_key_here"
```

### Security Best Practices

1. **Never commit API keys** to version control
2. **Use .env files** with .gitignore
3. **Rotate keys regularly** (monthly recommended)
4. **Use read-only keys** where possible
5. **Monitor usage** for unusual activity
6. **Set IP whitelists** if available

---

## Error Handling

### Common HTTP Status Codes

| Code | Meaning | Action |
|------|---------|--------|
| 200 | OK | Process response |
| 400 | Bad Request | Check parameters |
| 401 | Unauthorized | Check API key |
| 403 | Forbidden | Check permissions |
| 429 | Rate Limited | Backoff and retry |
| 500 | Server Error | Retry with backoff |
| 503 | Service Unavailable | Fallback to backup |

### Retry Strategy

```python
async def call_with_retry(api_call, max_attempts=4):
    for attempt in range(max_attempts):
        try:
            return await api_call()
        except RateLimitError:
            wait_time = min(2 ** attempt, 30)
            await asyncio.sleep(wait_time)
        except Exception as e:
            if attempt == max_attempts - 1:
                raise
            await asyncio.sleep(2 ** attempt)
```

---

*Last Updated: 2026-02-09*
