# Spider configuration

**Last updated:** 2026-11-23

Configuration is environment-variable only: there is no config file and no
command-line flag path. `LoadConfig` in `services/spider/internal/config/config.go`
starts from policy defaults, applies `os.Getenv` overrides and always returns a
`nil` error. `main.go` discards that error slot, so invalid numeric values are
either defaulted silently (`getIntWithDefault`) or printed to stdout and
defaulted for second-based knobs (`secondsWithDefault`).

!!! warning "Variable names"
    The application-level variables have **no** `SPIDER_` prefix. Only
    `SPIDER_REDIS_PREFIX` and `SPIDER_USER_AGENT` carry it. Older service docs
    that list `SPIDER_MAX_CRAWLERS`, `SPIDER_HTTP_TIMEOUT` or microseconds for
    `CRAWLER_DELAY` are stale.

## Crawler and process

| Variable | Default | Unit / meaning |
|---|---|---|
| `MAX_CRAWLERS` | `20` | Worker goroutines |
| `MAX_CONCURRENT_FETCH` | `200` | Capacity of the shared `fetchpool` semaphore |
| `HTTP_TIMEOUT` | `60` | Seconds; `http.Client.Timeout` |
| `CRAWLER_TIMEOUT` | `60` | Seconds; deadline per crawl cycle |
| `CRAWLER_DELAY` | `200` | **Milliseconds**; ticker period per worker |
| `LOGS_PATH` | `./logs.json` | JSON log file; the container entrypoint defaults to `/app/logs/logs.json` |

The console logger is a simple text handler; the file handler writes JSON. If
file logging cannot be opened, the container path can be stdout-only
(`services/spider/internal/utils/logger.go`).

## PostgreSQL

| Variable | Default | Notes |
|---|---|---|
| `PG_HOST` | `localhost` | Required by `entrypoint.sh` |
| `PG_PORT` | `5432` | Required by `entrypoint.sh` |
| `PG_USER` | `admin` | Required by `entrypoint.sh` |
| `PG_PASSWORD` | `1234` | Required by `entrypoint.sh`; redacted in logs |
| `PG_DBNAME` | `se` | Required by `entrypoint.sh` |
| `PG_MAX_OPEN_CONNS` | `20` | `sql.DB` pool setting |
| `PG_MAX_IDLE_CONNS` | `20` | `sql.DB` pool setting |
| `PG_MAX_CONN_LIFETIME` | `0` | Seconds; `0` = unlimited |
| `PG_BATCH_SIZE` | `30` | Loaded into config but not referenced by the current SQL layer |

The DSN uses `sslmode=disable` (`services/spider/internal/store/db.go`).

## Redis

| Variable | Default | Notes |
|---|---|---|
| `REDIS_ADDR` | `localhost` | Required by `entrypoint.sh` |
| `REDIS_PORT` | `6379` | Required by `entrypoint.sh` |
| `REDIS_DB` | `1` | Database index |
| `REDIS_PASSWORD` | empty | Redacted in logs |
| `REDIS_MAX_RETRY` | `10` | Fallback for `FRONTIER_POP_BATCH` only |
| `REDIS_DELAY` | `5` (entrypoint) | **Not read by any Go source** |

The client uses go-redis v9 with a 5 s dial/read/write timeout and RESP2
(`services/spider/internal/store/cache.go`). The frontier, delayed set, visited
set, host state and stats live here, not in PostgreSQL.

## RabbitMQ

| Variable | Default | Notes |
|---|---|---|
| `RABBITMQ_USER` | `guest` | Required by `entrypoint.sh` |
| `RABBITMQ_PASSWORD` | `guest` | Required by `entrypoint.sh`; redacted in logs |
| `RABBITMQ_HOST` | `localhost` | Required by `entrypoint.sh` |
| `RABBITMQ_PORT` | `5672` | Used by Go; not required by `entrypoint.sh` |
| `RABBITMQ_PROTOCOL` | `amqp` | **Ignored** — see caveat below |
| `RABBITMQ_QUEUE` | — | Highest-precedence queue name |
| `RABBITMQ_JOB_QUEUE` | — | Fallback if `RABBITMQ_QUEUE` is empty |

Resolution is `RABBITMQ_QUEUE` > `RABBITMQ_JOB_QUEUE` > `indexer.jobs`. Both
sides must agree with the indexer; the default matches it. The queue is declared
durable at startup.

!!! danger "RABBITMQ_PROTOCOL is a dead knob"
    It is read into config, but `getRabbitURL` in
    `services/spider/internal/messaging/rabbitmq.go` hard-codes the `amqp://`
    scheme. Setting `amqps` does not enable TLS.

