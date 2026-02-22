"""MMH v3.1 - Main Application Entry Point.

Orchestrates all services following the build order:
1. Configuration loading
2. Database connection
3. Redis connection
4. WAL initialization
5. Redis Streams event bus
6. Collectors (per chain)
7. L0 Sanitizer
8. Enrichment/Security
9. Scoring Engine
10. RiskFabric
11. Executor
12. Position Manager
13. ControlPlane
14. Observability (metrics, tracing, decision journal)

Axioms:
- SAFETY > PROFIT
- Single Source of Truth (WAL for events, Position Ledger for positions, Control Log for commands)
- At-least-once + idempotency everywhere
- Replay is a mode of operation, not a feature
"""

import asyncio
import logging
import logging.config
import signal
import sys
import json
import os
from typing import Optional


# ---------------------------------------------------------------------------
# Logging setup -- MUST happen before any other import that calls getLogger
# ---------------------------------------------------------------------------

def setup_logging(log_level: str = "INFO", log_format: str = "json"):
    """Setup structured JSON logging.

    Two modes are supported:
    - ``json``  -- machine-parseable JSON-ish format (default)
    - ``text``  -- human-readable format for local development

    Args:
        log_level: Root log level (DEBUG, INFO, WARNING, ERROR, CRITICAL).
        log_format: ``"json"`` or ``"text"``.
    """
    if log_format == "json":
        config = {
            "version": 1,
            "disable_existing_loggers": False,
            "formatters": {
                "json": {
                    "class": "logging.Formatter",
                    "format": json.dumps({
                        "time": "%(asctime)s",
                        "level": "%(levelname)s",
                        "module": "%(name)s",
                        "message": "%(message)s",
                    }),
                }
            },
            "handlers": {
                "console": {
                    "class": "logging.StreamHandler",
                    "formatter": "json",
                    "stream": "ext://sys.stdout",
                }
            },
            "root": {
                "level": log_level,
                "handlers": ["console"],
            },
        }
    else:
        config = {
            "version": 1,
            "disable_existing_loggers": False,
            "formatters": {
                "standard": {
                    "format": "%(asctime)s [%(levelname)s] %(name)s: %(message)s",
                }
            },
            "handlers": {
                "console": {
                    "class": "logging.StreamHandler",
                    "formatter": "standard",
                    "stream": "ext://sys.stdout",
                }
            },
            "root": {
                "level": log_level,
                "handlers": ["console"],
            },
        }
    logging.config.dictConfig(config)


logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Application orchestrator
# ---------------------------------------------------------------------------

