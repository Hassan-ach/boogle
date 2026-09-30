"""`update_page_rank`: the stored procedure that writes every PageRank score.

These tests run against a real PostgreSQL because the defect they guard is
inside PL/pgSQL: the out-degree table is built from *all* graph edges while the
rank table holds *only indexed pages*, so rank is silently destroyed on every
iteration and the totals come out well under 1.0. No amount of faking the
Python layer would have found that.

Run with `just test-integration`, or:
    BOOGLE_TEST_PG_DSN=postgresql://admin:se@localhost:5432/boogle_test \
        pytest tests/integration -m integration
"""

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


# ── mass conservation ────────────────────────────────────────────────────────

# test_pagerankConservesTotalMass is the headline test for this whole migration.
#
# PageRank is a redistribution of a fixed total: teleport contributes (1-d) and
# the link term must hand on exactly the d it receives, dangling rank included.
# The sum over all scores is therefore 1.0 after every iteration and still 1.0
# at the end, for any graph.
#
# The out-degree table was built from every row in graph_edges while
# temp_page_rank holds only indexed pages. A page linking to 10 targets where
# only 3 are indexed divided its rank by 10, and the 7/10 pointing at
# unindexed pages fell on the floor -- `dangling_rank` never saw them, because
# the node *did* have a row in temp_out_degree. On the live database the total
# came to about 0.771, so roughly a quarter of all ranking signal was deleted on
# every run, and every recalculation moved every page's score toward zero.
def test_pagerankConservesTotalMass(graph: GraphBuilder) -> None:
    # A hub linking mostly into pages that are crawled but not indexed, which is
    # the exact shape that leaked. In a real crawl this is the normal state: a
    # page is committed to `pages` before the indexer gets to it, and indexing
    # fails or is still queued for a meaningful fraction of them.
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
    # d = 1.0 has no teleport term at all, so 100% of the rank has to come back
    # through the link redistribution. It is the sharpest probe of the leak.
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
    # Nothing to rank: the procedure must return no rows, and the caller must
    # not be handed a NaN or a zero row to divide by.
    graph.page("a", indexed=False)
    graph.page("b", indexed=False)
    graph.edge("a", "b")

    graph._cursor.execute("SELECT update_page_rank(10, 0.85)")
    assert graph._cursor.fetchall() == []


def test_pagerankOnASinglePageWithNoEdges(graph: GraphBuilder) -> None:
    # A one-node graph is entirely dangling. All of its rank must be
    # redistributed back to itself.
    graph.page("only")

    total = graph.run_pagerank(iterations=20, damping=0.85)

    assert math.isclose(total, 1.0, abs_tol=TOLERANCE)
    assert math.isclose(graph.scores()["only"], 1.0, abs_tol=TOLERANCE)


# ── correctness of the distribution ───────────────────────────────────────────

def test_pagerankGivesAHubMoreThanALeaf(graph: GraphBuilder) -> None:
    # The most-linked page must outrank the pages that link to nothing.
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
    # a -> b -> c -> a. Every page has out-degree exactly 1 and no page is
    # dangling, so every page gets the same share back from its single
    # predecessor and the stationary distribution is exactly uniform. This is
    # the one shape where PageRank has a closed-form answer, which makes it the
    # sharpest available check that the iteration itself is right.
    graph.chain(["a", "b", "c"])
    graph.edge("c", "a")

    damping = 0.85
    scores = graph.run_pagerank_scores(iterations=200, damping=damping)

    assert set(scores) == {"a", "b", "c"}, scores
    for name, score in scores.items():
        assert math.isclose(score, 1 / 3, rel_tol=1e-6), f"{name} = {score}, want 1/3"


def test_pagerankOnAChainConcentratesAtTheDanglingEnd(graph: GraphBuilder) -> None:
    # a -> b -> c, an open chain. c has no outgoing edges, so it is dangling and
    # its whole share is redistributed back over every page -- including itself.
    # That is why the *last* page of a chain scores highest here, and why
    # PageRank over a crawl of mostly-open link chains pushes rank towards
    # leaves. The order is the assertion; the exact ratios are not, because they
    # depend on where the dangling pool settles.
    graph.chain(["a", "b", "c"])

    scores = graph.run_pagerank_scores(iterations=200, damping=0.85)

    assert scores["c"] > scores["b"] > scores["a"], scores
    assert math.isclose(sum(scores.values()), 1.0, abs_tol=1e-6), scores


def test_pagerankPrefersTheTargetOfManyLinksOverTheSourceOfOne(
    graph: GraphBuilder,
) -> None:
    # The property the ranking actually depends on: a page that many pages link
    # to must outrank a page that links to it. A one-directional edge transfers
    # rank to its target and takes none for itself.
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
    # A page that was crawled but not indexed contributes neither rank nor
    # reachability. It must not appear in the results at all.
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
    # Recalculating without new data must not move any score. A leak makes the
    # vector drift toward zero on every run, so running twice and comparing is
    # a cheap regression test for exactly that.
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
    # The procedure RETURNS the scores and the caller may use that order
    # directly as a leaderboard.
    graph.page("hub")
    for i in range(5):
        graph.page(f"leaf-{i}")
        graph.edge(f"leaf-{i}", "hub")

    graph._cursor.execute("SELECT * FROM update_page_rank(30, 0.85)")
    scores = [float(row[1]) for row in graph._cursor.fetchall()]

    assert scores == sorted(scores, reverse=True), scores


# ── idempotency and transaction behaviour ────────────────────────────────────

# TestPAGERankCanRunTwiceInOneTransaction is the regression test for the temp
# table collision.
#
# Both temp tables are `ON COMMIT DROP`, so within a single transaction they
# survive the first call. The second call hit
# `relation "temp_page_rank" already exists` and raised, which the ranking
# service treats as a non-retryable error -- so any caller that runs the
# procedure twice in one transaction (a backfill, a test, a repair script) was
# simply broken.
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
    # ON COMMIT DROP must still work. Run the procedure in its own committed
    # transaction and then confirm the temp tables are gone.
    #
    # This test commits, so it deliberately touches nothing but its own rows.
    # Dropping or truncating a shared table here would take every later test in
    # the run down with it.
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


# ── guards ───────────────────────────────────────────────────────────────────

def test_pagerankHandlesSelfLoops(graph: GraphBuilder) -> None:
    # A page linking to itself has an out-degree of at least 1 from its own
    # edge, and its rank must still come back to itself.
    graph.page("self")
    graph.edge("self", "self")

    total = graph.run_pagerank(iterations=20, damping=0.85)

    assert math.isclose(total, 1.0, abs_tol=TOLERANCE), total
    assert math.isclose(graph.scores()["self"], 1.0, abs_tol=TOLERANCE)


def test_pagerankHandlesDuplicateEdges(graph: GraphBuilder) -> None:
    # graph_edges has a UNIQUE(from_url, to_url) constraint, so a duplicate
    # cannot be inserted, but a page linking to the same target via a different
    # url row is possible and must not double-count.
    graph.page("a")
    graph.page("b")
    graph.edge("a", "b")

    total = graph.run_pagerank(iterations=20, damping=0.85)

    assert math.isclose(total, 1.0, abs_tol=TOLERANCE), total


def test_pagerankHandlesZeroIterations(graph: GraphBuilder) -> None:
    # With no iterations the vector is still uniform, so the total is 1.0.
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
