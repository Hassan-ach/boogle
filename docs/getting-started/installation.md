# Installation

**Last updated:** 2026-11-23

This page walks through a clean local setup: clone the repository, bootstrap
environment files, replace placeholder secrets, start the datastores, apply
migrations, then build and start the four services. Every command below is
taken from the root `justfile` or `scripts/`; none are invented.

!!! note "Verification basis"
    Commands and side effects were verified by reading the root `justfile`,
    `.env.example`, `scripts/docker-env.sh`, `scripts/migrate.sh`, and the five
    Compose files. They were not executed during this documentation pass.

## 1. Clone the repository

```bash
git clone https://github.com/Hassan-ach/boogle.git
cd boogle
```

The URL is the `repo_url` configured in `mkdocs.yml`.

## 2. Bootstrap the environment files

```bash
just env
```

This recipe:

1. Copies `.env.example` to `.env` if `.env` is missing.
2. Copies each `services/<svc>/.env.example` to `services/<svc>/.env` for
   `engine`, `spider`, `indexer` and `ranking` if that file is missing.
3. Runs `./scripts/docker-env.sh`, which generates `services/<svc>/.env.docker`
   overlays from the root `.env`.

Existing files are **never overwritten** — they hold whatever credentials an
operator already set.

!!! warning "`just env` needs `python3`"
    `scripts/docker-env.sh` uses `python3` for URL encoding and hard-requires
    `PG_USER`, `PG_PASSWORD`, `PG_DBNAME`, `RABBITMQ_DEFAULT_USER` and
    `RABBITMQ_DEFAULT_PASS`. All five are present in the tracked example, so
    bootstrap works with placeholders; `REDIS_PASSWORD` may be empty. See
    [Prerequisites](prerequisites.md).

## 3. Replace placeholder secrets

Edit the root `.env` first. Replace every `change-me-*` value with a real
secret before any non-local use:

| Variable | Placeholder in tracked example | Used for |
|---|---|---|
| `PG_PASSWORD` | `change-me-postgres` | Postgres container and every service DSN |
| `REDIS_PASSWORD` | `change-me-redis` | Redis `--requirepass` and the spider client |
| `RABBITMQ_DEFAULT_PASS` | `change-me-rabbitmq` | Broker container and generated AMQP URLs |

Then regenerate the overlays so the containers see the new values:

```bash
just env
```

It is safe to re-run: the existing `.env` files are kept and only the generated
`.env.docker` files are refreshed.

!!! warning "Overlay precedence matters"
    Each service Compose file loads `.env` and then `.env.docker`. Generated
    keys therefore override the same key in the service `.env`. Shared
    credentials and the container hostnames belong in the **root** `.env`;
    queue names and service-only tuning belong in the per-service `.env`. See
    [Configuration](configuration.md) and
    [Generated overlays](../operations/docker.md#generated-environment-overlays).

Keep queue names consistent across the producer and consumers:

- `RABBITMQ_QUEUE=indexer.jobs` — spider publishes, indexer consumes.
- `RABBITMQ_CONFIRMATION_QUEUE=indexer.confirmations` — indexer publishes,
  ranking consumes.

!!! note "Default database name"
    The tracked example sets `PG_DBNAME=se`. Older docs that refer to a
    `boogle` database are stale; the test helper uses a separate `boogle_test`
    database.

## 4. Start the datastores

```bash
just infra-up
```

This starts `psql`, `redis`, `rabbitmq` and `adminer`, then waits until Postgres
answers `pg_isready` and RabbitMQ answers `rabbitmq-diagnostics ping`.

!!! warning "Redis is not waited on"
    There is no readiness loop for Redis and no Compose `depends_on` between
    services and datastores. Container start order is not a readiness gate.

## 5. Apply the migrations (optional when using `just run`)

```bash
./scripts/migrate.sh
```

The script requires a running `psql` container and `PG_USER` / `PG_DBNAME` from
the root `.env`. It applies `migration/*.sql` in glob order with
`ON_ERROR_STOP=1`. Re-running it is safe — the SQL is idempotent, and Postgres
only runs the bind-mounted init scripts on an empty data directory.

The equivalent task is `just migrate`.

## 6. Build and start all four services

```bash
just run
```

`just run` has the prerequisites `env preflight infra-up migrate`, so steps 2,
4 and 5 happen again automatically. The `preflight` recipe aborts if another
process already owns host port `1323`. Services then start in this order:
**engine → spider → indexer → ranking**.

Expected output includes:

```text
boogle is up:
  engine  http://localhost:1323
  adminer http://127.0.0.1:8080
  broker  http://127.0.0.1:15672
```

## 7. Verify and observe

Open <http://localhost:1323> and follow the service logs:

```bash
just run-logs
```

Ctrl-C stops only the log streams, not the containers. To tear the stack down
while keeping the `pgdata` volume:

```bash
just stop
```

`just infra-down` stops only the root datastore stack.

## Alternative: step-by-step startup

If you prefer explicit control, the equivalent sequence is:

```bash
just env
# edit .env, then:
just env
just infra-up
./scripts/migrate.sh
docker compose -f services/engine/docker-compose.yml up -d --build
docker compose -f services/spider/docker-compose.yml up -d --build
docker compose -f services/indexer/docker-compose.yml up -d --build
docker compose -f services/ranking/docker-compose.yml up -d --build
```

These are the exact `up -d --build` commands from the `run` recipe. Unlike
`just run`, this sequence skips the port preflight and the readiness loops are
only those inside `infra-up`.

## Next

- [Quick start](quick-start.md) — the shortest path to a running UI.
- [Commands reference](../reference/commands.md) — all root and service tasks.
- [Docker Compose layout](../operations/docker.md) — networks, volumes and
  overlays.
- [Troubleshooting](../operations/troubleshooting.md) — common startup
  failures.
