# Indexer Service

Converts crawled HTML into searchable word index.

**Language**: Rust 2024  
**Purpose**: Extract words, calculate TF, build search index

## Quick Start

```bash
cargo build
cargo run
```

Press CTRL+C to stop gracefully.

## Features

- Consumes jobs from RabbitMQ (`indexer.jobs`)
- Concurrent workers with semaphore-based concurrency control
- HTML parsing with html5ever, skips script/style, normalizes tokens (lowercase, alphabetic only)
- Batches word upserts and page_word inserts
- Publishes confirmations to RabbitMQ (`indexer.confirmations`) on success
- Crash-safe with sweep/reconciliation for stale claims
- Retry with poison-message handling

## Project Structure

- `src/main.rs` - Entry point, task spawning, graceful shutdown
- `src/core/indexer.rs` - Indexing logic, queue consumption, sweep
- `src/core/psql.rs` - Database operations (claiming, batching, reconciliation)
- `src/core/text_sink.rs` - HTML text extraction and tokenization
- `src/core/messaging.rs` - RabbitMQ client
- `src/core/config.rs` - Configuration

## Configuration

Key environment variables (see `.env.example`):
- `DATABASE_URL`, `PG_MAX_CONNECTIONS`, pool/timeouts
- `RABBITMQ_URL`, `RABBITMQ_QUEUE` (indexer.jobs), `RABBITMQ_CONFIRMATION_QUEUE` (indexer.confirmations)
- `WORD_BATCH_SIZE`, `PAGE_WORD_BATCH_SIZE`, `MAX_RETRIES`
- `SWEEP_INTERVAL_SECONDS`, `SWEEP_GRACE_SECONDS`
- `LOG_PATH`

## Input/Output

**Reads from**:
- RabbitMQ jobs (`page_id`)
- `pages` table (claims unindexed pages)

**Writes to**:
- `words` table
- `page_word` table (with TF values)
- Publishes confirmations to RabbitMQ

## Details

See [docs/INDEXER.md](../../docs/INDEXER.md) for algorithm explanation, performance details, and troubleshooting.

## Notes

⚠️ Learning project - simple tokenization, English-only.
