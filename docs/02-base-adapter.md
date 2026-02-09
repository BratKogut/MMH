    AERO_POOL_CREATED: str = "0x2128d88d14c80cb081c1252a5acff7a264671bf199ce226b53788fb26065005e"

    # Safety limits
    MAX_TRADE_ETH: float = 0.1
    MAX_SLIPPAGE_PCT: float = 5.0
    MAX_BUY_TAX: float = 10.0
    MAX_SELL_TAX: float = 10.0
    REQUIRE_VERIFIED: bool = False    # Many new tokens unverified initially

    # Wallet
    PRIVATE_KEY: str = ""


class BaseAdapter(BaseChainAdapter):
    chain = ChainId.BASE

    KNOWN_TOKENS = {
        "0x4200000000000000000000000000000000000006",  # WETH
        "0x833589fcd6edb6e08f4c7c32d4f71b54bda02913",  # USDC
        "0xd9aaec86b65d86f6a7b5b1b0c42ffa531710b6ca",  # USDbC
    }

    def __init__(self, config: BaseConfig, redis_client: redis.Redis):
        super().__init__(redis_client, config.__dict__)
        self.cfg = config
        self._session: Optional[aiohttp.ClientSession] = None
        self._ws = None
        self._tasks: list[asyncio.Task] = []

        # Web3 for TX building & sending
        self.w3 = Web3(Web3.HTTPProvider(config.ALCHEMY_RPC))
        self.w3.middleware_onion.inject(ExtraDataToPOAMiddleware, layer=0)

    # ── LIFECYCLE ─────────────────────────────────────────
    async def start(self):
        self._session = aiohttp.ClientSession(
            timeout=aiohttp.ClientTimeout(total=30)
        )
        self._tasks = [
            asyncio.create_task(self._ws_loop()),
            asyncio.create_task(self._heartbeat_loop()),
        ]

    async def stop(self):
        for task in self._tasks:
            task.cancel()
        if self._session:
            await self._session.close()

    async def health_check(self) -> dict:
        return {
            "chain": self.chain.value,
            "ws_connected": self._ws is not None,
            "circuit_open": self._circuit_open,
            "ws_failures": self._ws_failures,
            "latest_block": self.w3.eth.block_number,
        }

    # ══════════════════════════════════════════════════════
    # DISCOVERY: WebSocket — Uniswap V3 + Aerodrome
    # ══════════════════════════════════════════════════════
    async def _ws_loop(self):
        """Monitor BOTH Uniswap V3 and Aerodrome pool creation"""
        while True:
            if self._circuit_open:
                await asyncio.sleep(10)
                continue
            try:
                async with websockets.connect(
                    self.cfg.ALCHEMY_WS,
                    ping_interval=30,
                    ping_timeout=10,
                    max_size=4 * 1024 * 1024
                ) as ws:
                    self._ws = ws
                    self._on_ws_success()

                    # Subscribe: Uniswap V3 PoolCreated
                    await ws.send(json.dumps({
                        "jsonrpc": "2.0", "id": 1,
                        "method": "eth_subscribe",
                        "params": ["logs", {
                            "address": self.cfg.UNI_V3_FACTORY,
                            "topics": [self.cfg.UNI_POOL_CREATED]
                        }]
                    }))

                    # Subscribe: Aerodrome V2 PoolCreated
                    await ws.send(json.dumps({
                        "jsonrpc": "2.0", "id": 2,
                        "method": "eth_subscribe",
                        "params": ["logs", {
                            "address": self.cfg.AERO_FACTORY,
                            "topics": [self.cfg.AERO_POOL_CREATED]
                        }]
                    }))

                    async for message in ws:
                        data = json.loads(message)
                        if "params" in data:
                            log = data["params"]["result"]
                            factory = log.get("address", "").lower()

                            if factory == self.cfg.UNI_V3_FACTORY.lower():
                                await self._handle_uni_pool_created(log)
                            elif factory == self.cfg.AERO_FACTORY.lower():
                                await self._handle_aero_pool_created(log)

            except Exception as e:
                self._ws = None
                await self._on_ws_failure(f"base_ws: {e}")

    async def _handle_uni_pool_created(self, log: dict):
        """Decode Uniswap V3 PoolCreated → find new token → publish"""
        topics = log.get("topics", [])
        if len(topics) < 4:
            return

        token0 = "0x" + topics[1][-40:]
        token1 = "0x" + topics[2][-40:]
        fee = int(topics[3], 16)
        pool = "0x" + log["data"][-40:]

        new_token = self._identify_new_token(token0, token1)
        if not new_token:
            return

        await self._process_new_base_token(
            new_token, pool, "uniswap_v3", fee=fee,
            tx_hash=log.get("transactionHash")
        )

    async def _handle_aero_pool_created(self, log: dict):
        """Decode Aerodrome PoolCreated → find new token → publish"""
        topics = log.get("topics", [])
        if len(topics) < 4:
            return

        token0 = "0x" + topics[1][-40:]
        token1 = "0x" + topics[2][-40:]
        stable = int(topics[3], 16) == 1
        # pool address is in first 32 bytes of data
        data_hex = log.get("data", "0x")[2:]
        pool = "0x" + data_hex[24:64]  # strip left padding

        new_token = self._identify_new_token(token0, token1)
        if not new_token:
            return

        # Skip stable pools for memecoins (volatile only)
        if stable:
            return

        await self._process_new_base_token(
            new_token, pool, "aerodrome",
            stable=stable, tx_hash=log.get("transactionHash")
        )

    def _identify_new_token(self, token0: str, token1: str) -> Optional[str]:
        """Return the non-WETH/non-USDC token, or None if both known"""
        t0 = token0.lower()
        t1 = token1.lower()
        known = {k.lower() for k in self.KNOWN_TOKENS}

        if t0 not in known and t1 in known:
            return token0
        elif t1 not in known and t0 in known:
            return token1
        return None  # Both known or both unknown

    async def _process_new_base_token(self, token: str, pool: str,
                                       dex: str, **kwargs):
        """Security check + publish for any new Base token"""
        security, dex_data = await asyncio.gather(
            self.check_security_cached(token),
            self._get_dexscreener(token),
            return_exceptions=True
        )

        event = TokenEvent(
            chain=ChainId.BASE,
            address=token,
            name="", symbol="",
            creator="",
            timestamp=time.time(),
            launchpad=dex,
            pool_address=pool,
            dex=dex,
            tx_hash=kwargs.get("tx_hash")
        )

        # Enrich from dexscreener
        if isinstance(dex_data, dict) and dex_data.get("pairs"):
            pair = dex_data["pairs"][0]
            event.name = pair.get("baseToken", {}).get("name", "")
            event.symbol = pair.get("baseToken", {}).get("symbol", "")
            event.initial_liquidity_usd = \
                pair.get("liquidity", {}).get("usd")

        await self.redis.xadd(
            f"tokens:new:{self.chain.value}",
            {"data": json.dumps({
                **event.__dict__,
                "security": security.__dict__
                    if isinstance(security, SecurityResult) else None,
                "dex_extra": kwargs,
                "source": f"ws_{dex}"
            }, default=str)},
            maxlen=5000
        )

    # ══════════════════════════════════════════════════════
    # SECURITY
    # ══════════════════════════════════════════════════════
    async def check_security(self, address: str) -> SecurityResult:
        """GoPlus + optional Basescan verification"""
        flags = []
        is_safe = True

        # 1. GoPlus Security
        try:
            async with self._session.get(
                f"https://api.gopluslabs.io/api/v1/token_security/8453",
                params={"contract_addresses": address}
            ) as resp:
                if resp.status == 200:
                    gp = (await resp.json()).get("result", {}) \
                        .get(address.lower(), {})

                    checks = [
                        ("is_honeypot", "1", "HONEYPOT"),
                        ("is_mintable", "1", "MINTABLE"),
                        ("owner_change_balance", "1", "OWNER_CAN_DRAIN"),
                        ("hidden_owner", "1", "HIDDEN_OWNER"),
                        ("selfdestruct", "1", "SELFDESTRUCT"),
                        ("transfer_pausable", "1", "PAUSABLE"),
                    ]

                    for key, bad_val, flag in checks:
                        if gp.get(key) == bad_val:
                            flags.append(flag)
                            is_safe = False

                    # Tax checks
                    buy_tax = float(gp.get("buy_tax", "0")) * 100
                    sell_tax = float(gp.get("sell_tax", "0")) * 100

                    if buy_tax > self.cfg.MAX_BUY_TAX:
                        flags.append(f"HIGH_BUY_TAX:{buy_tax:.1f}%")
                        is_safe = False
                    if sell_tax > self.cfg.MAX_SELL_TAX:
                        flags.append(f"HIGH_SELL_TAX:{sell_tax:.1f}%")
                        is_safe = False

                    # Proxy check (warning, not hard block)
                    if gp.get("is_proxy") == "1":
                        flags.append("UPGRADEABLE_PROXY")
                else:
                    flags.append("GOPLUS_CHECK_FAILED")
        except Exception:
            flags.append("GOPLUS_UNREACHABLE")

        # 2. Basescan verification (optional, non-blocking)
        if self.cfg.REQUIRE_VERIFIED:
            try:
                async with self._session.get(
                    "https://api.basescan.org/api",
                    params={
                        "module": "contract",
                        "action": "getsourcecode",
                        "address": address,
                        "apikey": self.cfg.BASESCAN_KEY
                    }
                ) as resp:
                    source = (await resp.json()).get("result", [{}])[0]
                    if source.get("ABI") == \
                       "Contract source code not verified":
                        flags.append("UNVERIFIED_CONTRACT")
            except Exception:
                pass

        risk = "LOW" if is_safe else (
            "CRITICAL" if any(f in ("HONEYPOT", "OWNER_CAN_DRAIN",
                                     "SELFDESTRUCT") for f in flags)
            else "HIGH"
        )

        return SecurityResult(
            is_safe=is_safe,
            flags=flags,
            risk_level=risk,
            checked_at=time.time()
        )

    # ══════════════════════════════════════════════════════
    # PRICE DATA
    # ══════════════════════════════════════════════════════
    async def get_price_data(self, address: str) -> PriceData:
        dex = await self._get_dexscreener(address)
        if not dex.get("pairs"):
            return PriceData(
                chain=ChainId.BASE, token_address=address,
                price_usd=0, volume_1h=0, volume_24h=0,
                liquidity_usd=0, price_change_1h=0,
                price_change_24h=0, buys_1h=0, sells_1h=0,
                timestamp=time.time()
            )

        pair = dex["pairs"][0]
        return PriceData(
            chain=ChainId.BASE,
            token_address=address,
            price_usd=float(pair.get("priceUsd", 0)),
            volume_1h=pair.get("volume", {}).get("h1", 0),
            volume_24h=pair.get("volume", {}).get("h24", 0),
            liquidity_usd=pair.get("liquidity", {}).get("usd", 0),
            price_change_1h=pair.get("priceChange", {}).get("h1", 0),
            price_change_24h=pair.get("priceChange", {}).get("h24", 0),
            buys_1h=pair.get("txns", {}).get("h1", {}).get("buys", 0),
            sells_1h=pair.get("txns", {}).get("h1", {}).get("sells", 0),
            timestamp=time.time()
        )

    async def get_holder_distribution(self, address: str) -> dict:
        # EVM doesn't have native holder enumeration via RPC
        # Use Basescan or GoPlus data
        try:
            async with self._session.get(
                f"https://api.gopluslabs.io/api/v1/token_security/8453",
                params={"contract_addresses": address}
            ) as resp:
                gp = (await resp.json()).get("result", {}) \
                    .get(address.lower(), {})
                return {
                    "holder_count": int(gp.get("holder_count", 0)),
                    "lp_holder_count": int(gp.get("lp_holder_count", 0)),
                    "top_holders": gp.get("holders", [])[:10]
                }
        except Exception:
            return {"holder_count": 0, "lp_holder_count": 0, "top_holders": []}

    # ══════════════════════════════════════════════════════
    # EXECUTION
    # ══════════════════════════════════════════════════════
    async def get_quote(self, params: SwapParams) -> dict:
        """Get best price via 0x aggregator"""
        is_eth_in = params.token_in.lower() == self.cfg.WETH.lower() or \
                    params.token_in == "0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE"

        async with self._session.get(
            "https://base.api.0x.org/swap/v1/price",
            params={
                "sellToken": "0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE"
                    if is_eth_in else params.token_in,
                "buyToken": params.token_out,
                "sellAmount": str(params.amount_raw)
            },
            headers={"0x-api-key": self.cfg.ZERO_X_KEY}
        ) as resp:
            return await resp.json()

    async def execute_swap(self, params: SwapParams) -> SwapResult:
        """SAFETY-FIRST EVM swap via Uniswap V3"""

        WETH = self.cfg.WETH
        is_buy = params.token_in.lower() == WETH.lower()

        # ── SAFETY CHECK 1: Amount limit ──────────────────
        if is_buy:
            eth_amount = params.amount_raw / 1e18
            if eth_amount > self.cfg.MAX_TRADE_ETH:
                return SwapResult(
                    success=False, tx_hash="",
                    error=f"SAFETY: {eth_amount:.4f} ETH > "
                          f"max {self.cfg.MAX_TRADE_ETH}"
                )

        # ── SAFETY CHECK 2: Fresh security (pre-execution) ─
        target = params.token_out if is_buy else params.token_in
        if target.lower() not in {k.lower() for k in self.KNOWN_TOKENS}:
            security = await self.check_security_fresh(target)
            if not security.is_safe:
                return SwapResult(
                    success=False, tx_hash="",
                    error=f"SAFETY: {security.flags}"
                )

        # ── STEP 1: Get quote for min output ──────────────
        quote = await self.get_quote(params)
        if "buyAmount" not in quote:
            return SwapResult(
                success=False, tx_hash="",
                error=f"Quote failed: {quote}"
            )

        buy_amount = int(quote["buyAmount"])
        slippage = params.slippage_bps / 10000
        min_out = int(buy_amount * (1 - slippage))

        # ── STEP 2: Build TX ──────────────────────────────
        wallet = self.w3.eth.account.from_key(self.cfg.PRIVATE_KEY)

        # Uniswap V3 exactInputSingle
        router = self.w3.eth.contract(
            address=Web3.to_checksum_address(self.cfg.UNI_V3_ROUTER),
            abi=UNISWAP_V3_ROUTER_ABI  # defined separately
        )

        swap_params = {
            "tokenIn": Web3.to_checksum_address(WETH),
            "tokenOut": Web3.to_checksum_address(params.token_out),
            "fee": 10000,  # 1% tier (most common for memecoins)
            "recipient": wallet.address,
            "amountIn": params.amount_raw,
            "amountOutMinimum": min_out,
            "sqrtPriceLimitX96": 0
        }

        gas_price = self.w3.eth.gas_price
        tx = router.functions.exactInputSingle(swap_params) \
            .build_transaction({
                "from": wallet.address,
                "value": params.amount_raw if is_buy else 0,
                "gas": 350000,
                "maxFeePerGas": gas_price * 2,
                "maxPriorityFeePerGas": self.w3.to_wei(0.001, "gwei"),
                "nonce": self.w3.eth.get_transaction_count(wallet.address),
                "chainId": self.cfg.CHAIN_ID,
            })

        # ── STEP 3: Sign & Send ───────────────────────────
        signed = self.w3.eth.account.sign_transaction(
            tx, self.cfg.PRIVATE_KEY
        )
        try:
            tx_hash = self.w3.eth.send_raw_transaction(
                signed.raw_transaction
            )
        except Exception as e:
            return SwapResult(
                success=False, tx_hash="",
                error=f"TX send failed: {e}"
            )

        # ── STEP 4: Confirm ───────────────────────────────
        try:
            receipt = self.w3.eth.wait_for_transaction_receipt(
                tx_hash, timeout=params.deadline_seconds
            )
            success = receipt["status"] == 1
        except Exception as e:
            return SwapResult(
                success=False,
                tx_hash=tx_hash.hex(),
                error=f"TX confirmation failed: {e}"
            )

        result = SwapResult(
            success=success,
            tx_hash=tx_hash.hex(),
            amount_out=buy_amount,
            gas_cost_usd=None,  # calculate from receipt
            confirmed=success,
            error=None if success else "TX reverted"
        )

        await self.publish_trade_result(result, params)
        return result

    # ── INTERNAL HELPERS ──────────────────────────────────
    async def _get_dexscreener(self, token: str) -> dict:
        async with self._session.get(
            f"https://api.dexscreener.com/latest/dex/tokens/{token}"
        ) as resp:
            return await resp.json()

    async def _heartbeat_loop(self):
        while True:
            status = await self.health_check()
            await self.publish_health(status)
            await asyncio.sleep(10)

    async def subscribe_new_tokens(self):
        pass  # Handled by _ws_loop

    async def get_token_metadata(self, address: str) -> dict:
        # Read ERC-20 standard fields
        try:
            token = self.w3.eth.contract(
                address=Web3.to_checksum_address(address),
                abi=ERC20_ABI
            )
            return {
                "name": token.functions.name().call(),
                "symbol": token.functions.symbol().call(),
                "decimals": token.functions.decimals().call(),
                "totalSupply": str(token.functions.totalSupply().call()),
            }
        except Exception:
            return {}


