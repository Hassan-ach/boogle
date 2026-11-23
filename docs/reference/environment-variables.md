# Environment variables

**Last updated:** 2026-11-23

This is the exhaustive variable matrix for the Boogle stack, derived from the
tracked `.env.example` files, `scripts/docker-env.sh`, the service entrypoints
and application source.

!!! warning "Scope and secret handling"
    Example values below are **tracked `.env.example` placeholders only**.
    Values from working-copy `.env` and `.env.docker` files are intentionally
    not reproduced. "Secret" marks a credential or a URL containing one;
    rotate every placeholder before any non-local use.

!!! note "Verification basis"
    Consumers and defaults were verified by static inspection. No live
    database was queried and no runtime probe was run during this
    documentation pass. Names listed as "no read found" are explicitly
    **unverified as functional settings**.

Legend: **Secret** = credential-bearing. "Overlay" = value forced by a
generated `services/<svc>/.env.docker` file.

## PostgreSQL and shared database URLs

| Variable | Example / default | Consumed by | Secret? |
|---|---|---|---|
| `PG_HOST` | `psql`; overlay forces `psql`; code default `localhost` | engine, spider, ranking | no |
| `PG_PORT` | `5432`; root Compose publish | root Compose, engine, spider, ranking | no |
| `PG_USER` | `admin` | root Compose, overlay generator, engine, spider, ranking | no (identifier) |
| `PG_PASSWORD` | `change-me-postgres` | root Compose, overlay generator, engine, spider, ranking | **yes** |
| `PG_DBNAME` | `se` | root Compose, engine, spider, ranking, `migrate.sh` | no |
| `PG_SSLMODE` | `disable` (engine example only; code default `disable`) | engine | no |
| `PG_MAX_OPEN_CONNS` | `20` | engine, spider | no |
| `PG_MAX_IDLE_CONNS` | `20` | engine, spider | no |
| `PG_MAX_CONN_LIFETIME` | `0` seconds (`0` = unlimited) | spider | no |
| `PG_BATCH_SIZE` | `30` | spider | no |
| `DATABASE_URL` | `postgres://admin:change-me-postgres@psql:5432/se`; generated form `postgres://<urlenc user>:<urlenc pw>@psql:5432/<db>` | indexer (required), ranking (overrides `PG_*`), integration tests | **yes** |
| `PG_MAX_CONNECTIONS` | `10`; also sets task concurrency and AMQP prefetch | indexer | no |
| `PG_MIN_CONNECTIONS` | `2` | indexer | no |
| `ACQUIRE_TIMEOUT_SECONDS` | `5` | indexer | no |
| `PG_LOCK_TIMEOUT_MS` | `5000` | indexer (indexer example and entrypoint only) | no |
| `PG_STATEMENT_TIMEOUT_MS` | `60000` | indexer (indexer example and entrypoint only) | no |

Migrations are applied by `./scripts/migrate.sh`, which requires `PG_USER` and
`PG_DBNAME` from the root `.env` and a running `psql` container. See
[Migrations](../data/migrations.md).

## Redis

| Variable | Example / default | Consumed by | Secret? |
|---|---|---|---|
| `REDIS_ADDR` | `redis`; overlay forces `redis`; code default `localhost` | spider | no |
| `REDIS_PORT` | `6379`; root Compose publish | root Compose, spider | no |
| `REDIS_PASSWORD` | `change-me-redis`; overlay may be empty | root Compose (`--requirepass`), spider | **yes** |
| `REDIS_DB` | `1` | spider | no |
| `REDIS_DELAY` | `5` | exported by spider entrypoint only; **no code read found** | no |
| `REDIS_MAX_RETRY` | `10` | exported by spider entrypoint only; **no code read found** | no |

There is no Redis volume or `appendonly` configuration, so frontier state is
lost on a Redis restart. See [Docker Compose layout](../operations/docker.md).

## RabbitMQ

