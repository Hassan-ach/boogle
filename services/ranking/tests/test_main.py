"""Startup validation and log formatting in the ranking service.

`validate_environment` is the last thing standing between a misconfigured
deployment and a container that starts, connects to nothing, and exits with a
confusing error thirty seconds later. It is pure, so it is tested directly.
"""

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
    for key in PG_VARS + ["DATABASE_URL", "BROKER_URL", "RABBITMQ_URL"]:
        monkeypatch.delenv(key, raising=False)


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


# ── validate_environment ─────────────────────────────────────────────────────


def test_missing_pg_variables_are_all_reported_at_once(monkeypatch: pytest.MonkeyPatch) -> None:
    """Reporting one variable per restart turns a typo into a guessing game."""
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
    """`PG_PASSWORD=` in a compose file is a misconfiguration, not a valid value."""
    set_pg_env(monkeypatch, PG_PASSWORD="")

    with pytest.raises(ValueError, match="PG_PASSWORD"):
        RankingService.validate_environment()


def test_database_url_satisfies_every_pg_requirement(monkeypatch: pytest.MonkeyPatch) -> None:
    """A single connection string is the documented alternative to the five parts."""
    monkeypatch.setenv("DATABASE_URL", "postgresql://admin:secret@db:5432/boogle")
    monkeypatch.setenv("BROKER_URL", "amqp://admin:admin@rabbit:5672")

    RankingService.validate_environment()


def test_missing_broker_url_is_reported(monkeypatch: pytest.MonkeyPatch) -> None:
    set_pg_env(monkeypatch)

    with pytest.raises(ValueError, match="BROKER_URL or RABBITMQ_URL"):
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
    """Fixing the broker first should not send anyone chasing database errors."""
    monkeypatch.delenv("DATABASE_URL", raising=False)

    with pytest.raises(ValueError) as excinfo:
        RankingService.validate_environment()

    assert "PG_HOST" in str(excinfo.value)
    assert "BROKER_URL" not in str(excinfo.value)


def test_a_complete_environment_validates(monkeypatch: pytest.MonkeyPatch) -> None:
    set_pg_env(monkeypatch)
    monkeypatch.setenv("BROKER_URL", "amqp://admin:admin@rabbit:5672")

    assert RankingService.validate_environment() is None


# ── JsonFormatter ────────────────────────────────────────────────────────────


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
    # The configured format has no sub-second component.
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
    """`ensure_ascii=True` keeps the log stream pure ASCII for any sink."""
    rendered = JsonFormatter().format(make_record(msg="café ☕", args=()))

    assert "café" not in rendered
    assert "caf\\u00e9" in rendered
    json.loads(rendered)


def test_formatter_escapes_quotes_in_messages() -> None:
    """A naive `\"` would emit JSON that no parser can read back."""
    rendered = JsonFormatter().format(make_record(msg='said "hi" \\ then left', args=()))

    assert json.loads(rendered)["message"] == 'said "hi" \\ then left'


def test_formatter_round_trips_braces_quotes_and_percent_signs() -> None:
    """Braces, quotes and backslashes are common in SQL and URLs from operators.

    `%s`-style interpolation is not applied here, so a literal percent sign must
    survive untouched, and every one of these characters must come back out of
    `json.loads` unchanged.
    """
    message = "100% of {these} said \"go\" \\ then {left}"
    payload = json.loads(JsonFormatter().format(make_record(msg=message, args=())))

    assert payload["message"] == message