# ABI stubs (minimal for compilation)
UNISWAP_V3_ROUTER_ABI = [{"inputs":[{"components":[
    {"name":"tokenIn","type":"address"},
    {"name":"tokenOut","type":"address"},
    {"name":"fee","type":"uint24"},
    {"name":"recipient","type":"address"},
    {"name":"amountIn","type":"uint256"},
    {"name":"amountOutMinimum","type":"uint256"},
    {"name":"sqrtPriceLimitX96","type":"uint160"}
],"name":"params","type":"tuple"}],
"name":"exactInputSingle",
"outputs":[{"name":"amountOut","type":"uint256"}],
"type":"function"}]

ERC20_ABI = [
    {"constant":True,"inputs":[],"name":"name","outputs":[{"type":"string"}],"type":"function"},
    {"constant":True,"inputs":[],"name":"symbol","outputs":[{"type":"string"}],"type":"function"},
    {"constant":True,"inputs":[],"name":"decimals","outputs":[{"type":"uint8"}],"type":"function"},
    {"constant":True,"inputs":[],"name":"totalSupply","outputs":[{"type":"uint256"}],"type":"function"},
    {"constant":True,"inputs":[{"name":"","type":"address"}],"name":"balanceOf","outputs":[{"type":"uint256"}],"type":"function"},
]
```

---

## 5. UNIFIED EVENT BUS (Redis Streams)

### 5.1 Stream Architecture

```
REDIS STREAMS:
═══════════════════════════════════════════════════════════

