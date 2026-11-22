from __future__ import annotations

import asyncio
import json
import logging
import os
import signal
import sys
from typing import Any, Optional

import pytest

import main as main_module
from main import JsonFormatter, RankingService

PG_VARS = ["PG_HOST", "PG_PORT", "PG_DBNAME", "PG_USER", "PG_PASSWORD"]


@pytest.fixture(autouse=True)
def clean_env(monkeypatch: pytest.MonkeyPatch) -> None:
    for key in PG_VARS + [
        "DATABASE_URL",
        "BROKER_URL",
        "RABBITMQ_URL",
        "LOG_PATH",
        "RANKING_CONFIRMATION_THRESHOLD",
        "RANKING_RUN_ON_STARTUP",
    ]:
        monkeypatch.delenv(key, raising=False)
    # RankingService constructs its consumer eagerly, and messaging fails closed
    # without a broker URL. Individual tests remove this when they exercise that.
    monkeypatch.setenv("BROKER_URL", "amqp://admin:admin@rabbit:5672")


def set_pg_env(monkeypatch: pytest.MonkeyPatch, **overrides: str) -> None:
    values = {
        "PG_HOST": "localhost",
        "PG_PORT": "5432",
        "PG_DBNAME": "boogle",
        "PG_USER": "admin",
        "PG_PASSWORD": "secret",
    }
    values.update(overrides)
    for key, value in values.items():
        monkeypatch.setenv(key, value)


def test_missing_pg_variables_are_all_reported_at_once(monkeypatch: pytest.MonkeyPatch) -> None:
    with pytest.raises(ValueError) as excinfo:
        RankingService.validate_environment()

    message = str(excinfo.value)
    for name in PG_VARS:
        assert name in message, f"{name} missing from: {message}"


def test_a_single_missing_pg_variable_is_named(monkeypatch: pytest.MonkeyPatch) -> None:
    set_pg_env(monkeypatch)
    monkeypatch.delenv("PG_PASSWORD")

    with pytest.raises(ValueError, match="PG_PASSWORD"):
        RankingService.validate_environment()


def test_empty_values_count_as_missing(monkeypatch: pytest.MonkeyPatch) -> None:
    set_pg_env(monkeypatch, PG_PASSWORD="")

    with pytest.raises(ValueError, match="PG_PASSWORD"):
        RankingService.validate_environment()


