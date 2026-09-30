"""`RankingCalculator` orchestration.

The heavy lifting happens inside PostgreSQL, so these tests focus on what the
Python side is actually responsible for: which statement it sends, whether it
commits or rolls back, what it returns, and how failures propagate into the
retry decorator. The numerical correctness of the statements themselves is
covered by the integration suite in `test_integration_ranking.py`.
"""

from __future__ import annotations

import asyncio

import pytest
from psycopg import OperationalError

from calculator import IDF_UPDATE_SQL, PAGERANK_UPDATE_SQL, RankingCalculator

from .conftest import FakeConnection, FakeDBManager


def make_calculator(rowcount: int = 0, errors=None) -> tuple[RankingCalculator, FakeDBManager]:
    db = FakeDBManager(rowcount=rowcount, errors=errors)
    return RankingCalculator(db), db


# ── compute_idf ─────────────────────────────────────────────────────────────


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
    """Committing with an open cursor on the same connection is a protocol error."""
    calc, db = make_calculator(rowcount=1)

    await calc.compute_idf()

    events = db.connection.event_names()
    assert events.index("cursor_exit") < events.index("commit")


async def test_compute_idf_uses_numeric_division_not_integer() -> None:
    """Integer division truncates and collapses most IDF values to LOG(1) == 0.

    `378 / 352` is 1 in bigint arithmetic, so every common word was stored with
    an IDF of exactly zero and stopped contributing to search scores.
    """
    assert "::numeric" in IDF_UPDATE_SQL
    assert "GREATEST(df.df, 1)" in IDF_UPDATE_SQL


async def test_compute_idf_never_takes_the_logarithm_of_zero() -> None:
    """An empty corpus makes `COUNT(*) FROM pages` zero, and `LOG(0)` raises.

    That error is not an `OperationalError`, so the retry decorator treats it as
    fatal and the entire pipeline dies before it can ever index a page.
    """
    assert "GREATEST(COUNT(*), 1)" in IDF_UPDATE_SQL
    assert "GREATEST(df.df, 1)" in IDF_UPDATE_SQL


async def test_compute_idf_never_stores_a_negative_weight() -> None:
    """The engine multiplies tf by idf, so a negative idf subtracts from a score.

    A word on every page of a small corpus has n/df == 1, and the old smoothed
    denominator `df + 1` pushed that to n/(n+1) < 1, giving a negative IDF. The
    engine then made the most common word on a page *reduce* that page's score.
    Flooring the ratio at 1 makes it exactly 0 instead. A fresh crawl is when
    this bites hardest: the index starts at a handful of pages, so words present
    in all of them are everywhere.
    """
    assert "LOG(GREATEST(" in IDF_UPDATE_SQL
    assert "(SELECT n FROM corpus) / GREATEST(df.df, 1)," in IDF_UPDATE_SQL


async def test_compute_idf_only_touches_words_that_appear_on_a_page() -> None:
    """A word with no `page_word` rows has no defined document frequency."""
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
    """A dropped connection must be retried, and must not leave a partial commit."""
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


# ── update_pagerank ──────────────────────────────────────────────────────────


async def test_update_pagerank_passes_iterations_and_damping_through() -> None:
    calc, db = make_calculator(rowcount=378)

    assert await calc.update_pagerank(iterations=30, damping_factor=0.9) == 378

    query, params = db.connection.statements[0]
    assert query == PAGERANK_UPDATE_SQL
    assert params == (30, 0.9)


async def test_update_pagerank_defaults_match_the_stored_procedure() -> None:
    """The defaults must agree with `update_page_rank`'s signature in the migration."""
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
    """Values must be bound, not interpolated, so they cannot alter the statement."""
    calc, db = make_calculator(rowcount=1)

    await calc.update_pagerank(iterations=1, damping_factor=0.85)

    query, _ = db.connection.statements[0]
    assert "0.85" not in query
    assert query.count("%s") == 2


# ── run_pipeline ─────────────────────────────────────────────────────────────


async def test_run_pipeline_runs_idf_and_pagerank() -> None:
    calc, db = make_calculator(rowcount=4)

    await calc.run_pipeline(iterations=5, damping_factor=0.8)

    assert len(db.connection.statements) == 2
    assert db.connection.queries[0].strip() == IDF_UPDATE_SQL.strip()
    assert db.connection.statements[1][1] == (5, 0.8)


async def test_run_pipeline_gathers_both_stages_concurrently() -> None:
    """`asyncio.gather` is what makes this a pipeline rather than a sequence."""
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