DISCOVERY (per-chain, niezależny backpressure):
├── tokens:new:solana          ← SolanaAdapter publishes
├── tokens:new:base            ← BaseAdapter publishes
├── tokens:new:bsc             ← (future)
├── tokens:new:ton             ← (future)
├── tokens:graduated:solana    ← pump.fun → Raydium migrations
└── trades:bonding:solana      ← bonding curve trades (pre-graduation)

SCORING & ALERTS:
├── scoring:requests           ← Discovery → Scoring worker
├── scoring:results            ← Scoring → alert / execution
├── alerts:telegram            ← → Telegram bot sends
└── alerts:system              ← circuit breaker, errors

EXECUTION:
├── execution:requests         ← Manual or auto-triggered
├── trades:executed:solana     ← execution results
├── trades:executed:base       ← execution results
└── positions:updates          ← Position Manager events

MONITORING:
├── health:solana              ← adapter heartbeat
├── health:base                ← adapter heartbeat
├── health:scoring             ← scoring worker heartbeat
└── health:executor            ← execution worker heartbeat

DEAD LETTER:
└── dlq:{original_stream}     ← failed processing after N retries
```

### 5.2 Consumer Groups & XACK

```python
# ═══════════════════════════════════════════════════════════
# CONSUMER GROUP SETUP — Run once at startup
# ═══════════════════════════════════════════════════════════

