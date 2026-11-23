import logging
import time
import asyncio
from psql import DatabaseManager, retry_on_db_error

logger = logging.getLogger(__name__)

# Recomputes IDF for every word in one statement. GREATEST keeps df >= 1 and
# clamps the ratio at 1 so rounding cannot make IDF negative; corpus size is
# clamped too so an empty corpus cannot divide by zero.
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
    SET idf = LOG(GREATEST(
        (SELECT n FROM corpus) / GREATEST(df.df, 1),
        1
    ))
    FROM doc_freq df
    WHERE words.id = df.word_id
"""

# PageRank runs in PL/pgSQL: each pass uses the previous pass's scores, so doing
# it in Python would hold the whole graph in memory and round-trip per iteration.
PAGERANK_UPDATE_SQL = "SELECT update_page_rank(%s, %s);"


class RankingCalculator:
    """Runs the two periodic scoring jobs.

    Both jobs rewrite the whole table in one statement and take the full corpus
    lock while they do, so they are meant to run on a timer and be cheap to skip
    rather than tuned for throughput.
    """

    def __init__(self, db_manager: DatabaseManager):
        self.db_manager = db_manager

    @retry_on_db_error(max_retries=3, delay=1.0, backoff=2.0)
    async def compute_idf(self) -> int:
        """Recompute IDF for every word. Returns the number of words updated."""
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
        """Recompute PageRank. Returns the number of pages updated.

        ``iterations`` is the iteration count and ``damping_factor`` the
        probability of following a link (0.85 is Google's original value). More
        iterations converge but cost time linearly.
        """
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
        """Run both jobs concurrently on separate pooled connections.

        A TaskGroup cancels the sibling on failure, so a partial pair cannot
        commit alone; the cancelled pair leaves the previous scores intact.
        """
        logger.info("Executing full ranking pipeline (IDF + PageRank)...")
        async with asyncio.TaskGroup() as group:
            group.create_task(self.compute_idf())
            group.create_task(
                self.update_pagerank(
                    iterations=iterations, damping_factor=damping_factor
                )
            )
        logger.info("Ranking pipeline execution completed.")
