# Dependencies

**Last updated:** 2026-11-23

Versions below come from the manifests, Dockerfiles and workflow files in this
repository. They describe the checked-in stack at the time of this
documentation pass; nothing here is a production-support claim.

!!! note "Verification basis"
    Values were read from the files named in each table. No package manager,
    image registry or CI run was consulted during this pass.

## Host tools

| Tool | Requirement | Evidence |
|---|---|---|
| Docker Engine + Compose V2 | Long `env_file: { path, required }` syntax needs Compose **≥ 2.24** | `services/*/docker-compose.yml`, root `justfile` |
| `just` | Required at root and in every service; version not pinned | root and service justfiles |
| `python3` | URL encoding in `scripts/docker-env.sh`; docs tooling | `scripts/docker-env.sh`, `docs/requirements.txt` |
| Git | Clone and CI checkout | repo metadata |
| `bash` | `scripts/setup-test-db.sh` | script shebang |
| `ss` (optional) | Port-1323 preflight | root `justfile` preflight recipe |
| Host `psql` client | Integration-test database setup only | `scripts/setup-test-db.sh` |

The README's "only Docker and Git are required" statement is incomplete for
local development. See [Prerequisites](../getting-started/prerequisites.md).

## Language toolchains

| Component | Version / edition | Manifest or source |
|---|---|---|
| Go (engine) | `go 1.27.1`; templ `v0.3.1020`; Echo v5.0.4 | `services/engine/go.mod` |
| Go (spider) | `go 1.27`; builder image `golang:1.27.1-alpine3.23`; templ `v0.3.943` is installed but unused by `just build` | `services/spider/go.mod`, `Dockerfile` |
| Rust (indexer) | Edition 2024; `rust:alpine3.23` builder with musl target; release profile uses `lto = "fat"`, `panic = "abort"`, `strip = true` | `services/indexer/Cargo.toml`, `Dockerfile` |
| Python (ranking) | `python:3.12-alpine` builder; CI pins `3.12.10` | `services/ranking/Dockerfile`, `ranking_ci.yaml` |
| Node (engine assets) | `node:22-alpine3.23`, `npm ci`; Tailwind `^4.2.1`, esbuild `^0.27.3`, htmx `^2.0.8`, Alpine.js `^3.15.8` | `services/engine/Dockerfile`, `package.json` |
| Docs build | Python 3.12 in CI; `mkdocs==1.6.1`, `mkdocs-material==9.6.23` | `.github/workflows/docs_ci.yaml`, `docs/requirements.txt` |

No Rust toolchain file pins a specific compiler version in this repository.
The indexer Docker build sets `RUSTFLAGS="-C target-cpu=x86-64-v2"` to avoid
`SIGILL` on older hosts, while local `just build` uses
`-C target-cpu=native`.

## Container images

| Image | Purpose |
|---|---|
| `postgres:18.6-alpine3.24` | Shared database, container name `psql` |
| `redis:alpine3.23` | Spider frontier and URL/host state; floating minor/patch |
| `rabbitmq:4-management-alpine` | Broker and management UI; floating patch |
| `adminer:5` | Database UI, loopback only |
| `alpine:3.23` | Final runtime base for all four services |
| `golang:1.27.1-alpine3.23`, `rust:alpine3.23`, `python:3.12-alpine`, `node:22-alpine3.23` | Service builder stages |

Service images are built locally from `services/<svc>/Dockerfile`. CD workflows
separately publish images to `ghcr.io/<repo>/<service>` on tags such as
`engine@v*`; Compose does not pin those images. See
[Docker Compose layout](../operations/docker.md) and
[Deployment](../operations/deployment.md).

## Key library pins

| Service | Notable dependencies |
|---|---|
| Engine | `github.com/a-h/templ v0.3.1020`, `github.com/labstack/echo/v5 v5.0.4`, `github.com/lib/pq v1.11.2`, `go-aspell` (cgo), `golang.org/x/time v0.14.0` |
| Spider | `github.com/lib/pq v1.10.9`, `amqp091-go v1.15.0`, `go-redis/v9 v9.14.0`, `golang.org/x/net v0.44.0` |
| Indexer | `html5ever 0.35`, `tokio 1.0`, `sqlx 0.8` with rustls, `lapin 4.12`, `slog 2.8`, `thiserror 2.0` |
| Ranking | `faststream[rabbit]==0.7.7`, `psycopg[binary,pool]==3.3.6`, `python-dotenv==1.0.0`, `pytest==9.1.1`, `pytest-cov==7.0.0`, `pyinstaller==6.19.0` |

Ranking also lists `APScheduler`, `numpy`, `scipy`, `pytz` and `tzlocal`, which
are **not imported by `services/ranking/src/`**. APScheduler in particular
does not provide a scheduler in this service; recomputation is event-driven.
See [Known issues](known-issues.md).

## Manifests

| File | Declares |
|---|---|
| `services/engine/go.mod`, `go.sum` | Engine Go module and checksums |
| `services/spider/go.mod`, `go.sum` | Spider Go module and checksums |
| `services/engine/package.json`, `package-lock.json` | Asset build dependencies |
| `services/indexer/Cargo.toml`, `Cargo.lock` | Rust crate versions and release profile |
| `services/ranking/requirements.txt` | Pinned Python dependencies |
| `services/ranking/pytest.ini` | Test paths, `pythonpath=src`, `asyncio_mode=auto`, `integration` marker |
| `docker-compose.yml`, `services/*/docker-compose.yml` | Image names, ports, overlay loading, network |
| `services/*/Dockerfile`, `services/*/entrypoint.sh` | Build stages, runtime user, required env, healthchecks |
| `mkdocs.yml`, `docs/requirements.txt` | Docs site nav, exclusions, build dependencies |
| `migration/*.sql` | Schema and ranking functions |

## CI/CD

| Workflow | What it does |
|---|---|
| `infra_ci.yaml` | Validates all five Compose files against `.env.example`, runs `shellcheck`, greps migrations for SQL keywords |
| `engine_ci.yaml`, `spider_ci.yaml` | Go format check, tests, build and vet |
| `indexer_ci.yaml` | `cargo fmt --check`, offline tests, build |
| `ranking_ci.yaml` | Pytest with `--cov-fail-under=80`, PyInstaller build |
| `docs_ci.yaml` | Strict docs check via `scripts/docs.py check`, plus each service doc build |
| `<service>_cd.yaml` | On `service@v*` tags, build and push the image to GHCR using `GITHUB_TOKEN` |

There is no deploy job, no signing or provenance step, and no integration test
in CD. CI triggers on pull requests to `main` / `dev`. See
[Deployment](../operations/deployment.md).

## Related

- [Prerequisites](../getting-started/prerequisites.md) — what you need on the
  host for each workflow.
- [Commands](commands.md) — recipes that use these tools.
- [Known issues](known-issues.md) — drift between declared versions and
  actual behavior.