async def setup_consumer_groups(redis_client: redis.Redis):
    """Create consumer groups for reliable processing"""
    streams_and_groups = {
        "tokens:new:solana": ["scoring_group", "alert_group"],
        "tokens:new:base": ["scoring_group", "alert_group"],
        "tokens:graduated:solana": ["scoring_group"],
        "scoring:results": ["executor_group", "alert_group"],
        "execution:requests": ["executor_group"],
        "trades:executed:solana": ["position_group"],
        "trades:executed:base": ["position_group"],
    }

    for stream, groups in streams_and_groups.items():
        for group in groups:
            try:
                await redis_client.xgroup_create(
                    stream, group, id="0", mkstream=True
                )
            except redis.ResponseError as e:
                if "BUSYGROUP" not in str(e):
                    raise


# ═══════════════════════════════════════════════════════════
# RELIABLE CONSUMER — With XACK + retry + DLQ
# ═══════════════════════════════════════════════════════════

class ReliableConsumer:
    def __init__(self, redis_client: redis.Redis,
                 stream: str, group: str, consumer: str,
                 max_retries: int = 3):
        self.redis = redis_client
        self.stream = stream
        self.group = group
        self.consumer = consumer
        self.max_retries = max_retries

    async def consume(self, handler, batch_size: int = 10,
                       block_ms: int = 5000):
        """
        Read → Process → XACK pattern
        If handler raises: retry up to max_retries → DLQ
        """
        while True:
            try:
                # 1. First: process pending (unACKed) messages
                pending = await self.redis.xreadgroup(
                    self.group, self.consumer,
                    {self.stream: "0"},
                    count=batch_size
                )
                for stream_name, messages in pending:
                    for msg_id, data in messages:
                        if not data:  # already ACKed
                            continue
                        await self._process_with_retry(
                            handler, msg_id, data
                        )

                # 2. Then: read new messages
                results = await self.redis.xreadgroup(
                    self.group, self.consumer,
                    {self.stream: ">"},
                    count=batch_size,
                    block=block_ms
                )
                for stream_name, messages in results:
                    for msg_id, data in messages:
                        await self._process_with_retry(
                            handler, msg_id, data
                        )

            except Exception as e:
                await asyncio.sleep(1)

    async def _process_with_retry(self, handler, msg_id, data):
        for attempt in range(self.max_retries):
            try:
                await handler(json.loads(data.get(b"data", b"{}")))
                await self.redis.xack(
                    self.stream, self.group, msg_id
                )
                return
            except Exception as e:
                if attempt == self.max_retries - 1:
                    # Send to Dead Letter Queue
                    await self.redis.xadd(
                        f"dlq:{self.stream}",
                        {
                            "original_id": str(msg_id),
                            "data": data.get(b"data", b"{}"),
                            "error": str(e),
                            "attempts": str(self.max_retries)
                        },
                        maxlen=1000
                    )
                    await self.redis.xack(
                        self.stream, self.group, msg_id
                    )
                await asyncio.sleep(0.5 * (attempt + 1))
