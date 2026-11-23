# Configuration

Configuration comes from environment variables, optionally seeded by a `.env` file. All loading and sanitization logic is in `services/indexer/src/core/config.rs`; Docker defaults are applied by `services/indexer/entrypoint.sh`.

## Loading behavior

1. `load_config(".env")` calls `dotenv::from_path`. The path is relative to the **process working directory**, not the binary location. A missing/unreadable file prints `Could not load .env file: ...` to stderr and startup continues with process environment.
2. `load_app_config`, `load_psql_config`, and `load_rabbit_config` read the variables below.
3. `sanitize()` corrects a few zero/negative values that would otherwise panic or wedge a worker.

`DATABASE_URL` and `RABBITMQ_URL` are required: `env::var(...).expect(...)` panics at startup if absent.

!!! warning "Two classes of numeric parsing"
    `PG_MAX_CONNECTIONS`, `PG_MIN_CONNECTIONS`, `ACQUIRE_TIMEOUT_SECONDS`, `PG_LOCK_TIMEOUT_MS`, `PG_STATEMENT_TIMEOUT_MS`, `WORD_BATCH_SIZE`, and `PAGE_WORD_BATCH_SIZE` use `.expect(...)` and **panic on a non-numeric value**. `SWEEP_INTERVAL_SECONDS`, `SWEEP_GRACE_SECONDS`, `SWEEP_BATCH_SIZE`, `MAX_INDEX_ATTEMPTS`, and `MAX_RETRIES` fall back to their defaults when parsing fails. Sanitization runs after parsing and cannot rescue a value that already panicked.

!!! note "Inline comments in `.env`"
    As `services/indexer/.env.example` notes, dotenv only strips an inline `#` comment when whitespace precedes it. `PG_MAX_CONNECTIONS=10 # fine` works; `PG_MAX_CONNECTIONS=10#not fine` makes the value unparseable and panics startup.

## Environment variables

The "Docker entrypoint" column shows values `entrypoint.sh` exports when unset; variables marked *not set* fall back to the Rust default inside the binary.

| Variable | Rust default / behavior | Docker entrypoint | Purpose |
|---|---|---|---|
| `DATABASE_URL` | **Required**; panics `DATABASE_URL must be set` | **Required** (`:?`) | Postgres connection URL, parsed by `PgConnectOptions` |
| `RABBITMQ_URL` | **Required**; panics `RABBITMQ_URL must be set` | **Required** (`:?`) | AMQP connection URL |
| `RABBITMQ_QUEUE` | `indexer.jobs` | **Required** (`:?`) | Job queue: consume, declarations, sweep republish |
| `RABBITMQ_CONFIRMATION_QUEUE` | `indexer.confirmations` | **Required** (`:?`) | Confirmation queue: declarations, confirmed publish |
| `LOG_PATH` | `indexer.log` (relative to CWD) | `./logs/indexer.log` | JSON log file; parent directories are created |
| `PG_MAX_CONNECTIONS` | `10`; parsed as both `usize` and `u32` | `10` | Pool max connections, task semaphore, AMQP prefetch (see below) |
| `PG_MIN_CONNECTIONS` | `2` | `2` | Pool minimum connections |
| `ACQUIRE_TIMEOUT_SECONDS` | `5` | `5` | Pool connection-acquire timeout |
| `PG_LOCK_TIMEOUT_MS` | `5000` | `5000` | Startup `lock_timeout` on each connection (ms) |
| `PG_STATEMENT_TIMEOUT_MS` | `60000` | `60000` | Startup `statement_timeout` on each connection (ms) |
| `WORD_BATCH_SIZE` | `1000`; zero sanitized to `1000` | `1000` | Chunk size for word upserts |
| `PAGE_WORD_BATCH_SIZE` | `500`; zero sanitized to `500` | `500` | Chunk size for `page_word` inserts |
| `MAX_RETRIES` | `3`; invalid → `3` | `3` | Attempts for per-page DB writes and confirmation serialize/publish, with `.max(1)` at call sites |
| `SWEEP_INTERVAL_SECONDS` | `300`; invalid → `300` | `300` | Delay before the first sweep and between ticks |
| `SWEEP_GRACE_SECONDS` | `600`; invalid → `600` | `600` | Age after which an open claim is considered stale |
| `SWEEP_BATCH_SIZE` | `500`; invalid → `500` | not set | Max pages selected per sweep tick |
| `MAX_INDEX_ATTEMPTS` | `3`; invalid → `3` | not set | Page claim/sweep attempt cap |

`sanitize()` (`services/indexer/src/core/config.rs`) replaces values that would panic or hang workers:

