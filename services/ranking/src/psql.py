# In this module, we define a function to calculate the Inverse Document Frequency (IDF)
# and update it in the 'words' table of a PostgreSQL database in a single batch operation.
# and a function to establish a connection to the PostgreSQL database using environment variables.

import os
import psycopg2
from psycopg2 import pool
import uuid
import numpy as np
import logging
import time
from functools import wraps
from psycopg2.extras import execute_values

logger = logging.getLogger(__name__)

# Global connection pool
_connection_pool = None


def retry_on_db_error(max_retries=3, delay=1.0, backoff=2.0):
    """
    Decorator to retry database operations on transient failures.
    
    Args:
        max_retries: Maximum number of retry attempts
        delay: Initial delay between retries in seconds
        backoff: Multiplier for delay after each retry
    """
    def decorator(func):
        @wraps(func)
        def wrapper(*args, **kwargs):
            current_delay = delay
            last_exception = None
            
            for attempt in range(max_retries + 1):
                try:
                    return func(*args, **kwargs)
                except (psycopg2.OperationalError, psycopg2.InterfaceError) as e:
                    last_exception = e
                    if attempt < max_retries:
                        logger.warning(
                            f"Database operation failed (attempt {attempt + 1}/{max_retries + 1}): {e}. "
                            f"Retrying in {current_delay:.1f}s..."
                        )
                        time.sleep(current_delay)
                        current_delay *= backoff
                    else:
                        logger.error(f"Database operation failed after {max_retries + 1} attempts: {e}")
                        raise
                except Exception as e:
                    # Don't retry on non-transient errors
                    logger.error(f"Database operation failed with non-retryable error: {e}")
                    raise
        
        return wrapper
    return decorator


def initialize_connection_pool(minconn=1, maxconn=10):
    """Initialize the database connection pool."""
    global _connection_pool
    
    if _connection_pool is not None:
        logger.warning("Connection pool already initialized")
        return
    
    try:
        _connection_pool = pool.SimpleConnectionPool(
            minconn,
            maxconn,
            host=os.getenv('PG_HOST'),
            port=os.getenv('PG_PORT'),
            database=os.getenv('PG_DBNAME'),
            user=os.getenv('PG_USER'),
            password=os.getenv('PG_PASSWORD')
        )
        logger.info(f"Connection pool initialized with {minconn}-{maxconn} connections")
    except Exception as e:
        logger.error(f"Failed to initialize connection pool: {e}", exc_info=True)
        raise


def close_connection_pool():
    """Close all connections in the pool."""
    global _connection_pool
    
    if _connection_pool is not None:
        _connection_pool.closeall()
        _connection_pool = None
        logger.info("Connection pool closed")


def release_connection(conn):
    """Return a connection to the pool."""
    if _connection_pool is not None:
        _connection_pool.putconn(conn)
    else:
        conn.close()


class NodeMapper:
    def __init__(self):
        self.node_id = {}
        self.node_uuid = {}
        self.next_id = 0

    def get_id(self, u: uuid.UUID) -> int:
        if u not in self.node_id:
            self.node_id[u] = self.next_id
            self.node_uuid[self.next_id] = u
            self.next_id += 1
        return self.node_id[u]

    def get_uuid(self, id: int) -> uuid.UUID:
        return self.node_uuid[id]




def get_connection():
    """Get a connection from the pool, or create a new one if pool is not initialized."""
    if _connection_pool is not None:
        return _connection_pool.getconn()
    
    # Fallback to direct connection if pool is not initialized
    logger.warning("Connection pool not initialized, creating direct connection")
    return psycopg2.connect(
        host=os.getenv('PG_HOST'),
        port=os.getenv('PG_PORT'),
        database=os.getenv('PG_DBNAME'),
        user=os.getenv('PG_USER'),
        password=os.getenv('PG_PASSWORD')
    )

@retry_on_db_error(max_retries=3, delay=1.0, backoff=2.0)
def get_graph_edges(m: NodeMapper) -> list[tuple[int, int]]:
    start_time = time.time()
    logger.info("Fetching graph edges from database...")
    conn = get_connection()
    try:
        with conn.cursor() as cursor:
            cursor.execute(
                """
                SELECT from_url, to_url
                FROM graph_edges;
                """
            )
            edges = [(m.get_id(f), m.get_id(t)) for f, t in cursor.fetchall()]
            duration = time.time() - start_time
            logger.info(f"Graph edges fetched successfully. N = {len(edges)}, Duration: {duration:.2f}s")
            return edges
    except Exception as e:
        logger.error(f"Failed to fetch graph edges: {e}", exc_info=True)
        raise
    finally:
        release_connection(conn)



@retry_on_db_error(max_retries=3, delay=1.0, backoff=2.0)
def persist_pagerank(m: NodeMapper, pagerank: np.ndarray):
    start_time = time.time()
    logger.info("Persisting PageRank scores to database...")
    conn = get_connection()
    try:
        with conn.cursor() as cursor:
            data = [
                (m.get_uuid(i), float(pr))
                for i, pr in enumerate(pagerank)
            ]

            execute_values(
                cursor,
                """
                INSERT INTO page_rank (url_id, score)
                VALUES %s
                ON CONFLICT (url_id) DO UPDATE
                SET score = EXCLUDED.score
                """,
                data,
                page_size=10_000  
            )

        conn.commit()
        duration = time.time() - start_time
        logger.info(f"PageRank scores persisted successfully. {len(pagerank)} nodes updated in {duration:.2f}s")
    except Exception as e:
        logger.error(f"Failed to persist PageRank scores: {e}", exc_info=True)
        raise
    finally:
        release_connection(conn)