```

---

## 6. SCORING SERVICE

### 6.1 Chain-Specific Weights

```python
CHAIN_SCORING_WEIGHTS = {
    ChainId.SOLANA: {
        "lp_burned": 0.20,
        "holder_distribution": 0.20,
        "creator_history": 0.15,
        "pump_fun_graduation": 0.15,  # Solana-specific
        "volume_organic": 0.15,
        "bonding_curve_momentum": 0.15,  # NEW: pre-graduation signal
    },
    ChainId.BASE: {
        "honeypot_check": 0.25,       # GoPlus critical
        "lp_locked": 0.20,
        "holder_distribution": 0.20,
        "contract_verified": 0.10,
        "liquidity_depth": 0.15,
        "tax_level": 0.10,            # buy/sell tax impact
    },
    ChainId.BSC: {
        "honeypot_check": 0.30,       # Highest — BSC has most scams
        "lp_locked": 0.20,
        "holder_distribution": 0.15,
        "pinksale_audit": 0.15,
        "contract_verified": 0.10,
        "tax_level": 0.10,
    },
    ChainId.TON: {
        "lp_locked": 0.25,
        "holder_distribution": 0.25,
        "jetton_compliant": 0.20,
        "creator_history": 0.15,
        "telegram_presence": 0.15,
    },
}
```

### 6.2 Scoring Output

```python
@dataclass
class TokenScore:
    chain: ChainId
    address: str
    risk_score: int         # 0-100 (higher = SAFER)
    momentum_score: int     # 0-100 (higher = more momentum)
    overall_score: int      # weighted combination
    flags: list[str]
    recommendation: str     # STRONG_BUY | BUY | WATCH | AVOID
    details: dict           # per-factor breakdown
    scored_at: float

    @property
    def should_alert(self) -> bool:
        return self.recommendation in ("STRONG_BUY", "BUY")

    @property
    def should_auto_trade(self) -> bool:
        return self.recommendation == "STRONG_BUY" and \
               self.risk_score >= 70
