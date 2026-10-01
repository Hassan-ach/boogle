from __future__ import annotations

import math
from typing import Iterator

import psycopg
import pytest

from .conftest import TOLERANCE
from .graph import GraphBuilder

pytestmark = pytest.mark.integration


@pytest.fixture
def graph(cursor: psycopg.Cursor, schema_ready: bool) -> Iterator[GraphBuilder]:
    if not schema_ready:
        pytest.skip("the boogle schema is not present in the test database")
    return GraphBuilder(cursor)


def test_pagerankConservesTotalMass(graph: GraphBuilder) -> None:
    for i in range(3):
        graph.page(f"indexed-{i}")
    for i in range(7):
        graph.unindexed_page(f"pending-{i}")
    graph.page("hub")
    graph.page("lonely")

    for i in range(3):
        graph.edge("hub", f"indexed-{i}")
    for i in range(7):
        graph.edge("hub", f"pending-{i}")
    graph.edge("indexed-0", "indexed-1")
    graph.edge("lonely", "indexed-2")

    total = graph.run_pagerank(iterations=40, damping=0.85)

    assert math.isclose(
        total,
        1.0,
        abs_tol=TOLERANCE,
    ), f"total rank is {total!r}, want 1.0; PageRank must not create or destroy rank"


@pytest.mark.parametrize("damping", [0.0, 0.5, 0.85, 0.95, 1.0])
def test_pagerankConservesMassAtEveryDampingFactor(
    graph: GraphBuilder, damping: float
) -> None:
    for i in range(4):
        graph.page(f"a-{i}")
    for i in range(4):
        graph.unindexed_page(f"unindexed-{i}")
    graph.edge("a-0", "a-1")
    graph.edge("a-0", "unindexed-0")
    graph.edge("a-0", "unindexed-1")
    graph.edge("a-2", "a-3")

    total = graph.run_pagerank(iterations=30, damping=damping)

    assert math.isclose(total, 1.0, abs_tol=1e-8), f"d={damping}: total is {total!r}, want 1.0"


def test_pagerankConservesMassWhenNoPagesAreIndexed(graph: GraphBuilder) -> None:
    graph.page("a", indexed=False)
    graph.page("b", indexed=False)
    graph.edge("a", "b")

    graph._cursor.execute("SELECT update_page_rank(10, 0.85)")
    assert graph._cursor.fetchall() == []


def test_pagerankOnASinglePageWithNoEdges(graph: GraphBuilder) -> None:
    graph.page("only")

    total = graph.run_pagerank(iterations=20, damping=0.85)

    assert math.isclose(total, 1.0, abs_tol=TOLERANCE)
    assert math.isclose(graph.scores()["only"], 1.0, abs_tol=TOLERANCE)


def test_pagerankGivesAHubMoreThanALeaf(graph: GraphBuilder) -> None:
    graph.page("hub")
    graph.page("leaf-a")
    graph.page("leaf-b")
    graph.edge("leaf-a", "hub")
    graph.edge("leaf-b", "hub")

    scores = graph.run_pagerank_scores(iterations=40, damping=0.85)

    assert scores["hub"] > scores["leaf-a"], scores
    assert scores["hub"] > scores["leaf-b"], scores
    assert math.isclose(sum(scores.values()), 1.0, abs_tol=TOLERANCE), scores


def test_pagerankOnADirectedCycleConvergesToUniform(graph: GraphBuilder) -> None:
    graph.chain(["a", "b", "c"])
    graph.edge("c", "a")

    damping = 0.85
    scores = graph.run_pagerank_scores(iterations=200, damping=damping)

    assert set(scores) == {"a", "b", "c"}, scores
    for name, score in scores.items():
        assert math.isclose(score, 1 / 3, rel_tol=1e-6), f"{name} = {score}, want 1/3"