| Input | Corrected value |
|---|---|
| `WORD_BATCH_SIZE == 0` | `1000` |
| `PAGE_WORD_BATCH_SIZE == 0` | `500` |
| Effective `max_concurrent_tasks == 0` | `10` |
| `SWEEP_BATCH_SIZE <= 0` | `500` |
| `MAX_INDEX_ATTEMPTS <= 0` | `3` |

### `PG_MAX_CONNECTIONS` is overloaded

This single value controls three independent limits (`services/indexer/src/core/config.rs`, `services/indexer/src/core/indexer.rs`, `services/indexer/src/core/messaging.rs`):

1. PostgreSQL pool `max_connections`.
2. Tokio `Semaphore` permits for concurrent indexing tasks.
3. AMQP basic QoS/prefetch, cast to `u16`.

!!! warning "Size it deliberately"
    Increasing it raises both DB load and unacked deliveries in flight. Values above 65535 do not translate linearly to prefetch because of the `u16` cast. Batch sizes and SQL timeouts are separate knobs.

## Attempt budgets

Three distinct budgets are in play; they are not interchangeable (`services/indexer/src/core/indexer.rs`, `services/indexer/src/core/utils.rs`, `services/indexer/src/core/config.rs`):

| Budget | Value | Scope |
|---|---|---|
| `MAX_ATTEMPTS` | Fixed constant `3` | Consumer `consume()` startup, sweep republish, and the default retry helpers |
| `MAX_RETRIES` | Configurable, default `3` | `batch_words()` and confirmation serialize/publish per page; call sites use `.max(1)` |
| `MAX_INDEX_ATTEMPTS` | Configurable, default `3` | SQL claim and sweep eligibility cap in `pages.index_attempts` |

`retry_async`/`retry_sync` count total attempts, not retries after the first, and sleep a fixed 500 ms between attempts.

!!! warning "Fixed retry delay"
    There is no backoff or jitter. This suits a restarted connection but not sustained overload, as the source comment in `services/indexer/src/core/utils.rs` acknowledges.

## Queues and cross-service agreement

The Indexer declares both configured queues durable (`services/indexer/src/core/messaging.rs`). Their names are read from `RABBITMQ_QUEUE` and `RABBITMQ_CONFIRMATION_QUEUE` by both the consumer/sweep and the RabbitMQ client configuration; tests assert the two config loaders agree.

- Spider publishes jobs to `indexer.jobs` by default (`services/spider/internal/messaging/type.go`).
- Ranking consumes confirmations from `indexer.confirmations` by default (`services/ranking/src/messaging.py`).

!!! note "Template drift"
    `services/indexer/.env.example` contains an outdated comment saying the Indexer never declares the confirmation queue. The current code declares both queues. Do not rely on that comment for deployment decisions.

## Logging

`init_logger()` (`services/indexer/src/core/utils.rs`) duplicates logs to colored stderr and an append-only JSON file. The JSON drain adds the fixed field `service=indexer` plus slog's default keys; it does not add `worker_id` or `page_count` fields.

- Local binary: `LOG_PATH` default `indexer.log` relative to the working directory.
- Docker: `LOG_PATH` defaults to `./logs/indexer.log` under `/app`, with `/app/logs` created and owned by the app user in the image.

## Local and Docker setup

From `services/indexer/` (commands in `services/indexer/justfile`):

```bash
cargo run                                # dev
RUSTFLAGS="-C target-cpu=native" cargo build --release
cargo test
cargo test -- --ignored --nocapture      # needs live PostgreSQL
cargo llvm-cov --summary-only || cargo test
```

Docker-specific facts:

- `services/indexer/docker-compose.yml` loads `services/indexer/.env` and then `.env.docker` (both optional); the latter is generated from the root `.env` for container-network addresses by `scripts/docker-env.sh`, invoked through `just env`.
- The image builds with `SQLX_OFFLINE=true` and `-C target-cpu=x86-64-v2`, using checked-in `.sqlx` metadata.
- The runtime image runs as UID/GID 10001 and `entrypoint.sh` ends with `exec ./indexer`.
- `HEALTHCHECK` only checks `pidof indexer`; it does not verify database or broker health.

!!! warning "Signals in containers"
    Only Ctrl-C is handled (`tokio::signal::ctrl_c()` in `services/indexer/src/main.rs`). There is no SIGTERM handler, so `docker stop` does not run the graceful drain path even though the binary is PID 1. Release builds also use `panic = "abort"` (`services/indexer/Cargo.toml`).

## Related pages

- [Overview](index.md) — responsibilities and source map
- [Architecture](architecture.md) — clients, lifecycle, failure handling, data ownership
- [Indexing pipeline](indexing-pipeline.md) — claim, parse, write, confirm, sweep
