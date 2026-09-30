"""Graph fixtures for the PageRank integration tests.

These build a small, fully-known web graph in the test database so the expected
ranking can be reasoned about exactly, rather than compared against whatever the
function happens to produce.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Iterable, Optional, Sequence

import psycopg


@dataclass(frozen=True)
class UrlId:
    """A url_id, kept as a plain uuid string so comparisons read clearly."""

    value: str


class GraphBuilder:
    """Builds a small graph: urls, pages, and the edges between them.

    Every insert is a plain statement on the caller's connection, so it joins
    the surrounding transaction and is rolled back with it.
    """

    def __init__(self, cursor: psycopg.Cursor) -> None:
        self._cursor = cursor
        self._urls: dict[str, str] = {}

    def url(self, name: str) -> str:
        """Create (or reuse) a url and return its id."""
        if name in self._urls:
            return self._urls[name]
        self._cursor.execute(
            "INSERT INTO urls (url) VALUES (%s) RETURNING id", (f"https://{name}/",)
        )
        self._urls[name] = str(self._cursor.fetchone()[0])
        return self._urls[name]

    def page(self, name: str, *, indexed: bool = True, html: Optional[str] = None) -> str:
        """Create a page for `name` and return its *url_id*.

        Idempotent: `pages.url_id` is UNIQUE, and a test that pre-creates a page
        and then calls a helper that also creates one should not have to know
        which order those happen in.
        """
        url_id = self.url(name)
        self._cursor.execute(
            "SELECT indexed FROM pages WHERE url_id = %s", (url_id,)
        )
        existing = self._cursor.fetchone()
        if existing is not None:
            if existing[0] != indexed:
                self._cursor.execute(
                    "UPDATE pages SET indexed = %s WHERE url_id = %s", (indexed, url_id)
                )
            return url_id
        self._cursor.execute(
            """
            INSERT INTO pages (url_id, html, indexed)
            VALUES (%s, %s, %s)
            """,
            (url_id, html if html is not None else f"<html>{name}</html>", indexed),
        )
        return url_id

    def unindexed_page(self, name: str) -> str:
        """A page that exists in `pages` but is not part of the ranking.

        This is the case that matters: a crawled page whose indexing failed.
        """
        return self.page(name, indexed=False)

    def edge(self, source: str, target: str) -> None:
        """Record that `source` links to `target`."""
        self._cursor.execute(
            "INSERT INTO graph_edges (from_url, to_url) VALUES (%s, %s)",
            (self.url(source), self.url(target)),
        )

    def chain(self, names: Sequence[str]) -> None:
        """Create an indexed page per name and link each to the next."""
        for name in names:
            self.page(name)
        for source, target in zip(names, names[1:]):
            self.edge(source, target)

    def star(self, hub: str, spokes: Iterable[str]) -> None:
        """Link the hub to every spoke, creating indexed pages for both ends."""
        self.page(hub)
        for spoke in spokes:
            self.page(spoke)
            self.edge(hub, spoke)

    def scores(self) -> dict[str, float]:
        """The stored PageRank score per page name."""
        self._cursor.execute(
            """
            SELECT u.url, pr.score
            FROM page_rank pr
            JOIN urls u ON u.id = pr.url_id
            """
        )
        return {
            url.replace("https://", "").strip("/"): float(score) for url, score in self._cursor.fetchall()
        }

    def idf(self) -> dict[str, float]:
        """The stored IDF per word."""
        self._cursor.execute("SELECT word, idf FROM words")
        return {word: float(idf) for word, idf in self._cursor.fetchall()}

    def word(self, text: str, *, page_names: Sequence[str], tf: int = 1) -> None:
        """Index a word onto several pages, the way the indexer would."""
        word_id = self._word_id(text)
        for name in page_names:
            self._cursor.execute(
                "SELECT id FROM pages WHERE url_id = %s", (self.url(name),)
            )
            row = self._cursor.fetchone()
            if row is None:
                raise AssertionError(f"page {name!r} does not exist")
            self._cursor.execute(
                """
                INSERT INTO page_word (page_id, word_id, tf)
                VALUES (%s, %s, %s)
                ON CONFLICT (page_id, word_id) DO UPDATE SET tf = page_word.tf + EXCLUDED.tf
                """,
                (row[0], word_id, tf),
            )

    def _word_id(self, text: str) -> str:
        self._cursor.execute(
            """
            INSERT INTO words (word, doc_frequency)
            VALUES (%s, 0)
            ON CONFLICT (word) DO UPDATE SET word = EXCLUDED.word
            RETURNING id
            """,
            (text,),
        )
        return str(self._cursor.fetchone()[0])

    def run_pagerank(self, iterations: int = 20, damping: float = 0.85) -> float:
        """Run the stored procedure and return the total rank it produced."""
        return sum(score for _, score in self.run_pagerank_rows(iterations, damping))

    def run_pagerank_rows(
        self, iterations: int = 20, damping: float = 0.85
    ) -> list[tuple[str, float]]:
        """Run the stored procedure and return (url_id, score) rows.

        `SELECT * FROM update_page_rank(...)` rather than `SELECT
        update_page_rank(...)`: a function with OUT parameters only expands them
        into separate columns when it appears in FROM, so the plain SELECT
        returns a single `record` column. The service itself uses the plain form
        because it only wants the row count.
        """
        self._cursor.execute("SELECT * FROM update_page_rank(%s, %s)", (iterations, damping))
        return [(str(url), float(score)) for url, score in self._cursor.fetchall()]

    def run_pagerank_scores(
        self, iterations: int = 20, damping: float = 0.85
    ) -> dict[str, float]:
        """Run the stored procedure and return the scores by page name."""
        by_id = {url: score for url, score in self.run_pagerank_rows(iterations, damping)}
        return {
            name: by_id[url_id] for name, url_id in self._urls.items() if url_id in by_id
        }
