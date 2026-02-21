# NEXUS — AI-Native Trading & Intelligence Platform
## Plan Architektoniczny v1.0
### Autorzy: Human (spec/wizja) + Claude (architektura/implementacja)

---

## 0. Filozofia — 5 zasad, które nie podlegają negocjacji

```
1. LLM NIE HANDLUJE.
   LLM = ekstrakcja, klasyfikacja, synteza.
   Trading core jest deterministyczny i audytowalny.

2. JEDNA PRAWDA DANYCH.
   Te same eventy dla backtest / paper / live.
   Replay to tryb działania, nie feature.

3. KOSZTY SĄ CZĘŚCIĄ STRATEGII.
   fees + slippage + infra + LLM — per strategia, per trade.
   Strategia z +5% PnL i +6% kosztów to strata. Zawsze.

4. SAFETY > PROFIT > SPEED.
   Twarde limity strat działają ZAWSZE.
   Żaden moduł nie może ich obejść.
   Kill switch działa nawet gdy Kafka leży.

5. SELEKTYWNA INTELIGENCJA.
   Nie mielimy internetu. Pytamy o konkretne rzeczy,
   z budżetem, TTL i mierzalnym ROI.
```

### Czym ten plan RÓŻNI SIĘ od Twojego specu:

| Twój spec mówi | Ja mówię | Dlaczego |
|---|---|---|
| Rust core | **Go core** | 3x szybszy development, 80% performance Rusta, lepszy ekosystem exchange. Rust tylko dla hot-path execution jeśli Go okaże się za wolny (mierz, nie zgaduj). |
| Kafka od startu | **RedPanda** (Kafka-compatible API) | Zero JVM, single binary, 10x prostszy ops. Migracja do Kafka = zmiana adresu brokera. |
| DEX i CEX osobno | **Unified Execution Layer** | Ten sam interfejs `ExchangeAdapter` dla Kraken, Binance, Jupiter, Uniswap. Strategia nie wie czy handluje na CEX czy DEX. |
| Intel Pipeline w S4 | **Intel schema od S0, pipeline od S3** | Schema musi istnieć od początku żeby audit trail i eventy były spójne. Ale pipeline budujemy po trading core. |
| ClickHouse | **ClickHouse — zgoda** | Tutaj się zgadzam w 100%. Najlepszy wybór dla trading analytics. |
| Brak regime detection | **Regime Detector jako first-class moduł** | Wszystkie strategie widzą regime. To nie jest "w strategii" — to infrastruktura. |

---

## 1. Architektura — widok z lotu ptaka

```
┌─────────────────────────────────────────────────────────────────┐
│                        NEXUS PLATFORM                           │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐       │
│  │ Kraken   │  │ Binance  │  │ Jupiter  │  │ Uniswap  │  ...  │
│  │ Adapter  │  │ Adapter  │  │ (Solana) │  │ (Base)   │       │
│  └────┬─────┘  └────┬─────┘  └────┬─────┘  └────┬─────┘       │
│       │              │              │              │             │
│  ─────┴──────────────┴──────────────┴──────────────┴─────────── │
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
│  │         Observability (Prometheus/Grafana/Loki)       │       │
│  └──────────────────────────────────────────────────────┘       │
└─────────────────────────────────────────────────────────────────┘
```

---

## 2. Tech Stack — uzasadnienie każdego wyboru

### Warstwa Core (Go)
```
Język:        Go 1.22+
Uzasadnienie: Goroutines idealne dla concurrent exchange connections.
              Kompilacja do single binary. Deploy = scp.
              Silne typowanie, ale 3x szybszy dev niż Rust.
              Ekosystem: ccxt-go, gorilla/websocket, shopspring/decimal.

Kiedy Rust:   TYLKO jeśli Go hot-path execution > 100μs
              i pomiar pokaże że to bottleneck.
              Wtedy: Rust library wywoływana z Go przez CGO lub gRPC.
```

### Warstwa Event Bus
```
RedPanda:     Kafka-compatible API, zero JVM, single binary
              Topic naming: <domain>.<category>.<entity>.<variant>
              Retencja: od 72h (orderbook) do 8760h (audit)

Dlaczego nie Kafka: Kafka wymaga ZooKeeper/KRaft + 3 brokerów.
                     RedPanda = 1 binary, te same klienty, ta sama semantyka.
                     Gdy trzeba będzie skalować: migracja = zmiana broker address.

Dlaczego nie NATS:  Kafka API to standard. ClickHouse, Flink, Debezium
                     — wszystko ma natywne Kafka connectors. NATS nie.
```

