# Services Overview

Four services implementing a complete search engine pipeline. They share only
PostgreSQL, RabbitMQ and (for the Spider) Redis; there are no direct service calls.

## Pipeline

```text
Web → Spider ──indexer.jobs────────→ Indexer ──indexer.confirmations────────→ Ranking
       │                              │                                      │
       │ urls, pages, graph_edges     │ words, page_word                     │ words.idf, page_rank
       └──────────────┬───────────────┴──────────────────┬───────────────────┘
                      ▼                                  ▼
                  PostgreSQL index  ←── reads ── Engine ──→ User
```

| Service | Language | Owns |
|---------|----------|------|
| [Engine](engine/) | Go | HTTP UI, query parsing, cosine ranking over the index |
| [Spider](spider/) | Go | Crawl frontier in Redis, policy, `urls`, `pages`, `graph_edges` |
| [Indexer](indexer/) | Rust | Page claims, tokenization, `words`, `page_word`, confirmations |
| [Ranking](ranking/) | Python | `words.idf` and `page_rank` recomputation |

Ranking computes **IDF**, not full TF-IDF. The Engine computes per-query
`tf * idf` and blends it with PageRank.

## Service-owned documentation

These Markdown trees are the source of truth for service internals. The root
MkDocs build copies them into ignored `docs/services/<service>/` directories.

- [Spider docs](spider/docs/index.md) — [architecture](spider/docs/architecture.md), [implementation](spider/docs/implementation.md), [configuration](spider/docs/configuration.md)
- [Engine docs](engine/docs/index.md) — [architecture](engine/docs/architecture.md), [HTTP interface](engine/docs/api.md), [implementation](engine/docs/implementation.md), [configuration](engine/docs/configuration.md)
- [Indexer docs](indexer/docs/index.md) — [architecture](indexer/docs/architecture.md), [indexing pipeline](indexer/docs/indexing-pipeline.md), [configuration](indexer/docs/configuration.md)
- [Ranking docs](ranking/docs/index.md) — [architecture](ranking/docs/architecture.md), [algorithms](ranking/docs/algorithms.md), [database functions](ranking/docs/database-functions.md), [configuration](ranking/docs/configuration.md)

Build or validate them from the repository root:

```bash
just docs-service spider     # also: engine, indexer, ranking
just docs-check
just docs-serve
```

See [Documentation workflow](../docs/development/documentation.md) for the
aggregation contract and CI checks.

## Quick start

From the root, see the [top-level README](../README.md) for the full sequence:

```bash
just env                  # bootstrap .env files and container overlays
just infra-up             # PostgreSQL, Redis, RabbitMQ, Adminer
./scripts/migrate.sh
just run                  # build and start all four services
open http://localhost:1323
```

`just run-logs` follows service logs; `just stop` tears down services and
datastores but keeps the `pgdata` volume.

## Data flow details

1. Spider admits a URL, fetches it, stores HTML in `pages`, records links in
   `graph_edges`, and publishes `{"page_id": "..."}` to `indexer.jobs`.
2. Indexer claims the page with `FOR UPDATE SKIP LOCKED`. `indexed = TRUE` means
   *claimed*; `indexed_at IS NOT NULL` means *finished*.
3. Indexer tokenizes HTML, writes raw integer term frequencies to `page_word`,
   publishes an `indexer.confirmations` message, then marks the page complete.
4. Ranking recomputes IDF over all pages and PageRank over indexed pages after
   enough confirmations accumulate. It does not publish to the Engine.
5. Engine queries candidate rows from PostgreSQL and re-ranks them in Go.

See [Data flow](../docs/architecture/data-flow.md),
[Message contracts](../docs/data/message-contracts.md) and
[Known issues](../docs/reference/known-issues.md) for failure handling and
currently verified gaps.

## Individual service READMEs

- [Engine README](engine/README.md)
- [Spider README](spider/README.md)
- [Indexer README](indexer/README.md)
- [Ranking README](ranking/README.md)

See [root README](../README.md) for prerequisites and full setup details.
