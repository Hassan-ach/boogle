"""`RankingCalculator` against a real PostgreSQL.

The unit tests for `calculator.py` assert on the SQL text and drive it through a
fake connection. That pins the *shape* of the queries but not their
*arithmetic*, and the arithmetic is where both defects lived:

  * `pages.id` and the document-frequency count are both `bigint`, so the ratio
    was computed by integer division. 378 / 352 truncated to 1, and LOG(1) is 0,
    so every word appearing on more than half the corpus was stored with an IDF
    of exactly zero -- weighted as if it carried no information at all.
  * On an empty corpus the argument to LOG was 0, which raises "cannot take
    logarithm of zero". That is not a retryable error, so it took the ranking
    pipeline down rather than degrading.

Neither is visible until the statement actually runs, which is what this file
does.

The adapter below is a fake in exactly one respect and it is deliberate: it does
not honour `commit()`. Everything else is psycopg, the real driver. Suppressing
the commit keeps the fixture rows inside the test's transaction so teardown can
roll the whole thing back -- without it every test would leave its graph behind
for the next one. That `commit` is called, and is called on the failure path
too, is asserted in the unit tests against the fake connection.
"""

from __future__ import annotations

import math
from typing import Any, Iterator

import pytest

from calculator import RankingCalculator

from .conftest import TOLERANCE
from .graph import GraphBuilder

pytestmark = pytest.mark.integration


# ── asyncpg-shaped adapter over psycopg ──────────────────────────────────────


class _Cursor:
    """Exposes the handful of asyncpg methods the calculator uses."""

    def __init__(self, raw: Any) -> None:
        self._raw = raw

    async def execute(self, query: str, params: tuple = ()) -> None:
        self._raw.execute(query, params or None)

    @property
    def rowcount(self) -> int:
        return self._raw.rowcount

    async def __aenter__(self) -> "_Cursor":
        return self

    async def __aexit__(self, *exc: object) -> bool:
        self._raw.close()
        return False


class _Connection:
    def __init__(self, raw: Any) -> None:
        self._raw = raw
        self.commits = 0
        self.rollbacks = 0

    def cursor(self) -> _Cursor:
        return _Cursor(self._raw.cursor())

    async def commit(self) -> None:
        # Not honoured on purpose; see the module docstring.
        self.commits += 1

    async def rollback(self) -> None:
        self.rollbacks += 1


class _ConnectionContext:
    def __init__(self, raw: Any) -> None:
        self._raw = raw
        self._wrapper: _Connection | None = None

    async def __aenter__(self) -> _Connection:
        self._wrapper = _Connection(self._raw)
        return self._wrapper

    async def __aexit__(self, *exc: object) -> bool:
        return False


class LiveDBManager:
    """A `DatabaseManager` whose connections are real but never committed.

    Every call to `get_connection()` hands back the *same* underlying
    connection, so the rows a test fixture writes and the rows the calculator
    sees live in one transaction. A real pool would hand out different sessions;
    what these tests care about is the SQL, not session isolation.
    """

    def __init__(self, dsn: str) -> None:
        self._dsn = dsn
        self._raw: Any = None

    def _connection(self) -> Any:
        if self._raw is None or self._raw.closed:
            import psycopg

            self._raw = psycopg.connect(self._dsn, autocommit=False)
        return self._raw

    def get_connection(self) -> _ConnectionContext:
        return _ConnectionContext(self._connection())

    def open_cursor(self) -> Any:
        """A plain psycopg cursor sharing the same transaction.

        The graph fixtures need psycopg's synchronous API, which is the one
        `GraphBuilder` is written against.
        """
        return self._connection().cursor()

    def close(self) -> None:
        if self._raw is not None:
            try:
                self._raw.rollback()
            finally:
                self._raw.close()
                self._raw = None


@pytest.fixture
def db_manager(dsn: str) -> Iterator[LiveDBManager]:
    manager = LiveDBManager(dsn)
    try:
        yield manager
    finally:
        manager.close()


@pytest.fixture
def calculator(db_manager: LiveDBManager) -> RankingCalculator:
    return RankingCalculator(db_manager)  # type: ignore[arg-type]


@pytest.fixture
def graph(
    db_manager: LiveDBManager, schema_ready: bool
) -> Iterator[GraphBuilder]:
    if not schema_ready:
        pytest.skip("the boogle schema is not present in the test database")
    yield GraphBuilder(db_manager.open_cursor())


def _idf(builder: GraphBuilder) -> dict[str, float]:
    builder._cursor.execute("SELECT word, idf FROM words")
    return {word: float(idf) for word, idf in builder._cursor.fetchall()}


# ── the integer-division defect ──────────────────────────────────────────────

