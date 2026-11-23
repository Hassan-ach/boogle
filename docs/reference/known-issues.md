# Known issues

**Last updated:** 2026-11-23

This page records verified limitations and open gaps in the checked-in stack.
It is a documentation ledger, **not a production-readiness assessment**: Boogle
is a study codebase.

!!! note "Status meanings"
    - **Confirmed (source)** — verified by reading current files in this pass
      or in the source-of-truth fact sheets prepared for this documentation;
      no runtime probe or live database query was run.
    - **Review-only** — reported by `project-review-2026-11-22.md` but not
      reverified in this documentation pass. These are listed for completeness
      and are **not** asserted as current behavior.

## Confirmed from current source

| ID | Status | Component | What happens | Evidence | Doc impact |
|---|---|---|---|---|---|
| K-01 | Confirmed (source) | Indexer | The reconciliation sweep republishes jobs with plain `basic_publish` (no mandatory flag or confirms) *after* deleting `page_word` rows. A silently dropped republish stalls that page until the next sweep tick. | `services/indexer/src/core/indexer.rs`, `messaging.rs`; review M-05 marks it "Likely" | Do not describe sweep recovery as guaranteed; use "best-effort" |
| K-02 | Confirmed (source) | Indexer | Only `tokio::signal::ctrl_c()` is handled. Docker sends SIGTERM to PID 1 (the entrypoint `exec`s the binary), so `docker stop` does not take the graceful drain path. | `services/indexer/src/main.rs`, `entrypoint.sh`; fact sheet §1 | Do not repeat older docs' SIGTERM claim; document Ctrl-C only |
| K-03 / M-06 | Confirmed (source) | Engine | `GET /health` unconditionally returns `{"status":"ok"}` and the Docker HEALTHCHECK uses it. A dead database still shows a healthy container while `/search` fails. | `services/engine/internal/handlers/health.handler.go`, `Dockerfile`; review M-06 | Health is process-level, not DB readiness; do not imply dependency health |
| K-04 / H-02 / M-26 | Confirmed (source) | Engine | Routes are `/`, `/public`, `/search`, `/health`, `/feeling-lucky`. There is no `/metrics` route, no authentication, no result cache, and no TLS. Port `1323` is published on all host interfaces. | `services/engine/cmd/server/main.go`, `docker-compose.yml`, fact sheets §4 and §6 | Do not document metrics, auth, caching or a loopback-only engine |
| K-05 / M-16 | Confirmed (source) | Ranking | `RANKING_DAMPING_FACTOR`, `RANKING_ITERATIONS` and `RANKING_SCHEDULE_INTERVAL` are not read. PageRank uses fixed defaults (20 iterations, `0.85`), there is no wall-clock scheduler, and IDF and PageRank are committed in separate transactions. | `services/ranking/src/calculator.py`, `main.py`, `migration/02_pg_rank.sql`; ranking fact sheet §5 | Treat those names as unimplemented; do not imply atomic recomputation |
| K-06 | Confirmed (source) | Spider / Engine config | `RABBITMQ_PROTOCOL` is read by spider config but the AMQP URI builder hard-codes `amqp://`. `REDIS_DELAY` and `REDIS_MAX_RETRY` are exported by the spider entrypoint, and `DEBUG` / `LOG_LEVEL` by the engine entrypoint, with no code reads found. | `services/spider/internal/config/config.go`, `internal/messaging/rabbitmq.go`, both entrypoints | Mark these as unverified as functional settings |
| K-07 / M-20 | Confirmed (config) | Build context | Every `.dockerignore` excludes `**/.env` but not `**/.env.docker`. Service Dockerfiles run `COPY . .` in a builder stage, so locally generated credential overlays enter builder layers. Final runtime stages copy only compiled artifacts. | root and service `.dockerignore` files, all four `Dockerfile`s, `scripts/docker-env.sh`; review M-20 | Do not claim overlays are excluded from builds; recommend `**/.env*` |
| K-08 | Confirmed (source) | Legacy docs and assets | Excluded legacy pages and screenshots describe a monitoring service, a cleanup scheduler, `/metrics` and a `monitor_state` table that do not exist in current config or SQL. The architecture image also misspells "indexer". | `mkdocs.yml` `exclude_docs`, fact sheet §7, review M-24 | Legacy `docs/*.md` files are historical only and must not be used as evidence |
| K-09 | Confirmed (source) | Startup and observability | Service Compose files have no `depends_on` against datastores; `infra-up` waits only for Postgres and RabbitMQ, never Redis. Three of four service healthchecks are `pidof`-only, and no Compose file configures log rotation, resource limits or backups. | root and service Compose files, `justfile`, Dockerfiles, fact sheets §1, §2, §6 | Do not claim readiness ordering, dependency health checks or operational hardening |
| K-10 / H-01 | Confirmed (source) | Spider policy | A missing or forbidden `/robots.txt` (404/403) is converted to an error, `EnsureHost` sends that error to `markHostUnreachable`, and `hostGate` then skips the host as dead. The review's contradicting fake-fetcher test never applies the production conversion. | `services/spider/internal/policy/hosts.go`, `policy.go`; review H-01; [Spider implementation](../services/spider/implementation.md) | Do not assert that hosts without robots.txt are crawled |
| K-11 / H-02 | Confirmed (source) | Engine rate limiting | Echo's limiter keys on `RealIP()`, which trusts `X-Forwarded-For` / `X-Real-IP`; the container also publishes `1323:1323` on all interfaces. The header-spoofing bypass itself was not reproduced in a running server. | `services/engine/cmd/server/main.go`, `docker-compose.yml`, Echo v5 vendored source; review H-02 | Do not describe the limiter as an abuse control against untrusted clients |
| K-12 / H-03 | Confirmed (source) | Engine spellcheck | If aspell initialization fails, `DummySpeller` returns the joined query phrase as one token, so multi-word searches normally return nothing. How often initialization fails in deployment was not measured. | `services/engine/cmd/server/main.go`, `dummy-spell-checker.go`, `search.handler.go`; review H-03 | Mention spellcheck fallback only with this caveat |
| K-13 / X-01 | Confirmed (source) | Spider → Indexer | Re-crawled HTML is never stored or re-indexed: the spider upsert updates metadata but leaves `html`, `indexed` and `indexed_at` unchanged; the indexer claim requires `indexed = FALSE`. | `services/spider/internal/store/db.go`, `internal/spider/spider.go`, `services/indexer/src/core/psql.rs`; [Data flow](../architecture/data-flow.md) | State plainly that content freshness on re-crawl is not implemented |
| K-14 / X-02 | Confirmed (source) | Ranking / Engine / SQL | Three different "indexed page" predicates feed one score: IDF counts all `pages`, PageRank counts `indexed = TRUE`, the engine applies no filter. | `services/ranking/src/calculator.py`, `migration/02_pg_rank.sql`, `services/engine/internal/store/store.go`; [Data flow](../architecture/data-flow.md) | Do not claim a single consistent corpus definition |