## Policy and crawl limits

Defaults come from `policy.DefaultConfig()` in `services/spider/internal/policy/policy.go`;
the environment overrides are applied in `loadPolicyConfig()`.

| Variable | Default | Meaning |
|---|---|---|
| `SPIDER_REDIS_PREFIX` | `boogle:spider` | Prefix for all Redis keys |
| `SPIDER_USER_AGENT` | `BoogleBot` | Bare tokens gain `/1.0 (+https://boogle.example/bot)` on the wire |
| `MAX_PAGES_PER_HOST` | `5000` | Page budget per rolling window |
| `URL_MAX_ATTEMPTS` | `3` | Attempts before a URL is retired |
| `MAX_REDIRECTS` | `10` | Bounded redirect chain |
| `MAX_BODY_BYTES` | `10485760` | 10 MiB response cap, in bytes |
| `FRONTIER_POP_BATCH` | `16` | Fallback: `REDIS_MAX_RETRY`, then 16 |
| `DELAYED_PROMOTE_BATCH` | `100` | Promotions due per `TakeNext` |
| `URL_STATE_TTL_SEC` | `604800` | 7 days; applied to URL hashes in `BumpAttempts` |
| `ROBOTS_TTL_SEC` | `86400` | 24 hours; cached robots rules |
| `HOST_COLD_PERIOD_SEC` | `3600` | 1 hour; cold marker and page-budget window |
| `MIN_CRAWL_DELAY_SEC` | `1` | Floor for a parsed robots `Crawl-delay` |
| `DEAD_HOST_BASE_SEC` | `60` | Base dead-host backoff |
| `DEAD_HOST_PROBE_AFTER_SEC` | `300` | Floor on retry-after for DNS, connection and TLS failures |
| `DEAD_HOST_MAX_EXP` | `6` | Max exponential shift |
| `HOST_COOLDOWN_BASE_SEC` | `10` | Base cooldown; raised to `Crawl-delay` when larger |
| `HOST_COOLDOWN_MAX_EXP` | `3` | Max exponential shift |
| `URL_BACKOFF_BASE_SEC` | `30` | Base retry delay for a URL |
| `URL_BACKOFF_MAX_SEC` | `900` | Cap on URL retry delay |

Second-based values must be positive integers; an unparseable or non-positive
value prints a warning to stdout and uses the default. Integers read via
`getIntWithDefault` fall back with no warning.

!!! note "Parsed but not used for pacing"
    `Crawl-delay` is parsed, floored at `MIN_CRAWL_DELAY_SEC` and stored, and it
    can extend a host-cooldown TTL. It does **not** pace requests; only the
    global `CRAWLER_DELAY` ticker does. See
    [Known limitations](implementation.md#known-limitations-verified-drift).

## Entrypoint requirements

`services/spider/entrypoint.sh` uses `set -eu` and requires these ten variables
to be present, even if empty:

```text
PG_HOST PG_PORT PG_USER PG_PASSWORD PG_DBNAME
REDIS_ADDR REDIS_PORT
RABBITMQ_USER RABBITMQ_PASSWORD RABBITMQ_HOST
```

It then exports defaults for pool sizes, Redis DB/password/delay/retry,
crawler sizes and timeouts, and `LOGS_PATH`, creates the log directory and
executes `./spider`. `RABBITMQ_PORT` and `RABBITMQ_PROTOCOL` are not checked by
the shell. `services/spider/.env.example` and the root `.env.example` contain
the deployment placeholders.

## Secrets

Passwords are typed as `config.Secret`, whose `String`, `GoString`, `LogValue`
and `MarshalJSON` methods return `[redacted]`. The startup dump calls
`conf.Redacted()` (`services/spider/internal/spider/spider.go`). The DSN and
AMQP URL must call `Reveal()` explicitly and must not be logged
(`services/spider/internal/store/db.go`, `internal/messaging/rabbitmq.go`).

## Local run and tests

From `services/spider/`, using the verified recipes in `services/spider/justfile`:

```bash
just dev                                   # go run ./cmd/spider/main.go
just build                                 # produces ./spider
just test                                  # go test ./internal/... -count=1
just test-cov
just test-integration                      # go test -tags integration ./internal/... -count=1
```

The seed URLs are hard-coded, so a local run crawls the public web through the
same policy gates as a deployment. Unit tests do not need infrastructure;
integration tests need live PostgreSQL, Redis and RabbitMQ.
