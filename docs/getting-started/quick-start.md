# Quick start

**Last updated:** 2026-11-23

The shortest path from a clean clone to a running search UI. This uses the
container workflow only; see [Prerequisites](prerequisites.md) for the host
tooling and [Installation](installation.md) for the detailed steps.

!!! note "Verification basis"
    Commands are taken verbatim from the root `justfile`. They were verified by
    source inspection, not executed during this documentation pass.

## 1. Bootstrap and edit secrets

```bash
just env
```

Then edit the root `.env` and replace the three `change-me-*` credential
values (`PG_PASSWORD`, `REDIS_PASSWORD`, `RABBITMQ_DEFAULT_PASS`). Re-run
`just env` to regenerate the `.env.docker` overlays.

For a local learning setup the tracked placeholders are sufficient to bring
the stack up, but they are **not secrets**. See
[Configuration](configuration.md#how-the-layers-combine).

## 2. Start everything

```bash
just run
```

This runs `env preflight infra-up migrate`, then builds and starts the services
in the order engine → spider → indexer → ranking. The preflight refuses to
continue if another process already owns host port `1323`.

When it finishes, the UI is at <http://localhost:1323>.

| URL | What it is | Bind |
|---|---|---|
| <http://localhost:1323> | Engine search UI | All host interfaces |
| <http://127.0.0.1:8080> | Adminer | Loopback |
| <http://127.0.0.1:15672> | RabbitMQ management UI | Loopback |

!!! warning "Do not expose this stack"
    This is a study codebase. The engine has no authentication, TLS or result
    cache, and the datastore credentials are placeholders unless you changed
    them. See [Known issues](../reference/known-issues.md).

## 3. Watch the logs

```bash
just run-logs
```

This tails the last 100 lines of all four services. Ctrl-C stops only the log
streams; the containers keep running.

## 4. Understand what you are seeing

1. The **Spider** starts from hard-coded seed URLs, applies robots/policy
   checks, and stores pages and graph edges in Postgres. It publishes
   `indexer.jobs` messages with a `page_id`.
2. The **Indexer** claims each page, tokenizes the HTML, writes `words` and
   `page_word`, publishes `indexer.confirmations`, then ACKs the job.
3. The **Ranking** service recomputes IDF and PageRank after 100 confirmations
   (the default), and once at startup because `RANKING_RUN_ON_STARTUP=true`.
4. The **Engine** serves `GET /search` from the index and renders HTML or an
   htmx fragment.

A fresh crawl takes time, and `RANKING_RUN_ON_STARTUP` scores only what is
already indexed at that moment. Run the stack again after the crawler and
indexer have made progress to refresh the scores.

!!! note "Known caveats for a first run"
    Re-crawled content is not re-indexed, and the three components that decide
    "which pages count" use different predicates. Both are described in
    [Known issues](../reference/known-issues.md). Images and Graph result tabs
    are placeholders, and "I'm Feeling Lucky" is a hard-coded text response.

## 5. Stop

```bash
just stop
```

This stops the four service projects and the root Compose stack. The `pgdata`
volume is kept, so a later `just run` reuses the same database. `just
infra-down` stops only the datastores.

## Where to go next

- [Installation](installation.md) — the explicit step-by-step variant.
- [Commands reference](../reference/commands.md) — every root and service task.
- [Architecture overview](../architecture/overview.md) — services, queues and
  shared state.
- [Docker Compose layout](../operations/docker.md) — topology and overlays.
- [Troubleshooting](../operations/troubleshooting.md) — startup failures.