| Variable | Example / default | Consumed by | Secret? |
|---|---|---|---|
| `RABBITMQ_DEFAULT_USER` | `admin` | root Compose, overlay generator (required), ranking fallback | no (identifier) |
| `RABBITMQ_DEFAULT_PASS` | `change-me-rabbitmq` | root Compose, overlay generator (required), ranking fallback | **yes** |
| `RABBITMQ_PROTOCOL` | `amqp` | spider reads it, but its URI builder hard-codes `amqp://` — **not honored** | no |
| `RABBITMQ_HOST` | `rabbitmq`; overlay forces `rabbitmq` | spider, ranking fallback (`localhost`) | no |
| `RABBITMQ_PORT` | `5672`; root Compose publish is fixed | spider, ranking fallback (`5672`) | no |
| `RABBITMQ_USER` | `admin`; spider code default `guest` | spider, ranking, generated overlay | no (identifier) |
| `RABBITMQ_PASSWORD` | `change-me-rabbitmq`; spider code default `guest` | spider, ranking, generated overlay | **yes** |
| `RABBITMQ_URL` | `amqp://admin:change-me-rabbitmq@rabbitmq:5672`; generated URL-encoded form | indexer (required), ranking | **yes** |
| `BROKER_URL` | `amqp://admin:change-me-rabbitmq@rabbitmq:5672` | ranking (either URL is enough) | **yes** |
| `RABBITMQ_QUEUE` | `indexer.jobs` | spider (preferred over `RABBITMQ_JOB_QUEUE`), indexer | no |
| `RABBITMQ_JOB_QUEUE` | not present in any example; legacy alias, default `indexer.jobs` | spider | no |
| `RABBITMQ_CONFIRMATION_QUEUE` | `indexer.confirmations` | indexer, ranking (entrypoint supplies the default) | no |

`docker-env.sh` bridges the root names (`RABBITMQ_DEFAULT_USER` /
`RABBITMQ_DEFAULT_PASS`) to the per-service names (`RABBITMQ_USER` /
`RABBITMQ_PASSWORD`) and generates the URL forms for indexer and ranking.
Both queue names must match across producer and consumer; see
[Message contracts](../data/message-contracts.md).

## Spider tuning and logging

| Variable | Example / default | Consumed by |
|---|---|---|
| `MAX_CRAWLERS` | `20` | spider config, entrypoint |
| `MAX_CONCURRENT_FETCH` | `200` | spider config, entrypoint |
| `HTTP_TIMEOUT` | `60` seconds | spider config, entrypoint |
| `CRAWLER_TIMEOUT` | `60` seconds | spider config, entrypoint |
| `CRAWLER_DELAY` | `200` milliseconds | spider config, entrypoint |
| `LOGS_PATH` | root `./logs/spider.log`; service `./logs.json`; overlay `/app/logs/logs.json` | spider config, entrypoint |
| `MAX_PAGES_PER_HOST` | `5000` (spider example only) | spider config |
| `URL_MAX_ATTEMPTS` | `3` (spider example only) | spider config |
| `MAX_REDIRECTS` | `10` (spider example only) | spider config |
| `MAX_BODY_BYTES` | `10485760` (10 MiB; spider example only) | spider config |
| `FRONTIER_POP_BATCH` | `16` (spider example only) | spider config |
| `DELAYED_PROMOTE_BATCH` | `100` (spider example only) | spider config |
| `SPIDER_REDIS_PREFIX` | `boogle:spider` (spider example only) | spider config |

The spider writes a JSON log at `/app/logs/logs.json` inside the container;
that path is not bind-mounted to the host. See
[Logging](../operations/logging.md).

## Indexer tuning and logging

| Variable | Example / default | Consumed by |
|---|---|---|
| `WORD_BATCH_SIZE` | `1000` (must be ≥ 1) | indexer config, entrypoint |
| `PAGE_WORD_BATCH_SIZE` | `500` (must be ≥ 1) | indexer config, entrypoint |
| `MAX_RETRIES` | `3` | indexer config, entrypoint |
| `SWEEP_INTERVAL_SECONDS` | `300` | indexer config, entrypoint |
| `SWEEP_GRACE_SECONDS` | `600` | indexer config, entrypoint |
| `SWEEP_BATCH_SIZE` | `500` | indexer config |
| `MAX_INDEX_ATTEMPTS` | `3` | indexer config |
| `LOG_PATH` | `./logs/indexer.log` | indexer config, entrypoint; also an optional ranking file sink |

