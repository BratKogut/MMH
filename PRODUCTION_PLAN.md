# MMH v3.1 — Plan osiagniecia wersji produkcyjnej

**Data:** 2026-02-22 (updated: 2026-02-22)
**Stan aktualny:** Pre-produkcja — wszystkie 8 faz zaimplementowane
**Codebase:** ~15 000+ LOC Python, 60+ modulow, 1500+ LOC testow

---

## Podsumowanie stanu repozytorium

### Co juz jest (mocne strony)

- Kompletna architektura event-driven (Redis Streams + consumer groups + DLQ)
- Pipeline: Collector → L0 Sanitizer → Enrichment → Scoring → RiskFabric → Executor → PositionManager
- WAL (Write-Ahead Log) oparty na RocksDB z replay i snapshotami
- Idempotency na Executorze (dedup store w Redis)
- Circuit breaker na poziomie wykonywania transakcji
- First-class dry-run mode (generuje eventy bez realnych TX)
- RiskFabric z 6 politykami (exposure, pozycje/chain, single position, daily loss, duplikaty, freeze)
- Metryki Prometheus (stream lag, DLQ, scoring latency, risk rejects, exposure gauges)
- Decision Journal (audyt kazdej decyzji scoring/risk/execution)
- Konfiguracja przez pydantic-settings z prefixem `MMH_`
- Docker Compose z PostgreSQL/TimescaleDB, Redis, Prometheus, Grafana
- Collectory dla Solana (PumpPortal WS) i EVM (Base, BSC, Arbitrum)
- Scoring z wagami per-chain (konfiguracja YAML)
- Testy jednostkowe dla kluczowych modulow (events, executor, sanitizer, scoring, risk, position_manager)

### Co wymaga pracy (krytyczne braki)

- Brak chain executors (swap logic) — Executor nie ma implementacji realnych transakcji
- Brak Alembic migrations — schemat tworzony przez `create_all()`
- Brak CI/CD pipeline
- Brak Redis auth i slabe domyslne haslo PostgreSQL
- Brak indeksow na czesto odpytywanych kolumnach
- Retention policies w TimescaleDB zakomentowane
- Dockerfile uruchamiany jako root
- Testy pokrywaja ~6% kodu (680/11300 LOC)
- Brak integration tests z prawdziwym Redis/PostgreSQL
- Brak Alembic, brak strategii migracji schematu
- Brak walletow/kluczy prywatnych w architekturze
- Brak Telegram bot command handler

---

## Fazy wdrozenia produkcyjnego

### FAZA 0 — Bezpieczenstwo i fundament (krytyczne, blokujace)

> Bez tej fazy NIGDY nie uruchamiaj z prawdziwymi srodkami.

#### 0.1 Zarzadzanie sekretami i kluczami prywatnych
- [x] Implementacja wallet management (Solana keypair, EVM private key) → `src/wallet/wallet_manager.py`
- [ ] Klucze NIGDY w kodzie/env file — uzyj systemu zarzadzania sekretami:
  - Opcja prosta: encrypted `.env` + runtime decrypt (sops/age)
  - Opcja produkcyjna: HashiCorp Vault / AWS Secrets Manager / GCP Secret Manager
- [x] Oddzielne wallety: hot wallet (trading) vs cold wallet (glowne srodki)
- [ ] Limit srodkow na hot wallet (max 1-2 dniowy budzet)
- [x] Redis AUTH: dodaj `requirepass` do konfiguracji Redis → `docker-compose.yml`
- [x] PostgreSQL: usun domyslne haslo `mmh`, wymusz silne haslo przez env → `docker-compose.yml`

#### 0.2 Hardening Dockerfile
- [x] Dodaj `RUN useradd -r -s /bin/false mmh` + `USER mmh` przed CMD → `Dockerfile`
- [x] Multi-stage build (builder stage z build-essential, final stage slim) → `Dockerfile`
- [x] Przypnij wersje obrazow (nie `latest`) → `docker-compose.yml`
  - `timescale/timescaledb:2.14.2-pg15`
  - `redis:7.2-alpine`
  - `prom/prometheus:v2.50.0`
  - `grafana/grafana:10.3.1`
