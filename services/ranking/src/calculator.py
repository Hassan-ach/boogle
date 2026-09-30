import logging
import time
import asyncio
from psql import DatabaseManager, retry_on_db_error

logger = logging.getLogger(__name__)


class RankingCalculator:
    """Handles IDF and PageRank computation algorithms against PostgreSQL."""

    def __init__(self, db_manager: DatabaseManager):
        self.db_manager = db_manager

    @retry_on_db_error(max_retries=3, delay=1.0, backoff=2.0)
    async def compute_idf(self) -> int:
        """Compute and update IDF (Inverse Document Frequency) values in the database."""
        start_time = time.time()
        logger.info("Starting IDF calculation...")

        async with self.db_manager.get_connection() as conn:
            try:
                async with conn.cursor() as cursor:
                    await cursor.execute("""
                        UPDATE words
                        SET idf = LOG((SELECT COUNT(*) FROM pages) / (1 + sub.df))
                        FROM (
                            SELECT word_id, COUNT(DISTINCT page_id) AS df
                            FROM page_word
                            GROUP BY word_id
                        ) sub
                        WHERE words.id = sub.word_id
                    """)
                    affected_rows = cursor.rowcount

                await conn.commit()
                duration = time.time() - start_time
                logger.info(
                    f"IDF updated for {affected_rows} words in {duration:.2f}s"
                )
                return affected_rows
            except Exception:
                logger.exception("IDF calculation failed")
                await conn.rollback()
                raise

    @retry_on_db_error(max_retries=3, delay=1.0, backoff=2.0)
    async def update_pagerank(
        self, iterations: int = 20, damping_factor: float = 0.85
    ) -> int:
        """Execute PL/pgSQL function to calculate and update PageRank values."""
        start_time = time.time()
        logger.info("Starting PageRank calculation...")

        async with self.db_manager.get_connection() as conn:
            try:
                async with conn.cursor() as cursor:
                    await cursor.execute(
                        "SELECT update_page_rank(%s, %s);",
                        (iterations, damping_factor),
                    )
                    affected_rows = cursor.rowcount

                await conn.commit()
                duration = time.time() - start_time
                logger.info(
                    f"PageRank updated for {affected_rows} pages in {duration:.2f}s"
                )
                return affected_rows
            except Exception:
                logger.exception("Failed to update PageRank values")
                await conn.rollback()
                raise

    async def run_pipeline(
        self, iterations: int = 20, damping_factor: float = 0.85
    ) -> None:
        """Run full ranking pipeline: IDF and PageRank concurrently."""
        logger.info("Executing full ranking pipeline (IDF + PageRank)...")
        await asyncio.gather(
            self.compute_idf(),
            self.update_pagerank(
                iterations=iterations, damping_factor=damping_factor
            ),
        )
        logger.info("Ranking pipeline execution completed.")