!!! warning "Environment and deployment caveats that follow from the above"
    Redis has no persistence volume, so frontier state is lost on restart.
    The datastore and admin ports are loopback-bound but the engine is not.
    The default credentials in tracked examples are placeholders. None of
    these statements is a production-readiness claim. See
    [Configuration](../getting-started/configuration.md) and
    [Environment variables](environment-variables.md).

## Review-only items not asserted here

The 2026-11-22 review contains additional findings that were not reverified for
this documentation pass. They are excluded from the behavior described in the
site; consult the review file if you need the original evidence.

| Review ID | Component | Reported claim | Why it is not asserted here |
|---|---|---|---|
| M-01 | Spider | Robots directives leak between `user-agent` groups | Not re-run against current source |
| M-03 / M-04 | Indexer | HTML comments and foster-parented table text are mis-tokenized | Not re-run against current source |
| M-07 / M-08 | Engine | `Inf` ranker weight and count/data snapshot skew | Not re-run against current source |
| M-09…M-15 | Engine UI | Missing labels, live regions, contrast and duplicate tab bindings | Static review only; no browser or screen-reader test was run |
| M-18 | Ranking | Failed pipeline leaves the batch pending with no retry/backoff | Code path not re-verified in this pass |
| M-19 | Test tooling | `just test-integration` echoes the full DSN, including the password | Not re-run in this pass |
| M-23 | CD workflows | Missing explicit `packages: write` permission | Repository settings are not observable offline |

## What is *not* claimed here

- No statement in this ledger implies the stack is secure, supported or
  production-ready.
- No finding is marked fixed. Several are deliberate study-code simplifications
  that are documented elsewhere as current behavior.
- Runtime reproduction of review findings and live-database validation of
  migration behavior were out of scope for this documentation pass.

## Related

- [Architecture overview](../architecture/overview.md) and
  [Data flow](../architecture/data-flow.md) — where the caveats apply.
- [Troubleshooting](../operations/troubleshooting.md) — symptoms and checks.
- [Service documentation](../services/spider/index.md) — start with the
  [Spider](../services/spider/index.md), then
  [Engine](../services/engine/index.md),
  [Indexer](../services/indexer/index.md) and
  [Ranking](../services/ranking/index.md).
- [Environment variables](environment-variables.md) — unimplemented and
  unread names.