!!! note "Comment syntax in the indexer example"
    Its `dotenv` loader strips an inline comment only when whitespace precedes
    `#`. Keep a space before comments or numeric parsing can panic at startup.

## Engine and ranking tuning

| Variable | Example / default | Consumed by |
|---|---|---|
| `PAGE_SIZE` | `20` | engine config, entrypoint |
| `RANKER_MAX_RESULTS` | `100` | engine config, entrypoint |
| `RANKER_WEIGHT_TF` | `0.5` | engine config, entrypoint |
| `RANKING_CONFIRMATION_THRESHOLD` | `100` (must be an integer ≥ 1) | ranking |
| `RANKING_RUN_ON_STARTUP` | `true` (false-y: `0`, `false`, `no`) | ranking |
| `DEBUG` | `false` (engine entrypoint only); **no code read found** | none verified |
| `LOG_LEVEL` | `info` (engine entrypoint only); **no code read found** | none verified |

!!! warning "Unimplemented ranking settings"
    `RANKING_DAMPING_FACTOR`, `RANKING_ITERATIONS` and
    `RANKING_SCHEDULE_INTERVAL` are not read anywhere in
    `services/ranking/src/`. Iterations and damping are fixed function
    defaults (20 and `0.85`), and no wall-clock scheduler exists. These names
    are therefore **unverified as functional settings** and must not be
    presented as working controls. See
    [Known issues](known-issues.md).

## Generated overlays

`scripts/docker-env.sh` writes these untracked files from the root `.env`:

| File | Keys generated |
|---|---|
| `services/engine/.env.docker` | `PG_HOST`, `PG_PORT`, `PG_USER`, `PG_PASSWORD`, `PG_DBNAME` |
| `services/spider/.env.docker` | the five PG keys, plus `REDIS_ADDR`, `REDIS_PORT`, `REDIS_PASSWORD`, `RABBITMQ_HOST`, `RABBITMQ_PORT`, `RABBITMQ_USER`, `RABBITMQ_PASSWORD`, `LOGS_PATH=/app/logs/logs.json` |
| `services/indexer/.env.docker` | `DATABASE_URL`, `RABBITMQ_URL` |
| `services/ranking/.env.docker` | the five PG keys, plus `BROKER_URL`, `RABBITMQ_URL` |

Compose loads `.env` then `.env.docker`, so overlapping keys come from the
overlay. Queue names, sweep tuning and the confirmation threshold are **not**
generated; they continue to come from each service's `.env`.

## Test-only variables

| Variable | Shape / default | Where used |
|---|---|---|
| `TEST_DATABASE_URL` | `postgres://<user>:<pw>@localhost:<port>/boogle_test?sslmode=disable` (secret) | `justfile` integration recipe, engine and spider integration tests |
| `BOOGLE_TEST_DB` | `boogle_test` | `scripts/setup-test-db.sh` |
| `BOOGLE_TEST_PGHOST` | host override for host-run `psql` | `scripts/setup-test-db.sh` |

The test helper drops and recreates `boogle_test` without a confirmation
prompt and falls back to `PGPASSWORD=1234`. Never point it at the live
database. See [Testing](../development/testing.md).

## Names reported but not verified as functional

The following names were reported from an **untracked working-copy `.env`**
and are listed here only as **unverified**: `LOOP_DELAY_MS`, `INDEXER_COUNT`,
`MAX_GO_ROUTINES`, `BACK_LINKS_RUN_ON_STARTUP`, `PR_SCHEDULE`, `IDF_SCHEDULE`
and a commented `LOG_LEVEL`. A grep of current application source found no
reads for them. No values are reproduced. Do not rely on these settings until
a consumer is confirmed.

## Related

- [Configuration](../getting-started/configuration.md) — practical grouping
  and precedence.
- [Commands](commands.md) — recipes that create and consume these files.
- [Dependencies](dependencies.md) — tool versions behind the manifests.
- [Known issues](known-issues.md) — drift and unimplemented settings.
