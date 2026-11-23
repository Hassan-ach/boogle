# Commands

**Last updated:** 2026-11-23

Every command below is taken from the root `justfile`, one of the four service
justfiles, or a script they call. No command has been invented for this page.

!!! note "Verification basis"
    Recipes and their bodies were verified by reading the justfiles and
    scripts. They were not executed during this documentation pass. Root
    recipes run under `/bin/sh` (dash on Debian), which is why loop variables
    use a single `$` rather than `$$`.

## Root commands

Run these from the repository root.

### Everyday stack

| Command | Effect |
|---|---|
| `just env` | Copy missing `.env.example` files to `.env`, then generate `services/<svc>/.env.docker` overlays |
| `just infra-up` | Start `psql`, `redis`, `rabbitmq`, `adminer`; wait for Postgres and RabbitMQ readiness |
| `just preflight` | Abort if port `1323` is in use by a process other than the `engine` container |
| `just migrate` | Apply `migration/*.sql` to the running Compose database via `./scripts/migrate.sh` |
| `just run` | `env preflight infra-up migrate`, then build and start engine → spider → indexer → ranking |
| `just run-logs` | Follow the last 100 lines of all four service containers; Ctrl-C stops only the streams |
| `just stop` | `down` all four service projects, then the root Compose stack; keeps `pgdata` |
| `just infra-down` | `down` the root Compose stack only |

`just run` prints the engine, Adminer and RabbitMQ URLs when it finishes. It
does not wait for Redis, and service Compose files have no `depends_on` against
the datastores. See [Installation](../getting-started/installation.md) and
[Docker Compose layout](../operations/docker.md).

### Tests

| Command | Effect |
|---|---|
| `just test` | Default target. Go unit tests for engine and spider, `cargo test` for indexer, ranking pytest excluding `integration` |
| `just test-cov` | Same fast suites with coverage output (ranking uses `--cov=src --cov-report=term-missing`) |
| `just test-db` | `bash scripts/setup-test-db.sh` — drop and recreate `boogle_test`, then apply migrations |
| `just test-integration` | Depends on `test-db`; exports `TEST_DATABASE_URL` and runs all integration suites |
| `just test-all` | `test` then `test-integration` |
| `just clean` | Remove `services/*/coverage.out` and ranking `__pycache__` directories |

!!! warning "Test database"
    Integration tests write scores. `just test-db` and `just test-integration`
    reset the isolated `boogle_test` database using a **host** `psql` client;
    they never target the live database. See [Testing](../development/testing.md).

### Documentation

| Command | Effect |
|---|---|
| `just docs-install` | `python3 scripts/docs.py install` — create `.venv-docs` and install docs dependencies |
| `just docs-build` | `python3 scripts/docs.py build` |
| `just docs-serve` | `python3 scripts/docs.py serve` |
| `just docs-service <name>` | `python3 scripts/docs.py service <name>` for one service (positional argument, e.g. `just docs-service engine`) |
| `just docs-check` | `python3 scripts/docs.py check` — strict nav/link validation |

The MkDocs `on_pre_build` hook also runs `scripts/docs.py aggregate` before
every build or serve, copying `services/*/docs/` into `docs/services/`.
See [Documentation workflow](../development/documentation.md) and
[Dependencies](dependencies.md).

## Service commands

Each service has its own justfile; run these from `services/<svc>/`, or use the
root recipes above for the container workflow.

### Engine (`services/engine/justfile`)

| Command | Effect |
|---|---|
| `just` / `just dev` | `templ generate`, build Tailwind and JS, then run `air` |
| `just templ-generate`, `just templ-watch` | Generate templates once or in watch mode |
| `just tailwind-build`, `just tailwind-watch` | Build or watch Tailwind CSS; `-prod` variants minify |
| `just js-build`, `just js-watch` | Bundle JS with esbuild; `just js-prod` minifies |
| `just build` | Generate assets and `go build -o ./bin/main ./cmd/server/main.go` |
| `just build-emb` | `CGO_ENABLED=1 GOOS=linux GOARCH=amd64` stripped build to `boogle` (used by the Docker image) |
| `just cli "<text>"` | `go run cmd/cli/main.go "<text>"` (positional argument; do not write `query=`) |
| `just tidy` | `go mod tidy` |
| `just test`, `just test-cov` | Fast unit tests under `./internal/...`, with optional coverage |
| `just test-integration` | Tests tagged `integration` against live Postgres |

### Spider (`services/spider/justfile`)

| Command | Effect |
|---|---|
| `just` / `just dev` | `go run ./cmd/spider/main.go` |
| `just build` | Build `spider` with version ldflags and `GCFLAGS="all=-B"` |
| `just run` | `build` then execute `./spider` |
| `just lint` | `golangci-lint run ./...` (tool not installed by the repo) |
| `just test`, `just test-cov` | Fast unit tests under `./internal/...`, with optional coverage |
| `just test-integration` | Tests tagged `integration` against live Postgres, Redis and RabbitMQ |

### Indexer (`services/indexer/justfile`)

| Command | Effect |
|---|---|
| `just` / `just dev` | `cargo run` |
| `just build` | `RUSTFLAGS="-C target-cpu=native" cargo build --release` (local builds only; Docker/CI use an x86-64-v2 baseline) |
| `just test` | `cargo test` |
| `just test-cov` | `cargo llvm-cov --summary-only`, falling back to `cargo test` |
| `just test-integration` | `cargo test -- --ignored --nocapture` against live Postgres |

### Ranking (`services/ranking/justfile`)

| Command | Effect |
|---|---|
| `just` / `just run` | `venv/bin/python src/main.py` |
| `just venv` | Create `venv` with `python3 -m venv` |
| `just install` | Depends on `venv`; upgrade pip and install `requirements.txt` |
| `just build` | Depends on `install`; `pyinstaller --onefile src/main.py` |
| `just test`, `just test-cov` | Pytest excluding `integration`, with optional coverage |
| `just test-integration` | Run `scripts/setup-test-db.sh`, then pytest marked `integration` |
| `just clean` | Remove `venv`, `__pycache__`, `src/__pycache__` and `dist` |

## Scripts called by the recipes

| Script | Called by | Notes |
|---|---|---|
| `scripts/docker-env.sh` | `just env` | Requires five root variables; uses `python3` for URL encoding; writes `.env.docker` overlays |
| `scripts/migrate.sh` | `just migrate`, `just run` | Requires a running `psql` container and `PG_USER` / `PG_DBNAME`; applies `migration/*.sql` with `ON_ERROR_STOP=1` |
| `scripts/setup-test-db.sh` | `just test-db`, `just test-integration` | Uses host `psql`; drops and recreates `boogle_test` (fallback password `1234`) |
| `scripts/docs.py` | docs recipes and the MkDocs hook | Subcommands: `aggregate`, `build`, `serve`, `service`, `check`, `install` |

## Related

- [Quick start](../getting-started/quick-start.md) — the commands used first.
- [Environment variables](environment-variables.md) — what the env recipes
  create.
- [Dependencies](dependencies.md) — tools the recipes assume.
- [Known issues](known-issues.md) — gaps in what these commands guarantee.
