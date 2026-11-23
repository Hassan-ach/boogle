# Environment setup

There are two distinct development modes in this repository:

- **Container dev loop** — `just run` builds four images from their Dockerfiles
  and starts the shared datastores. Only Docker, Compose, `just`, and Git are
  needed on the host.
- **Local service loop** — `just test`, service `just dev` recipes, and running
  binaries directly. This requires the full toolchain for every service you
  touch, plus reachable datastores for any service that needs them.

The Dockerfiles are authoritative for container builds; the service justfiles
and `go.mod` / `Cargo.toml` / `requirements.txt` are authoritative for local
development.

## Toolchain

Versions below are the ones declared in config as of 2026-11-23. No repository
file pins `just` or a local Rust toolchain version.

| Tool | Version / source | Needed for |
|---|---|---|
| `just` | unpinned; used by root and all four service justfiles | every workflow |
| Docker + Compose V2 | Compose ≥ 2.24 for the long `env_file: {path, required}` syntax | container loop, `just env`, migrations |
| Go | `1.27.1` (`services/engine/go.mod`), `1.27` (`services/spider/go.mod`) | engine, spider |
| `templ` | `v0.3.1020` in engine CI/Dockerfile; spider Dockerfile installs `v0.3.943` | engine templates; spider image only |
| Node + npm | Node 22 in `services/engine/Dockerfile`; Tailwind 4 / esbuild / htmx / Alpine in `package.json` | engine assets |
| C compiler + aspell headers | `libaspell-dev`, `aspell-en` in engine CI/Dockerfile | engine build (cgo) |
| Rust + Cargo | edition 2024 (`services/indexer/Cargo.toml`), musl target for the image | indexer |
| Python | 3.12 in Docker/CI; ≥ 3.11 required by `asyncio.TaskGroup` | ranking, docs scripts |
| `python3` | used by `scripts/docker-env.sh` for URL quoting | `just env` |
| host `psql` client | `scripts/setup-test-db.sh` runs `psql` outside Docker | integration tests |
| `mkdocs`, `mkdocs-material` | pinned in `docs/requirements.txt` | docs site (see [Documentation](documentation.md)) |

!!! note "The README requirement list is incomplete"
    The root README says only Docker/Compose V2 and Git are required. That is
    true for the container loop, but `just test` needs Go, Rust and a Python
    venv with requirements installed; `just env` needs `python3`; and
    `just test-db` needs a host `psql` client.

## Bootstrap environment files

```bash
just env
```

`just env` (root `justfile`) copies a missing `.env` from `.env.example` and
missing `services/<svc>/.env` files from each service example, then runs
`scripts/docker-env.sh` to generate `services/<svc>/.env.docker` overlays.
Existing `.env` files are never overwritten.

`scripts/docker-env.sh` hard-requires `PG_USER`, `PG_PASSWORD`, `PG_DBNAME`,
`RABBITMQ_DEFAULT_USER`, and `RABBITMQ_DEFAULT_PASS` in the root `.env`.
It rewrites hostnames to Compose service names (`psql`, `redis`, `rabbitmq`)
and builds URL-encoded `DATABASE_URL` / `RABBITMQ_URL` / `BROKER_URL` values.
Compose loads each service `.env` first and the `.env.docker` overlay second,
so overlapping keys use container-network values.

!!! warning "Secrets"
    Replace every `change-me-*` placeholder before deployment. `.env` and
    `.env.docker` are gitignored; do not commit them.

## Container loop

```bash
just run        # env -> preflight -> infra-up -> migrate -> build/start 4 services
just run-logs   # tail all four containers; Ctrl-C stops only the streams
just stop       # down services and root stack; keeps the pgdata volume
just infra-up   # datastores only, with Postgres/RabbitMQ wait loops
just infra-down # root stack down
just migrate    # apply migration/*.sql to the running psql container
```

`just run` starts services in the order **engine, spider, indexer, ranking**.
`preflight` aborts when port 1323 is already owned by a local process.
The root stack publishes Postgres, Redis, RabbitMQ, and Adminer on
`127.0.0.1` only; the engine publishes `1323:1323` on all host interfaces.

URLs printed by `just run`:

| UI | URL |
|---|---|
| Engine | <http://localhost:1323> |
| Adminer | <http://127.0.0.1:8080> |
| RabbitMQ management | <http://127.0.0.1:15672> |

Per-service Compose files attach to the external `search_net` network created
by the root Compose file; they have no `depends_on` for the datastores. The
root wait loops cover Postgres and RabbitMQ only, so container start order is
not a readiness guarantee. See
[Known issues](../reference/known-issues.md).

Container builds happen inside each Dockerfile:

- **engine** — Node stage runs `npm ci`, Tailwind and esbuild; Go stage installs
  templ and builds `./cmd/server` with cgo + aspell. The final image copies the
  built `public/` assets and binary.
- **spider** — Go builder with templ installed and `just build` (version
  ldflags).
- **indexer** — Rust musl build with `SQLX_OFFLINE=true` (query metadata in
  `services/indexer/.sqlx/`) and `-C target-cpu=x86-64-v2` for portability.
- **ranking** — Python 3.12 builder installs `requirements.txt`, then
  PyInstaller produces a one-file executable.

## Local service loop

The Go services read configuration from the **process environment**; they do
not load `.env` files. Export values in your shell or use the Compose overlays.
The indexer and ranking service load `.env` from the working directory, with a
warning if it is missing (ranking) or an explicit `dotenv` call (indexer).

| Service | Setup and run | Notes |
|---|---|---|
| engine | `cd services/engine && npm ci && just templ-generate && just tailwind-build && just js-build`, then `go run ./cmd/server` | `just dev` runs the same generation steps plus `air`. Generated `*_templ.go` and `public/` are gitignored. Requires local Postgres + aspell. `just cli "query"` probes ranking without HTTP. |
| spider | `cd services/spider && go mod tidy && just dev` | Needs reachable Postgres, Redis, and RabbitMQ. `just build` adds version ldflags. |
| indexer | `cd services/indexer && just dev` | `DATABASE_URL` and `RABBITMQ_URL` are required. `just build` uses `-C target-cpu=native`, unlike the image. CI runs `SQLX_OFFLINE=true cargo test`. |
| ranking | `cd services/ranking && just install && just run` | `just install` creates `venv/` and installs `requirements.txt`; root recipes call `venv/bin/python` directly. |

!!! tip "Engine env files"
    `services/engine/.env.example` contains inline ` # comment` text. Shell
    sourcing strips the comment; passing the raw line to Go's integer/float
    parsers silently falls back to defaults. Keep clean `KEY=value` lines when
    exporting manually.

## Test database

Integration tests use a dedicated database, never the live `boogle` database:

```bash
just infra-up   # start Postgres (and the rest of the stack) if not running
just test-db    # DROP/CREATE boogle_test and apply migration/*.sql
just test-integration
```

`just test-db` calls `scripts/setup-test-db.sh`, which uses a **host** `psql`
client (not `docker exec`), defaults to `localhost:5432` / `admin` / `1234`,
and reads `PG_*` values from the root `.env` when present. `BOOGLE_TEST_DB`
overrides the database name and `BOOGLE_TEST_PGHOST` overrides the host.
`just test-integration` exports `TEST_DATABASE_URL` pointing at
`boogle_test`; see [Testing](testing.md) for the per-suite commands and caveats.

## Next steps

- [Project structure](project-structure.md) — where source of truth lives
- [Testing](testing.md) — fast, coverage, and integration tiers
- [Commands reference](../reference/commands.md) — full recipe inventory