### Warstwa Analytics
```
ClickHouse:   OLAP, kolumnowy, partycjonowanie po dniu/symbolu.
              ReplacingMergeTree dla dedup.
              Materialized views dla OHLCV aggregation.
              Backtest queries: full scan 1 roku danych < 2s.
```

### Warstwa Intel
```
LLM Internal: vLLM lub llama.cpp (Mistral/Llama 3 8B quantized)
              Do: klasyfikacja, ekstrakcja claims, dedup, tłumaczenie
              Koszt: ~$0 po hardware (GPU serwer)

LLM External: Claude API (analiza złożona), Grok (real-time X/Twitter)
              TYLKO przez Query Brain z budżetem i circuit breaker.
              Koszt: budżet dzienny per provider, accounting per request.

Vector DB:    Weaviate (self-hosted) lub Qdrant
              Do: similar event retrieval, historical context dla LLM
              Embedding model: local (nomic-embed lub bge-small)
```

### Warstwa Research (Python)
```
Python 3.12:  TYLKO do research, backtestów, notebooków
              Typed (mypy strict), Pydantic v2
              NIE w produkcyjnym hot-path
              Łączy się z ClickHouse i Kafka (confluent-kafka)
```

---

## 3. Moduły — odpowiedzialności i granice

### 3.1 Exchange Adapters (Go)
```
Odpowiedzialność:
  - Połączenie z exchange (WS + REST)
  - Normalizacja danych do unified schema
  - Publikacja do md.* topics
  - Heartbeat + data quality metrics

Interfejs (ten sam dla CEX i DEX):
  type ExchangeAdapter interface {
      Connect(ctx context.Context) error
      StreamTrades(symbol string) <-chan Trade
      StreamOrderbook(symbol string) <-chan OrderbookUpdate
      SubmitOrder(order OrderIntent) (OrderAck, error)
      CancelOrder(orderID string) error
      GetBalances() ([]Balance, error)
      Health() HealthStatus
  }

Implementacje:
  - KrakenAdapter (CEX, fiat pairs, WS v2)
  - BinanceAdapter (CEX, crypto pairs, WS)
  - JupiterAdapter (DEX, Solana, Jupiter API)
  - UniswapAdapter (DEX, Base/ETH, on-chain)

Kluczowa decyzja:
  Strategia NIGDY nie wie czy to CEX czy DEX.
  Adapter normalizuje wszystko do tych samych typów.
  To daje nam portability i łatwe A/B testowanie venue.
```

### 3.2 Feature Engine (Go)
```
Odpowiedzialność:
  - Konsumpcja md.* eventów
  - Obliczanie feature vectorów w czasie rzeczywistym
  - Publikacja do signals.features.<symbol>
  - OHLCV aggregation (1m/5m/1h)

Features (start):
  - VWAP, TWAP
  - Orderbook imbalance (bid/ask ratio)
  - Trade flow toxicity (VPIN-like)
  - Volatility (realized, Parkinson, Yang-Zhang)
  - Spread (quoted, effective, realized)
  - Momentum (ROC, RSI — ale obliczane na eventach, nie na OHLCV)

Reguła twarda:
  Feature Engine ZERO network I/O do zewnątrz.
  Dostaje eventy z Kafka, produkuje features do Kafka.
  Deterministic replay gwarantowany.
```

### 3.3 Regime Detector (Go) — MOJ DODATEK
```
Odpowiedzialność:
  - Klasyfikacja aktualnego reżimu rynkowego
  - Publikacja do signals.regime.<symbol>
  - Strategia widzi regime jako input, nie oblicza sama

Reżimy (start):
  - TRENDING_UP / TRENDING_DOWN
  - MEAN_REVERTING
  - HIGH_VOLATILITY / LOW_VOLATILITY
  - BREAKOUT
  - UNKNOWN / TRANSITION

Dlaczego osobny moduł:
  Każda strategia potrzebuje regime. Jeśli to w strategii,
  masz 5 strategii × 5 różnych detektorów × 5 różnych opinii.
  Jeden Regime Detector = jedna prawda o stanie rynku.

Metoda (start):
  - Hidden Markov Model (2-3 stany) na features
  - Threshold-based fallback (volatility percentile)
  - NIE LLM — to musi być deterministyczne i szybkie
```

