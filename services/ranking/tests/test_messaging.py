"""Threshold trigger logic in `MessagingService`.

The service is told "the indexer finished N pages, go rank" once every
`max_indexer_pages` confirmations. Getting that counter wrong does not crash
anything -- ranking just silently stops happening -- so the interesting cases
are all about *when the counter is reset*, which is why they are driven
through `record_confirmation` rather than a live broker.
"""

from __future__ import annotations

import asyncio
import os
from typing import Any, Optional

import pytest

import messaging as messaging_module
from messaging import MessagingService


@pytest.fixture(autouse=True)
def clean_env(monkeypatch: pytest.MonkeyPatch) -> None:
    for key in list(os.environ):
        if key.startswith(("RABBITMQ_", "BROKER_")):
            monkeypatch.delenv(key, raising=False)


def make_service(max_indexer_pages: int = 3) -> MessagingService:
    return MessagingService(
        broker_url="amqp://guest:guest@localhost:5672",
        queue_name="indexer.confirmations",
        max_indexer_pages=max_indexer_pages,
    )


class Gate:
    """A handler that blocks until the test releases it."""

    def __init__(self) -> None:
        self.calls = 0
        self.started = asyncio.Event()
        self.release = asyncio.Event()

    async def __call__(self) -> None:
        self.calls += 1
        self.started.set()
        await self.release.wait()


async def settle() -> None:
    """Let spawned pipeline tasks run until they are waiting or finished."""
    for _ in range(5):
        await asyncio.sleep(0)


# ── configuration ────────────────────────────────────────────────────────────


def test_queue_name_defaults_to_the_indexer_confirmation_queue() -> None:
    service = make_service()

    assert service.queue_name == "indexer.confirmations"
    assert service.max_indexer_pages == 3


def test_explicit_arguments_win_over_the_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("RABBITMQ_CONFIRMATION_QUEUE", "from.env")

    service = make_service()

    assert service.queue_name == "indexer.confirmations"


def test_queue_name_falls_back_to_the_environment() -> None:
    os.environ["RABBITMQ_CONFIRMATION_QUEUE"] = "custom.confirmations"
    try:
        service = MessagingService(broker_url="amqp://guest:guest@localhost:5672")
        assert service.queue_name == "custom.confirmations"
    finally:
        del os.environ["RABBITMQ_CONFIRMATION_QUEUE"]


