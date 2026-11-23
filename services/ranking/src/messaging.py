import os
import logging
import asyncio
from typing import Optional, Callable, Awaitable, Any
from urllib.parse import quote

from faststream import FastStream
from faststream.rabbit import RabbitBroker, RabbitQueue

logger = logging.getLogger(__name__)

DEFAULT_CONFIRMATION_THRESHOLD = 100


def broker_url_from_env() -> str:
    """Build the broker URL from the environment, failing closed."""
    configured = os.getenv("BROKER_URL") or os.getenv("RABBITMQ_URL")
    if configured:
        return configured

    user = os.getenv("RABBITMQ_USER") or os.getenv("RABBITMQ_DEFAULT_USER")
    password = os.getenv("RABBITMQ_PASSWORD") or os.getenv("RABBITMQ_DEFAULT_PASS")
    if not user or password is None:
        raise ValueError(
            "Missing BROKER_URL/RABBITMQ_URL, or RABBITMQ_USER and RABBITMQ_PASSWORD"
        )

    host = os.getenv("RABBITMQ_HOST") or "localhost"
    port = os.getenv("RABBITMQ_PORT") or "5672"
    # Credentials can contain ':' and '@', which would otherwise be parsed as
    # URL structure rather than as part of the password.
    return (
        f"amqp://{quote(user, safe='')}:{quote(password, safe='')}"
        f"@{host}:{port}"
    )


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
        max_indexer_pages: Optional[int] = None,
    ):
        self.broker_url = broker_url or broker_url_from_env()

        self.queue_name = (
            queue_name
            or os.getenv("RABBITMQ_CONFIRMATION_QUEUE")
            or "indexer.confirmations"
        )

        if max_indexer_pages is not None:
            self.max_indexer_pages = max_indexer_pages
        else:
            raw_threshold = os.getenv(
                "RANKING_CONFIRMATION_THRESHOLD",
                str(DEFAULT_CONFIRMATION_THRESHOLD),
            )
            try:
                self.max_indexer_pages = int(raw_threshold)
            except ValueError as exc:
                raise ValueError(
                    "RANKING_CONFIRMATION_THRESHOLD must be an integer, got "
                    f"{raw_threshold!r}"
                ) from exc
        if self.max_indexer_pages < 1:
            raise ValueError("RANKING_CONFIRMATION_THRESHOLD must be at least 1")

        self._indexer_pages = 0
        self._job_lock = asyncio.Lock()
        self._pipeline_running = False
        self._stopping = False
        self._tasks: set = set()

        self.broker = RabbitBroker(self.broker_url)
        # after_shutdown runs after FastStream stops the broker, including on
        # SIGTERM (FastStream replaces any handler installed before run()).
        self.app = FastStream(self.broker, after_shutdown=[self.drain])
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
            # Log the identifier only: the full body is a 1:1 copy of an internal
            # message and carries no operational value beyond the page id.
            page_id = body.get("page_id") if isinstance(body, dict) else None
            logger.info("Received index confirmation for page %s", page_id)
            self.record_confirmation()

    def track_task(self, coro: Awaitable[Any]) -> asyncio.Task:
        task = asyncio.create_task(coro)
        self._tasks.add(task)
        task.add_done_callback(self._log_task_failure)
        return task

    def _log_task_failure(self, task: asyncio.Task) -> None:
        self._tasks.discard(task)
        if task.cancelled():
            return
        exc = task.exception()
        if exc is not None:
            logger.error("Ranking task failed: %s", exc, exc_info=exc)

    def record_confirmation(self) -> bool:
        """Count one indexed page. Returns True if a pipeline run was started.

        The counter is deliberately not reset here. It is reset by the run itself,
        so a confirmation that arrives while a run is in flight still counts
        towards the next threshold instead of being lost.
        """
        self._indexer_pages += 1

        if self._stopping or self._indexer_pages < self.max_indexer_pages:
            return False

        if self._pipeline_handler is None:
            return False

        return self.trigger_pipeline()

    def trigger_pipeline(self, force: bool = False) -> bool:
        """Start a pipeline run unless one is already in flight.

        `force` runs once regardless of the confirmation counter; the startup
        run uses it so an empty corpus still gets scored.
        """
        if self._stopping or self._pipeline_handler is None:
            return False
        if self._pipeline_running:
            return True

        self._pipeline_running = True
        self.track_task(self._safe_run_pipeline(force=force))
        return True

    async def _safe_run_pipeline(self, force: bool = False) -> None:
        """Run the pipeline until the page counter drops below the threshold.

        The counter is only consumed while the lock is held, so two runs cannot
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
                    and not self._stopping
                    and (force or self._indexer_pages >= self.max_indexer_pages)
                ):
                    force = False
                    # Snapshot the batch so confirmations arriving during the run
                    # stay counted for the next threshold.
                    batch = self._indexer_pages
                    try:
                        await self._pipeline_handler()
                    except Exception:
                        # Leave the batch counted so the next confirmation
                        # retries it instead of losing it permanently.
                        logger.exception(
                            "Ranking pipeline failed; the confirmation batch "
                            "stays pending"
                        )
                        break
                    self._indexer_pages = max(0, self._indexer_pages - batch)
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

    async def stop(self) -> None:
        """Ask FastStream's run loop to exit.

        `app.stop()` only closes the broker; in faststream 0.7.7 the run loop is
        released by `exit()`. FastStream then performs its own shutdown and calls
        `drain` as an after_shutdown hook.
        """
        self._stopping = True
        try:
            self.app.exit()
        except Exception:
            logger.exception("Error while stopping the FastStream app")

    async def drain(self) -> None:
        """Cancel in-flight pipeline work and wait for it to unwind.

        Idempotent, and also called from `start()`'s finally so a broker crash
        takes the same path as a clean signal.
        """
        self._stopping = True
        tasks = [task for task in self._tasks if not task.done()]
        for task in tasks:
            task.cancel()
        if tasks:
            results = await asyncio.gather(*tasks, return_exceptions=True)
            for result in results:
                if isinstance(result, Exception) and not isinstance(
                    result, asyncio.CancelledError
                ):
                    logger.error("Ranking task failed during shutdown: %s", result)
