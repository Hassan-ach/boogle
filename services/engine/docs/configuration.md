# Engine configuration

**Last updated:** 2026-11-23

Configuration is read from the **process environment** only. The Go code never loads `.env` files: `internal/util/dorenv.go` calls `os.Getenv` directly. Values must be exported by the shell, supplied by Docker `env_file`, or set by the entrypoint before `boogle` starts.

## How values are parsed

`GetWithDefault`, `GetIntWithDefault`, and `GetFloatWithDefault` in `services/engine/internal/util/dorenv.go`:

- an empty string is treated as unset → default is used;
- `strconv.Atoi` / `strconv.ParseFloat` failures fall back to the supplied default;
- `ParseFloat` also rejects NaN, but **not** `Inf` or values outside `[0,1]`.

`config.LoadConfig()` combines `ranker.NewRankingConfig()` and `store.NewStoreConfig()`. It always returns a non-nil config with a nil error today.

## Environment variables

Defaults below are the values used by Go when a variable is absent or unparseable.

| Variable | Code default | Source | `.env.example` | `entrypoint.sh` | Notes |
|---|---|---|---|---|---|
| `PG_HOST` | `localhost` | `internal/config/store/psql.config.go` | `psql` | required | `.env.docker` is generated with `psql`. |
| `PG_PORT` | `5432` | same | `5432` | required | |
| `PG_USER` | `admin` | same | `admin` | required | |
| `PG_PASSWORD` | `1234` | same | `change-me-postgres` | required | Code default is a placeholder; never use it in production. |
| `PG_DBNAME` | `se` | same | `se` | required | |
| `PG_SSLMODE` | `disable` | same; also forced at `store.go` | `disable` | not set | Root `scripts/docker-env.sh` does not write it; container uses the code default. |
| `PG_MAX_OPEN_CONNS` | `20` | same | `20` | default 20 | Applied via `SetMaxOpenConns`. |
| `PG_MAX_IDLE_CONNS` | `20` | same | `20` | default 20 | Applied via `SetMaxIdleConns`. |
| `PAGE_SIZE` | `20` | `internal/config/store/store.config.go` | `20` | default 20 | `< 1` → 20. |
| `RANKER_MAX_RESULTS` | `100` | same | `100` | default 100 | Raised to `PAGE_SIZE` if smaller; used as SQL `LIMIT $2`. |
| `RANKER_WEIGHT_TF` | `0.5` | `internal/config/ranker/ranker.config.go` | `0.5` | default 0.5 | TF-IDF blend weight; not range-checked. |
| `TEST_DATABASE_URL` | local `boogle_test` DSN | `internal/store/store_integration_test.go` | — | — | Integration tests only; root `justfile` exports it. |
| `DEBUG`, `LOG_LEVEL` | — | not read by Go at all | — | set to `false` / `info` | Dead configuration in the engine. |

!!! danger "`.env.example` inline comments are not valid env syntax for this loader"
    `services/engine/.env.example` lines 16–18 contain trailing ` # comment` text (for example `PAGE_SIZE=20                      # Results per page`). If those raw strings are passed to the Go process, `Atoi`/`ParseFloat` fails and the code silently uses defaults. Shell sourcing strips the comments; when exporting manually, use clean `KEY=value` lines.

!!! warning "Unbounded tuning values"
    `RANKER_MAX_RESULTS` and `PAGE_SIZE` have no upper bound in code (only a lower floor/coercion), so a large value enlarges the candidate window and per-request work. `RANKER_WEIGHT_TF` accepts negative, `Inf`, and values above 1; a non-finite weight can make scores NaN and sort order nondeterministic. (review finding M-07)

## Coercions and fallbacks

| Condition | Result |
|---|---|
| `PAGE_SIZE < 1` | `20` |
| `RANKER_MAX_RESULTS < PAGE_SIZE` | raised to `PAGE_SIZE` |
| `RANKER_MAX_RESULTS < 1` at query time | SQL `LIMIT` uses `100` |
| Missing/unparseable `page` query param | page `1` |
| `page` greater than `totalPages` | clamped to `totalPages` |
| Unknown `tab` value | `all` |
| All cosine scores equal in the window | scores `0`; PageRank decides |
| NaN/Inf cosine denominator or score | `0` |
| `PG_SSLMODE` empty | `disable` |
| Aspell init failure | `DummySpeller` with the degraded multi-word behavior |