```

---

## 7. EXECUTION SERVICE

### 7.1 Trade Flow

```
EXECUTION FLOW:
═══════════════════════════════════════

  scoring:results (STRONG_BUY)
          │
          ▼
  ┌───────────────────┐
  │ EXECUTION SERVICE  │
  │                    │
  │ 1. Validate score  │ ← is it still STRONG_BUY?
  │ 2. Check balance   │ ← enough SOL/ETH?
  │ 3. Check position  │ ← not already holding?
  │ 4. FRESH security  │ ← pre-execution check
  │ 5. Size position   │ ← risk-based sizing
  │ 6. Execute swap    │ ← via chain adapter
  │ 7. Create position │ ← Position Manager
  │ 8. Set TP/SL       │ ← auto stop-loss
  └───────┬───────────┘
          │
          ▼
  trades:executed:{chain}
          │
          ▼
  ┌───────────────────┐
  │ POSITION MANAGER   │
  │                    │
  │ • Monitor price    │
  │ • Execute TP/SL    │
  │ • Trailing stops   │
  │ • Time-based exit  │
  └───────────────────┘
```

### 7.2 Position Sizing

```python
POSITION_SIZING = {
    # Based on recommendation + chain
    ("STRONG_BUY", ChainId.SOLANA): {
        "base_size_usd": 50,
        "max_size_usd": 100,
    },
    ("BUY", ChainId.SOLANA): {
        "base_size_usd": 25,
        "max_size_usd": 50,
    },
    ("STRONG_BUY", ChainId.BASE): {
        "base_size_usd": 30,
        "max_size_usd": 80,
    },
    ("BUY", ChainId.BASE): {
        "base_size_usd": 15,
        "max_size_usd": 40,
    },
}

# Global safety limits
MAX_PORTFOLIO_EXPOSURE_USD = 500    # total open positions
MAX_POSITIONS_PER_CHAIN = 5
MAX_SINGLE_POSITION_PCT = 20       # % of portfolio
```

### 7.3 MEV Protection Summary

| Chain | Method | How |
|-------|--------|-----|
| Solana | **Jito Bundles** | Send TX via `mainnet.block-engine.jito.wtf`, private mempool |
| Base | **Low priority fee** | Base L2 has minimal MEV; use low priority fee |
| BSC | **None available** | Public mempool, use high slippage + fast execution |
| Arbitrum | **Flashbots Protect** | Via Flashbots RPC endpoint |

---

## 8. POSITION MANAGER

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
    amount_tokens: int          # raw amount

    current_price: float = 0
    pnl_percent: float = 0
    pnl_usd: float = 0

    # Exit rules
    take_profit_levels: list = None  # [50, 100, 200] %
    stop_loss_pct: float = -30      # -30%
    trailing_stop_pct: float = 0    # 0 = disabled
    max_holding_seconds: int = 3600  # 1h default

    status: str = "OPEN"            # OPEN | PARTIAL_EXIT | CLOSED

    def __post_init__(self):
        if self.take_profit_levels is None:
            self.take_profit_levels = [50, 100, 200]

    def should_take_profit(self) -> Optional[float]:
        """Returns TP level hit, or None"""
        for level in self.take_profit_levels:
            if self.pnl_percent >= level:
                return level
        return None

    def should_stop_loss(self) -> bool:
        return self.pnl_percent <= self.stop_loss_pct

    def should_time_exit(self) -> bool:
        return (time.time() - self.entry_timestamp) > \
               self.max_holding_seconds
```

---

## 9. INFRASTRUCTURE & RESILIENCE

### 9.1 Retry & Rate Limiting

```python
import asyncio
from functools import wraps


def with_retry(max_retries=3, backoff_base=1.0, max_backoff=30.0):
    """Decorator: exponential backoff retry for API calls"""
    def decorator(func):
        @wraps(func)
        async def wrapper(*args, **kwargs):
            for attempt in range(max_retries):
                try:
                    return await func(*args, **kwargs)
                except Exception as e:
                    if attempt == max_retries - 1:
                        raise
                    delay = min(
                        backoff_base * (2 ** attempt),
                        max_backoff
                    )
                    await asyncio.sleep(delay)
        return wrapper
    return decorator


class RateLimiter:
    """Token bucket rate limiter for API calls"""
    def __init__(self, calls_per_second: float):
        self._rate = calls_per_second
        self._tokens = calls_per_second
        self._last_refill = time.time()
        self._lock = asyncio.Lock()

    async def acquire(self):
        async with self._lock:
            now = time.time()
            elapsed = now - self._last_refill
            self._tokens = min(
                self._rate,
                self._tokens + elapsed * self._rate
            )
            self._last_refill = now

            if self._tokens < 1:
                wait = (1 - self._tokens) / self._rate
                await asyncio.sleep(wait)
                self._tokens = 0
            else:
                self._tokens -= 1


# Usage per provider:
RATE_LIMITERS = {
    "birdeye": RateLimiter(calls_per_second=1),     # ~60/min
    "goplus": RateLimiter(calls_per_second=0.5),     # 30/min
    "basescan": RateLimiter(calls_per_second=5),     # 5/sec
    "helius": RateLimiter(calls_per_second=10),      # high tier
    "dexscreener": RateLimiter(calls_per_second=5),  # generous
}
```

