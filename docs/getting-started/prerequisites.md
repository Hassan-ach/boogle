# Prerequisites

**Last updated:** 2026-11-23

Boogle can be run two ways. The **container path** is enough for the normal quick
start and needs only a small host toolchain. The **local development path**
compiles and runs services on the host and needs the full language toolchains.

!!! note "Verification basis"
    Requirements below were derived from the root `justfile`, the five Compose
    files, `scripts/`, the Dockerfiles, and the language manifests. They were
    read, not executed, during this documentation pass.

## Container path (recommended)

| Tool | Why it is needed | Notes |
|---|---|---|
| Docker Engine with Compose V2 | Runs Postgres, Redis, RabbitMQ, Adminer and builds the four service images | The long `env_file: { path, required }` form needs Compose **≥ 2.24**; plain "V2" is not specific enough |
| Git | Clone the repository | |
| `just` | Entry point for every documented workflow | Root `justfile` and all four service justfiles use it; no version is pinned |
| `python3` | URL-encodes credentials in `scripts/docker-env.sh` | The only Python dependency of `just env` |
| `bash` | Runs `scripts/setup-test-db.sh` | Only needed for integration tests |
| `ss` (optional) | Port-1323 preflight in `just run` | If unavailable, the preflight check is skipped |

Datastore images are named in Compose (Redis and RabbitMQ use floating tags);
the four service images are built from local Dockerfiles. Go, Rust, Node and
the service Python packages are not needed on the host — `python3` is only used
by the env bootstrap script. See [Dependencies](../reference/dependencies.md).

### What the container path does not require

Go, Rust, Python packages, Node, templ and aspell all live inside the builder
or runtime images. You do not need them on the host to use `just run`.

!!! warning "Host `psql` is the exception"
    `just test-db` and `just test-integration` call `scripts/setup-test-db.sh`,
    which uses a **host** `psql` client rather than `docker compose exec`.
    Install a PostgreSQL client if you intend to run integration tests.

## Local development path

Running a service outside Docker (`go run`, `cargo run`, `python src/main.py`,
`air`) requires the toolchain for that service plus reachable datastores.

| Service | Toolchain | Manifest / version source |
|---|---|---|
| Engine | Go 1.27.1, templ v0.3.1020, Node 22 (Tailwind 4, esbuild 0.27), `just`, `air`, `libaspell-dev` + `aspell-en` for cgo | `services/engine/go.mod`, `package.json`, `Dockerfile`, `justfile` |
| Spider | Go 1.27 (builder image `golang:1.27.1-alpine3.23`), templ v0.3.943 is installed in the image but not used by `just build` | `services/spider/go.mod`, `Dockerfile`, `justfile` |
| Indexer | Rust edition 2024; musl target for shipped builds, while local `just build` uses `target-cpu=native` | `services/indexer/Cargo.toml`, `Dockerfile`, `justfile` |
| Ranking | Python 3.12 (CI pins 3.12.10) with `requirements.txt` in a `venv`; PyInstaller only for `just build` | `services/ranking/requirements.txt`, `pytest.ini`, `justfile` |

Cross-cutting requirements for local development:

- `just` and `python3` as above.
- Reachable PostgreSQL, Redis and RabbitMQ. Generated `.env.docker` overlays
  point at Compose service names (`psql`, `redis`, `rabbitmq`), so host-run
  processes normally use their own `.env` with `localhost` hosts. See
  [Configuration](configuration.md) and the
  [Environment variables reference](../reference/environment-variables.md).
- The full test tiers are described in [Testing](../development/testing.md).

## Ports you should expect

| Endpoint | Bind | Source |
|---|---|---|
| Engine UI `http://localhost:1323` | **All** host interfaces (`1323:1323`) | `services/engine/docker-compose.yml` |
| Postgres `127.0.0.1:${PG_PORT}` | Loopback | root `docker-compose.yml` |
| Redis `127.0.0.1:${REDIS_PORT}` | Loopback | root `docker-compose.yml` |
| RabbitMQ AMQP `127.0.0.1:5672`, management UI `127.0.0.1:15672` | Loopback | root `docker-compose.yml` |
| Adminer `http://127.0.0.1:8080` | Loopback | root `docker-compose.yml` |

!!! warning "The engine is published on all interfaces"
    Unlike the datastores, the engine binds `1323` on every host interface.
    Do not expose this study stack to an untrusted network: there is no
    authentication, TLS or result cache in the engine. See
    [Known issues](../reference/known-issues.md).

## Next

Continue with [Installation](installation.md) or go straight to the
[Quick start](quick-start.md). For the full topology see
[Docker Compose layout](../operations/docker.md) and
[Infrastructure](../architecture/infrastructure.md).