### 3.4 Strategy Runtime (Go)
```
Odpowiedzialność:
  - Hostowanie strategii (plugin interface)
  - Routing eventów do strategii
  - Zbieranie StrategySignal
  - Przekazywanie do Risk Engine
  - Checkpointing state

Interface (zgoda z Twoim specem, z moimi dodatkam):
  type Strategy interface {
      Init(config StrategyConfig, features FeatureRegistry) error
      OnEvent(event MarketEvent) []StrategySignal
      OnTimer(ts time.Time, timerID string) []StrategySignal
      OnIntel(event IntelEvent) []StrategySignal    // <-- MÓJ DODATEK
      SnapshotState() []byte
      RestoreState([]byte) error                     // <-- MÓJ DODATEK
  }

Moje dodtki vs Twój spec:
  1. OnIntel() — osobny handler, bo intel events mają inną
     kadencję i confidence. Strategia decyduje CZY i JAK reaguje.
  2. RestoreState() — bez tego checkpoint jest bezużyteczny.

Conflict Resolution (brakuje w Twoim specu):
  - Gdy 2 strategie chcą kupić ten sam instrument:
    Priority = capital_allocation_weight × confidence
  - Gdy strategia A mówi BUY a B mówi SELL:
    Net exposure = A.target + B.target (per-strategy sub-accounts)
  - Global position netting na poziomie Risk Engine, nie Strategy
```

### 3.5 Risk Engine (Go)
```
Odpowiedzialność:
  - Pre-trade checks (Twój spec — zgoda 100%)
  - Position limits (per-strategy + global)
  - Daily/weekly loss limits → auto-stop
  - Kill switches (global / per-strategy / per-exchange)
  - Circuit breakers (feed lag, error rate, reject spike)

Architektura:
  StrategySignal → Risk Engine → OrderIntent (approved) → Execution
                              ↘ RiskDenied (reasons) → Audit

Hardcoded minimums (nie konfigurowane, nie wyłączane):
  - max_daily_loss: ZAWSZE aktywny
  - max_total_exposure: ZAWSZE aktywny
  - kill_switch: ZAWSZE responsywny (in-process, nie przez Kafka)

Kill switch implementation:
  Kill switch NIE IDZIE przez Kafka.
  In-process atomic flag. Kafka może leżeć — kill switch działa.
  To jest "hardware fuse" systemu.

Mój dodatek — Cooldown:
  Po N consecutive losses na strategii → cooldown T minut.
  Nie freeze całego systemu — tylko ta strategia odpoczynka.
```

### 3.6 Execution Engine (Go)
```
Odpowiedzialność:
  - Order state machine (idempotent)
  - Routing do proper ExchangeAdapter
  - Retry logic (z backoff, max retries)
  - Partial fill handling
  - Nonce/sequence management (DEX)
  - Emit exec.* events

Order State Machine:
  CREATED → SUBMITTED → ACKED → PARTIAL → FILLED
                     ↘ REJECTED
           → CANCEL_PENDING → CANCELLED
                            → CANCEL_REJECTED

Idempotency:
  Każdy OrderIntent ma deterministic intent_id.
  Execution Engine sprawdza PRZED submittem:
  - Czy ten intent_id już był executed?
  - Jeśli tak: skip (log duplicate)
  - Jeśli crash między submit a ack: recovery z exchange API

Slippage tracking (mój dodatek):
  Każdy fill zapisuje:
  - expected_price (z momentu sygnału)
  - submitted_price (z momentu orderu)
  - actual_fill_price
  - slippage_bps = (actual - expected) / expected × 10000
  To idzie do cost attribution.
```