- [x] Dodaj `COPY --chown=mmh:mmh` zamiast COPY jako root → `Dockerfile`
- [x] Dodaj `.dockerignore` (wyklucz `.git`, `tests`, `__pycache__`, `.env`) → `.dockerignore`

#### 0.3 Ograniczenie zasobow Docker
- [x] Dodaj `deploy.resources.limits` w docker-compose → `docker-compose.yml`
- [x] Wlacz log rotation: `logging.driver: json-file` z `max-size`/`max-file` → `docker-compose.yml`

---

### FAZA 1 — Chain Executors (core trading logic)

> Bez tego system nie moze handlowac — Executor ma framework, ale brak implementacji swap.

#### 1.1 Solana Executor
- [x] Implementacja `SolanaExecutor` → `src/executor/solana_executor.py`
  - Jupiter v6 API (swap quote + swap)
  - Jito bundles (MEV protection via `mainnet.block-engine.jito.wtf`)
  - Obsuga priority fees (compute budget)
  - Transaction confirmation polling (z timeout)
  - Slippage protection (configurable max BPS z settings)
- [x] Mapowanie w `main.py`: `chain_executors={"solana": solana_executor.execute}` → `src/main.py`
- [x] Dry-run vs real: dry-run pobiera quote bez submit TX

#### 1.2 Base/EVM Executor
- [x] Implementacja `EVMExecutor` → `src/executor/evm_executor.py`
  - Uniswap V3 Router swap (bezposrednio lub przez 0x API)
  - Gas estimation + EIP-1559 dynamic fees
  - Nonce management (kolejkowanie + retry na nonce conflict)
  - Transaction receipt polling
- [x] Slippage protection (minAmountOut)
- [x] Mapowanie: `chain_executors={"base": evm_executor.execute}` → `src/main.py`

#### 1.3 Exit Executor (sprzedaz pozycji)
- [x] ExitExecutor deleguje do chain-specific executors → `src/executor/exit_executor.py`
- [x] Reverse swap path (token → SOL/ETH) — via SolanaExecutor SELL + EVMExecutor SELL
- [ ] Partial sell (np. 33% na kazdym TP level)

---

### FAZA 2 — Baza danych produkcyjna

#### 2.1 Alembic migrations
- [x] Alembic configuration → `alembic.ini`, `src/db/migrations/env.py`, `src/db/migrations/script.py.mako`
- [x] Comprehensive init_db.sql with all tables, indexes, retention → `scripts/init_db.sql`
- [ ] Generuj initial migration z obecnego `models.py`
- [ ] Zamien `create_all()` na `alembic upgrade head` w startup

#### 2.2 Brakujace indeksy
- [ ] `tokens`: index na `(chain, address)` — juz jest UniqueConstraint, ale dodaj B-tree
- [x] `token_scores` / `scoring_results`: index na `(chain, token_address)` → `scripts/init_db.sql`
- [x] `security_checks`: compound index `(chain, token_address)`, `(provider, created_at)` → `scripts/init_db.sql`
- [x] `transactions` / `trades`: index na `position_id`, `tx_hash` → `scripts/init_db.sql`
- [x] `decision_journal`: index na `(decision_type, module)`, `created_at` → `scripts/init_db.sql`

#### 2.3 Retencja danych
- [x] Wlacz retention policies → `scripts/init_db.sql`
  - scoring_results: 30 days
  - security_checks: 14 days
  - decision_journal: 90 days
  - token_events: 30 days
  - risk_decisions: 60 days
  - system_events: 30 days
- [x] Wlacz TimescaleDB continuous aggregates → `scripts/init_db.sql` (scoring_hourly)
- [x] Decision journal retention: 90 dni

#### 2.4 Foreign keys i integralnosc
- [x] Dodaj `REFERENCES positions(id)` na `trades.position_id` → `scripts/init_db.sql`
- [x] Auto-update triggers for `updated_at` → `scripts/init_db.sql`

#### 2.5 Backup
- [x] Redis RDB/AOF backup (AOF + periodic RDB snapshot via `save 900 1; save 300 10`) → `docker-compose.yml`
- [ ] Skrypt `pg_dump` dzienny do S3/GCS/local
- [ ] Testowanie restore procedure

---

### FAZA 3 — Testy i CI/CD

