import json
import logging
import asyncio
import os
import sys
from dotenv import load_dotenv

from psql import DatabaseManager
from calculator import RankingCalculator
from messaging import MessagingService

logger = logging.getLogger(__name__)


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


def run_on_startup_enabled() -> bool:
    return os.getenv("RANKING_RUN_ON_STARTUP", "true").strip().lower() not in {
        "0",
        "false",
        "no",
    }


class RankingService:

    def __init__(self):
        self.db_manager = DatabaseManager(min_conn=2, max_conn=10)
        self.calculator = RankingCalculator(self.db_manager)
        self.messaging = MessagingService()

    @staticmethod
    def configure_logging() -> None:
        if logging.getLogger().handlers:
            return

        # stdout is the container log stream and the only sink guaranteed writable;
        # LOG_PATH opts into a file sink, and failure to open it is non-fatal.
        handlers: list[logging.Handler] = [logging.StreamHandler()]
        log_path = os.getenv("LOG_PATH")
        if log_path:
            try:
                handlers.append(logging.FileHandler(log_path))
            except OSError as exc:
                print(
                    f"ranking: cannot open LOG_PATH {log_path!r} ({exc}); "
                    "logging to stdout only",
                    file=sys.stderr,
                )

        formatter = JsonFormatter()
        for handler in handlers:
            handler.setFormatter(formatter)

        logging.basicConfig(level=logging.INFO, handlers=handlers)

    @staticmethod
    def validate_environment() -> None:
        database_url = os.getenv("DATABASE_URL")
        if not database_url:
            required_vars = [
                "PG_HOST",
                "PG_PORT",
                "PG_DBNAME",
                "PG_USER",
                "PG_PASSWORD",
            ]
            missing_vars = [var for var in required_vars if not os.getenv(var)]
            if missing_vars:
                raise ValueError(
                    "Missing DATABASE_URL or database environment variables: "
                    f"{', '.join(missing_vars)}"
                )

        # Mirror broker_url_from_env: a URL is preferred, but the individual
        # credentials are an accepted alternative and must not be rejected here
        # after messaging has already accepted them.
        broker_url = os.getenv("BROKER_URL") or os.getenv("RABBITMQ_URL")
        if not broker_url:
            user = os.getenv("RABBITMQ_USER") or os.getenv("RABBITMQ_DEFAULT_USER")
            password = os.getenv("RABBITMQ_PASSWORD")
            if password is None:
                password = os.getenv("RABBITMQ_DEFAULT_PASS")
            if not user or password is None:
                raise ValueError(
                    "Missing BROKER_URL/RABBITMQ_URL, or RABBITMQ_USER and "
                    "RABBITMQ_PASSWORD"
                )

        logger.info("Environment variables validated successfully")

    async def _initialize_db_with_retry(self, attempts: int = 5) -> None:
        """Open the pool, retrying a database that is still starting.

        Bounded attempts bridge a slow startup while `wait=True` keeps a bad DSN
        fatal at boot instead of on the first query.
        """
        delay = 1.0
        for attempt in range(1, attempts + 1):
            try:
                await self.db_manager.initialize()
                return
            except Exception as exc:
                if attempt == attempts:
                    logger.error(
                        "Database connection pool failed after %d attempts: %s",
                        attempts,
                        exc,
                    )
                    raise
                logger.warning(
                    "Database connection pool failed (attempt %d/%d): %s; retrying in %.0fs",
                    attempt,
                    attempts,
                    exc,
                    delay,
                )
                await asyncio.sleep(delay)
                delay = min(delay * 2, 30.0)

    async def start(self) -> None:
        self.configure_logging()
        load_dotenv()
        self.validate_environment()

        logger.info("Initializing Database Connection Pool...")
        await self._initialize_db_with_retry()

        self.messaging.register_pipeline_handler(self.calculator.run_pipeline)

        # The startup run goes through the same lock as a threshold-triggered
        # run, so the first confirmations cannot start a second pipeline
        # concurrently.
        if run_on_startup_enabled():
            logger.info("Scheduling an initial ranking run")
            self.messaging.trigger_pipeline(force=True)

        logger.info("Starting Ranking Service FastStream Consumer...")
        try:
            await self.messaging.run()
        finally:
            await self.messaging.drain()
            await self.db_manager.close()


def main():
    load_dotenv()
    try:
        service = RankingService()
        service.configure_logging()
        logger.info("Starting Ranking Service application...")
        asyncio.run(service.start())
    except KeyboardInterrupt:
        logger.info("Service stopped by user")
    except Exception as e:
        # A failed startup must exit nonzero so the supervisor restarts it.
        logger.error(f"Service failed with error: {e}", exc_info=True)
        sys.exit(1)


if __name__ == "__main__":
    main()