### 3.7 Intel Pipeline (Go orchestrator + Python LLM workers)
```
Architektura:
  ┌──────────────┐
  │  Query Brain  │ (Go) — decyduje CO pytać, KIEDY, ILE
  │  (orchestrator)│
  └──────┬───────┘
         │ tasks
  ┌──────┴───────────────────────────────────┐
  │          Worker Pool (Python)             │
  │  ┌─────────┐ ┌─────────┐ ┌────────────┐ │
  │  │ Internal│ │  Grok   │ │   Claude   │ │
  │  │  LLM   │ │ Connector│ │ Connector  │ │
  │  └────┬────┘ └────┬────┘ └─────┬──────┘ │
  │       └───────────┼────────────┘         │
  │            structured output              │
  └──────────────────┬───────────────────────┘
                     │
  ┌──────────────────┴───────────────────────┐
  │  Validation + Dedup + Confidence Scoring  │
  └──────────────────┬───────────────────────┘
                     │
              intel.events.global (Kafka)

Query Brain decyzje:
  1. TRIGGERY (co uruchamia query):
     - Market anomaly (vol spike, gap, correlation break)
     - Schedule (makro kalendarz: FOMC, CPI, earnings)
     - Strategy request (strategia pyta o kontekst)
     - Cascade (jeden intel event triggeruje głębsze pytanie)

  2. BUDŻET per topic per dzień:
     exchange_outage:    max 5 queries/day, provider: internal_llm
     regulatory_action:  max 10 queries/day, provider: claude
     macro_event:        max 20 queries/day, provider: internal_llm + grok
     sentiment_shift:    max 30 queries/day, provider: internal_llm

  3. CIRCUIT BREAKER per provider:
     Jeśli error_rate > 20% w oknie 5min → disable provider na 15min.
     Jeśli latency p95 > 30s → downgrade do internal_llm only.

  4. NOISE CONTROL:
     Normalny rynek: max 10 intel events/hour
     Volatile rynek: max 50 intel events/hour
     Powyżej: alarm + budget exhaustion → stop queries

Intel Event Schema — biorę Twój (jest świetny), dodaję:
  {
    // ... Twój schemat bez zmian ...
    "cost": {
      "provider_cost_usd": 0.003,    // koszt tego query
      "cumulative_topic_cost_today": 0.45,
      "budget_remaining_today": 1.55
    },
    "latency_ms": 2340,
    "downstream_actions": []  // wypełniane post-hoc: jakie trades spowodował
  }

ROI Measurement (mój kluczowy dodatek):
  Po każdym tradzie sprawdzamy:
  - Które intel events były w oknie decyzyjnym strategii?
  - Czy trade byłby inny BEZ tych eventów?
  - PnL z intel vs PnL bez intel (counterfactual)
  To daje nam TWARDY ROI na każdy dollar wydany na LLM.
```

### 3.8 Audit & Event Store
```
Topic: audit.event_store (retention: 1 rok)

Każda decyzja w systemie ma trace:
  market_event → feature_update → regime_update →
  intel_event (optional) → strategy_signal →
  risk_check (allow/deny + reasons) →
  order_intent → order_ack → fill →
  position_update → pnl_update

Wszystko połączone przez:
  - trace_id (per decision chain)
  - correlation_id (per "case" / lifecycle)
  - causation_id (which event caused this)

ClickHouse tables:
  audit_events (
    event_id     String,
    trace_id     String,
    causation_id String,
    event_type   LowCardinality(String),
    ts           DateTime64(6),
    payload      String,  -- JSON
    schema_ver   UInt16
  ) ENGINE = MergeTree()
  PARTITION BY toYYYYMMDD(ts)
  ORDER BY (event_type, ts, event_id)
  TTL ts + INTERVAL 365 DAY
```

---

## 4. ClickHouse Schema (brakuje w Twoim specu)

