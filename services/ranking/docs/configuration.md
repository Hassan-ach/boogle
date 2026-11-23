# Configuration

Configuration comes from environment variables. `load_dotenv()` is called both in `main()` and at the start of `RankingService.start()`, so a `.env` file in the working directory is honored. `services/ranking/.env.example` lists the implemented settings.

## Database settings

| Variable | Code default | Behavior |
|---|---|---|
| `DATABASE_URL` | none | Preferred DSN; overrides all `PG_*` values and satisfies DB validation on its own |
| `PG_HOST` | none | Required when `DATABASE_URL` is absent |
| `PG_PORT` | none | Required when `DATABASE_URL` is absent |
| `PG_DBNAME` | none | Required when `DATABASE_URL` is absent |
| `PG_USER` | none | Required when `DATABASE_URL` is absent |
| `PG_PASSWORD` | none | Required when `DATABASE_URL` is absent |

Startup validation (`services/ranking/src/main.py`) rejects missing **or empty** `PG_*` values when there is no `DATABASE_URL`. The DSN builder in `services/ranking/src/psql.py` checks only for `None` and uses `psycopg.conninfo.make_conninfo()`. The pool opens at startup with `wait=True` and retries 5 times before failing boot.

The container entrypoint also requires every `PG_*` variable (non-empty) when `DATABASE_URL` is unset.

## Broker settings

| Variable | Code default | Behavior |
|---|---|---|
| `BROKER_URL` | none | Preferred full AMQP URL; accepts any URL FastStream accepts |
| `RABBITMQ_URL` | none | Fallback full AMQP URL |
| `RABBITMQ_USER` | none | Username fallback when no URL is set; `RABBITMQ_DEFAULT_USER` is also accepted |
| `RABBITMQ_PASSWORD` | none | Password fallback when no URL is set; `RABBITMQ_DEFAULT_PASS` is also accepted |
| `RABBITMQ_HOST` | `localhost` | Used only when assembling a URL from credentials |
| `RABBITMQ_PORT` | `5672` | Used only when assembling a URL from credentials |
| `RABBITMQ_CONFIRMATION_QUEUE` | `indexer.confirmations` | Durable confirmation queue consumed by ranking |

With no URL, the service builds `amqp://<quoted-user>:<quoted-password>@<host>:<port>`, URL-quoting credentials with `quote(..., safe='')`. Broker configuration is resolved when `MessagingService` is constructed, before `start()` runs validation. The container entrypoint requires non-empty `RABBITMQ_USER` and `RABBITMQ_PASSWORD` when neither URL is set, and exports the confirmation-queue default.

!!! warning "Keep queue names aligned"
    The indexer publishes confirmations to `indexer.confirmations` by default. If `RABBITMQ_CONFIRMATION_QUEUE` is changed, it must be changed consistently for both services or ranking will never count confirmations.

## Ranking behavior

| Variable | Code default | Behavior |
|---|---|---|
| `RANKING_CONFIRMATION_THRESHOLD` | `100` | Confirmations per scoring pass; must parse as an integer and be `>= 1` |
| `RANKING_RUN_ON_STARTUP` | `true` | Enables one forced pass at startup; only `0`, `false`, or `no` (after strip/lower) disable it |
| `LOG_PATH` | none | Adds a file log sink in addition to stdout; failure to open it is non-fatal |

The startup pass uses `force=True`, so it runs even with an empty confirmation counter. It shares the job lock with threshold-triggered runs.

## Variables that are NOT implemented

| Variable | Reality |
|---|---|
| `RANKING_ITERATIONS` | Not read by code. Iteration count is the `iterations` argument of `update_pagerank()` / `run_pipeline()`, defaulting to `20` |
| `RANKING_DAMPING_FACTOR` | Not read by code. Default is `0.85` in both Python and SQL |
| `RANKING_SCHEDULE_INTERVAL` | Not read by code. No wall-clock scheduler exists |

!!! warning "Do not tune these variables"
    Setting any of the three variables above has no effect. `APScheduler`, `numpy`, `scipy`, `pytz`, and `tzlocal` appear in `services/ranking/requirements.txt` but are not imported by `services/ranking/src/`.

## Logging

`JsonFormatter` emits one JSON object per record with keys `service` (`"ranking"`), `timestamp` (`%Y-%m-%dT%H:%M:%S`), `level`, `logger`, `message`, and `error` (traceback) when an exception is present. The root logger is configured at `INFO` with a stdout handler; `LOG_PATH` adds a `FileHandler`.

Configuration is skipped if the root logger already has handlers. If opening `LOG_PATH` fails, a message is printed to stderr and logging continues to stdout only. In the container the process runs as uid `10001`, so any configured log path must be writable by that user.

## Runtime and container

| Aspect | Verified detail |
|---|---|
| Language level | Python **3.11+** required by `asyncio.TaskGroup`; builder image is `python:3.12-alpine` |
| Package | `pyinstaller --onefile src/main.py` in the builder stage; runtime image is Alpine 3.23 |
| Runtime user | `10001:10001` |
| Entrypoint | `services/ranking/entrypoint.sh` validates env, exports the confirmation-queue default, then `exec ./main` |
| Healthcheck | `pidof main` every 30s |

Documentation claiming **Python 3.9+** is inaccurate: the code cannot run on versions below 3.11.

## Commands

Verified from `services/ranking/justfile`; run from `services/ranking/`:

```bash
just install           # create venv and install requirements
just run               # venv/bin/python src/main.py
just build             # pyinstaller --onefile src/main.py
just test              # pytest -m "not integration"
just test-cov          # unit tests with --cov=src --cov-report=term-missing
just test-integration  # recreate boogle_test, then pytest -m integration
```

`just test-integration` runs `scripts/setup-test-db.sh`, which drops and recreates the isolated `boogle_test` database and applies every `migration/*.sql` file. Integration tests resolve their DSN from `BOOGLE_TEST_PG_DSN`, then `TEST_DATABASE_URL`, then `DATABASE_URL`, then `postgresql://admin:se@localhost:5432/boogle_test`, and skip when PostgreSQL is unreachable.

## Unverified claims

No benchmark or profiling evidence exists in this service's code or tests, so older documentation's ranking runtimes and NumPy/SciPy optimization advice are not reproduced here. Log examples showing per-iteration PageRank progress and combined TF-IDF scoring are likewise not implemented: logs are JSON, the PL/pgSQL loop emits no per-iteration logs, and no combined scoring step exists in this service.
