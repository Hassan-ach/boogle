# Configuration

**Last updated:** 2026-11-23

Boogle is configured entirely through environment variables. There is no YAML
configuration file and no CLI flag surface for service tuning.

!!! note "Verification basis"
    Variable names, defaults and consumers were checked against the tracked
    `.env.example` files, `scripts/docker-env.sh`, the service entrypoints and
    source. Values shown are **tracked placeholder values only**. The
    exhaustive matrix is in the
    [Environment variables reference](../reference/environment-variables.md).

## How the layers combine

1. `just env` copies each missing `.env.example` to `.env` (root plus four
   services), then runs `scripts/docker-env.sh`.
2. `scripts/docker-env.sh` reads the **root** `.env` and writes a generated
   `.env.docker` overlay per service. It hard-requires `PG_USER`,
   `PG_PASSWORD`, `PG_DBNAME`, `RABBITMQ_DEFAULT_USER` and
   `RABBITMQ_DEFAULT_PASS`; `REDIS_PASSWORD` may be empty.
3. Each service Compose file loads `.env` first and `.env.docker` second.
   Overlapping keys are therefore **overridden by the generated overlay**.
   Keys absent from the overlay keep the service `.env` value or the
   application default.

| Layer | Typical content |
|---|---|
| Root `.env` | Shared credentials, container hosts, most tuning values |
| Generated `services/<svc>/.env.docker` | Container-view hosts (`psql`, `redis`, `rabbitmq`) and credential URLs |
| Service `services/<svc>/.env` | Queue names, service-only tuning, log paths |
| Entrypoint / config loader | Fallback defaults |

!!! warning "Edit shared credentials in the root `.env`"
    Because overlays win, changing `PG_PASSWORD` only in `services/engine/.env`
    has no effect inside containers. Edit root `.env`, then re-run `just env`
    to regenerate the overlays.

## PostgreSQL family

| Variable | Example | Consumers |
|---|---|---|
| `PG_HOST` | `psql` | engine, spider, ranking (overlay forces `psql`) |
| `PG_PORT` | `5432` | root Compose publish, engine, spider, ranking |
| `PG_USER` | `admin` | root Compose, overlay generator, engine, spider, ranking |
| `PG_PASSWORD` | `change-me-postgres` (secret) | same set |
| `PG_DBNAME` | `se` | root Compose, all three PG clients, migration helper |
| `DATABASE_URL` | `postgres://admin:change-me-postgres@psql:5432/se` (secret) | indexer (required), ranking (overrides `PG_*`) |

Pooling and batching:

- Spider: `PG_MAX_OPEN_CONNS=20`, `PG_MAX_IDLE_CONNS=20`,
  `PG_MAX_CONN_LIFETIME=0` (seconds, `0` = unlimited), `PG_BATCH_SIZE=30`.
- Engine: `PG_MAX_OPEN_CONNS=20`, `PG_MAX_IDLE_CONNS=20`, and
  `PG_SSLMODE=disable` (engine example only; default `disable`).
- Indexer: `PG_MAX_CONNECTIONS=10` (also sets task concurrency and AMQP
  prefetch per its example), `PG_MIN_CONNECTIONS=2`,
  `ACQUIRE_TIMEOUT_SECONDS=5`, `PG_LOCK_TIMEOUT_MS=5000`,
  `PG_STATEMENT_TIMEOUT_MS=60000`.

!!! note "TLS"
    The spider DSN hard-codes `sslmode=disable`; there is no TLS field in its
    config. The engine exposes `PG_SSLMODE`, defaulting to `disable`. Keep this
    in mind when pointing services at a non-loopback database.

## Redis family (spider only)

| Variable | Example | Notes |
|---|---|---|
| `REDIS_ADDR` | `redis` | Overlay forces `redis`; code default `localhost` |
| `REDIS_PORT` | `6379` | Overlay forces `6379` |
| `REDIS_PASSWORD` | `change-me-redis` (secret) | Root Compose `--requirepass`; overlay may be empty |
| `REDIS_DB` | `1` | Spider frontier/state database |
| `REDIS_DELAY` | `5` | Exported by the entrypoint; **no code read found** |
| `REDIS_MAX_RETRY` | `10` | Exported by the entrypoint; **no code read found** |

!!! warning "Redis state is not persisted"
    There is no Redis volume or `appendonly` command in root Compose. A Redis
    restart loses the crawl frontier and host/URL state.

## RabbitMQ family

