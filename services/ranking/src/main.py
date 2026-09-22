import asyncio
import json
import logging
import functools
import time
import signal
from psql import get_graph_edges, persist_pagerank, NodeMapper
from psql import initialize_connection_pool, close_connection_pool
from plpgsql_pagerank import update_pagerank
from idf import idf
from dotenv import load_dotenv
import os

shutdown_event = asyncio.Event()


async def shutdown(signame, loop):
    print(f"Received {signame}, shutting down...")
    shutdown_event.set()

    tasks = [
        task
        for task in asyncio.all_tasks(loop)
        if task is not asyncio.current_task()
    ]

    await asyncio.gather(*tasks, return_exceptions=True)

    loop.stop()

class JsonFormatter(logging.Formatter):
    def format(self, record: logging.LogRecord) -> str:
        payload = {
            "service": "ranking",
            "timestamp": self.formatTime(record, datefmt="%Y-%m-%dT%H:%M:%S"),
            "level": record.levelname,
            "logger": record.name,
            "message": record.getMessage(),
        }

        if record.exc_info:
            payload["error"] = self.formatException(record.exc_info)

        return json.dumps(payload, ensure_ascii=True)


def configure_logging() -> None:
    if logging.getLogger().handlers:
        return

    handlers: list[logging.Handler] = [
        logging.FileHandler("ranking.log"),
        logging.StreamHandler(),
    ]
    formatter = JsonFormatter()

    for handler in handlers:
        handler.setFormatter(formatter)

    logging.basicConfig(level=logging.INFO, handlers=handlers)

logger = logging.getLogger(__name__)


def validate_environment():
    """Validate that all required environment variables are set."""
    required_vars = ['PG_HOST', 'PG_PORT', 'PG_DBNAME', 'PG_USER', 'PG_PASSWORD']
    missing_vars = [var for var in required_vars if not os.getenv(var)]
    
    if missing_vars:
        raise ValueError(f"Missing required environment variables: {', '.join(missing_vars)}")
    
    logger.info("Environment variables validated successfully")


async def run_pagerank():
    while not shutdown_event.is_set():
        try:
            start_time = time.time()
            logger.info("Starting PageRank calculation...")
            update_pagerank()
            
            duration = time.time() - start_time
            logger.info(f"PageRank calculation and persistence completed in {duration:.2f}s")
        except Exception as e:
            logger.error(f"PageRank job failed: {e}", exc_info=True)


async def run_idf():
    while not shutdown_event.is_set():
        try:
            start_time = time.time()
            logger.info("Starting IDF calculation...")
            idf()
            
            duration = time.time() - start_time
            logger.info(f"IDF calculation completed in {duration:.2f}s")
        except Exception as e:
            logger.error(f"IDF job failed: {e}", exc_info=True)

async def work():
    await asyncio.gather(run_idf(), run_pagerank())

async def main():
    configure_logging()
    load_dotenv()
    
    # Validate environment variables
    try:
        validate_environment()
    except ValueError as e:
        logger.error(f"Configuration error: {e}")
        return
    
    # Initialize connection pool
    try:
        initialize_connection_pool(minconn=2, maxconn=10)
    except Exception as e:
        logger.error(f"Failed to initialize connection pool: {e}")
        return

    loop = asyncio.get_running_loop()
    for signame in {signal.SIGINT, signal.SIGTERM}:
        loop.add_signal_handler(signame, functools.partial(shutdown,signal.Signals(signame).name, loop))

    await work()



if __name__ == "__main__":
    try:
        configure_logging()
        logger.info("Starting ranking Service...")
        asyncio.run(main())
    except KeyboardInterrupt:
        logger.info("Service stopped by user")
    except Exception as e:
        logger.error(f"Service failed with error: {e}", exc_info=True)
    finally:
        close_connection_pool()