### 9.2 Main Orchestrator

```python
async def main():
    """Start all components"""
    redis_client = redis.Redis(host="localhost", port=6379, db=0)

    # Setup consumer groups
    await setup_consumer_groups(redis_client)

    # Initialize adapters
    solana = SolanaAdapter(SolanaConfig(...), redis_client)
    base = BaseAdapter(BaseConfig(...), redis_client)

    # Start adapters
    await solana.start()
    await base.start()

    # Start consumers
    scoring_consumer = ReliableConsumer(
        redis_client, "tokens:new:solana",
        "scoring_group", "scorer_1"
    )
    scoring_consumer_base = ReliableConsumer(
        redis_client, "tokens:new:base",
        "scoring_group", "scorer_1"
    )

    # Run forever
    await asyncio.gather(
        scoring_consumer.consume(score_token_handler),
        scoring_consumer_base.consume(score_token_handler),
        # ... more consumers
    )

if __name__ == "__main__":
    asyncio.run(main())
```

---

## 10. DATABASE SCHEMA

```sql
-- PostgreSQL 15 + TimescaleDB

-- Tokens discovered across all chains
CREATE TABLE tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chain VARCHAR(20) NOT NULL,
    address VARCHAR(100) NOT NULL,
    symbol VARCHAR(50),
    name VARCHAR(200),
    creator_address VARCHAR(100),
    launchpad VARCHAR(50),
    dex VARCHAR(50),
    pool_address VARCHAR(100),
    created_at TIMESTAMPTZ,
    discovered_at TIMESTAMPTZ DEFAULT NOW(),

    UNIQUE(chain, address)
);
CREATE INDEX idx_tokens_chain ON tokens(chain);
CREATE INDEX idx_tokens_discovered ON tokens(discovered_at DESC);

-- Scoring snapshots (time-series)
CREATE TABLE token_scores (
    token_id UUID REFERENCES tokens(id),
    scored_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    risk_score SMALLINT,
    momentum_score SMALLINT,
    overall_score SMALLINT,
    flags JSONB,
    recommendation VARCHAR(20),
    details JSONB,

    PRIMARY KEY (token_id, scored_at)
);
SELECT create_hypertable('token_scores', 'scored_at');

-- Positions
CREATE TABLE positions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chain VARCHAR(20) NOT NULL,
    token_address VARCHAR(100) NOT NULL,
    token_symbol VARCHAR(50),

    entry_price DECIMAL(30, 18),
    entry_amount DECIMAL(30, 18),
    entry_timestamp TIMESTAMPTZ,
    entry_tx_hash VARCHAR(100),

    exit_price DECIMAL(30, 18),
    exit_timestamp TIMESTAMPTZ,
    exit_tx_hash VARCHAR(100),

    take_profit_levels JSONB DEFAULT '[50, 100, 200]',
    stop_loss_pct DECIMAL(10, 4) DEFAULT -30,
    trailing_stop_pct DECIMAL(10, 4) DEFAULT 0,

    status VARCHAR(20) DEFAULT 'OPEN',
    pnl_usd DECIMAL(20, 2),
    pnl_pct DECIMAL(10, 4),

    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);
CREATE INDEX idx_positions_status ON positions(status);
CREATE INDEX idx_positions_chain ON positions(chain);

-- Transaction log
CREATE TABLE transactions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    position_id UUID REFERENCES positions(id),
    chain VARCHAR(20) NOT NULL,
    tx_hash VARCHAR(100) NOT NULL,
    tx_type VARCHAR(20),  -- BUY, SELL, PARTIAL_SELL
    amount_in DECIMAL(30, 18),
    amount_out DECIMAL(30, 18),
    price DECIMAL(30, 18),
    gas_cost_usd DECIMAL(10, 4),
    executed_at TIMESTAMPTZ DEFAULT NOW()
);

-- Security check cache
CREATE TABLE security_checks (
    chain VARCHAR(20) NOT NULL,
    token_address VARCHAR(100) NOT NULL,
    is_safe BOOLEAN,
    risk_level VARCHAR(20),
    flags JSONB,
    raw_data JSONB,
    checked_at TIMESTAMPTZ DEFAULT NOW(),

    PRIMARY KEY (chain, token_address, checked_at)
);
SELECT create_hypertable('security_checks', 'checked_at');
```

---

## 11. KOSZTY & PROVIDERS

### 11.1 Realistyczny budżet MVP (Solana + Base)