def test_configure_logging_attaches_json_to_both_handlers(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Any,
) -> None:
    files: list[logging.FileHandler] = []
    basic_configs: list[dict[str, Any]] = []

    class RecordingFileHandler(logging.FileHandler):
        def __init__(self, filename: str) -> None:
            super().__init__(str(tmp_path / "ranking.log"))
            files.append(self)

    monkeypatch.setattr(logging, "FileHandler", RecordingFileHandler)
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

    # A file handler and a stream handler, both writing JSON.
    assert len(files) == 1, "the log file must not be the only sink"
    assert len(basic_configs) == 1
    handlers = basic_configs[0]["handlers"]
    assert len(handlers) == 2
    assert isinstance(handlers[0], RecordingFileHandler)
    assert isinstance(handlers[1], logging.StreamHandler)
    for handler in handlers:
        assert isinstance(handler.formatter, JsonFormatter)

    assert basic_configs[0]["level"] == logging.INFO


def test_configure_logging_is_idempotent(monkeypatch: pytest.MonkeyPatch) -> None:
    """`start()` and `main()` both call it; a second call must not duplicate output."""
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


# ── shutdown ─────────────────────────────────────────────────────────────────


def test_shutdown_closes_the_database_pool(monkeypatch: pytest.MonkeyPatch) -> None:
    service = RankingService()
    closed = 0

    async def fake_close() -> None:
        nonlocal closed
        closed += 1

    monkeypatch.setattr(service.db_manager, "close", fake_close)

    asyncio.run(service.shutdown("SIGTERM"))

    assert closed == 1


# ── signal handling ──────────────────────────────────────────────────────────


def test_signal_handlers_are_registered_for_int_and_term(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    service = RankingService()
    registered: list[signal.Signals] = []

    class FakeLoop:
        def add_signal_handler(self, signum: Any, handler: Any) -> None:
            registered.append(signum)

    service.setup_signal_handlers(FakeLoop())  # type: ignore[arg-type]

    assert set(registered) == {signal.SIGINT, signal.SIGTERM}


def test_signal_handlers_trigger_shutdown(monkeypatch: pytest.MonkeyPatch) -> None:
    """A registered handler must actually schedule the graceful shutdown."""
    service = RankingService()
    shutdowns: list[str] = []

    async def fake_shutdown(name: str) -> None:
        shutdowns.append(name)

    monkeypatch.setattr(service, "shutdown", fake_shutdown)

    handlers: list[Any] = []

    class FakeLoop:
        def add_signal_handler(self, signum: Any, handler: Any) -> None:
            handlers.append((signum, handler))

    async def drive() -> None:
        service.setup_signal_handlers(FakeLoop())  # type: ignore[arg-type]
        for _, handler in handlers:
            handler()
        await asyncio.sleep(0)

    asyncio.run(drive())

    assert sorted(shutdowns) == ["SIGINT", "SIGTERM"]


# ── wiring ───────────────────────────────────────────────────────────────────


def test_start_registers_the_calculator_as_the_pipeline_handler(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """The messaging threshold drives the calculator; if this link is missing,
    confirmations arrive forever and ranking never happens."""
    service = RankingService()
    registered: list[Any] = []

    monkeypatch.setattr(service.messaging, "register_pipeline_handler", registered.append)
    monkeypatch.setattr(service.messaging, "run", _async_noop)
    monkeypatch.setattr(RankingService, "configure_logging", staticmethod(lambda: None))
    monkeypatch.setattr(main_module, "load_dotenv", lambda *a, **k: None)
    monkeypatch.setattr(RankingService, "setup_signal_handlers", lambda self, loop: None)
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
    """`finally` around the consumer is what makes Ctrl-C release the pool."""
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
    monkeypatch.setattr(RankingService, "setup_signal_handlers", lambda self, loop: None)
    monkeypatch.setattr(service.db_manager, "initialize", fake_initialize)
    monkeypatch.setattr(service.db_manager, "close", fake_close)
    set_pg_env(monkeypatch)
    monkeypatch.setenv("BROKER_URL", "amqp://admin:admin@rabbit:5672")

    with pytest.raises(RuntimeError, match="broker went away"):
        asyncio.run(service.start())

    assert closed == 1


def test_start_refuses_to_run_with_an_invalid_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    """Configuration errors must surface before any resource is opened."""
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