#### 3.1 Pokrycie testami (cel: >60% coverage)
- [x] Unit tests dla brakujacych modulow:
  - [x] `collector/solana.py` i `collector/evm.py` → `tests/test_collector.py`
  - [x] providerow (birdeye, goplus) → `tests/test_birdeye.py`, `tests/test_goplus.py`
  - [x] `wal/raw_wal.py` → `tests/test_wal.py`
  - [x] `utils/circuit_breaker.py` → `tests/test_circuit_breaker.py`
  - [x] `utils/rate_limiter.py` → `tests/test_rate_limiter.py`
  - [x] `utils/idempotency.py` → `tests/test_idempotency.py`
  - [x] `wallet/wallet_manager.py` → `tests/test_wallet.py`
  - [ ] `bus/redis_streams.py` i `bus/consumer.py`
  - [ ] `control/control_plane.py`
- [ ] Integration tests:
  - Redis Streams: publish → consume → ack → DLQ flow
  - PostgreSQL: CRUD na wszystkich modelach
  - Full pipeline: collector mock → ... → position created (dry-run)
- [x] Dodaj `pyproject.toml` z konfiguracja testow → `pyproject.toml`
- [ ] Fixture'y z testcontainers (Redis + Postgres w Docker)

#### 3.2 CI/CD Pipeline (GitHub Actions)
- [x] `.github/workflows/ci.yml` → `.github/workflows/ci.yml`
  - lint (black, isort, flake8, mypy)
  - test (Redis + Postgres services, pytest --cov)
  - build (Docker build + Trivy scan)
  - security (TruffleHog secrets scan)
- [ ] `.github/workflows/deploy.yml` (na main):
  - Build + push do container registry
  - Deploy do VPS (ssh + docker-compose pull + up)

#### 3.3 Pre-commit hooks
- [x] `.pre-commit-config.yaml` → `.pre-commit-config.yaml`
  - black, isort, flake8
  - trailing-whitespace, end-of-file-fixer, check-yaml
  - detect-private-key, check-merge-conflict
  - gitleaks (secrets detection)

---

### FAZA 4 — Observability produkcyjna

#### 4.1 Rozszerzenie logow
- [ ] Dodaj `exc_info=True` do wszystkich `logger.error()` (traceback zamiast samego message)
- [ ] Strukturalne logi dla krytycznych decyzji:
  - Scoring: log token_address, score, recommendation, factors
  - Risk: log intent_id, decision, reason_codes, exposure_snapshot
  - Execution: log intent_id, chain, tx_hash, amount, price, status
- [ ] Request/response logging dla external APIs (birdeye, goplus) z latency

#### 4.2 Alarmy
- [x] Prometheus alerting rules → `config/prometheus/alerts.yml`:
  - `mmh_stream_lag > 100` przez 5m → WARN
  - `mmh_dlq_additions_total` rate > 5/min → CRITICAL
  - `mmh_execution_failures_total` rate > 3/min → CRITICAL
  - `mmh_circuit_breaker_state == OPEN` → CRITICAL
  - `mmh_exposure_usd > MAX_PORTFOLIO * 0.9` → WARN
  - Heartbeat missing > 60s → CRITICAL
- [ ] Alertmanager → Telegram webhook

#### 4.3 Grafana dashboards
- [x] Grafana provisioning setup (datasources + dashboard folder) → `config/grafana/provisioning/`
- [ ] Dashboard: Pipeline Overview (tokens/min, scoring latency P95, risk decisions pie chart)
- [ ] Dashboard: Trading (open positions, PnL timeline, execution success rate)
- [ ] Dashboard: Infrastructure (Redis memory, PG connections, container CPU/mem)
- [ ] Dashboard: Risk (exposure gauge, daily PnL, frozen chains, reject reasons)

#### 4.4 Log aggregation (opcjonalnie)
- [ ] Rozwazyc Grafana Loki + Promtail (lekkie, integracja z Grafana)
- [ ] Lub: centralized syslog do pliku z logrotate

---

### FAZA 5 — Telegram Bot (command interface)

