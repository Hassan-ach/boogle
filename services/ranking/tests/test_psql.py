from __future__ import annotations

import asyncio
from typing import Any

import pytest
from psycopg import InterfaceError, OperationalError

from psql import retry_on_db_error


class Recorder:
    def __init__(self, fail_times: int, error: BaseException) -> None:
        self.calls = 0
        self.fail_times = fail_times
        self.error = error

    async def __call__(self) -> str:
        self.calls += 1
        if self.calls <= self.fail_times:
            raise self.error
        return "ok"


class Boom:

    def __init__(self) -> None:
        self.calls = 0

    async def __call__(self) -> str:
        self.calls += 1
        raise ValueError("syntax error at or near \"SELCT\"")


@pytest.fixture(autouse=True)
def no_real_sleeps(monkeypatch: pytest.MonkeyPatch) -> list[float]:
    delays: list[float] = []

    async def fake_sleep(seconds: float) -> None:
        delays.append(seconds)

    monkeypatch.setattr(asyncio, "sleep", fake_sleep)
    return delays


async def test_successful_call_is_not_retried() -> None:
    recorder = Recorder(fail_times=0, error=OperationalError())
    wrapped = retry_on_db_error(max_retries=3)(recorder)

    assert await wrapped() == "ok"
    assert recorder.calls == 1


async def test_transient_failure_is_retried_until_success(no_real_sleeps: list[float]) -> None:
    recorder = Recorder(fail_times=2, error=OperationalError())
    wrapped = retry_on_db_error(max_retries=3, delay=1.0, backoff=2.0)(recorder)

    assert await wrapped() == "ok"
    assert recorder.calls == 3, "two failures then a success"
    assert no_real_sleeps == [1.0, 2.0], "delay must grow by the backoff factor"


async def test_max_retries_means_retries_after_the_first_attempt(
    no_real_sleeps: list[float],
) -> None:
    recorder = Recorder(fail_times=99, error=OperationalError())
    wrapped = retry_on_db_error(max_retries=3, delay=0.0)(recorder)

    with pytest.raises(OperationalError):
        await wrapped()

    assert recorder.calls == 4, "1 initial attempt + 3 retries"
    assert len(no_real_sleeps) == 3, "a sleep between each pair of attempts"


async def test_non_retryable_errors_fail_immediately(no_real_sleeps: list[float]) -> None:
    boom = Boom()
    wrapped = retry_on_db_error(max_retries=5, delay=1.0)(boom)

    with pytest.raises(ValueError, match="SELCT"):
        await wrapped()

    assert boom.calls == 1, "a bad statement will never succeed on retry"
    assert no_real_sleeps == [], "a fatal error must not be retried or slept on"


async def test_interface_errors_are_retryable(no_real_sleeps: list[float]) -> None:
    recorder = Recorder(fail_times=1, error=InterfaceError("connection closed"))
    wrapped = retry_on_db_error(max_retries=2, delay=0.0)(recorder)

    assert await wrapped() == "ok"
    assert recorder.calls == 2


async def test_final_error_is_propagated_unwrapped(no_real_sleeps: list[float]) -> None:
    original = OperationalError("server closed the connection unexpectedly")
    recorder = Recorder(fail_times=99, error=original)
    wrapped = retry_on_db_error(max_retries=1, delay=0.0)(recorder)

    with pytest.raises(OperationalError) as excinfo:
        await wrapped()

    assert excinfo.value is original, "the original exception must reach the caller"


async def test_arguments_and_return_value_survive_the_wrapper() -> None:
    @retry_on_db_error(max_retries=1)
    async def add(a: int, b: int = 0) -> int:
        return a + b

    assert await add(2, b=3) == 5


async def test_decorator_preserves_function_metadata() -> None:
    @retry_on_db_error(max_retries=1)
    async def documented() -> None:
        """A docstring that operators rely on."""

    assert documented.__name__ == "documented"
    assert documented.__doc__ == "A docstring that operators rely on."


async def test_zero_retries_still_makes_one_attempt() -> None:
    recorder = Recorder(fail_times=99, error=OperationalError())
    wrapped = retry_on_db_error(max_retries=0, delay=0.0)(recorder)

    with pytest.raises(OperationalError):
        await wrapped()

    assert recorder.calls == 1, "the initial attempt is unconditional"


async def test_backoff_is_capped_by_the_attempt_budget() -> None:
    recorder = Recorder(fail_times=99, error=OperationalError())
    wrapped = retry_on_db_error(max_retries=2, delay=0.5, backoff=3.0)(recorder)

    with pytest.raises(OperationalError):
        await wrapped()

    assert recorder.calls == 3


@pytest.mark.parametrize("retryable", [OperationalError, InterfaceError])
async def test_both_documented_retryable_types_are_covered(retryable: Any) -> None:
    recorder = Recorder(fail_times=1, error=retryable("transient"))
    wrapped = retry_on_db_error(max_retries=2, delay=0.0)(recorder)

    assert await wrapped() == "ok"
    assert recorder.calls == 2


async def test_concurrent_calls_do_not_share_a_delay_counter() -> None:
    calls: dict[str, int] = {"a": 0, "b": 0}

    @retry_on_db_error(max_retries=1, delay=1.0, backoff=2.0)
    async def flaky(name: str) -> str:
        calls[name] += 1
        if calls[name] == 1:
            raise OperationalError(f"{name} failed once")
        return name

    results = await asyncio.gather(flaky("a"), flaky("b"))

    assert sorted(results) == ["a", "b"]
    assert calls == {"a": 2, "b": 2}
