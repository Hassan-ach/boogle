from __future__ import annotations

import os
from typing import Any, Iterator

import psycopg
import pytest

DEFAULT_DSN = "postgresql://admin:se@localhost:5432/boogle_test"

TOLERANCE = 1e-9


def _dsn() -> str:
    for key in ("BOOGLE_TEST_PG_DSN", "TEST_DATABASE_URL", "DATABASE_URL"):
        value = os.environ.get(key)
        if value:
            return value
    return DEFAULT_DSN


def _admin_dsn() -> str:
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
    cursor.execute(
        """
        SELECT count(*) = 5
        FROM information_schema.tables
        WHERE table_schema = 'public'
          AND table_name IN ('urls', 'pages', 'words', 'page_word', 'graph_edges')
        """
    )
    return bool(cursor.fetchone()[0])