class MMHApplication:
    """Main application orchestrator.

    Owns the lifecycle of every service in the pipeline.  Initialisation
    follows a strict order (see :meth:`initialize`) and shutdown tears
    everything down in reverse.

    Usage::

        app = MMHApplication()
        await app.initialize()
        await app.run()        # blocks until shutdown signal
        await app.shutdown()
    """

    def __init__(self):
        self._settings = None
        self._db = None
        self._redis = None
        self._bus = None
        self._wal_instances: dict = {}  # chain -> RawWAL
        self._services: list = []
        self._tasks: list = []
        self._shutdown_event = asyncio.Event()

    # ------------------------------------------------------------------
    # Initialisation (strict order)
    # ------------------------------------------------------------------

    async def initialize(self):
        """Initialize all components in dependency order.

        The order matters -- later stages depend on earlier ones:

        1.  Configuration
        2.  Observability (metrics server)
        3.  Redis connection
        4.  PostgreSQL connection
        5.  Redis Streams event bus
        6.  WAL per chain
        7.  Collectors per chain
        8.  L0 Sanitizer
        9.  Enrichment / Security
        10. Scoring Engine
        11. RiskFabric
        12. Executor
        13. Position Manager
        14. ControlPlane
        15. Decision Journal
        """
        logger.info("=" * 60)
        logger.info("MMH v3.1 - Starting up")
        logger.info("SAFETY > PROFIT")
        logger.info("=" * 60)

        # ---- 1. Load configuration ----------------------------------------
        from src.config.settings import get_settings

        self._settings = get_settings()
        setup_logging(self._settings.log_level, self._settings.log_format)

        logger.info("Instance: %s", self._settings.instance_id)
        logger.info("Environment: %s", self._settings.environment)
        logger.info("Dry-run: %s", self._settings.dry_run)
        logger.info("Enabled chains: %s", self._settings.enabled_chains)

        # ---- 2. Initialize observability (metrics) -------------------------
        try:
            from src.observability.metrics import get_metrics

            metrics = get_metrics(self._settings.prometheus_port)
            metrics.start_server()
            metrics.set_build_info(
                "3.1.0",
                self._settings.instance_id,
                self._settings.environment,
            )
            logger.info("Prometheus metrics server started on port %d", self._settings.prometheus_port)
        except ImportError:
            logger.warning(
                "src.observability.metrics not available -- "
                "metrics server will not start"
            )
        except Exception as exc:
            logger.warning("Metrics server failed to start: %s", exc)

        # ---- 3. Connect to Redis ------------------------------------------
        import redis.asyncio as aioredis

        self._redis = aioredis.from_url(
            self._settings.redis_url,
            decode_responses=True,
        )
        await self._redis.ping()
        logger.info("Redis connected at %s", self._settings.redis_url)

        # ---- 4. Connect to PostgreSQL --------------------------------------
        from src.db.connection import Database

        self._db = Database(self._settings.postgres_dsn)
        try:
            await self._db.connect()
            await self._db.create_tables()
            await self._db.setup_timescale()
            logger.info("Database connected and tables created")
        except Exception as exc:
            logger.warning(
                "Database connection failed (continuing without PG): %s", exc
            )
            self._db = None

        # ---- 5. Initialize Redis Event Bus ---------------------------------
        from src.bus.redis_streams import RedisEventBus

        self._bus = RedisEventBus(
            redis_url=self._settings.redis_url,
            maxlen=self._settings.redis_stream_maxlen,
            instance_id=self._settings.instance_id,
        )
        await self._bus.connect()
        logger.info("Redis Event Bus initialized")

        # ---- 6. Initialize WAL per chain -----------------------------------
        # RawWAL(data_dir, chain) internally creates the chain sub-directory
        # via the backend, so we pass the *root* WAL directory -- not a
        # per-chain path -- to avoid double-nesting.
        from src.wal.raw_wal import RawWAL

        for chain in self._settings.enabled_chains:
            wal = RawWAL(self._settings.wal_data_dir, chain)
            self._wal_instances[chain] = wal
            logger.info("WAL initialized for %s", chain)

        # ---- 7. Initialize Collectors --------------------------------------
        from src.collector.solana import SolanaCollector
        from src.collector.evm import EVMCollector

        collectors: list = []
        for chain in self._settings.enabled_chains:
            if chain == "solana":
                collector = SolanaCollector(
                    bus=self._bus,
                    wal=self._wal_instances.get("solana"),
                    config={
                        "pumpportal_ws_url": self._settings.pumpportal_ws_url,
                        "helius_ws_url": self._settings.helius_ws_url,
                        "helius_api_key": self._settings.helius_api_key,
                    },
                )
            elif chain in ("base", "bsc", "arbitrum"):
                # Resolve per-chain WS / RPC URLs with fallback to the
                # ``base_*`` settings for chains that share the same config
                # key structure.
                ws_url = (
                    getattr(self._settings, f"{chain}_ws_url", "")
                    if hasattr(self._settings, f"{chain}_ws_url")
                    else self._settings.base_ws_url
                )
                rpc_url = (
                    getattr(self._settings, f"{chain}_rpc_url", "")
                    if hasattr(self._settings, f"{chain}_rpc_url")
                    else self._settings.base_rpc_url
                )
                collector = EVMCollector(
                    chain=chain,
                    bus=self._bus,
                    wal=self._wal_instances.get(chain),
                    config={
                        f"{chain}_ws_url": ws_url,
                        f"{chain}_rpc_url": rpc_url,
                    },
                )
            else:
                logger.warning("No collector implementation for chain: %s", chain)
                continue
            collectors.append(collector)

        # ---- 8. Initialize L0 Sanitizer ------------------------------------
        from src.sanitizer.l0_sanitizer import L0Sanitizer

        sanitizer = L0Sanitizer(
            bus=self._bus,
            redis_client=self._redis,
            enabled_chains=self._settings.enabled_chains,
        )

        # ---- 9. Initialize Enrichment / Security ---------------------------
        from src.enrichment.security import EnrichmentService
        from src.enrichment.providers.birdeye import BirdeyeProvider
        from src.enrichment.providers.goplus import GoPlusProvider

        providers: dict = {}
        for chain in self._settings.enabled_chains:
            if chain == "solana" and self._settings.birdeye_api_key:
                providers[chain] = [
                    BirdeyeProvider(
                        api_key=self._settings.birdeye_api_key,
                        rate_limit_per_sec=self._settings.rate_limit_birdeye,
                    )
                ]
            elif chain in ("base", "bsc"):
                providers[chain] = [
                    GoPlusProvider(
                        rate_limit_per_sec=self._settings.rate_limit_goplus,
                    )
                ]

        enrichment = EnrichmentService(
            bus=self._bus,
            redis_client=self._redis,
            providers=providers,
            enabled_chains=self._settings.enabled_chains,
        )

        # ---- 10. Initialize Scoring Engine ---------------------------------
        from src.scoring.scoring_engine import ScoringEngine, ScoringConfig

        scoring_config = ScoringConfig(
            min_score_strong_buy=self._settings.min_score_strong_buy,
            min_score_buy=self._settings.min_score_buy,
        )
        scoring = ScoringEngine(
            bus=self._bus,
            config=scoring_config,
            enabled_chains=self._settings.enabled_chains,
        )

        # ---- 11. Initialize RiskFabric -------------------------------------
        from src.risk.risk_fabric import RiskFabric

        risk_config = {
            "max_portfolio_exposure_usd": self._settings.max_portfolio_exposure_usd,
            "max_positions_per_chain": self._settings.max_positions_per_chain,
            "max_single_position_pct": self._settings.max_single_position_pct,
            "daily_loss_limit_usd": self._settings.daily_loss_limit_usd,
        }
        risk_fabric = RiskFabric(
            bus=self._bus,
            redis_client=self._redis,
            config=risk_config,
        )

        # ---- 11b. Initialize Wallet Manager ----------------------------------
        from src.wallet.wallet_manager import WalletManager

        wallet_manager = WalletManager(self._settings)
        for chain in self._settings.enabled_chains:
            if wallet_manager.is_configured(chain):
                logger.info("Wallet configured for %s: %s", chain, wallet_manager.get_address(chain))
            else:
                logger.warning("No wallet configured for %s (dry-run only)", chain)

        # ---- 11c. Initialize Chain Executors ---------------------------------
        from src.executor.solana_executor import SolanaExecutor
        from src.executor.evm_executor import EVMExecutor

        chain_executors: dict = {}
        solana_executor = None
        evm_executors: dict = {}

        for chain in self._settings.enabled_chains:
            if chain == "solana" and self._settings.solana_rpc_url:
                sol_exec = SolanaExecutor(
                    rpc_url=self._settings.solana_rpc_url,
                    wallet_manager=wallet_manager,
                    settings=self._settings,
                )
                chain_executors["solana"] = sol_exec.execute
                solana_executor = sol_exec
                logger.info("SolanaExecutor initialized (Jupiter V6 + Jito)")
            elif chain in ("base", "bsc", "arbitrum"):
                rpc_url = getattr(self._settings, f"{chain}_rpc_url", "") or self._settings.base_rpc_url
                evm_exec = EVMExecutor(
                    chain=chain,
                    rpc_url=rpc_url,
                    wallet_manager=wallet_manager,
                    settings=self._settings,
                )
                chain_executors[chain] = evm_exec.execute
                evm_executors[chain] = evm_exec
                logger.info("EVMExecutor initialized for %s (0x + Uniswap V3)", chain)

        # ---- 12. Initialize Executor ---------------------------------------
        from src.executor.executor import Executor

        executor_config = {
            "circuit_breaker_threshold": self._settings.circuit_breaker_threshold,
            "circuit_breaker_timeout_seconds": self._settings.circuit_breaker_timeout_seconds,
            "max_execution_retries": self._settings.max_execution_retries,
        }
        executor = Executor(
            bus=self._bus,
            redis_client=self._redis,
            config=executor_config,
            chain_executors=chain_executors,
            dry_run=self._settings.dry_run,
        )

        # ---- 13. Initialize Position Manager --------------------------------
        from src.position.position_manager import PositionManager

        position_config = {
            "default_take_profit_levels": self._settings.default_take_profit_levels,
            "default_stop_loss_pct": self._settings.default_stop_loss_pct,
            "default_trailing_stop_pct": self._settings.default_trailing_stop_pct,
            "max_holding_seconds": self._settings.max_holding_seconds,
            "enabled_chains": self._settings.enabled_chains,
        }
        position_manager = PositionManager(
            bus=self._bus,
            redis_client=self._redis,
            db=self._db,
            config=position_config,
        )

        # ---- 14. Initialize ControlPlane ------------------------------------
        from src.control.control_plane import ControlPlane

        control_config = {
            "heartbeat_timeout_seconds": self._settings.heartbeat_timeout_seconds,
        }
        control_plane = ControlPlane(
            bus=self._bus,
            redis_client=self._redis,
            db=self._db,
            config=control_config,
        )

        # ---- 14b. Initialize Exit Executor -----------------------------------
        from src.executor.exit_executor import ExitExecutor

        exit_executor = ExitExecutor(
            solana_executor=solana_executor,
            evm_executors=evm_executors,
            bus=self._bus,
            position_manager=position_manager,
            dry_run=self._settings.dry_run,
        )
        logger.info("ExitExecutor initialized")

        # ---- 15. Initialize Decision Journal --------------------------------
        journal = None
        try:
            from src.observability.decision_journal import DecisionJournal

            journal = DecisionJournal(bus=self._bus, db=self._db)
            logger.info("Decision Journal initialized")
        except ImportError:
            logger.warning(
                "src.observability.decision_journal not available -- "
                "decision journal will not start"
            )

        # ---- 16. Initialize Telegram Bot ------------------------------------
        telegram_bot = None
        if self._settings.telegram_bot_token and self._settings.telegram_chat_id:
            try:
                from src.telegram.bot import TelegramBot

                telegram_bot = TelegramBot(
                    bot_token=self._settings.telegram_bot_token,
                    chat_id=self._settings.telegram_chat_id,
                    redis_client=self._redis,
                    bus=self._bus,
                )
                logger.info("Telegram bot initialized")
            except ImportError:
                logger.warning("python-telegram-bot not available, skipping Telegram bot")
            except Exception as exc:
                logger.warning("Telegram bot init failed: %s", exc)

        # ---- Register all services for lifecycle management -----------------
        # Order matters: this list is iterated forwards for start() and
        # backwards for stop() (graceful reverse teardown).
        self._services = [
            ("collectors", collectors),
            ("sanitizer", sanitizer),
            ("enrichment", enrichment),
            ("scoring", scoring),
            ("risk_fabric", risk_fabric),
            ("executor", executor),
            ("exit_executor", exit_executor),
            ("position_manager", position_manager),
            ("control_plane", control_plane),
        ]
        if journal is not None:
            self._services.append(("decision_journal", journal))
        if telegram_bot is not None:
            self._services.append(("telegram_bot", telegram_bot))

        logger.info("All services initialized")

    # ------------------------------------------------------------------
    # Run loop
    # ------------------------------------------------------------------

    async def run(self):
        """Start all services and block until shutdown signal."""
        logger.info("Starting all services...")

        for name, service in self._services:
            if isinstance(service, list):
                # A list of services (e.g. collectors -- one per chain)
                for svc in service:
                    chain_label = getattr(svc, "_chain", "unknown")
                    task = asyncio.create_task(
                        svc.start(),
                        name=f"{name}:{chain_label}",
                    )
                    self._tasks.append((f"{name}:{chain_label}", task))
                    logger.info("Started %s:%s", name, chain_label)
            else:
                task = asyncio.create_task(
                    service.start(),
                    name=name,
                )
                self._tasks.append((name, task))
                logger.info("Started %s", name)

        logger.info("=" * 60)
        logger.info("MMH v3.1 - All services running")
        logger.info("Dry-run mode: %s", self._settings.dry_run)
        logger.info("=" * 60)

        # Wait for shutdown signal (set by signal handler or fatal error)
        await self._shutdown_event.wait()

    # ------------------------------------------------------------------
    # Graceful shutdown
    # ------------------------------------------------------------------

    async def shutdown(self):
        """Graceful shutdown of all services in reverse order."""
        logger.info("Shutting down MMH v3.1...")

        # 1. Stop all services in reverse order
        for name, service in reversed(self._services):
            try:
                if isinstance(service, list):
                    for svc in service:
                        await svc.stop()
                else:
                    await service.stop()
                logger.info("Stopped %s", name)
            except Exception as exc:
                logger.error("Error stopping %s: %s", name, exc)

        # 2. Cancel remaining async tasks
        for name, task in self._tasks:
            if not task.done():
                task.cancel()
                try:
                    await task
                except asyncio.CancelledError:
                    pass

        # 3. Close event bus
        if self._bus:
            try:
                await self._bus.disconnect()
            except Exception as exc:
                logger.error("Error disconnecting event bus: %s", exc)

        # 4. Close direct Redis connection
        if self._redis:
            try:
                await self._redis.aclose()
            except Exception as exc:
                logger.error("Error closing Redis: %s", exc)

        # 5. Close database
        if self._db:
            try:
                await self._db.disconnect()
            except Exception as exc:
                logger.error("Error disconnecting database: %s", exc)

        # 6. Close WAL instances (flushes indices)
        for chain, wal in self._wal_instances.items():
            try:
                wal.close()
            except Exception as exc:
                logger.error("Error closing WAL for %s: %s", chain, exc)

        logger.info("MMH v3.1 shutdown complete")


# ---------------------------------------------------------------------------
# Async entry point
# ---------------------------------------------------------------------------

async def main():
    """Main async entry point."""
    app = MMHApplication()

    # Register OS signal handlers for graceful shutdown
    loop = asyncio.get_event_loop()

    def signal_handler():
        logger.info("Received shutdown signal")
        app._shutdown_event.set()

    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, signal_handler)

    try:
        await app.initialize()
        await app.run()
    except KeyboardInterrupt:
        logger.info("Keyboard interrupt received")
    except Exception as exc:
        logger.critical("Fatal error: %s", exc, exc_info=True)
    finally:
        await app.shutdown()


if __name__ == "__main__":
    asyncio.run(main())
