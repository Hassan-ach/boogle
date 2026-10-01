import os
import logging
import asyncio
from typing import Optional, Callable, Awaitable, Any
from faststream import FastStream
from faststream.rabbit import RabbitBroker, RabbitQueue

logger = logging.getLogger(__name__)


class MessagingService:
    """Counts indexer confirmations and triggers IDF/PageRank when enough arrive.

    The ranking service is a follower: it does not decide when the corpus has
    changed enough to be worth rescoring. Instead it counts pages the indexer
    reports as indexed and runs the pipeline every `max_indexer_pages` of them.
    """

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
        self._pipeline_running = False
        self._tasks: set = set()

        self.broker = RabbitBroker(self.broker_url)
        self.app = FastStream(self.broker)
        self.queue = RabbitQueue(self.queue_name, durable=True)
        self._pipeline_handler: Optional[Callable[[], Awaitable[None]]] = None

        self._configure_subscribers()

    def register_pipeline_handler(
        self, handler: Callable[[], Awaitable[None]]
    ) -> None:
        self._pipeline_handler = handler

    def _configure_subscribers(self) -> None:
        @self.broker.subscriber(self.queue)
        async def handle_confirmation(body: dict[str, Any]) -> None:
            logger.info(f"Received confirmation message: {body}")
            self.record_confirmation()

    def record_confirmation(self) -> bool:
        """Count one indexed page. Returns True if a pipeline run was started.

        The counter is deliberately not reset here. It is reset by the run itself,
        so a confirmation that arrives while a run is in flight still counts
        towards the next threshold instead of being lost.
        """
        self._indexer_pages += 1

        if self._indexer_pages < self.max_indexer_pages:
            return False

        if self._pipeline_handler is None:
            return False

        if self._pipeline_running:
            return True

        self._pipeline_running = True
        task = asyncio.create_task(self._safe_run_pipeline())
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)
        return True

    async def _safe_run_pipeline(self) -> None:
        """Run the pipeline until the page counter drops below the threshold.

        The counter is only zeroed while the lock is held, so two runs cannot
        both believe they are covering the same pages. When the lock is already
        taken the run is skipped rather than queued: the pending run's loop will
        notice the threshold is still met and pick up the slack itself.
        """
        if self._job_lock.locked():
            logger.info(
                "Ranking pipeline is already running. Skipping this run; the "
                "threshold stays reached and will be handled when it finishes."
            )
            self._pipeline_running = False
            return

        try:
            async with self._job_lock:
                while (
                    self._pipeline_handler is not None
                    and self._indexer_pages >= self.max_indexer_pages
                ):
                    self._indexer_pages = 0
                    try:
                        await self._pipeline_handler()
                    except Exception as e:
                        logger.error(
                            f"Error executing ranking pipeline handler: {e}",
                            exc_info=True,
                        )
        finally:
            self._pipeline_running = False

    async def publish_message(
        self, message: str, queue_name: Optional[str] = None
    ) -> None:
        target_queue = queue_name or self.queue_name
        logger.info(f"Publishing message to queue: {target_queue}")
        await self.broker.publish(message, routing_key=target_queue)

    async def run(self) -> None:
        logger.info(
            f"Starting FastStream consumer on queue '{self.queue_name}'..."
        )
        await self.app.run()