| Provider | Plan | Koszt/mo | Użycie |
|----------|------|----------|--------|
| **Helius** | Business | $199 | RPC + WS + Webhooks + DAS API |
| **Alchemy** | Growth (Base) | $49 | RPC + WS for Base |
| **Birdeye** | Standard | $99 | Token security + OHLCV + overview |
| **PumpPortal** | Free (Data API) | $0 | pump.fun WS (rate-limited) |
| **GoPlus** | Free | $0 | Base/BSC token security (30 req/min) |
| **DexScreener** | Free | $0 | Pair data (no key needed) |
| **Jupiter** | Free | $0 | Solana swap routing |
| **0x** | Free tier | $0 | Base swap quotes (100k req/mo) |
| **Basescan** | Free | $0 | Contract verification (5 req/sec) |
| **Jito** | Free | $0 | MEV protection (tip included in TX) |
| **VPS** | Hetzner AX41 | $45 | 64GB RAM, AMD Ryzen, NVMe |
| **Redis** | Self-hosted on VPS | $0 | Or Redis Cloud $30/mo |
| **PostgreSQL** | Self-hosted + TimescaleDB | $0 | On same VPS |
| **Telegram Bot** | Free | $0 | Alerting |
| | | **~$392/mo** | |

### 11.2 Expansion costs (per additional chain)

| Chain | RPC Provider | Extra cost |
|-------|-------------|-----------|
| BSC | NodeReal/Ankr | $0-50/mo |
| TON | TON Center | $0-30/mo |
| Arbitrum | Alchemy (same plan) | $0 (included) |
| Tron | TronGrid | $0-30/mo |

---

## 12. ROADMAP

### Phase 1 — MVP (2-3 tygodnie)

```
WEEK 1:
  ☐ SolanaAdapter — PumpPortal WS (new tokens + migrations)
  ☐ SolanaAdapter — Helius WS fallback (Raydium + Orca)
  ☐ SolanaAdapter — Security checks (on-chain + Birdeye)
  ☐ Redis Streams setup + consumer groups

WEEK 2:
  ☐ BaseAdapter — WS (Uniswap V3 + Aerodrome pool creation)
  ☐ BaseAdapter — Security checks (GoPlus)
  ☐ Basic Scoring Service (risk + momentum)
  ☐ Telegram alerts (new tokens + recommendations)

WEEK 3:
  ☐ Jupiter swap execution (Solana)
  ☐ Uniswap V3 swap execution (Base)
  ☐ Two-level security (pre-filter + pre-execution)
  ☐ Circuit breaker + exponential backoff
  ☐ PostgreSQL + basic position tracking
```

### Phase 2 — Semi-Auto Trading (2 tygodnie)

```
  ☐ Telegram: buy/sell commands
  ☐ Position Manager (TP/SL monitoring)
  ☐ Auto stop-loss execution
  ☐ Portfolio summary via Telegram
  ☐ Dead letter queue
  ☐ Grafana monitoring dashboard
```

### Phase 3 — Expansion (2-3 tygodnie)

```
  ☐ BSC adapter (PancakeSwap)
  ☐ TON adapter (STON.fi / DeDust)
  ☐ Cross-chain portfolio view
  ☐ Advanced scoring (creator wallet analysis, social signals)
  ☐ Bonding curve sniper (buy during pump.fun curve, sell on graduation)
```

### Phase 4 — Hardening (ongoing)

```
  ☐ Arbitrum + Tron adapters
  ☐ Full auto mode with risk limits
  ☐ Web dashboard (React)
  ☐ Backtesting engine (historical token data)
  ☐ ML-based scoring improvements
```

---

## APPENDIX: Quick Reference — All API Endpoints

| Service | URL | Auth | Rate Limit |
|---------|-----|------|-----------|
| Helius RPC | `https://mainnet.helius-rpc.com/?api-key={KEY}` | URL param | Plan-based |
| Helius WS | `wss://mainnet.helius-rpc.com/?api-key={KEY}` | URL param | Plan-based |
| Helius Webhooks | `https://api.helius.xyz/v0/webhooks?api-key={KEY}` | URL param | Plan-based |
| PumpPortal WS | `wss://pumpportal.fun/api/data` | None (free) | Rate-limited |
| PumpPortal Trade | `https://pumpportal.fun/api/trade-local` | None | 0.5% fee |
| Jupiter Quote | `https://quote-api.jup.ag/v6/quote` | None | Unlimited |
| Jupiter Swap | `https://quote-api.jup.ag/v6/swap` | None | Unlimited |
| Jupiter Price | `https://price.jup.ag/v6/price` | None | Unlimited |
| Birdeye | `https://public-api.birdeye.so/defi/*` | Header: X-API-KEY | Plan-based |
| Jito | `https://mainnet.block-engine.jito.wtf/api/v1/transactions` | None | Unlimited |
| Alchemy RPC | `https://base-mainnet.g.alchemy.com/v2/{KEY}` | URL param | Plan-based |
| Alchemy WS | `wss://base-mainnet.g.alchemy.com/v2/{KEY}` | URL param | Plan-based |
| GoPlus | `https://api.gopluslabs.io/api/v1/token_security/{CHAIN_ID}` | None | 30/min |
| Basescan | `https://api.basescan.org/api` | URL param: apikey | 5/sec |
| 0x | `https://base.api.0x.org/swap/v1/*` | Header: 0x-api-key | 100k/mo |
| DexScreener | `https://api.dexscreener.com/latest/dex/tokens/{ADDR}` | None | Unlimited |
