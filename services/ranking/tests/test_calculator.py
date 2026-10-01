from __future__ import annotations

import asyncio

import pytest
from psycopg import OperationalError

from calculator import IDF_UPDATE_SQL, PAGERANK_UPDATE_SQL, RankingCalculator

from .conftest import FakeConnection, FakeDBManager


def make_calculator(rowcount: int = 0, errors=None) -> tuple[RankingCalculator, FakeDBManager]:
    db = FakeDBManager(rowcount=rowcount, errors=errors)
    return RankingCalculator(db), db


async def test_compute_idf_returns_the_number_of_updated_words() -> None:
    calc, db = make_calculator(rowcount=17_098)

    assert await calc.compute_idf() == 17_098


async def test_compute_idf_runs_exactly_one_statement_and_commits() -> None:
    calc, db = make_calculator(rowcount=5)

    await calc.compute_idf()

    assert len(db.connection.statements) == 1
    assert db.connection.queries[0].strip() == IDF_UPDATE_SQL.strip()
    assert db.connection.event_names() == ["cursor_enter", "execute", "cursor_exit", "commit"]


async def test_compute_idf_closes_its_cursor_before_committing() -> None:
    calc, db = make_calculator(rowcount=1)

    await calc.compute_idf()

    events = db.connection.event_names()
    assert events.index("cursor_exit") < events.index("commit")


async def test_compute_idf_uses_numeric_division_not_integer() -> None:
    assert "::numeric" in IDF_UPDATE_SQL
    assert "GREATEST(df.df, 1)" in IDF_UPDATE_SQL


async def test_compute_idf_never_takes_the_logarithm_of_zero() -> None:
    assert "GREATEST(COUNT(*), 1)" in IDF_UPDATE_SQL
    assert "GREATEST(df.df, 1)" in IDF_UPDATE_SQL


async def test_compute_idf_never_stores_a_negative_weight() -> None:
    assert "LOG(GREATEST(" in IDF_UPDATE_SQL
    assert "(SELECT n FROM corpus) / GREATEST(df.df, 1)," in IDF_UPDATE_SQL


async def test_compute_idf_only_touches_words_that_appear_on_a_page() -> None:
    assert "FROM doc_freq df" in IDF_UPDATE_SQL
    assert "WHERE words.id = df.word_id" in IDF_UPDATE_SQL


async def test_compute_idf_returns_zero_for_an_empty_corpus() -> None:
    calc, db = make_calculator(rowcount=0)

    assert await calc.compute_idf() == 0
    assert "rollback" not in db.connection.event_names()


async def test_compute_idf_rolls_back_and_propagates_on_failure() -> None:
    calc, db = make_calculator(errors={"UPDATE words": RuntimeError("relation does not exist")})

    with pytest.raises(RuntimeError):
        await calc.compute_idf()

    assert "rollback" in db.connection.event_names()
    assert "commit" not in db.connection.event_names()


async def test_compute_idf_retries_transient_database_errors(monkeypatch) -> None:
    sleeps: list[float] = []

    async def fake_sleep(seconds: float) -> None:
        sleeps.append(seconds)

    monkeypatch.setattr(asyncio, "sleep", fake_sleep)

    attempts = 0

    class FlakyConnection(FakeConnection):
        async def commit(self) -> None:
            nonlocal attempts
            attempts += 1
            if attempts == 1:
                await super().rollback()
                raise OperationalError("server closed the connection unexpectedly")
            await super().commit()

    db = FakeDBManager(connection=FlakyConnection(rowcount=3))
    calc = RankingCalculator(db)

    assert await calc.compute_idf() == 3
    assert attempts == 2
    assert sleeps == [1.0], "the first retry waits one second"


async def test_update_pagerank_passes_iterations_and_damping_through() -> None:
    calc, db = make_calculator(rowcount=378)

    assert await calc.update_pagerank(iterations=30, damping_factor=0.9) == 378

    query, params = db.connection.statements[0]
    assert query == PAGERANK_UPDATE_SQL
    assert params == (30, 0.9)


async def test_update_pagerank_defaults_match_the_stored_procedure() -> None:
    calc, db = make_calculator(rowcount=1)

    await calc.update_pagerank()

    _, params = db.connection.statements[0]
    assert params == (20, 0.85)


async def test_update_pagerank_rolls_back_and_propagates_on_failure() -> None:
    calc, db = make_calculator(errors={"update_page_rank": RuntimeError("relation temp_page_rank already exists")})

    with pytest.raises(RuntimeError, match="already exists"):
        await calc.update_pagerank()

    assert "rollback" in db.connection.event_names()
    assert "commit" not in db.connection.event_names()


async def test_update_pagerank_uses_a_parameterised_call() -> None:
    calc, db = make_calculator(rowcount=1)

    await calc.update_pagerank(iterations=1, damping_factor=0.85)

    query, _ = db.connection.statements[0]
    assert "0.85" not in query
    assert query.count("%s") == 2


async def test_run_pipeline_runs_idf_and_pagerank() -> None:
    calc, db = make_calculator(rowcount=4)

    await calc.run_pipeline(iterations=5, damping_factor=0.8)

    assert len(db.connection.statements) == 2
    assert db.connection.queries[0].strip() == IDF_UPDATE_SQL.strip()
    assert db.connection.statements[1][1] == (5, 0.8)


async def test_run_pipeline_gathers_both_stages_concurrently() -> None:
    calc, db = make_calculator(rowcount=1)

    await calc.run_pipeline()

    assert db.connection.queries[0].strip() == IDF_UPDATE_SQL.strip()
    assert db.connection.queries[1] == PAGERANK_UPDATE_SQL


async def test_run_pipeline_propagates_a_stage_failure() -> None:
    calc, _ = make_calculator(errors={"UPDATE words": RuntimeError("words table is gone")})

    with pytest.raises(RuntimeError, match="words table is gone"):
        await calc.run_pipeline()


async def test_run_pipeline_propagates_a_pagerank_failure() -> None:
    calc, _ = make_calculator(errors={"update_page_rank": RuntimeError("LOG(0) detected")})

    with pytest.raises(RuntimeError, match="LOG"):
        await calc.run_pipeline()


async def test_run_pipeline_returns_none() -> None:
    calc, _ = make_calculator(rowcount=1)

    assert await calc.run_pipeline() is None