def test_pagerankOnAChainConcentratesAtTheDanglingEnd(graph: GraphBuilder) -> None:
    graph.chain(["a", "b", "c"])

    scores = graph.run_pagerank_scores(iterations=200, damping=0.85)

    assert scores["c"] > scores["b"] > scores["a"], scores
    assert math.isclose(sum(scores.values()), 1.0, abs_tol=1e-6), scores


def test_pagerankPrefersTheTargetOfManyLinksOverTheSourceOfOne(
    graph: GraphBuilder,
) -> None:
    graph.page("source")
    graph.page("target")
    for i in range(3):
        graph.page(f"linker-{i}")
        graph.edge(f"linker-{i}", "target")
    graph.edge("source", "target")

    scores = graph.run_pagerank_scores(iterations=60, damping=0.85)

    assert scores["target"] > scores["source"], scores
    assert scores["target"] > scores["linker-0"], scores


def test_pagerankIgnoresUnindexedPagesEntirely(graph: GraphBuilder) -> None:
    graph.page("indexed-a")
    graph.page("indexed-b")
    graph.unindexed_page("pending-c")
    graph.edge("indexed-a", "indexed-b")
    graph.edge("indexed-a", "pending-c")

    scores = graph.run_pagerank_scores(iterations=30, damping=0.85)

    assert "pending-c" not in scores, scores
    assert set(scores) == {"indexed-a", "indexed-b"}, scores
    assert math.isclose(sum(scores.values()), 1.0, abs_tol=TOLERANCE), scores


def test_pagerankTieBreakOnMassIsStableAcrossRepeatedRuns(graph: GraphBuilder) -> None:
    graph.chain(["a", "b", "c", "d"])

    first = graph.run_pagerank_scores(iterations=30, damping=0.85)
    second = graph.run_pagerank_scores(iterations=30, damping=0.85)

    for name, score in first.items():
        assert math.isclose(score, second[name], abs_tol=1e-9), (
            f"{name} moved from {score!r} to {second[name]!r} on a recalculation "
            "with no new data"
        )


def test_pagerankUpdatesRatherThanDuplicatingRows(graph: GraphBuilder) -> None:
    graph.chain(["a", "b", "c"])
    graph.run_pagerank(iterations=20, damping=0.85)
    graph.run_pagerank(iterations=20, damping=0.85)

    graph._cursor.execute("SELECT count(*) FROM page_rank")
    count = graph._cursor.fetchone()[0]

    assert count == 3, f"page_rank has {count} rows for 3 pages"


def test_pagerankReturnsRowsOrderedByDescendingScore(graph: GraphBuilder) -> None:
    graph.page("hub")
    for i in range(5):
        graph.page(f"leaf-{i}")
        graph.edge(f"leaf-{i}", "hub")

    graph._cursor.execute("SELECT * FROM update_page_rank(30, 0.85)")
    scores = [float(row[1]) for row in graph._cursor.fetchall()]

    assert scores == sorted(scores, reverse=True), scores


def test_pagerankCanRunTwiceInOneTransaction(graph: GraphBuilder) -> None:
    graph.chain(["a", "b", "c"])

    first = graph.run_pagerank_scores(iterations=20, damping=0.85)
    second = graph.run_pagerank_scores(iterations=20, damping=0.85)

    assert set(first) == set(second)
    for name, score in first.items():
        assert math.isclose(score, second[name], abs_tol=1e-9), name


def test_pagerankCanRunTwiceWithDifferentParameters(graph: GraphBuilder) -> None:
    graph.chain(["a", "b", "c"])

    graph.run_pagerank(iterations=5, damping=0.5)
    total = graph.run_pagerank(iterations=50, damping=0.95)

    assert math.isclose(total, 1.0, abs_tol=1e-8), total


