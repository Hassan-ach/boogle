# Boogle - Overall Architecture (Detailed)

## Overview

Boogle is a polyglot microservices search engine implementing a classic crawl-index-rank-search pipeline with event-driven communication via RabbitMQ. All services share a PostgreSQL database for persistent state and Redis where used by individual services.

This document is the top-level overview. Each service section is a summary; the per-service documents are authoritative for internals.

## Contents

1. [High-Level Pipeline](#high-level-pipeline)
2. [Services](#services)
3. [Data Model (Core Tables)](#data-model-core-tables)
4. [Messaging (RabbitMQ)](#messaging-rabbitmq)
5. [Infrastructure](#infrastructure)
6. [Configuration](#configuration)
7. [Design Notes](#design-notes)

## High-Level Pipeline

```
Web → Spider (Go) → RabbitMQ (indexer.jobs) → Indexer (Rust) → RabbitMQ (indexer.confirmations) → Ranking (Python) → PostgreSQL (page_rank, words.idf) → Engine (Go) → User
```

## Services

### Spider (Go)
- **Purpose**: Web crawling, URL discovery, link extraction, frontier management
- **Detail**: see [SPIDER.md](./SPIDER.md)
- **Entry**: `services/spider/cmd/spider/main.go`
- **Key components**:
  - Policy engine (`internal/policy/`) for admission control, backoff, host/rate limiting, robots enforcement
  - Parser (`internal/parser/`) for HTML, sitemap, robots.txt
  - Store (`internal/store/`) for Postgres/Redis persistence
  - Messaging (`internal/messaging/`) publishes to `indexer.jobs`
- **Behavior**: Fetches pages, normalizes/canonicalizes URLs, extracts links, filters by language/rules/robots, persists `urls`, `pages`, `graph_edges`, enqueues discovered URLs, publishes `Job{page_id}` to RabbitMQ after successful persist.
- **State**: Frontier/host state managed via policy with Redis-backed caching.

### Indexer (Rust)
- **Purpose**: HTML parsing, tokenization, term frequency calculation, search index building
- **Detail**: see [INDEXER.md](./INDEXER.md)
- **Entry**: `services/indexer/src/main.rs`
- **Key components**:
  - `core/indexer.rs`: Consumer loop, concurrency control (semaphore), task tracking, sweep reconciliation
  - `core/text_sink.rs`: html5ever-based text extraction (skips script/style), tokenization (alphabetic, lowercase, punctuation trimmed)
  - `core/psql.rs`: Atomic page claiming (`FOR UPDATE SKIP LOCKED`), batched word upserts, page_word linking, stale claim recovery
  - `core/messaging.rs`: RabbitMQ consumption/publishing with confirms
- **Behavior**: Consumes from `indexer.jobs`, claims unindexed page (sets `indexed=TRUE`), parses HTML in blocking task, batches words into `words` and `page_word` (TF), publishes `IndexConfirmation{page_id}` to `indexer.confirmations`, acks on success; on failure undoes claim/increments attempts and nacks per requeue policy.
- **Resilience**: Sweep loop reclaims stale claims, republishes unindexed pages (with attempt limits), poison messages not requeued.

### Ranking (Python)
- **Purpose**: Compute relevance/importance scores (TF-IDF, PageRank)
- **Detail**: see [RANKING.md](./RANKING.md)
- **Entry**: `services/ranking/src/main.py`
- **Key components**:
  - `messaging.py`: FastStream consumer of confirmations, threshold-based triggering (default 100)
  - `calculator.py`: IDF computation via SQL CTEs, PageRank via `update_page_rank()` PL/pgSQL function
  - `psql.py`: Async connection pool with retries
- **Behavior**: Counts confirmations; when threshold reached and idle, runs IDF and PageRank concurrently over full corpus/graph, writes `words.idf` and `page_rank` scores, resets counter (catch-up if confirmations arrived during run).

### Engine (Go)
- **Purpose**: Web search interface
- **Detail**: see [ENGINE.md](./ENGINE.md)
- **Entry**: `services/engine/cmd/server/main.go`
- **Key components**:
  - Handlers for search/home/health
  - Store querying Postgres with joins across `words`, `page_word`, `pages`, `page_rank`, `urls`
  - Ranking service blends TF-IDF (cosine similarity over result window) with PageRank
  - Spellchecker (aspell with fallback)
  - Templ templates + HTMX
- **Behavior**: Accepts query, gets suggestions, fetches matching pages (coarse SQL ranking by distinct word count + PageRank), re-ranks with TF-IDF cosine similarity blended with PageRank, renders results.

## Data Model (Core Tables)

From `migration/01_schema.sql`:
- `urls(id UUID PK, url TEXT UNIQUE)`
- `pages(id UUID PK, url_id UUID UNIQUE FK->urls, html TEXT, metadata JSONB, indexed BOOLEAN, created_at, updated_at, last_index_attempt_at, index_attempts)`
- `words(id UUID PK, word VARCHAR(25) UNIQUE, idf DOUBLE PRECISION, doc_frequency INTEGER)`
- `page_word(page_id UUID FK->pages, word_id UUID FK->words, tf INTEGER, PK(page_id,word_id))`
- `graph_edges(id BIGSERIAL PK, from_url UUID FK->urls, to_url UUID FK->urls, UNIQUE(from_url,to_url))`
- `page_rank(url_id UUID PK FK->urls, score DOUBLE PRECISION, updated_at)`
- `monitor_state(key TEXT PK, value BIGINT, updated_at)` — unused after the monitoring service was removed; nothing reads or writes it, so it is safe to drop

PageRank function in `migration/02_pg_rank.sql` computes scores over indexed pages only using restricted edge set.

## Messaging (RabbitMQ)

- **Exchange/Queues** (typical setup): 
  - `indexer.jobs` - Spider publishes page jobs to Indexer
  - `indexer.confirmations` - Indexer publishes confirmations to Ranking
- Messages are JSON serialized (`Job{page_id}`, `IndexConfirmation{page_id}`).
- Indexer uses publisher confirms and prefetch for flow control.

## Infrastructure

- **PostgreSQL**: Primary data store
- **RabbitMQ**: Message broker (with management UI)
- **Redis**: Used by Spider for caching/state
- **Docker Compose**: Orchestrates infra (`psql`, `redis`, `rabbitmq`, `adminer`)

## Configuration

All services configured via environment variables (see root `.env.example` and per-service `.env.example` files).

## Design Notes

- Event-driven pipeline decouples services and enables independent scaling
- Atomic claiming with `SKIP LOCKED` prevents double-processing
- Sweep/reconciliation provides crash recovery
- Poison message handling prevents infinite retry loops
- Full corpus/graph recomputation in Ranking (simple, learning-oriented)
- Coarse SQL ranking + fine-grained re-ranking in Engine balances performance and flexibility