#### 5.1 Bot command handler
- [x] Implementacja `TelegramBot` class → `src/telegram/bot.py`
  - `/status` — pipeline overview (services running, positions, exposure)
  - `/positions` — lista otwartych pozycji z PnL
  - `/portfolio` — summary (total exposure, daily PnL, win rate)
  - `/sell <position_id>` — manual sell position
  - `/freeze <chain>` — freeze chain
  - `/resume <chain>` — resume chain
  - `/kill` — emergency stop all trading
- [x] Integracja z ControlPlane (komendy przez control:commands stream)
- [x] Autoryzacja: whitelist chat_id (juz jest w config)
- [x] Alerty automatyczne:
  - Nowa pozycja otwarta
  - TP/SL hit
  - Circuit breaker activated
  - Daily loss limit approaching

---

### FAZA 6 — Hardening i edge cases

#### 6.1 Error handling
- [ ] Zamien `except Exception as e: logger.error(f"...")` na:
  - Kategorie bledow: `RetryableError`, `FatalError`, `ConfigError`
  - Retryable: network timeout, rate limit → exponential backoff
  - Fatal: invalid config, auth failure → freeze + alert
- [ ] Timeout handling na external API calls:
  - Birdeye: 10s timeout, 3 retries
  - GoPlus: 15s timeout, 2 retries
  - RPC nodes: 5s timeout, fallback RPC URL
- [ ] Graceful degradation: jesli enrichment fail → scoring z partial data (lower confidence)

#### 6.2 Input validation
- [ ] Walidacja adresow on-chain:
  - Solana: base58 decode + 32 bytes check
  - EVM: checksum validation (EIP-55)
- [ ] Numeryczne: ochrona przed NaN/Inf w float conversion
- [ ] Sanityzacja danych z WebSocket (malformed JSON, unexpected fields)

#### 6.3 Reorg handling
- [ ] Solana: monitor slot confirmation level (finalized vs confirmed)
- [ ] EVM: czekaj na N potwierdzenia przed uznaniem TX za confirmed
- [ ] PositionManager: obsluga `ReorgDetected` event (ponowna walidacja)

#### 6.4 Nonce management (EVM)
- [ ] Local nonce tracker z sync do chain
- [ ] Nonce gap detection + recovery
- [ ] Stuck transaction replacement (speed up z wyzszym gas)

#### 6.5 Rate limiting
- [ ] Rate limiter juz istnieje w `utils/rate_limiter.py` — upewnic sie ze jest uzywany wszedzie:
  - Birdeye API calls
  - GoPlus API calls
  - RPC node calls
  - Dexscreener calls
- [ ] Adaptive rate limiting: slow down on 429 responses

---

### FAZA 7 — Staging i dry-run na mainnet

#### 7.1 Staging environment
- [x] Oddzielny docker-compose.staging.yml (inne porty, osobna DB) → `docker-compose.staging.yml`
- [ ] Dry-run na mainnet (prawdziwe dane, symulowane transakcje)
- [ ] Monitoring minimum 7 dni:
  - Ile tokenow przechodzi przez pipeline?
  - Jakie sa scoring decisions?
  - Ile by bylo STRONG_BUY/BUY?
  - Ile by RiskFabric odrzucil?
  - Jak wyglada latency P95?
  - Czy DLQ sie nie zapelnia?

#### 7.2 Paper trading analysis
- [ ] Decision Journal analysis:
  - Retrospektywna analiza: czy tokeny ktore system chcial kupic faktycznie wzrosly?
  - Hit rate estimation
  - Sredni PnL gdyby system realnie handlowal
- [ ] Scoring weights tuning na podstawie danych
- [ ] Risk limits calibration

#### 7.3 Chaos testing
- [ ] Redis restart → system recovers (consumer groups + pending messages)
- [ ] PostgreSQL restart → system continues (graceful degradation)
- [ ] WebSocket disconnect → auto-reconnect collectors
- [ ] Network partition → circuit breaker triggers

---

### FAZA 8 — Go-live (real trading)