Shared broker credentials come from the root example as
`RABBITMQ_DEFAULT_USER=admin` / `RABBITMQ_DEFAULT_PASS=change-me-rabbitmq`.
`docker-env.sh` maps them to the per-service names
`RABBITMQ_USER` / `RABBITMQ_PASSWORD` and generates the URL forms
`RABBITMQ_URL` (indexer) and `BROKER_URL` (ranking).

| Variable | Example | Consumers |
|---|---|---|
| `RABBITMQ_PROTOCOL` | `amqp` | Read by spider config but **not honored** (see below) |
| `RABBITMQ_HOST` | `rabbitmq` | Spider, ranking fallback |
| `RABBITMQ_PORT` | `5672` | Spider, ranking fallback |
| `RABBITMQ_USER` / `RABBITMQ_PASSWORD` | `admin` / `change-me-rabbitmq` (secret) | Spider, ranking fallback |
| `RABBITMQ_URL` | `amqp://admin:change-me-rabbitmq@rabbitmq:5672` (secret) | Indexer (required), ranking |
| `BROKER_URL` | same shape (secret) | Ranking; either URL is enough |

Queue names must agree across producers and consumers:

| Queue | Producer | Consumer | Default |
|---|---|---|---|
| `RABBITMQ_QUEUE` | spider | indexer | `indexer.jobs` |
| `RABBITMQ_CONFIRMATION_QUEUE` | indexer | ranking | `indexer.confirmations` |

!!! warning "`RABBITMQ_PROTOCOL` has no effect"
    The spider reads it at config load, but its AMQP URI builder hard-codes
    `amqp://`. Setting a different protocol will not change the connection.

## Service tuning

| Area | Variables (tracked example values) |
|---|---|
| Spider crawl | `MAX_CRAWLERS=20`, `MAX_CONCURRENT_FETCH=200`, `HTTP_TIMEOUT=60`, `CRAWLER_TIMEOUT=60`, `CRAWLER_DELAY=200` (ms), `LOGS_PATH=./logs/spider.log` |
| Spider policy | `MAX_PAGES_PER_HOST=5000`, `URL_MAX_ATTEMPTS=3`, `MAX_REDIRECTS=10`, `MAX_BODY_BYTES=10485760`, `FRONTIER_POP_BATCH=16`, `DELAYED_PROMOTE_BATCH=100`, `SPIDER_REDIS_PREFIX=boogle:spider` (spider example only) |
| Indexer batches | `WORD_BATCH_SIZE=1000`, `PAGE_WORD_BATCH_SIZE=500`, `MAX_RETRIES=3` |
| Indexer sweep | `SWEEP_INTERVAL_SECONDS=300`, `SWEEP_GRACE_SECONDS=600`, `SWEEP_BATCH_SIZE=500`, `MAX_INDEX_ATTEMPTS=3` |
| Indexer logging | `LOG_PATH=./logs/indexer.log` |
| Engine search | `PAGE_SIZE=20`, `RANKER_MAX_RESULTS=100`, `RANKER_WEIGHT_TF=0.5` |
| Ranking | `RANKING_CONFIRMATION_THRESHOLD=100` (integer ≥ 1), `RANKING_RUN_ON_STARTUP=true`; false-y values are `0`, `false`, `no` |

!!! warning "Ranking knobs that do nothing"
    `RANKING_DAMPING_FACTOR`, `RANKING_ITERATIONS` and
    `RANKING_SCHEDULE_INTERVAL` appear in older documentation but are **not
    read by code**. PageRank uses fixed function defaults (20 iterations,
    damping `0.85`), and there is no wall-clock scheduler: recomputation is
    driven by startup and confirmation batches only. IDF and PageRank are
    committed separately. See
    [Known issues](../reference/known-issues.md).

The engine entrypoint also exports `DEBUG=false` and `LOG_LEVEL=info`, but no
engine source file reads them.

## Integration-test variables

`just test-integration` exports `TEST_DATABASE_URL` pointing at the isolated
`boogle_test` database. `scripts/setup-test-db.sh` also reads `BOOGLE_TEST_DB`
(default `boogle_test`), `BOOGLE_TEST_PGHOST` and a fallback
`PGPASSWORD=1234`. These are test-only, not application runtime variables. See
[Testing](../development/testing.md).

## Next

- [Environment variables reference](../reference/environment-variables.md) —
  the complete per-variable matrix, including test-only and unread names.
- [Installation](installation.md) — where each file is created.
- [Service configuration pages](../services/spider/configuration.md) —
  start with the [Spider](../services/spider/configuration.md), then
  [Engine](../services/engine/configuration.md),
  [Indexer](../services/indexer/configuration.md) and
  [Ranking](../services/ranking/configuration.md).
