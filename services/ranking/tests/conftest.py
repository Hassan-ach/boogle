"""Shared fakes for the ranking service unit tests.

Everything here is hand-written rather than generated: the surface the ranking
service touches is tiny (one context manager, one cursor, one commit), and a
real object is easier to reason about than a mock's expectations.
"""

from __future__ import annotations

import os
import sys
from contextlib import asynccontextmanager
from typing import Any, Optional

import pytest

SRC = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src")
if SRC not in sys.path:
    sys.path.insert(0, SRC)


class FakeCursor:
    """Records every statement it is handed and replays canned results."""

    def __init__(self, connection: "FakeConnection") -> None:
        self._connection = connection
        self.rowcount = -1
        self.closed = False

    async def __aenter__(self) -> "FakeCursor":
        self._connection.events.append(("cursor_enter", None))
        return self

    async def __aexit__(self, *exc_info: Any) -> bool:
        self.closed = True
        self._connection.events.append(("cursor_exit", None))
        return False

    async def execute(self, query: str, params: Any = None) -> "FakeCursor":
        self._connection.statements.append((query, params))
        self._connection.events.append(("execute", query))
        for needle, error in self._connection.errors.items():
            if needle in query:
                raise error
        self.rowcount = self._connection.rowcount
        return self

    async def fetchall(self, *args: Any, **kwargs: Any) -> list[tuple[Any, ...]]:
        self._connection.events.append(("fetchall", None))
        return self._connection.fetch_result

    async def fetchone(self, *args: Any, **kwargs: Any) -> Optional[tuple[Any, ...]]:
        self._connection.events.append(("fetchone", None))
        if self._connection.fetch_result:
            return self._connection.fetch_result[0]
        return None


class FakeConnection:
    """A psycopg connection stand-in with an explicit, ordered event log.

    `errors` maps a substring of the statement to the exception it should raise,
    so a test can fail one stage of the pipeline and leave the other alone.
    """

    def __init__(
        self,
        rowcount: int = 0,
        errors: Optional[dict[str, BaseException]] = None,
        fetch_result: Optional[list[tuple[Any, ...]]] = None,
    ) -> None:
        self.rowcount = rowcount
        self.errors = errors or {}
        self.fetch_result = fetch_result or []
        self.statements: list[tuple[str, Any]] = []
        self.events: list[tuple[str, Any]] = []
        self.closed = False

    def cursor(self) -> FakeCursor:
        return FakeCursor(self)

    async def commit(self) -> None:
        self.events.append(("commit", None))

    async def rollback(self) -> None:
        self.events.append(("rollback", None))

    async def close(self) -> None:
        self.closed = True
        self.events.append(("close", None))

    @property
    def queries(self) -> list[str]:
        return [q for q, _ in self.statements]

    def event_names(self) -> list[str]:
        return [name for name, _ in self.events]


class FakeDBManager:
    """Replaces `DatabaseManager` so no pool or network is involved."""

    def __init__(
        self,
        rowcount: int = 0,
        errors: Optional[dict[str, BaseException]] = None,
        connection: Optional[FakeConnection] = None,
    ) -> None:
        self.connection = connection or FakeConnection(rowcount=rowcount, errors=errors)
        self.opened = 0
        self.closed = 0
        self.conninfo: Optional[str] = None

    @asynccontextmanager
    async def get_connection(self):
        yield self.connection

    async def initialize(self) -> None:
        self.opened += 1

    async def close(self) -> None:
        self.closed += 1


@pytest.fixture
def fake_db() -> FakeDBManager:
    return FakeDBManager()