# test_idfIsNonZeroForACommonWord is the regression test for the bigint
# truncation. The word is on 352 of 378 pages, so n/df is a little above 1 and
# the IDF is a small positive number. Integer division produced exactly 1, and
# LOG(1) is exactly 0, so the word was weighted as if it appeared nowhere.
async def test_idfIsNonZeroForACommonWord(
    calculator: RankingCalculator, graph: GraphBuilder
) -> None:
    for i in range(378):
        graph.page(f"p-{i}")
    graph.word("common", page_names=[f"p-{i}" for i in range(352)])
    graph.word("rare", page_names=["p-0"])

    affected = await calculator.compute_idf()

    assert affected == 2, f"updated {affected} words, want 2"
    idf = _idf(graph)
    assert idf["common"] > 0.0, f"a word on 352/378 pages got idf={idf['common']}"
    assert math.isclose(
        idf["common"], math.log10(378 / 352), rel_tol=1e-9
    ), f"idf['common'] = {idf['common']}, want log10(378/352)"


# test_idfIsNeverNegative is the regression test for the smoothed-denominator
# leak.
#
# The formula used to divide by (df + 1), which for a word on every page of a
# small corpus gives n/(n+1) < 1 and therefore a negative IDF. The engine
# multiplies tf by idf, so a negative weight does not merely fail to help, it
# subtracts from the page's score. A fresh crawl is exactly when this bites: the
# index starts at a handful of pages, so words present in all of them are
# everywhere, and a site-wide navigation label is a negative weight on every
# result.
async def test_idfIsNeverNegative(
    calculator: RankingCalculator, graph: GraphBuilder
) -> None:
    for i in range(5):
        graph.page(f"p-{i}")
    graph.word("on-every-page", page_names=[f"p-{i}" for i in range(5)])
    graph.word("on-most", page_names=[f"p-{i}" for i in range(4)])
    graph.word("on-one", page_names=["p-0"])

    await calculator.compute_idf()

    for word, value in _idf(graph).items():
        assert value >= 0.0, f"{word} has a negative idf: {value}"


# test_idfIsZeroOnlyForAWordOnEveryPage pins where the floor kicks in. The
# ratio is floored at 1 and LOG(1) is 0, so only a word on *every* page scores
# zero. A word on all but one still carries some information -- 4/3 against a
# corpus of 4 -- and is deliberately kept positive; the smoothing that would
# have pushed it below zero is exactly what made it a penalty.
async def test_idfIsZeroOnlyForAWordOnEveryPage(
    calculator: RankingCalculator, graph: GraphBuilder
) -> None:
    for i in range(4):
        graph.page(f"p-{i}")
    graph.word("everywhere", page_names=[f"p-{i}" for i in range(4)])
    graph.word("almost-everywhere", page_names=["p-0", "p-1", "p-2"])
    graph.word("somewhere", page_names=["p-0", "p-1"])

    await calculator.compute_idf()

    idf = _idf(graph)
    assert math.isclose(idf["everywhere"], 0.0, abs_tol=TOLERANCE), idf
    assert idf["almost-everywhere"] > 0.0, idf
    assert math.isclose(
        idf["almost-everywhere"], math.log10(4 / 3), rel_tol=1e-9
    ), f"idf['almost-everywhere'] = {idf['almost-everywhere']}, want log10(4/3)"
    assert math.isclose(
        idf["somewhere"], math.log10(4 / 2), rel_tol=1e-9
    ), f"idf['somewhere'] = {idf['somewhere']}, want log10(2)"


async def test_idfOrdersWordsByRarity(
    calculator: RankingCalculator, graph: GraphBuilder
) -> None:
    for i in range(10):
        graph.page(f"p-{i}")
    graph.word("on-all", page_names=[f"p-{i}" for i in range(10)])
    graph.word("on-half", page_names=[f"p-{i}" for i in range(5)])
    graph.word("on-one", page_names=["p-0"])

    await calculator.compute_idf()

    idf = _idf(graph)
    assert idf["on-one"] > idf["on-half"] > idf["on-all"], idf


async def test_idfGrowsWithCorpusSize(
    calculator: RankingCalculator, graph: GraphBuilder
) -> None:
    # The same word on the same number of pages is rarer -- and so more
    # informative -- in a bigger corpus. That is the whole point of IDF.
    for i in range(8):
        graph.page(f"p-{i}")
    graph.word("fixed", page_names=["p-0"])
    graph.word("growing", page_names=[f"p-{i}" for i in range(8)])

    await calculator.compute_idf()

    idf = _idf(graph)
    assert idf["fixed"] > idf["growing"], idf


# ── the empty-corpus defect ──────────────────────────────────────────────────