## Connection string and timeouts

`connectionString` in `internal/store/store.go` builds a libpq URL with `url.URL{Scheme:"postgres", User: url.UserPassword(...), Host: net.JoinHostPort(...), Path: DBName}`. Query parameters:

| Parameter | Value | Purpose |
|---|---|---|
| `sslmode` | `PG_SSLMODE`, forced to `disable` when empty | libpq treats empty as `require`; the code avoids silently negotiating TLS. |
| `connect_timeout` | `5` | Bound connection attempts. |
| `statement_timeout` | `30000` | "A search that runs away must not hold a pool connection forever." |

Other timeouts: boot ping uses `context.WithTimeout(..., 10*time.Second)`; the CLI wraps only `GetData` in 5 seconds. There is no server-side HTTP request deadline and no timeout around aspell calls.

## Container setup

- `services/engine/Dockerfile` is a three-stage build:
  1. Node builder runs `npm ci`, `just tailwind-build-prod`, `just js-prod`.
  2. Go 1.27.1 builder installs `build-base`, `aspell-dev`, `just`, pins `templ@v0.3.1020`, then runs `just build-emb`.
  3. Alpine runtime installs `aspell` and `aspell-en`, creates UID/GID 10001, copies the `boogle` binary and generated `public/`, and runs as non-root.
- `HEALTHCHECK` runs `wget -q --spider http://localhost:1323/health` every 30s. Because `/health` is DB-agnostic, this is liveness only.
- `services/engine/entrypoint.sh` uses `set -eu`, requires the five `PG_*` connection variables, defaults the tuning variables, exports `DEBUG`/`LOG_LEVEL` (which Go ignores), then `exec ./boogle "$@"`.
- `services/engine/docker-compose.yml` loads `.env` then optional `.env.docker`, and publishes `1323:1323`.

!!! danger "Network exposure (review findings H-02, M-26)"
    Compose publishes the container port on **all host interfaces** while every datastore is loopback-bound in the root stack. The engine has no authentication and its only abuse control — a 20 req/s in-process limiter — keys on a client-controlled `RealIP()` value. Bind to `127.0.0.1:1323:1323` by default and use a trusted proxy or explicit IP extractor before exposing it.

!!! note "Secret overlay caveat"
    `.env.docker` is generated from the root `.env` by `scripts/docker-env.sh` with only the five `PG_*` connection variables. `.dockerignore` excludes `.env` files from image layers; the final image copies only the binary and `public/`. Secret-overlay handling in build contexts is a cross-cutting infra concern and is not asserted here beyond what these files show.

## Verified commands

From `services/engine/justfile` and the verified fact sheet. The Go process does not read `.env` on its own.

```bash
# Standalone service: export env first, then generate templates/assets
cd services/engine
set -a; . ./.env.example; set +a      # shell sourcing strips inline comments
npm install
just templ-generate && just tailwind-build && just js-build
go run ./cmd/server                    # http://localhost:1323, GET /health

# Debug a ranking without HTTP
just cli "search terms"

# Tests (just test omits view/page/result)
just test
go test ./... -count=1                 # full suite, including view tests
just test-cov
just test-integration                  # requires TEST_DATABASE_URL / live Postgres

# Container (requires the external search_net network from the root compose stack)
docker compose -f services/engine/docker-compose.yml up --build
```

`just dev` runs template generation, dev asset builds, and `air` using `services/engine/.air.toml` (watches `.go`, `.tpl`, `.tmpl`, `.templ`, `.html`; 500 ms delay). A fresh checkout has no generated templates or `public/` assets — both are gitignored — so one of the build paths above must run first. Local aspell dictionaries (`en_US`) are available on this machine, so aspell-dependent tests execute rather than skip.

!!! note "Build prerequisites"
    Building requires cgo and aspell development headers (`build-base aspell-dev` in the Dockerfile). `go.mod` declares `go 1.27.1`; the local toolchain used for the fact sheet was `go1.27.4-X:nodwarf5 linux/amd64`.

## Related pages

- [Engine overview](index.md)
- [Architecture](architecture.md)
- [HTTP interface](api.md)
- [Implementation](implementation.md)