```sql
-- Ticks
CREATE TABLE md_ticks (
    exchange     LowCardinality(String),
    symbol       LowCardinality(String),
    ts           DateTime64(6),
    bid          Decimal64(8),
    ask          Decimal64(8),
    bid_size     Decimal64(8),
    ask_size     Decimal64(8),
    event_id     String
) ENGINE = ReplacingMergeTree(ts)
PARTITION BY (exchange, toYYYYMMDD(ts))
ORDER BY (symbol, ts);

-- Trades
CREATE TABLE md_trades (
    exchange     LowCardinality(String),
    symbol       LowCardinality(String),
    ts           DateTime64(6),
    price        Decimal64(8),
    qty          Decimal64(8),
    side         Enum8('buy'=1, 'sell'=2, 'unknown'=0),
    trade_id     String,
    event_id     String
) ENGINE = ReplacingMergeTree(ts)
PARTITION BY (exchange, toYYYYMMDD(ts))
ORDER BY (symbol, ts, trade_id);

-- OHLCV (materialized view from trades)
CREATE MATERIALIZED VIEW md_ohlcv_1m
ENGINE = AggregatingMergeTree()
PARTITION BY (exchange, toYYYYMMDD(ts_bucket))
ORDER BY (symbol, ts_bucket)
AS SELECT
    exchange,
    symbol,
    toStartOfMinute(ts) as ts_bucket,
    argMinState(price, ts) as open,
    maxState(price) as high,
    minState(price) as low,
    argMaxState(price, ts) as close,
    sumState(qty) as volume,
    countState() as trade_count
FROM md_trades
GROUP BY exchange, symbol, ts_bucket;

-- Fills (execution)
CREATE TABLE exec_fills (
    exchange     LowCardinality(String),
    symbol       LowCardinality(String),
    ts           DateTime64(6),
    order_id     String,
    intent_id    String,
    strategy_id  LowCardinality(String),
    side         Enum8('buy'=1, 'sell'=2),
    qty          Decimal64(8),
    price        Decimal64(8),
    fee          Decimal64(8),
    fee_currency LowCardinality(String),
    expected_price   Decimal64(8),  -- do slippage tracking
    slippage_bps     Float32,
    trace_id     String
) ENGINE = MergeTree()
PARTITION BY (exchange, toYYYYMMDD(ts))
ORDER BY (strategy_id, symbol, ts);

-- Intel Events
CREATE TABLE intel_events (
    event_id     String,
    ts           DateTime64(6),
    topic        LowCardinality(String),
    priority     Enum8('P0'=0, 'P1'=1, 'P2'=2, 'P3'=3),
    ttl_seconds  UInt32,
    confidence   Float32,
    impact_horizon   LowCardinality(String),
    directional_bias LowCardinality(String),
    asset_tags   Array(LowCardinality(String)),
    claims_json  String,
    sources_json String,
    provider     LowCardinality(String),
    cost_usd     Decimal64(6),
    latency_ms   UInt32,
    downstream_trade_ids Array(String)  -- post-hoc ROI linking
) ENGINE = MergeTree()
PARTITION BY toYYYYMMDD(ts)
ORDER BY (topic, ts);

-- Cost Attribution (daily rollup)
CREATE TABLE cost_attribution_daily (
    date         Date,
    strategy_id  LowCardinality(String),
    exchange     LowCardinality(String),
    trading_fees_usd    Decimal64(4),
    slippage_cost_usd   Decimal64(4),
    infra_cost_usd      Decimal64(4),
    llm_cost_usd        Decimal64(4),
    total_cost_usd      Decimal64(4),
    gross_pnl_usd       Decimal64(4),
    net_pnl_usd         Decimal64(4),
    trade_count          UInt32,
    intel_events_used    UInt32
) ENGINE = SummingMergeTree()
PARTITION BY toYYYYMM(date)
ORDER BY (strategy_id, date);

-- Data Quality Monitor
CREATE TABLE data_quality (
    ts           DateTime,
    exchange     LowCardinality(String),
    symbol       LowCardinality(String),
    feed_lag_ms  Float32,
    gap_count    UInt32,
    jitter_ms    Float32,
    uptime_pct   Float32,
    quality_score Float32  -- composite
) ENGINE = MergeTree()
PARTITION BY toYYYYMMDD(ts)
ORDER BY (exchange, symbol, ts)
TTL ts + INTERVAL 90 DAY;
```

---

## 5. Delivery Plan — moja wersja

```
Zasada: każdy stage kończy się DZIAŁAJĄCYM systemem.
Nie "zrobiłem moduł X" tylko "system robi Y end-to-end".
```

### S0: Fundament
```
Cel: Repo, schematy, CI, infrastruktura działa.

Deliverables:
  ├── Monorepo layout (Go modules + Python packages)
  ├── Protobuf/Avro schemas (md/exec/intel/signals)
  ├── RedPanda (docker-compose, topics provisioned)
  ├── ClickHouse (docker-compose, tabele z góry)
  ├── CI: lint + test + build (GitHub Actions)
  ├── Structured logging standard (JSON, trace_id)
  └── Replay test framework (empty, but wired)

Definition of Done:
  □ `make test` przechodzi
  □ `make run-infra` startuje RedPanda + ClickHouse + Prometheus
  □ Mogę opublikować event do Kafka i przeczytać go z ClickHouse
  □ Schema registry (lub convention) jest ustalony
```

