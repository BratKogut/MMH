"""
MMH v3.1 -- Centralised configuration via pydantic-settings.

Every tunable knob lives here.  Environment variables override defaults
using the ``MMH_`` prefix (e.g. ``MMH_DRY_RUN=true``).

Usage:
    from src.config.settings import get_settings
    settings = get_settings()          # cached singleton
    print(settings.solana_rpc_url)
"""

from __future__ import annotations

from functools import lru_cache

from pydantic import Field
from pydantic_settings import BaseSettings, SettingsConfigDict


class MMHSettings(BaseSettings):
    """Top-level configuration for the Multi-Chain Memecoin Hunter."""

    # ------------------------------------------------------------------
    # General
    # ------------------------------------------------------------------
    instance_id: str = "mmh-node-1"
    environment: str = "production"  # production | staging | development
    dry_run: bool = False  # First-class dry-run mode

    # ------------------------------------------------------------------
    # Redis
    # ------------------------------------------------------------------
    redis_url: str = "redis://localhost:6379"
    redis_max_memory: str = "2gb"
    redis_stream_maxlen: int = 5000

    # ------------------------------------------------------------------
    # PostgreSQL
    # ------------------------------------------------------------------
    postgres_dsn: str = "postgresql://mmh:mmh@localhost:5432/mmh"

    # ------------------------------------------------------------------
    # RocksDB WAL
    # ------------------------------------------------------------------
    wal_data_dir: str = "./data/wal"
    wal_checkpoint_interval_seconds: int = 300
    wal_retention_hours: int = 168  # 7 days

    # ------------------------------------------------------------------
    # ZMQ (optional fast-lane)
    # ------------------------------------------------------------------
    zmq_enabled: bool = False
    zmq_pub_address: str = "tcp://127.0.0.1:5555"
    zmq_sub_address: str = "tcp://127.0.0.1:5556"

    # ------------------------------------------------------------------
    # Chains
    # ------------------------------------------------------------------
    enabled_chains: list[str] = Field(default=["solana", "base"])

    # ------------------------------------------------------------------
    # Solana
    # ------------------------------------------------------------------
    solana_rpc_url: str = ""
    solana_ws_url: str = ""
    helius_api_key: str = ""
    helius_ws_url: str = ""
    birdeye_api_key: str = ""
    pumpportal_ws_url: str = "wss://pumpportal.fun/api/data"
    jito_tip_lamports: int = 10000

    # ------------------------------------------------------------------
    # Base / EVM
    # ------------------------------------------------------------------
    base_rpc_url: str = ""
    base_ws_url: str = ""
    alchemy_api_key: str = ""
    goplus_api_url: str = "https://api.gopluslabs.io/api/v1"
    zerox_api_key: str = ""

    # ------------------------------------------------------------------
    # Risk limits  (SAFETY > PROFIT)
    # ------------------------------------------------------------------
    max_portfolio_exposure_usd: float = 500.0
    max_positions_per_chain: int = 5
    max_single_position_pct: float = 20.0  # % of portfolio
    daily_loss_limit_usd: float = 200.0
    max_slippage_bps: int = 300

    # ------------------------------------------------------------------
    # Scoring thresholds
    # ------------------------------------------------------------------
    min_score_strong_buy: int = 80
    min_score_buy: int = 60

    # ------------------------------------------------------------------
    # Position management
    # ------------------------------------------------------------------
    default_take_profit_levels: list[float] = Field(
        default=[50.0, 100.0, 200.0],
    )
    default_stop_loss_pct: float = -30.0
    default_trailing_stop_pct: float = 0.0
    max_holding_seconds: int = 3600

    # ------------------------------------------------------------------
    # Circuit breaker
    # ------------------------------------------------------------------
    circuit_breaker_threshold: int = 5
    circuit_breaker_timeout_seconds: int = 60

    # ------------------------------------------------------------------
    # Rate limits (calls per second)
    # ------------------------------------------------------------------
    rate_limit_birdeye: float = 1.0
    rate_limit_goplus: float = 0.5
    rate_limit_helius: float = 10.0
    rate_limit_basescan: float = 5.0
    rate_limit_dexscreener: float = 5.0

    # ------------------------------------------------------------------
    # Heartbeat
    # ------------------------------------------------------------------
    heartbeat_interval_seconds: float = 10.0
    heartbeat_timeout_seconds: float = 30.0

    # ------------------------------------------------------------------
    # Observability
    # ------------------------------------------------------------------
    prometheus_port: int = 8000
    log_level: str = "INFO"
    log_format: str = "json"  # json | text

    # ------------------------------------------------------------------
    # Telegram
    # ------------------------------------------------------------------
    telegram_bot_token: str = ""
    telegram_chat_id: str = ""

    # ------------------------------------------------------------------
    # Idempotency
    # ------------------------------------------------------------------
    dedup_ttl_seconds: int = 86400  # 24 h

    # ------------------------------------------------------------------
    # Pydantic-settings config
    # ------------------------------------------------------------------
    model_config = SettingsConfigDict(
        env_prefix="MMH_",
        env_file=".env",
        env_file_encoding="utf-8",
    )


@lru_cache(maxsize=1)
def get_settings() -> MMHSettings:
    """Return a cached singleton of the application settings.

    Call ``get_settings.cache_clear()`` in tests to reset.
    """
    return MMHSettings()
