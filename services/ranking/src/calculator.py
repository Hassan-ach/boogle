import logging
import time
import asyncio
from psql import DatabaseManager, retry_on_db_error

logger = logging.getLogger(__name__)

# Recompute IDF for every word that appears on at least one page.
#
# Two things matter here and both were wrong before:
#
#   * The ratio must be computed in numeric, not integer arithmetic. `pages.id`
#     and the `df` count are both bigint, so `378 / 352` truncated to 1 and
#     LOG(1) is 0 -- every common word was stored with an IDF of exactly zero.
#   * The logarithm argument must never be zero. On an empty corpus
#     `COUNT(*) FROM pages` is 0, and `LOG(0)` raises
#     "cannot take logarithm of zero", which is not retryable and takes the whole
#     pipeline down. GREATEST(..., 1) floors the corpus size at 1 so the
#     statement degrades to "every word is maximally rare" instead of erroring.
IDF_UPDATE_SQL = """
    WITH corpus AS (
        SELECT GREATEST(COUNT(*), 1)::numeric AS n
        FROM pages
    ),
    doc_freq AS (
        SELECT word_id, COUNT(DISTINCT page_id) AS df
        FROM page_word
        GROUP BY word_id
    )
    UPDATE words
    SET idf = LOG((SELECT n FROM corpus) / GREATEST(df.df + 1, 1))
    FROM doc_freq df
    WHERE words.id = df.word_id
"""

PAGERANK_UPDATE_SQL = "SELECT update_page_rank(%s, %s);"


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
                    await cursor.execute(IDF_UPDATE_SQL)
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
                        PAGERANK_UPDATE_SQL,
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