### S1: Market Data Pipeline
```
Cel: Stabilny strumień danych z 1 exchange, 1-2 instrumentów.

Deliverables:
  ├── KrakenAdapter (WS v2: ticks, trades, orderbook L2)
  ├── Kafka producer (md.ticks.kraken.*, md.trades.kraken.*)
  ├── ClickHouse ingest (Kafka engine → tabele)
  ├── OHLCV materialized view (1m)
  ├── Data Quality Monitor (lag, gaps, jitter → ops.alerts)
  ├── Feature Engine v1 (VWAP, spread, volatility)
  └── Grafana dashboard: feed health

Definition of Done:
  □ 24h ciągłego działania, data loss < 0.1%
  □ Feed lag p95 < 2s (mierzone i alarmowane)
  □ ClickHouse: mogę query'ować dzisiejsze ticki < 100ms
  □ Feature vectors publikowane do signals.features.*
```

### S2: Trading Core + Risk + Paper
```
Cel: Deterministyczny execution path, paper trading działa.

Deliverables:
  ├── Order State Machine (Go, idempotent)
  ├── Paper Broker (symuluje fills z orderbooku)
  ├── Risk Engine v1:
  │   ├── Pre-trade: exposure, position size, daily loss
  │   ├── Kill switches: global, per-strategy, per-exchange
  │   └── Circuit breakers: feed lag, error rate
  ├── Execution Engine (order routing, retry, partial fills)
  ├── Position Manager (per-strategy + global netting)
  ├── Audit trail: pełny trace od signal do fill
  ├── Strategy Runtime v1 (OnEvent + OnTimer)
  ├── Strategy: SimpleMomentum (MVP, na feature vectors)
  └── Cost tracking: fees + slippage per trade

Definition of Done:
  □ Paper trading działa 24/7 na live data
  □ Każda decyzja ma audit trail (mogę odtworzyć "dlaczego ten trade")
  □ Kill switch zatrzymuje WSZYSTKO w < 100ms
  □ Replay: ten sam input → te same ordery (deterministic)
  □ Property-based tests: order SM pokrywa wszystkie stany
```

### S3: Backtest Parity + Research
```
Cel: Ta sama logika w backtest co w paper/live.

Deliverables:
  ├── Replay Runner (audit.event_store → Strategy Runtime)
  ├── Backtest engine (ClickHouse → event replay → metrics)
  ├── Slippage model (pluggable, per-exchange)
  ├── Fee model (maker/taker, per-exchange tier)
  ├── Metrics: PnL, Sharpe, drawdown, turnover, cost impact
  ├── Walk-forward validation tool
  ├── Regime Detector v1 (HMM, 3 stany)
  └── Jupyter environment (Python, connected to ClickHouse)

Definition of Done:
  □ Backtest odtwarza paper-run z tolerancją < 0.5% PnL
  □ Look-ahead bias detector: test sanity przechodzi
  □ Regime publikowany do signals.regime.*
  □ 2+ strategie mogą być backtestowane niezależnie
```

### S4: Intel Pipeline v1
```
Cel: Selektywne pozyskiwanie informacji z mierzalnym ROI.

Deliverables:
  ├── Taxonomia topiców v1 (25 topics z TTL i budżetami)
  ├── Query Brain (Go orchestrator)
  │   ├── Trigger engine (market anomaly + schedule)
  │   ├── Budget manager (per-topic, per-provider, per-day)
  │   └── Circuit breakers per provider
  ├── Internal LLM worker (Python + vLLM/llama.cpp)
  │   ├── Classification (topic assignment)
  │   ├── Extraction (claims + entities)
  │   └── Dedup (semantic similarity)
  ├── Weaviate: vector store for intel events
  ├── intel.events.global topic
  ├── Strategy interface: OnIntel() handler
  └── ROI tracking: intel event → trade linkage

Definition of Done:
  □ Intel events przechodzą walidację schematu
  □ Noise control: < 10 events/h w normalnym rynku
  □ Query Brain nie przekracza budżetów
  □ Circuit breaker działa (testowane fault injection)
  □ Mogę odpowiedzieć: "ile kosztował intel dzisiaj?"
```

### S5: External LLM + Multi-Exchange
```
Cel: Grok/Claude jako pluginy ROI. Drugi exchange.

Deliverables:
  ├── Grok connector (X/Twitter real-time)
  ├── Claude connector (complex analysis)
  ├── Cross-check + confidence scoring
  ├── Per-provider cost accounting
  ├── BinanceAdapter (drugi CEX)
  ├── JupiterAdapter (DEX Solana — port konceptów z MMH)
  └── Strategy B: Event-driven (intel-gated)

Definition of Done:
  □ Można wyłączyć provider bez wpływu na trading
  □ Koszt per request jest księgowany
  □ A/B: Strategy A (bez intel) vs Strategy B (z intel) — paper
  □ DEX adapter normalizuje do tego samego interfejsu co CEX
```