def test_pagerankLeavesNoTempTablesBehindAfterCommit(dsn: str) -> None:
    setup = psycopg.connect(dsn, autocommit=True)
    try:
        with setup.cursor() as cur:
            cur.execute(
                "SELECT EXISTS (SELECT 1 FROM information_schema.tables "
                "WHERE table_name = 'urls')"
            )
            if not cur.fetchone()[0]:
                pytest.skip("the boogle schema is not present in the test database")

            for name in ("tmp-pr-a", "tmp-pr-b"):
                cur.execute(
                    "INSERT INTO urls (url) VALUES (%s) ON CONFLICT (url) DO NOTHING",
                    (f"https://{name}/",),
                )
            cur.execute(
                """
                INSERT INTO pages (url_id, html, indexed)
                SELECT id, '<html>x</html>', TRUE FROM urls
                WHERE url IN ('https://tmp-pr-a/', 'https://tmp-pr-b/')
                ON CONFLICT (url_id) DO UPDATE SET indexed = TRUE
                """
            )
            cur.execute(
                """
                INSERT INTO graph_edges (from_url, to_url)
                SELECT a.id, b.id FROM urls a, urls b
                WHERE a.url = 'https://tmp-pr-a/' AND b.url = 'https://tmp-pr-b/'
                ON CONFLICT DO NOTHING
                """
            )
        setup.commit()

        run = psycopg.connect(dsn, autocommit=False)
        try:
            with run.cursor() as cur:
                cur.execute("SELECT update_page_rank(10, 0.85)")
                assert cur.fetchall(), "the procedure returned no rows"
            run.commit()
        finally:
            run.close()

        check = psycopg.connect(dsn, autocommit=True)
        try:
            with check.cursor() as cur:
                cur.execute(
                    """
                    SELECT count(*) FROM pg_class
                    WHERE relname IN ('temp_page_rank', 'temp_out_degree')
                    """
                )
                assert cur.fetchone()[0] == 0, "temp tables survived the commit"
        finally:
            check.close()
    finally:
        with setup.cursor() as cur:
            cur.execute(
                "DELETE FROM page_rank WHERE url_id IN "
                "(SELECT id FROM urls WHERE url LIKE 'https://tmp-pr-%')"
            )
            cur.execute(
                "DELETE FROM graph_edges WHERE from_url IN "
                "(SELECT id FROM urls WHERE url LIKE 'https://tmp-pr-%')"
            )
            cur.execute(
                "DELETE FROM pages WHERE url_id IN "
                "(SELECT id FROM urls WHERE url LIKE 'https://tmp-pr-%')"
            )
            cur.execute("DELETE FROM urls WHERE url LIKE 'https://tmp-pr-%'")
        setup.commit()
        setup.close()


def test_pagerankHandlesSelfLoops(graph: GraphBuilder) -> None:
    graph.page("self")
    graph.edge("self", "self")

    total = graph.run_pagerank(iterations=20, damping=0.85)

    assert math.isclose(total, 1.0, abs_tol=TOLERANCE), total
    assert math.isclose(graph.scores()["self"], 1.0, abs_tol=TOLERANCE)


def test_pagerankHandlesDuplicateEdges(graph: GraphBuilder) -> None:
    graph.page("a")
    graph.page("b")
    graph.edge("a", "b")

    total = graph.run_pagerank(iterations=20, damping=0.85)

    assert math.isclose(total, 1.0, abs_tol=TOLERANCE), total


def test_pagerankHandlesZeroIterations(graph: GraphBuilder) -> None:
    graph.chain(["a", "b", "c"])

    total = graph.run_pagerank(iterations=0, damping=0.85)

    assert math.isclose(total, 1.0, abs_tol=TOLERANCE), total


def test_pagerankScoresAreNeverNegative(graph: GraphBuilder) -> None:
    for i in range(6):
        graph.page(f"n-{i}")
    for i in range(5):
        graph.page(f"dead-{i}")
    graph.chain(["n-0", "n-1", "n-2"])
    graph.edge("n-0", "dead-0")
    graph.edge("n-3", "n-4")
    graph.edge("n-3", "dead-1")

    scores = graph.run_pagerank_scores(iterations=30, damping=0.85)

    for name, score in scores.items():
        assert score >= 0.0, f"{name} has a negative score: {score}"
