import json
import logging
import signal
import asyncio
import os
from dotenv import load_dotenv

from psql import DatabaseManager
from calculator import RankingCalculator
from messaging import MessagingService

logger = logging.getLogger(__name__)


class JsonFormatter(logging.Formatter):
    """Custom JSON log formatter."""

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


class RankingService:
    """Main application orchestrator for the Ranking Service."""

    def __init__(self):
        self.db_manager = DatabaseManager(min_conn=2, max_conn=10)
        self.calculator = RankingCalculator(self.db_manager)
        self.messaging = MessagingService()

    @staticmethod
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

    @staticmethod
    def validate_environment() -> None:
        """Validate required environment variables."""
        required_vars = [
            "PG_HOST",
            "PG_PORT",
            "PG_DBNAME",
            "PG_USER",
            "PG_PASSWORD",
        ]
        missing_vars = [
            var
            for var in required_vars
            if not os.getenv(var) and not os.getenv("DATABASE_URL")
        ]

        if missing_vars:
            raise ValueError(
                f"Missing required database environment variables: {', '.join(missing_vars)}"
            )

        broker_url = os.getenv("BROKER_URL") or os.getenv("RABBITMQ_URL")
        if not broker_url:
            raise ValueError(
                "Missing BROKER_URL or RABBITMQ_URL environment variable"
            )

        logger.info("Environment variables validated successfully")

    def setup_signal_handlers(self, loop: asyncio.AbstractEventLoop) -> None:
        """Configure graceful shutdown signal handlers."""

        for signame in {signal.SIGINT, signal.SIGTERM}:
            loop.add_signal_handler(
                signame,
                lambda sig=signame: asyncio.create_task(
                    self.shutdown(signal.Signals(sig).name)
                ),
            )

    async def shutdown(self, signame: str) -> None:
        logger.info(f"Received signal {signame}, shutting down RankingService...")
        await self.db_manager.close()

    async def start(self) -> None:
        """Initialize resources, bind handlers, and start service consumer loop."""
        self.configure_logging()
        load_dotenv()
        self.validate_environment()

        logger.info("Initializing Database Connection Pool...")
        await self.db_manager.initialize()

        loop = asyncio.get_running_loop()
        self.setup_signal_handlers(loop)

        # Register calculator pipeline to messaging threshold callback
        self.messaging.register_pipeline_handler(self.calculator.run_pipeline)

        logger.info("Starting Ranking Service FastStream Consumer...")
        try:
            await self.messaging.run()
        finally:
            await self.db_manager.close()


def main():
    load_dotenv()
    service = RankingService()
    try:
        service.configure_logging()
        logger.info("Starting Ranking Service application...")
        asyncio.run(service.start())
    except KeyboardInterrupt:
        logger.info("Service stopped by user")
    except Exception as e:
        logger.error(f"Service failed with error: {e}", exc_info=True)


if __name__ == "__main__":
    main()
