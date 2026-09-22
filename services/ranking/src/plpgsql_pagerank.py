import  logging
import time
from psql import get_connection, release_connection, retry_on_db_error

@retry_on_db_error(max_retries=3, delay=1.0, backoff=2.0)
def update_pagerank():
    start_time = time.time()
    logger = logging.getLogger(__name__)
    conn = get_connection()
    try:
        with conn.cursor() as cursor:
            logger.info("Updating PageRank values in the database...")
            cursor.execute("""
                SELECT update_page_rank(20, 0.85) ;
            """)
            affected_rows = cursor.rowcount
            conn.commit()
            duration = time.time() - start_time
            logger.info(f"PageRank updated for {affected_rows} pages in {duration:.2f}s")
    except Exception as e:
        logger.error(f"Failed to update PageRank values: {e}", exc_info=True)
        conn.rollback()
        raise
    finally:
        release_connection(conn)
