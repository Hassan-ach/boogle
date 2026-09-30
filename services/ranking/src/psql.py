import os
import logging
import asyncio
from functools import wraps
from typing import Optional
from contextlib import asynccontextmanager

import psycopg
from psycopg import OperationalError, InterfaceError
from psycopg_pool import AsyncConnectionPool

logger = logging.getLogger(__name__)


def retry_on_db_error(max_retries: int = 3, delay: float = 1.0, backoff: float = 2.0):
    def decorator(func):
        @wraps(func)
        async def wrapper(*args, **kwargs):
            current_delay = delay
            for attempt in range(max_retries + 1):
                try:
                    return await func(*args, **kwargs)
                except (OperationalError, InterfaceError) as e:
                    if attempt < max_retries:
                        logger.warning(
                            f"Database operation failed (attempt {attempt + 1}/{max_retries + 1}): {e}. "
                            f"Retrying in {current_delay:.1f}s..."
                        )
                        await asyncio.sleep(current_delay)
                        current_delay *= backoff
                    else:
                        logger.error(
                            f"Database operation failed after {max_retries + 1} attempts: {e}"
                        )
                        raise
                except Exception as e:
                    logger.error(
                        f"Database operation failed with non-retryable error: {e}"
                    )
                    raise

        return wrapper

    return decorator


class DatabaseManager:
    """Manages PostgreSQL async connection pool lifecycle and connections using psycopg v3."""

    def __init__(
        self,
        min_conn: int = 2,
        max_conn: int = 10,
        connection_url: Optional[str] = None,
    ):
        self.min_conn = min_conn
        self.max_conn = max_conn
        self.connection_url = connection_url or os.getenv("DATABASE_URL")
        self._pool: Optional[AsyncConnectionPool] = None

    async def initialize(self) -> None:
        """Initialize and open the async connection pool."""
        if self._pool is not None:
            logger.warning("Connection pool is already initialized")
            return

        try:
            if self.connection_url:
                conninfo = self.connection_url
            else:
                host = os.getenv("PG_HOST", "localhost")
                port = os.getenv("PG_PORT", "5432")
                dbname = os.getenv("PG_DBNAME", "boogle")
                user = os.getenv("PG_USER", "postgres")
                password = os.getenv("PG_PASSWORD", "")
                conninfo = f"postgresql://{user}:{password}@{host}:{port}/{dbname}"

            self._pool = AsyncConnectionPool(
                conninfo=conninfo,
                min_size=self.min_conn,
                max_size=self.max_conn,
                open=False,
            )
            await self._pool.open()
            logger.info(
                f"Database Connection pool opened ({self.min_conn}-{self.max_conn} connections)"
            )
        except Exception as e:
            logger.error(
                f"Failed to initialize database connection pool: {e}",
                exc_info=True,
            )
            raise

    async def close(self) -> None:
        """Close the async connection pool."""
        if self._pool is not None:
            await self._pool.close()
            self._pool = None
            logger.info("Database Connection pool closed")

    @asynccontextmanager
    async def get_connection(self):
        """Async context manager to get and return a connection from the pool."""
        if self._pool is None:
            logger.warning(
                "Connection pool not initialized, creating direct connection"
            )
            conn = await psycopg.AsyncConnection.connect(
                host=os.getenv("PG_HOST"),
                port=os.getenv("PG_PORT"),
                dbname=os.getenv("PG_DBNAME"),
                user=os.getenv("PG_USER"),
                password=os.getenv("PG_PASSWORD"),
            )
            try:
                yield conn
            finally:
                await conn.close()
        else:
            async with self._pool.connection() as conn:
                yield conn
