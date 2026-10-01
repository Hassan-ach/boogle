from __future__ import annotations

import math
from typing import Any, Iterator

import pytest

from calculator import RankingCalculator

from .conftest import TOLERANCE
from .graph import GraphBuilder

pytestmark = pytest.mark.integration


class _Cursor:

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
    return RankingCalculator(db_manager)


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
    for i in range(8):
        graph.page(f"p-{i}")
    graph.word("fixed", page_names=["p-0"])
    graph.word("growing", page_names=[f"p-{i}" for i in range(8)])

    await calculator.compute_idf()

    idf = _idf(graph)
    assert idf["fixed"] > idf["growing"], idf


async def test_idfOnAnEmptyCorpusDoesNotRaise(
    calculator: RankingCalculator, graph: GraphBuilder
) -> None:
    graph.word("orphan", page_names=[])

    affected = await calculator.compute_idf()

    assert affected == 0, affected


async def test_idfOnACorpusOfOnePage(
    calculator: RankingCalculator, graph: GraphBuilder
) -> None:
    graph.page("only")
    graph.word("word", page_names=["only"])

    await calculator.compute_idf()

    idf = _idf(graph)
    assert math.isclose(idf["word"], 0.0, abs_tol=TOLERANCE), idf


async def test_idfLeavesAWordWithNoOccurrencesAlone(
    calculator: RankingCalculator, graph: GraphBuilder
) -> None:
    graph.page("p-0")
    graph._cursor.execute("INSERT INTO words (word, idf) VALUES ('untouched', 42.0)")

    affected = await calculator.compute_idf()

    assert affected == 0, affected
    assert _idf(graph)["untouched"] == 42.0


async def test_idfCountsDistinctPagesNotOccurrences(
    calculator: RankingCalculator, graph: GraphBuilder
) -> None:
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
