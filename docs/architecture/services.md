# Services

**Last updated:** 2026-11-23

Boogle has four services with disjoint write ownership. This page summarises responsibilities, entry points, and data access. Full details live in the aggregated service pages.

## At a glance

| Service | Language / runtime | Main entry point | Responsibility | Aggregated docs |
|---|---|---|---|---|
| Spider | Go 1.27 | `services/spider/cmd/spider/main.go` | Crawl public pages, persist pages/edges, publish jobs | [Overview](../services/spider/index.md) |
| Indexer | Rust edition 2024, Tokio | `services/indexer/src/main.rs` | Claim pages, build term frequencies, confirm completion | [Overview](../services/indexer/index.md) |
| Ranking | Python 3.11+, FastStream | `services/ranking/src/main.py` | Recompute IDF and PageRank | [Overview](../services/ranking/index.md) |
| Engine | Go 1.27.1, Echo v5 | `services/engine/cmd/server/main.go` | Serve the search UI over the index | [Overview](../services/engine/index.md) |

## Spider

- **Responsibilities:** read hard-coded seed URLs, pop due URLs from a Redis sorted-set frontier, apply robots/policy admission, fetch with a public-only dialer (SSRF guard), parse HTML, and persist pages plus discovered links. One goroutine runs per `MAX_CRAWLERS`; a semaphore bounds concurrent fetches.
- **Lifecycle:** `NewSpider` fatals if RabbitMQ, Redis, or PostgreSQL is unavailable. `Start` seeds Redis only when the frontier is empty, then runs crawler goroutines until `SIGINT`/`SIGTERM`; `Stop` cancels the root context and waits for workers.
- **Writes:** `urls`, `pages` (`url_id`, `html`, `metadata`), `graph_edges` in one transaction per page ([`services/spider/internal/store/…`](../services/spider/implementation.md)).
- **Reads:** Redis frontier, delayed/visited sets, and per-URL/per-host policy state.
- **Publishes:** `indexer.jobs` with `{"page_id":"<uuid>"}` only after the database transaction commits; a publish failure parks the URL for 30 seconds.
- **Caveats:** per-host `Crawl-delay` is stored but not enforced as pacing; `RABBITMQ_PROTOCOL` is read but the AMQP URI is hard-coded to `amqp://`; sitemap index files are not followed.

See [Spider architecture](../services/spider/architecture.md) and [configuration](../services/spider/configuration.md).

## Indexer

- **Responsibilities:** consume `indexer.jobs`, claim one page, tokenize stored HTML in `spawn_blocking`, write `words`/`page_word` in batches, publish `indexer.confirmations`, mark completion, ACK. A sweep loop republishes stale unindexed pages.
- **Lifecycle:** `main.rs` loads configuration, opens the Postgres pool and RabbitMQ channels, then spawns the consume loop and sweep loop. It listens for **Ctrl-C only**, not SIGTERM; in Docker the entrypoint `exec`s the binary as PID 1.
- **Writes:** `words` (word strings only), `page_word` (`tf` is a raw integer count), and the page claim fields `indexed`, `indexed_at`, `last_index_attempt_at`, `index_attempts`.
- **Reads:** `pages.html`, `pages.indexed`, `index_attempts`, and its own claim timestamps.
- **Publishes:** `indexer.confirmations` with the same `{"page_id":"<uuid>"}` shape, using mandatory persistent publishing with confirms. The sweep's republish is best effort (no confirms or mandatory flag).
- **Delivery:** ACK on success; NACK with requeue for database (except not-found), messaging, and other errors; no requeue for serde, parse, or not-found errors. No dead-letter exchange is configured.

See [Indexer architecture](../services/indexer/architecture.md) and [indexing pipeline](../services/indexer/indexing-pipeline.md).

## Ranking

- **Responsibilities:** count confirmations and trigger full recomputation of IDF and PageRank; own `words.idf` and `page_rank`.
- **Lifecycle:** `main.py` loads env, builds the database manager and messaging service eagerly, opens the pool with retry, registers the pipeline handler, optionally forces a startup pass, then awaits the FastStream consumer. There is no scheduler.
- **Writes:** `words.idf` (one SQL `UPDATE`); `page_rank` (delete stale rows, then upsert scores).
- **Reads:** `pages` (all rows for IDF, indexed rows for PageRank), `page_word.word_id`/`page_id`, `graph_edges` (indexed → indexed only). It does not read `page_word.tf` and does not maintain `words.doc_frequency`.
- **Consumes:** `indexer.confirmations`; only `page_id` is read, for logging, and any dict body increments the counter.
- **Failure:** a failed pipeline keeps its confirmation batch pending until another confirmation arrives; IDF and PageRank commit independently.

See [Ranking architecture](../services/ranking/architecture.md) and [algorithms](../services/ranking/algorithms.md).

## Engine

- **Responsibilities:** serve `GET /`, `GET /search`, `GET /health`, `GET /feeling-lucky`, and `GET /public/*`; render `templ` views with htmx and Alpine.
- **Lifecycle:** `cmd/server/main.go` opens the Postgres pool (fail-fast on ping failure), selects aspell with a dummy fallback, registers middleware (panic recovery, request ID, path-only logger, gzip, in-process rate limiter), and listens on `:1323`. No graceful shutdown.
- **Reads only:** `words`, `page_word`, `pages`, `urls`, `page_rank` via two parameterized SELECTs. `graph_edges` is never queried. Missing `page_rank` rows become score `0`.
- **Writes:** nothing in PostgreSQL. Generated `public/` assets are read from disk; they are build output and are gitignored.
- **Absent:** no auth, sessions, result cache, `/metrics`, or analytics. The Images and Graph tabs are placeholders and "I'm Feeling Lucky" returns a fixed string.

See [Engine architecture](../services/engine/architecture.md) and [HTTP interface](../services/engine/api.md).

## Data ownership matrix

| Table / artifact | Spider | Indexer | Ranking | Engine |
|---|---|---|---|---|
| `urls`, `pages.html`, `pages.metadata` | write | — | — | read |
| `graph_edges` | write | — | read | — |
| `pages` claim/completion fields | — | write | read `indexed` | — |
| `words` (word text) | — | write | — | read |
| `words.idf` | — | — | write | read |
| `page_word` | — | write | read (no `tf`) | read (`tf` + `idf`) |
| `page_rank` | — | — | write | read via `LEFT JOIN` |
| Redis frontier | write | — | — | — |
| RabbitMQ queues | publish jobs | consume jobs, publish confirmations | consume confirmations | — |

!!! warning "Verified cross-service caveat X-01 — re-crawl gap"
    `services/spider/internal/store/db.go:109-111` upserts `pages` with `ON CONFLICT (url_id) DO UPDATE SET metadata = EXCLUDED.metadata`; it does not update `html` or reset `indexed`/`indexed_at`. The Indexer claim at `services/indexer/src/core/psql.rs:150` requires `indexed = FALSE`, so a job for an already-indexed page returns `NotFoundError` and is dropped instead of re-indexing changed content. Both sides were verified in source; no fix is present.

!!! warning "Verified cross-service caveat X-02 — differing corpus definitions"
    IDF uses `GREATEST(COUNT(*), 1) FROM pages` — **all** pages (`services/ranking/src/calculator.py:12-15`). PageRank counts only `pages WHERE indexed = TRUE` (`migration/02_pg_rank.sql:14-18`), and `indexed = TRUE` means *claimed*, not necessarily complete. The Engine applies no `indexed` filter in either SELECT (`services/engine/internal/store/store.go:83-114`, `:209-214`). One blended score therefore draws on three different populations.