#### 8.1 Pre-launch checklist
- [ ] Wszystkie testy przchodza (unit + integration)
- [ ] CI/CD pipeline dziala
- [ ] Minimum 7 dni dry-run bez bledow krytycznych
- [ ] Alerty Telegram dzialaja
- [ ] Backup PostgreSQL przetestowany (backup + restore)
- [ ] Hot wallet zasilony minimalna kwota (np. $50 SOL + $50 ETH)
- [ ] Risk limits ustawione konserwatywnie:
  - `MAX_PORTFOLIO_EXPOSURE_USD=100` (start niski)
  - `DAILY_LOSS_LIMIT_USD=50`
  - `MAX_POSITIONS_PER_CHAIN=2`
- [ ] Operator ma dostep do `/kill` i `/freeze` na Telegram

#### 8.2 Soft launch
- [ ] Wlacz na 1 chain (Solana — najlepiej przetestowany collector)
- [ ] Tylko STRONG_BUY (min_score_strong_buy=85)
- [ ] Malé pozycje ($10-25)
- [ ] Monitoring 24/7 przez pierwsze 3 dni
- [ ] Codzienne review Decision Journal

#### 8.3 Skalowanie
- [ ] Po 7 dniach stabilnej pracy: zwieksz limity
- [ ] Dodaj Base chain
- [ ] Zwieksz pozycje do docelowych wartosci
- [ ] Wlacz trailing stop
- [ ] Rozszerz scoring o dodatkowe czynniki

---

## Priorytety (co robic w jakiej kolejnosci)

```
FAZA 0 (Bezpieczenstwo)     ████████████████████  BLOKUJACE
FAZA 1 (Chain Executors)     ████████████████████  BLOKUJACE
FAZA 2 (DB produkcyjna)     ██████████████░░░░░░  WYSOKI
FAZA 3 (Testy + CI/CD)      ██████████████░░░░░░  WYSOKI
FAZA 4 (Observability)      ████████████░░░░░░░░  SREDNI
FAZA 5 (Telegram Bot)       ██████████░░░░░░░░░░  SREDNI
FAZA 6 (Hardening)          ████████░░░░░░░░░░░░  SREDNI
FAZA 7 (Staging/Dry-run)    ██████████████░░░░░░  WYSOKI
FAZA 8 (Go-live)            ████████████████████  FINALNA
```

**Sciezka krytyczna:** FAZA 0 → FAZA 1 → FAZA 2 → FAZA 3 → FAZA 7 → FAZA 8

FAZY 4, 5, 6 moga byc realizowane rownolegle z FAZA 2-3.

---

## Szacowane naklady pracy (zespol 1-2 osoby)

| Faza | Opis | Estymacja |
|------|------|-----------|
| 0 | Bezpieczenstwo | 2-3 dni |
| 1 | Chain Executors | 5-7 dni |
| 2 | DB produkcyjna | 2-3 dni |
| 3 | Testy + CI/CD | 4-5 dni |
| 4 | Observability | 2-3 dni |
| 5 | Telegram Bot | 3-4 dni |
| 6 | Hardening | 3-5 dni |
| 7 | Staging (7d dry-run) | 7-14 dni |
| 8 | Go-live | 1 dzien |
| **TOTAL** | | **~30-45 dni** |

---

## Ryzyka

| Ryzyko | Wplyw | Mitygacja |
|--------|-------|-----------|
| Utrata srodkow z hot wallet (hack/bug) | KRYTYCZNY | Min srodkow na wallet, kill switch, daily loss limit |
| MEV/sandwich attack | WYSOKI | Jito bundles (Solana), prywatne RPC (Base) |
| API provider downtime (Helius, Birdeye) | SREDNI | Fallback providers, circuit breaker, graceful degradation |
| Redis data loss | SREDNI | AOF persistence, WAL jako backup, idempotent replay |
| False positive scoring (kupno scamu) | WYSOKI | Two-level security check, konserwatywne progi, small positions |
| Rate limiting na API | NISKI | Rate limiter juz wbudowany, backoff strategy |
| Reorg na chainie | SREDNI | Confirmation wait, reorg event handling |

---

## Notatki koncowe

System jest architektonicznie dobrze zaprojektowany. Fundamenty (event bus, WAL, idempotency, circuit breaker, risk policies) sa solidne. Glownym brakiem jest **implementacja realnych transakcji** (chain executors) — bez tego system moze tylko analizowac, nie handlowac.

Kolejnosc priorytetow: **bezpieczenstwo → trading logic → database → testy → staging → produkcja**.