def test_database_url_satisfies_every_pg_requirement(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("DATABASE_URL", "postgresql://admin:secret@db:5432/boogle")
    monkeypatch.setenv("BROKER_URL", "amqp://admin:admin@rabbit:5672")

    RankingService.validate_environment()


def test_missing_broker_url_is_reported(monkeypatch: pytest.MonkeyPatch) -> None:
    set_pg_env(monkeypatch)
    monkeypatch.delenv("BROKER_URL", raising=False)

    with pytest.raises(ValueError, match="BROKER_URL/RABBITMQ_URL"):
        RankingService.validate_environment()


def test_rabbitmq_url_is_an_acceptable_broker_url(monkeypatch: pytest.MonkeyPatch) -> None:
    set_pg_env(monkeypatch)
    monkeypatch.setenv("RABBITMQ_URL", "amqp://admin:admin@rabbit:5672")

    RankingService.validate_environment()


def test_broker_url_is_preferred_when_both_are_set(monkeypatch: pytest.MonkeyPatch) -> None:
    set_pg_env(monkeypatch)
    monkeypatch.setenv("BROKER_URL", "amqp://from-broker-url:5672")
    monkeypatch.setenv("RABBITMQ_URL", "amqp://from-rabbitmq-url:5672")

    RankingService.validate_environment()


def test_broker_url_is_checked_after_the_database(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("DATABASE_URL", raising=False)

    with pytest.raises(ValueError) as excinfo:
        RankingService.validate_environment()

    assert "PG_HOST" in str(excinfo.value)
    assert "BROKER_URL" not in str(excinfo.value)


def test_a_complete_environment_validates(monkeypatch: pytest.MonkeyPatch) -> None:
    set_pg_env(monkeypatch)
    monkeypatch.setenv("BROKER_URL", "amqp://admin:admin@rabbit:5672")

    assert RankingService.validate_environment() is None


def make_record(**kwargs: Any) -> logging.LogRecord:
    defaults = dict(
        name="ranking",
        level=logging.INFO,
        pathname=__file__,
        lineno=1,
        msg="hello %s",
        args=("world",),
        exc_info=None,
    )
    defaults.update(kwargs)
    return logging.LogRecord(**defaults)


def test_formatter_emits_valid_json() -> None:
    payload = json.loads(JsonFormatter().format(make_record()))

    assert payload["message"] == "hello world"
    assert payload["level"] == "INFO"
    assert payload["service"] == "ranking"
    assert payload["logger"] == "ranking"


def test_formatter_includes_a_timestamp() -> None:
    payload = json.loads(JsonFormatter().format(make_record()))

    assert "timestamp" in payload
    assert len(payload["timestamp"]) == len("2026-01-01T00:00:00")


def test_formatter_omits_error_when_there_is_no_exception() -> None:
    payload = json.loads(JsonFormatter().format(make_record()))

    assert "error" not in payload


def test_formatter_includes_the_traceback_when_present() -> None:
    try:
        raise ValueError("kaboom")
    except ValueError:
        import sys

        exc_info = sys.exc_info()

    payload = json.loads(
        JsonFormatter().format(make_record(level=logging.ERROR, exc_info=exc_info))
    )

    assert "ValueError: kaboom" in payload["error"]
    assert payload["level"] == "ERROR"


def test_formatter_escapes_non_ascii() -> None:
    rendered = JsonFormatter().format(make_record(msg="café ☕", args=()))

    assert "café" not in rendered
    assert "caf\\u00e9" in rendered
    json.loads(rendered)


def test_formatter_escapes_quotes_in_messages() -> None:
    rendered = JsonFormatter().format(make_record(msg='said "hi" \\ then left', args=()))

    assert json.loads(rendered)["message"] == 'said "hi" \\ then left'


def test_formatter_round_trips_braces_quotes_and_percent_signs() -> None:
    message = "100% of {these} said \"go\" \\ then {left}"
    payload = json.loads(JsonFormatter().format(make_record(msg=message, args=())))

    assert payload["message"] == message


def test_configure_logging_defaults_to_stdout_only(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    basic_configs: list[dict[str, Any]] = []
    monkeypatch.setattr(
        logging, "basicConfig", lambda **kwargs: basic_configs.append(kwargs)
    )

    root = logging.getLogger()
    original = list(root.handlers)
    root.handlers = []
    try:
        RankingService.configure_logging()
    finally:
        root.handlers = original

    assert len(basic_configs) == 1
    handlers = basic_configs[0]["handlers"]
    assert len(handlers) == 1, "a container log sink must be stdout"
    assert isinstance(handlers[0], logging.StreamHandler)
    assert isinstance(handlers[0].formatter, JsonFormatter)
    assert basic_configs[0]["level"] == logging.INFO


def test_configure_logging_adds_an_opt_in_file_sink(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Any,
) -> None:
    log_path = tmp_path / "ranking.jsonl"
    monkeypatch.setenv("LOG_PATH", str(log_path))
    basic_configs: list[dict[str, Any]] = []
    monkeypatch.setattr(
        logging, "basicConfig", lambda **kwargs: basic_configs.append(kwargs)
    )

    root = logging.getLogger()
    original = list(root.handlers)
    root.handlers = []
    try:
        RankingService.configure_logging()
    finally:
        root.handlers = original

    handlers = basic_configs[0]["handlers"]
    assert len(handlers) == 2
    assert isinstance(handlers[0], logging.StreamHandler)
    assert isinstance(handlers[1], logging.FileHandler)
    for handler in handlers:
        assert isinstance(handler.formatter, JsonFormatter)


def test_an_unwritable_log_path_does_not_stop_the_service(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("LOG_PATH", "/proc/definitely-not-writable.log")
    basic_configs: list[dict[str, Any]] = []
    monkeypatch.setattr(
        logging, "basicConfig", lambda **kwargs: basic_configs.append(kwargs)
    )

    root = logging.getLogger()
    original = list(root.handlers)
    root.handlers = []
    try:
        RankingService.configure_logging()
    finally:
        root.handlers = original

    handlers = basic_configs[0]["handlers"]
    assert len(handlers) == 1
    assert isinstance(handlers[0], logging.StreamHandler)


def test_configure_logging_is_idempotent(monkeypatch: pytest.MonkeyPatch) -> None:
    root = logging.getLogger()
    original = list(root.handlers)
    root.handlers = [logging.NullHandler()]
    try:
        calls = 0

        def counting_basic_config(**kwargs: Any) -> None:
            nonlocal calls
            calls += 1

        monkeypatch.setattr(logging, "basicConfig", counting_basic_config)
        RankingService.configure_logging()

        assert calls == 0, "an already-configured root logger must be left alone"
    finally:
        root.handlers = original


def test_stop_asks_faststream_to_exit(monkeypatch: pytest.MonkeyPatch) -> None:
    service = RankingService()
    exited: list[bool] = []

    monkeypatch.setattr(service.messaging.app, "exit", lambda: exited.append(True))

    asyncio.run(service.messaging.stop())

    assert exited == [True], "stop() must release FastStream's run loop"
    assert service.messaging._stopping


def test_drain_cancels_and_awaits_inflight_pipeline_work(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    service = RankingService()
    cancelled: list[bool] = []

    async def slow_pipeline() -> None:
        try:
            await asyncio.sleep(60)
        except asyncio.CancelledError:
            cancelled.append(True)
            raise

    service.messaging.register_pipeline_handler(slow_pipeline)

    async def drive() -> None:
        service.messaging.trigger_pipeline(force=True)
        await asyncio.sleep(0)
        await service.messaging.drain()

    asyncio.run(drive())

    assert cancelled == [True], "in-flight pipeline work was not torn down"
    assert not service.messaging._tasks


def test_startup_pipeline_runs_even_below_the_confirmation_threshold(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    service = RankingService()
    calls = 0

    async def pipeline() -> None:
        nonlocal calls
        calls += 1

    service.messaging.register_pipeline_handler(pipeline)
    service.messaging.max_indexer_pages = 1000

    async def drive() -> None:
        service.messaging.trigger_pipeline(force=True)
        await asyncio.gather(*list(service.messaging._tasks))

    asyncio.run(drive())

    assert calls == 1


def test_start_registers_the_calculator_as_the_pipeline_handler(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    service = RankingService()
    registered: list[Any] = []

    monkeypatch.setattr(service.messaging, "register_pipeline_handler", registered.append)
    monkeypatch.setattr(service.messaging, "run", _async_noop)
    monkeypatch.setattr(RankingService, "configure_logging", staticmethod(lambda: None))
    monkeypatch.setattr(main_module, "load_dotenv", lambda *a, **k: None)
    monkeypatch.setenv("RANKING_RUN_ON_STARTUP", "false")
    set_pg_env(monkeypatch)
    monkeypatch.setenv("BROKER_URL", "amqp://admin:admin@rabbit:5672")

    async def fake_initialize() -> None:
        return None

    async def fake_close() -> None:
        return None

    monkeypatch.setattr(service.db_manager, "initialize", fake_initialize)
    monkeypatch.setattr(service.db_manager, "close", fake_close)

    asyncio.run(service.start())

    assert registered == [service.calculator.run_pipeline]


def test_start_closes_the_database_when_the_consumer_stops(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    service = RankingService()
    closed = 0

    async def fake_run() -> None:
        raise RuntimeError("broker went away")

    async def fake_initialize() -> None:
        return None

    async def fake_close() -> None:
        nonlocal closed
        closed += 1

    monkeypatch.setattr(service.messaging, "run", fake_run)
    monkeypatch.setattr(RankingService, "configure_logging", staticmethod(lambda: None))
    monkeypatch.setattr(main_module, "load_dotenv", lambda *a, **k: None)
    monkeypatch.setattr(service.db_manager, "initialize", fake_initialize)
    monkeypatch.setattr(service.db_manager, "close", fake_close)
    set_pg_env(monkeypatch)
    monkeypatch.setenv("BROKER_URL", "amqp://admin:admin@rabbit:5672")

    with pytest.raises(RuntimeError, match="broker went away"):
        asyncio.run(service.start())

    assert closed == 1


def test_start_retries_a_database_that_is_still_starting(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    service = RankingService()
    attempts: list[int] = []

    async def flaky_initialize() -> None:
        attempts.append(1)
        if len(attempts) < 3:
            raise ConnectionError("database is starting")

    async def fake_run() -> None:
        return None

    async def fake_close() -> None:
        return None

    monkeypatch.setattr(service.db_manager, "initialize", flaky_initialize)
    monkeypatch.setattr(service.db_manager, "close", fake_close)
    monkeypatch.setattr(service.messaging, "run", fake_run)
    monkeypatch.setattr(RankingService, "configure_logging", staticmethod(lambda: None))
    monkeypatch.setattr(main_module, "load_dotenv", lambda *a, **k: None)
    monkeypatch.setenv("RANKING_RUN_ON_STARTUP", "false")
    set_pg_env(monkeypatch)
    monkeypatch.setenv("BROKER_URL", "amqp://admin:admin@rabbit:5672")

    async def no_sleep(_: float) -> None:
        return None

    monkeypatch.setattr(main_module.asyncio, "sleep", no_sleep)

    asyncio.run(service.start())

    assert len(attempts) == 3, "the pool was opened without retrying"


def test_start_refuses_to_run_with_an_invalid_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    service = RankingService()
    monkeypatch.setattr(RankingService, "configure_logging", staticmethod(lambda: None))
    monkeypatch.setattr(main_module, "load_dotenv", lambda *a, **k: None)

    async def fail_initialize() -> None:
        raise AssertionError("the pool must not be opened")

    monkeypatch.setattr(service.db_manager, "initialize", fail_initialize)

    with pytest.raises(ValueError):
        asyncio.run(service.start())


async def _async_noop() -> None:
    return None
