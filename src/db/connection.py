"""Database connection management.

Uses SQLAlchemy 2.0 async engine with connection pooling.
"""

from sqlalchemy.ext.asyncio import create_async_engine, AsyncSession, async_sessionmaker
from sqlalchemy import text
import logging

logger = logging.getLogger(__name__)


class Database:
    """Async database connection manager."""

    def __init__(self, dsn: str, echo: bool = False):
        # Convert postgresql:// to postgresql+asyncpg://
        if dsn.startswith("postgresql://"):
            dsn = dsn.replace("postgresql://", "postgresql+asyncpg://", 1)
        self._dsn = dsn
        self._engine = None
        self._session_factory = None

    async def connect(self):
        """Create engine and session factory."""
        self._engine = create_async_engine(
            self._dsn,
            echo=False,
            pool_size=10,
            max_overflow=20,
            pool_timeout=30,
            pool_recycle=3600,
        )
        self._session_factory = async_sessionmaker(
            self._engine,
            class_=AsyncSession,
            expire_on_commit=False
        )
        logger.info("Database connected")

    async def disconnect(self):
        """Close engine."""
        if self._engine:
            await self._engine.dispose()
            logger.info("Database disconnected")

    def get_session(self) -> AsyncSession:
        """Get a new async session."""
        if not self._session_factory:
            raise RuntimeError("Database not connected")
        return self._session_factory()

    async def create_tables(self):
        """Create all tables."""
        from src.db.models import Base
        async with self._engine.begin() as conn:
            await conn.run_sync(Base.metadata.create_all)
        logger.info("Database tables created")

    async def setup_timescale(self):
        """Setup TimescaleDB hypertables. Requires TimescaleDB extension."""
        from src.db.models import TIMESCALE_SETUP_SQL
        try:
            async with self._engine.begin() as conn:
                await conn.execute(text("CREATE EXTENSION IF NOT EXISTS timescaledb CASCADE"))
                for stmt in TIMESCALE_SETUP_SQL.strip().split(';'):
                    stmt = stmt.strip()
                    if stmt and not stmt.startswith('--'):
                        try:
                            await conn.execute(text(stmt))
                        except Exception as e:
                            logger.warning(f"TimescaleDB setup warning: {e}")
            logger.info("TimescaleDB hypertables configured")
        except Exception as e:
            logger.warning(f"TimescaleDB not available, continuing without hypertables: {e}")

    async def health_check(self) -> bool:
        """Check database connectivity."""
        try:
            async with self._engine.begin() as conn:
                await conn.execute(text("SELECT 1"))
            return True
        except Exception:
            return False
