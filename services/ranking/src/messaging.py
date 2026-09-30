import os
import logging
import asyncio
from typing import Optional, Callable, Awaitable, Any
from faststream import FastStream
from faststream.rabbit import RabbitBroker, RabbitQueue

logger = logging.getLogger(__name__)


class MessagingService:
    """Manages FastStream RabbitMQ broker, subscriber queues, and threshold trigger logic."""

    def __init__(
        self,
        broker_url: Optional[str] = None,
        queue_name: Optional[str] = None,
        max_indexer_pages: int = 100,
    ):
        user = os.getenv("RABBITMQ_USER") or os.getenv("RABBITMQ_DEFAULT_USER") or "admin"
        password = os.getenv("RABBITMQ_PASSWORD") or os.getenv("RABBITMQ_DEFAULT_PASS") or "admin"
        host = os.getenv("RABBITMQ_HOST") or "localhost"
        port = os.getenv("RABBITMQ_PORT") or "5672"
        default_url = f"amqp://{user}:{password}@{host}:{port}"

        self.broker_url = (
            broker_url
            or os.getenv("BROKER_URL")
            or os.getenv("RABBITMQ_URL")
            or default_url
        )
        self.queue_name = (
            queue_name
            or os.getenv("RABBITMQ_CONFIRMATION_QUEUE")
            or "indexer.confirmations"
        )
        self.max_indexer_pages = max_indexer_pages
        self._indexer_pages = 0
        self._job_lock = asyncio.Lock()

        self.broker = RabbitBroker(self.broker_url)
        self.app = FastStream(self.broker)
        self.queue = RabbitQueue(self.queue_name, durable=True)
        self._pipeline_handler: Optional[Callable[[], Awaitable[None]]] = None

        self._configure_subscribers()

    def register_pipeline_handler(
        self, handler: Callable[[], Awaitable[None]]
    ) -> None:
        """Register async handler to trigger when threshold is reached."""
        self._pipeline_handler = handler

    def _configure_subscribers(self) -> None:
        @self.broker.subscriber(self.queue)
        async def handle_confirmation(body: dict[str, Any]) -> None:
            logger.info(f"Received confirmation message: {body}")
            self._indexer_pages += 1

            if self._indexer_pages >= self.max_indexer_pages:
                self._indexer_pages = 0
                if self._pipeline_handler is not None:
                    # Spawn task non-blockingly so the consumer callback returns immediately
                    asyncio.create_task(self._safe_run_pipeline())

    async def _safe_run_pipeline(self) -> None:
        """Executes pipeline under an asyncio lock to prevent concurrent runs."""
        if self._job_lock.locked():
            logger.info(
                "Ranking pipeline is already running. Skipping execution."
            )
            return

        async with self._job_lock:
            if self._pipeline_handler:
                try:
                    await self._pipeline_handler()
                except Exception as e:
                    logger.error(
                        f"Error executing ranking pipeline handler: {e}",
                        exc_info=True,
                    )

    async def publish_message(
        self, message: str, queue_name: Optional[str] = None
    ) -> None:
        """Publish a message asynchronously to a target queue."""
        target_queue = queue_name or self.queue_name
        logger.info(f"Publishing message to queue: {target_queue}")
        await self.broker.publish(message, routing_key=target_queue)

    async def run(self) -> None:
        """Run the FastStream application."""
        logger.info(
            f"Starting FastStream consumer on queue '{self.queue_name}'..."
        )
        await self.app.run()