def test_broker_credentials_are_built_from_the_individual_variables(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("RABBITMQ_USER", "u")
    monkeypatch.setenv("RABBITMQ_PASSWORD", "p")
    monkeypatch.setenv("RABBITMQ_HOST", "broker")
    monkeypatch.setenv("RABBITMQ_PORT", "1234")

    service = MessagingService()

    assert service.broker_url == "amqp://u:p@broker:1234"


def test_broker_url_falls_back_to_rabbitmq_url(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("RABBITMQ_URL", "amqp://from-env:5672")

    assert MessagingService().broker_url == "amqp://from-env:5672"


def test_credentials_default_when_nothing_is_configured() -> None:
    service = MessagingService()

    assert service.broker_url == "amqp://admin:admin@localhost:5672"


# ── threshold counting ───────────────────────────────────────────────────────


async def test_no_pipeline_runs_below_the_threshold() -> None:
    service = make_service(max_indexer_pages=3)
    gate = Gate()
    service.register_pipeline_handler(gate)

    assert service.record_confirmation() is False
    assert service.record_confirmation() is False
    await settle()

    assert gate.calls == 0


async def test_pipeline_runs_exactly_at_the_threshold() -> None:
    service = make_service(max_indexer_pages=3)
    gate = Gate()
    service.register_pipeline_handler(gate)

    service.record_confirmation()
    service.record_confirmation()
    assert service.record_confirmation() is True
    await settle()

    assert gate.calls == 1


async def test_counter_resets_after_a_run_completes() -> None:
    service = make_service(max_indexer_pages=2)
    gate = Gate()
    gate.release.set()
    service.register_pipeline_handler(gate)

    service.record_confirmation()
    service.record_confirmation()
    await settle()
    assert gate.calls == 1

    # The next two confirmations must be enough again, not four.
    service.record_confirmation()
    service.record_confirmation()
    await settle()

    assert gate.calls == 2


async def test_a_trigger_that_lands_mid_run_is_not_discarded() -> None:
    """Regression: the counter used to be reset before the lock was checked.

    The trigger was consumed, the pipeline task saw the lock held and returned
    without doing anything, and the 100 accumulated pages were never ranked.
    Since a full pipeline takes much longer than 100 pages take to arrive, this
    meant ranking effectively never ran again after the first success.
    """
    service = make_service(max_indexer_pages=2)
    gate = Gate()
    service.register_pipeline_handler(gate)

    service.record_confirmation()
    service.record_confirmation()
    await settle()
    await gate.started.wait()
    assert gate.calls == 1

    # A second batch arrives while the first run is still in flight.
    service.record_confirmation()
    service.record_confirmation()
    await settle()

    assert gate.calls == 1, "the second run must wait for the lock, not be lost"

    # The first run finishes; the pending threshold must then fire.
    gate.release.set()
    for _ in range(20):
        await asyncio.sleep(0.01)
        if gate.calls == 2:
            break

    assert gate.calls == 2, "the mid-run trigger must still cause a second run"


async def test_extra_confirmations_while_busy_do_not_queue_many_runs() -> None:
    """Only one follow-up run should be pending, not one per confirmation."""
    service = make_service(max_indexer_pages=2)
    gate = Gate()
    service.register_pipeline_handler(gate)

    service.record_confirmation()
    service.record_confirmation()
    await settle()
    await gate.started.wait()

    for _ in range(10):
        service.record_confirmation()
    await settle()

    assert gate.calls == 1, "a single run is in flight"

    gate.release.set()
    for _ in range(30):
        await asyncio.sleep(0.01)
        if gate.calls == 2:
            break

    assert gate.calls == 2, "one catch-up run, not ten"


async def test_counter_is_not_reset_when_no_handler_is_registered() -> None:
    service = make_service(max_indexer_pages=1)

    assert service.record_confirmation() is False
    assert service._indexer_pages == 1, "the count must survive until a run consumes it"


async def test_registering_a_handler_after_the_threshold_still_runs() -> None:
    service = make_service(max_indexer_pages=1)
    service.record_confirmation()

    gate = Gate()
    service.register_pipeline_handler(gate)

    assert service.record_confirmation() is True
    await settle()

    assert gate.calls == 1


# ── error containment ────────────────────────────────────────────────────────


async def test_a_failing_pipeline_does_not_kill_the_consumer() -> None:
    calls = 0

    async def exploding() -> None:
        nonlocal calls
        calls += 1
        raise RuntimeError("update_page_rank exploded")

    service = make_service(max_indexer_pages=1)
    service.register_pipeline_handler(exploding)

    service.record_confirmation()
    await settle()
    assert calls == 1

    # The lock must be released, so a later run is still possible.
    assert not service._job_lock.locked()
    assert not service._pipeline_running

    service.record_confirmation()
    await settle()
    assert calls == 2, "a failure must not wedge the trigger"


async def test_a_failing_pipeline_still_consumes_its_batch() -> None:
    """The pages were counted; re-running the same batch immediately would spin.

    A failure is reported through the log, not through a retry loop here, so the
    counter is still reset and the next threshold starts a fresh batch.
    """
    calls = 0

    async def exploding() -> None:
        nonlocal calls
        calls += 1
        raise RuntimeError("nope")

    service = make_service(max_indexer_pages=1)
    service.register_pipeline_handler(exploding)

    service.record_confirmation()
    await settle()

    assert calls == 1, "a failing handler must not be retried in a tight loop"
    assert service._indexer_pages == 0


async def test_pipeline_is_a_no_op_without_a_handler() -> None:
    service = make_service()

    await service._safe_run_pipeline()

    assert not service._job_lock.locked()
    assert not service._pipeline_running


async def test_concurrent_triggers_never_run_two_pipelines_at_once() -> None:
    """The lock, not the counter, is what guarantees a single concurrent run."""
    service = make_service(max_indexer_pages=1)
    running = 0
    peak = 0

    async def tracked() -> None:
        nonlocal running, peak
        running += 1
        peak = max(peak, running)
        await asyncio.sleep(0.01)
        running -= 1

    service.register_pipeline_handler(tracked)

    for _ in range(10):
        service.record_confirmation()
    await settle()
    await asyncio.sleep(0.05)

    assert peak == 1, f"observed {peak} concurrent pipelines"


# ── publishing ───────────────────────────────────────────────────────────────


async def test_publish_targets_the_confirmation_queue_by_default(monkeypatch) -> None:
    service = make_service()
    published: list[tuple[str, Any]] = []

    async def fake_publish(message: str, routing_key: str) -> None:
        published.append((message, routing_key))

    monkeypatch.setattr(service.broker, "publish", fake_publish)

    await service.publish_message("hello")

    assert published == [("hello", "indexer.confirmations")]


async def test_publish_can_target_an_explicit_queue(monkeypatch) -> None:
    service = make_service()
    published: list[tuple[str, Any]] = []

    async def fake_publish(message: str, routing_key: str) -> None:
        published.append((message, routing_key))

    monkeypatch.setattr(service.broker, "publish", fake_publish)

    await service.publish_message("hello", queue_name="elsewhere")

    assert published == [("hello", "elsewhere")]


def test_a_pipeline_handler_can_be_unregistered() -> None:
    service = make_service()
    service.register_pipeline_handler(Gate())
    service._pipeline_handler = None

    assert service.record_confirmation() is False
