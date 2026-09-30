"""Database fixtures for the ranking service integration tests.

These tests run against a real PostgreSQL because most of what the ranking
service does is expressed as SQL: the interesting failures live inside
`UPDATE words ... LOG(...)` and inside the `update_page_rank` PL/pgSQL
function, and neither can be exercised against a fake connection.

Every test runs inside a transaction that is rolled back, so the suite is safe
to point at a shared database and never leaves rows behind. Set
`BOOGLE_TEST_PG_DSN` to run against something other than the default
`boogle_test` database.
"""

from __future__ import annotations

import os
from typing import Any, Iterator

import psycopg
import pytest

DEFAULT_DSN = "postgresql://admin:se@localhost:5432/boogle_test"

#: Tolerance for float comparisons. PageRank is DOUBLE PRECISION and the
#: assertion that matters most is a sum over every row, so the error
#: accumulates; 1e-9 on a total of 1.0 is still far tighter than any ranking
#: decision the engine makes.
TOLERANCE = 1e-9


def _dsn() -> str:
    """The database to test against, most specific source first.

    `just test-integration` exports `TEST_DATABASE_URL`; `DATABASE_URL` is what
    the service itself reads, so exporting it in a shell is enough to aim the
    suite at a different cluster.
    """
    for key in ("BOOGLE_TEST_PG_DSN", "TEST_DATABASE_URL", "DATABASE_URL"):
        value = os.environ.get(key)
        if value:
            return value
    return DEFAULT_DSN


def _admin_dsn() -> str:
    """A connection to `postgres`, for creating the test database if needed."""
    dsn = _dsn()
    try:
        info = psycopg.conninfo.conninfo_to_dict(dsn)
    except psycopg.ProgrammingError:
        return DEFAULT_DSN
    info["dbname"] = "postgres"
    return psycopg.conninfo.make_conninfo(**info)


def pytest_configure(config: pytest.Config) -> None:
    config.addinivalue_line("markers", "integration: needs a live PostgreSQL")


def pytest_collection_modifyitems(config: pytest.Config, items: list[Any]) -> None:
    """Skip, rather than error, when no database is reachable.

    `just test` must keep working on a machine with no Postgres, and a hard
    failure at collection time would take the whole unit suite down with it.
    """
    try:
        with psycopg.connect(_dsn(), connect_timeout=3):
            return
    except psycopg.OperationalError as exc:
        reason = f"no PostgreSQL at {_dsn()}: {exc}".splitlines()[0]
        skip = pytest.mark.skip(reason=reason)
        for item in items:
            if "integration" in item.keywords:
                item.add_marker(skip)


@pytest.fixture(scope="session")
def dsn() -> str:
    return _dsn()


@pytest.fixture
def conn(dsn: str) -> Iterator[psycopg.Connection]:
    """A connection wrapped in a transaction that is always rolled back."""
    connection = psycopg.connect(dsn, autocommit=False)
    try:
        yield connection
    finally:
        connection.rollback()
        connection.close()


@pytest.fixture
def cursor(conn: psycopg.Connection) -> Iterator[psycopg.Cursor]:
    with conn.cursor() as cur:
        yield cur


@pytest.fixture
def function_installed(cursor: psycopg.Cursor) -> bool:
    """Whether `update_page_rank` exists in the target database."""
    cursor.execute(
        """
        SELECT EXISTS (
            SELECT 1
            FROM pg_proc p
            JOIN pg_namespace n ON n.oid = p.pronamespace
            WHERE p.proname = 'update_page_rank'
        )
        """
    )
    return bool(cursor.fetchone()[0])


@pytest.fixture
def schema_ready(cursor: psycopg.Cursor) -> bool:
    """Whether the tables the tests need are present."""
    cursor.execute(
        """
        SELECT count(*) = 5
        FROM information_schema.tables
        WHERE table_schema = 'public'
          AND table_name IN ('urls', 'pages', 'words', 'page_word', 'graph_edges')
        """
    )
    return bool(cursor.fetchone()[0])