# test_idfOnAnEmptyCorpusDoesNotRaise is the regression test for LOG(0).
# "cannot take logarithm of zero" is raised as a plain Error, which the retry
# decorator does not retry and the pipeline does not survive.
async def test_idfOnAnEmptyCorpusDoesNotRaise(
    calculator: RankingCalculator, graph: GraphBuilder
) -> None:
    # A word row exists but no page does, so the corpus size is 0.
    graph.word("orphan", page_names=[])

    affected = await calculator.compute_idf()

    assert affected == 0, affected


async def test_idfOnACorpusOfOnePage(
    calculator: RankingCalculator, graph: GraphBuilder
) -> None:
    # n == df == 1, so the ratio is 1 and the IDF is 0: the only word on the
    # only page cannot distinguish that page from any other.
    graph.page("only")
    graph.word("word", page_names=["only"])

    await calculator.compute_idf()

    idf = _idf(graph)
    assert math.isclose(idf["word"], 0.0, abs_tol=TOLERANCE), idf


# ── bookkeeping ──────────────────────────────────────────────────────────────

async def test_idfLeavesAWordWithNoOccurrencesAlone(
    calculator: RankingCalculator, graph: GraphBuilder
) -> None:
    # A word row can exist with doc_frequency 0 between the indexer inserting it
    # and its page_word rows landing. The UPDATE joins on doc_freq, so it must
    # leave the default alone rather than computing log(n/1) for it.
    graph.page("p-0")
    graph._cursor.execute("INSERT INTO words (word, idf) VALUES ('untouched', 42.0)")

    affected = await calculator.compute_idf()

    assert affected == 0, affected
    assert _idf(graph)["untouched"] == 42.0


async def test_idfCountsDistinctPagesNotOccurrences(
    calculator: RankingCalculator, graph: GraphBuilder
) -> None:
    # A word repeated on one page has tf > 1 but df == 1. Counting occurrences
    # instead of pages would make a heavily-repeated word look common.
    for i in range(10):
        graph.page(f"p-{i}")
    graph.word("repeated", page_names=["p-0"], tf=50)
    graph.word("spread", page_names=[f"p-{i}" for i in range(5)])

    await calculator.compute_idf()

    idf = _idf(graph)
    assert idf["repeated"] > idf["spread"], idf


async def test_idfIsStableWhenRecomputed(
    calculator: RankingCalculator, graph: GraphBuilder
) -> None:
    for i in range(6):
        graph.page(f"p-{i}")
    graph.word("w", page_names=[f"p-{i}" for i in range(3)])

    await calculator.compute_idf()
    first = _idf(graph)
    await calculator.compute_idf()
    second = _idf(graph)

    assert first == second, (first, second)


# ── update_pagerank ──────────────────────────────────────────────────────────

async def test_update_pagerankReportsTheRowsItTouched(
    calculator: RankingCalculator, graph: GraphBuilder
) -> None:
    graph.chain(["a", "b", "c"])

    affected = await calculator.update_pagerank(iterations=20, damping_factor=0.85)

    assert affected == 3, f"reported {affected} pages, want 3"


async def test_update_pagerankStoresRowsInThePageRankTable(
    calculator: RankingCalculator, graph: GraphBuilder
) -> None:
    graph.chain(["a", "b", "c", "d"])

    await calculator.update_pagerank(iterations=40, damping_factor=0.85)

    graph._cursor.execute("SELECT count(*), sum(score) FROM page_rank")
    count, total = graph._cursor.fetchone()
    assert count == 4, count
    assert float(total) == pytest.approx(1.0, abs=1e-6), total


async def test_update_pagerankOnAnEmptyIndexIsANoOp(
    calculator: RankingCalculator, graph: GraphBuilder
) -> None:
    graph.page("a", indexed=False)

    affected = await calculator.update_pagerank(iterations=10, damping_factor=0.85)

    assert affected == 0, affected
    graph._cursor.execute("SELECT count(*) FROM page_rank")
    assert graph._cursor.fetchone()[0] == 0


async def test_run_pipelineRunsBothStages(
    calculator: RankingCalculator, graph: GraphBuilder
) -> None:
    for i in range(4):
        graph.page(f"p-{i}")
    graph.word("term", page_names=["p-0", "p-1"])
    graph.chain(["p-0", "p-1", "p-2", "p-3"])

    await calculator.run_pipeline(iterations=30, damping_factor=0.85)

    graph._cursor.execute("SELECT idf FROM words WHERE word = 'term'")
    row = graph._cursor.fetchone()
    assert row is not None and float(row[0]) > 0.0, row
    graph._cursor.execute("SELECT sum(score) FROM page_rank")
    assert float(graph._cursor.fetchone()[0]) == pytest.approx(1.0, abs=1e-6)