### S6: Paper Marathon + Attribution
```
Cel: 2-4 tygodnie paper 24/7, mierzenie wszystkiego.

Deliverables:
  ├── 2+ strategie na paper 24/7
  ├── Weekly report generator (auto):
  │   ├── PnL (gross, net after costs)
  │   ├── Cost breakdown (fees, slippage, infra, LLM)
  │   ├── Intel ROI (A/B comparison)
  │   ├── Risk metrics (max drawdown, exposure utilization)
  │   └── Data quality summary
  ├── Post-mortem generator (na danych, nie hallucynacjach)
  ├── Alerting: PagerDuty/Telegram for critical issues
  └── Runbooks: co robić gdy X pada

Definition of Done:
  □ 2 tygodnie bez awarii krytycznej
  □ Weekly report odpowiada na: "czy warto iść live?"
  □ Intel ROI: twarda liczba ($ zarobione dzięki intel - $ wydane na intel)
  □ Runbooks pokrywają top 10 failure modes
```

### S7: Live — kontrolowany reżim
```
Cel: Live z małym kapitałem, twarde bezpieczniki.

Deliverables:
  ├── Live broker connection (Kraken API keys)
  ├── Capital allocation: mały % portfolio
  ├── Hard limits (nie konfigurowalne):
  │   ├── max_daily_loss: $X
  │   ├── max_total_exposure: $Y
  │   └── max_single_trade: $Z
  ├── Auto-stop: przekroczenie limitów → kill → alert
  ├── Incident response: alert → diagnoza → stop w < 5 min
  └── Tygodniowe review: keep / kill / modify

Definition of Done:
  □ Limity strat działają w 100% przypadków (testowane)
  □ Pierwszy tydzień live: kapitał i zysk/strata < X
  □ Pełna audytowalność: mogę odtworzyć każdy trade
  □ Stabilność > PnL (wolę $0 niż bug w produkcji)
```

---

## 6. Co BIORĘ z MMH v3.1 (koncepty, nie kod)

```
Z MMH v3.1                    →  Do NEXUS
───────────────────────────────────────────────
RiskFabric policies           →  Risk Engine rules (Go)
Decision Journal concept      →  Audit Event Store (ClickHouse)
IdempotencyContract           →  Execution Engine dedup (Go)
Circuit Breaker pattern       →  Per-component circuit breakers
WAL → Redis → Consumer        →  Kafka consumer groups
2-tier security (cached+fresh)→  Intel cache + fresh query
Position Manager ledger       →  Position Manager (per-strategy)
ControlPlane kill/freeze      →  Kill switches (in-process)
Heartbeat → auto-freeze       →  Health monitor → auto-stop
Config hash in decisions      →  Schema versioning everywhere
Dry-run first-class           →  Paper Broker as execution mode
Replay Engine                 →  Backtest/Replay Runner
```

---

## 7. Replay Divergence Budget (brakuje w obu planach)

```yaml
replay_tolerance:
  # Backtest vs Paper
  signal_match_rate: 99.0%    # % identycznych sygnałów
  position_delta_pct: 1.0%    # max odchylenie pozycji
  pnl_delta_pct: 2.0%         # max odchylenie PnL

  # Paper vs Live
  fill_rate_delta: 5.0%       # fills mogą się różnić (market impact)
  slippage_delta_bps: 50      # max dodatkowy slippage
  signal_match_rate: 95.0%    # mniej strict (latency differences)

  # Alert thresholds
  warn_at: 80%_of_tolerance
  stop_at: 100%_of_tolerance
```

---

## 8. Co NIE JEST w tym planie (i dlaczego)

```
1. ML model training pipeline
   → Za wcześnie. Najpierw zbierz 6 miesięcy danych.
   → Gdy będziesz miał dane, dodanie ML to S8.

2. Multi-region deployment
   → Overengineering na start. Jeden serwer blisko exchange.
   → Gdy live PnL > infra cost: rozważ co-location.

3. HFT / sub-millisecond latency
   → Ten system targetuje sekundy/minuty, nie mikrosekundy.
   → Go daje < 1ms per decision. Jeśli trzeba szybciej: Rust hot-path.

4. Frontend / Dashboard UI
   → Grafana + Jupyter wystarczą na start.
   → Custom UI to S9+ gdy system jest profitable.

5. Więcej niż 2-3 exchange
   → Complexity per exchange jest nieliniowy (edge cases, API quirks).
   → Start: Kraken + Binance + 1 DEX. Reszta po live.
```

