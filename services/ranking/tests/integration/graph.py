from __future__ import annotations

from dataclasses import dataclass
from typing import Iterable, Optional, Sequence

import psycopg


@dataclass(frozen=True)
class UrlId:

    value: str


class GraphBuilder:

    def __init__(self, cursor: psycopg.Cursor) -> None:
        self._cursor = cursor
        self._urls: dict[str, str] = {}

    def url(self, name: str) -> str:
        if name in self._urls:
            return self._urls[name]
        self._cursor.execute(
            "INSERT INTO urls (url) VALUES (%s) RETURNING id", (f"https://{name}/",)
        )
        self._urls[name] = str(self._cursor.fetchone()[0])
        return self._urls[name]

    def page(self, name: str, *, indexed: bool = True, html: Optional[str] = None) -> str:
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
        return self.page(name, indexed=False)

    def edge(self, source: str, target: str) -> None:
        self._cursor.execute(
            "INSERT INTO graph_edges (from_url, to_url) VALUES (%s, %s)",
            (self.url(source), self.url(target)),
        )

    def chain(self, names: Sequence[str]) -> None:
        for name in names:
            self.page(name)
        for source, target in zip(names, names[1:]):
            self.edge(source, target)

    def star(self, hub: str, spokes: Iterable[str]) -> None:
        self.page(hub)
        for spoke in spokes:
            self.page(spoke)
            self.edge(hub, spoke)

    def scores(self) -> dict[str, float]:
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
        self._cursor.execute("SELECT word, idf FROM words")
        return {word: float(idf) for word, idf in self._cursor.fetchall()}

    def word(self, text: str, *, page_names: Sequence[str], tf: int = 1) -> None:
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
        return sum(score for _, score in self.run_pagerank_rows(iterations, damping))

    def run_pagerank_rows(
        self, iterations: int = 20, damping: float = 0.85
    ) -> list[tuple[str, float]]:
        self._cursor.execute("SELECT * FROM update_page_rank(%s, %s)", (iterations, damping))
        return [(str(url), float(score)) for url, score in self._cursor.fetchall()]

    def run_pagerank_scores(
        self, iterations: int = 20, damping: float = 0.85
    ) -> dict[str, float]:
        by_id = {url: score for url, score in self.run_pagerank_rows(iterations, damping)}
        return {
            name: by_id[url_id] for name, url_id in self._urls.items() if url_id in by_id
        }
