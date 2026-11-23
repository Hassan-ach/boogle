# Testing

Tests are split into two tiers, deliberately kept separate in the root
`justfile`: a fast unit tier safe on every save, and an integration tier that
needs a real PostgreSQL (and, for spider, Redis/RabbitMQ).

| Tier | Command | Infrastructure | What runs |
|---|---|---|---|
| Fast unit | `just test` | none | engine + spider Go internal packages, `cargo test`, ranking pytest `-m "not integration"` |
| Coverage | `just test-cov` | none | Go per-package coverage, pytest `--cov=src --cov-report=term-missing` |
| Integration | `just test-integration` | Postgres (reset first), broker/Redis where configured | build-tagged Go tests, ignored Rust tests, pytest `-m integration` |
| Everything | `just test-all` | as above | `test` then `test-integration` |

## Fast unit suites

`just test` fans out to the service suites:

```bash
# engine and spider (root justfile loops over the two Go services)
cd services/engine && go test ./internal/... -count=1
cd services/spider && go test ./internal/... -count=1

# indexer (all inline #[cfg(test)] modules; live-DB tests are #[ignore])
cd services/indexer && cargo test

# ranking (uses the service venv; marker defined in pytest.ini)
cd services/ranking && venv/bin/python -m pytest tests -m "not integration"
```

Service-local recipes provide the same commands (`just test` in each service
directory), plus `just test-cov`. Engine has two useful variations:

- `cd services/engine && go test ./... -count=1` also runs
  `view/page/result/result_test.go`; the root `just test` and the service
  `just test` only cover `./internal/...`.
- `go test -race ./internal/...` passes locally; `go vet ./...` is clean.

Indexer `test-cov` uses `cargo llvm-cov --summary-only || cargo test`, so it
falls back to the plain suite when `cargo llvm-cov` is unavailable.

`services/ranking/pytest.ini` sets `testpaths = tests`, `pythonpath = src`,
`asyncio_mode = auto`, and `--strict-markers`. Unit tests use fake
cursor/connection/manager objects; no live database is required.

## Coverage

`just test-cov` reports Go coverage per package (via `go tool cover -func`) and
ranking coverage with missing lines. It does **not** produce an indexer coverage
report — use the indexer's own `just test-cov` for that.

CI enforces one coverage gate: ranking runs

```bash
python -m pytest tests -m "not integration" --cov=src --cov-fail-under=80
```

No other service has a numeric gate in CI. See
[Known issues](../reference/known-issues.md) for known test-quality gaps.

## Integration suites

Integration tests write scores and schema state, so they target an isolated
`boogle_test` database:

```bash
just infra-up          # start Postgres (and the rest of the stack)
just test-db           # DROP/CREATE boogle_test, apply migration/*.sql
just test-integration
```

`just test-integration` depends on `test-db`, then exports a single
`TEST_DATABASE_URL` (`postgres://<user>:<pw>@localhost:<port>/boogle_test?sslmode=disable`)
for all suites. The root recipe does not source `.env`, so credentials come
from its `PG_*` fallbacks (`admin` / `1234` / `5432`) or from an exported
environment. `scripts/setup-test-db.sh` needs a host `psql` client and reads
root `.env` `PG_*` values when present.

| Service | Integration command used by the root recipe | Extra requirements |
|---|---|---|
| engine | `TEST_DATABASE_URL=... go test -tags integration ./internal/... -count=1` | Postgres only; tests skip when no DB is reachable |
| spider | `TEST_DATABASE_URL=... go test -tags integration ./internal/... -count=1` | Its recipe documents live Postgres, Redis, and RabbitMQ; the root recipe only exports the DB DSN, so broker/Redis settings must already be in the environment |
| indexer | `DATABASE_URL=... cargo test -- --ignored` | runs only the 24 live-DB tests marked `#[ignore]`; the fast suite covers the other 72 |
| ranking | `TEST_DATABASE_URL=... venv/bin/python -m pytest tests -m integration` | Postgres; DSN lookup also accepts `BOOGLE_TEST_PG_DSN`, `TEST_DATABASE_URL`, `DATABASE_URL`, then a default `boogle_test` URL |

!!! danger "The test DB is dropped and recreated"
    `just test-db` runs `DROP DATABASE IF EXISTS boogle_test` without a
    confirmation prompt. Never point `BOOGLE_TEST_DB` at a database you want
    to keep.

## CI workflows

All per-service CI workflows run on pull requests to `main` or `dev` with path
filters. **No integration test runs in CI.**

| Workflow | Trigger paths | Checks |
|---|---|---|
| `engine_ci.yaml` | `services/engine/**` | templ generate, `go mod tidy`, gofmt check, `go test -v ./...`, `go build`, `go vet`; installs aspell |
| `spider_ci.yaml` | `services/spider/**` | `go mod tidy`, gofmt check, `go test -v ./...`, `go build`, `go vet`; golangci-lint job is commented out |
| `indexer_ci.yaml` | `services/indexer/**` | `cargo fmt --check`, `SQLX_OFFLINE=true cargo test`, `cargo build` |
| `ranking_ci.yaml` | `services/ranking/**` | install requirements, pytest with 80% gate, PyInstaller build |
| `infra_ci.yaml` | Compose/Dockerfile/entrypoint/scripts/migrations | compose config validation, shellcheck, keyword grep over migrations |
| `docs_ci.yaml` | docs paths, `mkdocs.yml`, root/service READMEs | `scripts/docs.py check` and standalone builds per service |

Tag pushes (`engine@v*`, `spider@v*`, `indexer@v*`, `ranking@v*`) trigger the CD
workflows, which build and publish GHCR images; they do not run test suites.

## Known test gaps

- **Robots fake fetcher false confidence.** A spider policy test asserts that
  204/4xx robots responses grant permission, but its fake fetcher returns a
  status without the production status→error conversion. The real fetcher maps
  those responses to an error that marks the host dead. See
  [Known issues](../reference/known-issues.md).
- The engine's view tests are outside `./internal/...`, so the default fast
  suite skips them.
- Indexer live-DB tests are ignored by default and only run via
  `cargo test -- --ignored`.
- Migration CI only greps for SQL keywords; it does not execute migrations.
  See [Commands](../reference/commands.md) for local equivalents.