---

## 9. Monorepo Structure

```
nexus/
├── go.mod
├── go.sum
├── Makefile
├── docker-compose.yml
├── .github/workflows/ci.yml
│
├── cmd/                        # Entry points
│   ├── nexus-core/main.go      # Main trading process
│   ├── nexus-intel/main.go     # Intel pipeline orchestrator
│   └── nexus-tools/            # CLI tools (replay, backtest trigger)
│
├── internal/                   # Go internal packages
│   ├── adapters/               # Exchange adapters
│   │   ├── adapter.go          # Interface definition
│   │   ├── kraken/
│   │   ├── binance/
│   │   ├── jupiter/
│   │   └── uniswap/
│   ├── bus/                    # Kafka producer/consumer
│   │   ├── producer.go
│   │   ├── consumer.go
│   │   └── schemas/            # Avro/Protobuf schemas
│   ├── features/               # Feature Engine
│   ├── regime/                 # Regime Detector
│   ├── strategy/               # Strategy Runtime
│   │   ├── runtime.go
│   │   ├── interface.go
│   │   └── strategies/         # Actual strategies
│   │       ├── momentum/
│   │       └── event_driven/
│   ├── risk/                   # Risk Engine
│   │   ├── engine.go
│   │   ├── policies.go
│   │   ├── killswitch.go
│   │   └── circuit_breaker.go
│   ├── execution/              # Execution Engine
│   │   ├── engine.go
│   │   ├── order_sm.go         # Order state machine
│   │   ├── position.go
│   │   └── paper_broker.go
│   ├── audit/                  # Audit trail
│   └── config/                 # Configuration
│
├── intel/                      # Python Intel Pipeline
│   ├── pyproject.toml
│   ├── intel/
│   │   ├── query_brain.py      # Orchestrator (called from Go via gRPC)
│   │   ├── workers/
│   │   │   ├── internal_llm.py
│   │   │   ├── grok_connector.py
│   │   │   └── claude_connector.py
│   │   ├── validation/
│   │   │   ├── dedup.py
│   │   │   ├── confidence.py
│   │   │   └── schema_validator.py
│   │   └── taxonomy/
│   │       └── topics_v1.yaml
│   └── tests/
│
├── research/                   # Python Research/Backtest
│   ├── notebooks/
│   ├── backtest/
│   └── analysis/
│
├── config/                     # Configuration files
│   ├── config.example.yaml
│   ├── risk_policies.yaml
│   ├── scoring_weights.yaml
│   ├── topics_taxonomy.yaml
│   └── exchanges/
│       ├── kraken.yaml
│       └── binance.yaml
│
├── deploy/                     # Deployment
│   ├── docker-compose.yml      # Local dev
│   ├── docker-compose.prod.yml
│   ├── Dockerfile.core
│   ├── Dockerfile.intel
│   ├── redpanda/
│   ├── clickhouse/
│   │   └── init.sql            # Schema z sekcji 4
│   ├── prometheus/
│   └── grafana/
│       └── dashboards/
│
├── scripts/                    # Utility scripts
│   ├── setup-dev.sh
│   ├── provision-topics.sh
│   └── replay.sh
│
└── tests/                      # Integration tests
    ├── replay_determinism_test.go
    ├── risk_invariants_test.go
    ├── crash_recovery_test.go
    └── chaos_test.go
```

---

## 10. Moje zobowiązanie jako partner

```
Co dostarczam:
  1. Architekturę — przemyślaną, uzasadnioną, z trade-offs
  2. Implementację — moduł po module, testowaną
  3. Szczerą opinię — gdy coś jest złe, mówię wprost
  4. Iterację — plan to plan, nie prawo. Zmieniam gdy dane mówią inaczej.

Czego oczekuję od Ciebie:
  1. Decyzji — gdy daję opcje A vs B, wybierasz
  2. Domain knowledge — znasz rynki lepiej niż ja
  3. Push-back — gdy moja architektura jest za skomplikowana, mów
  4. Cierpliwości — S0-S2 to fundamenty. Nudne, ale krytyczne.
```

---

*NEXUS v1.0 Plan — wersja do dyskusji, nie do wyrycia w kamieniu.*
