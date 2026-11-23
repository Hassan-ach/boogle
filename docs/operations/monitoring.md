# Monitoring and health

**Last updated:** 2026-11-23

Boogle has **no metrics stack**: no `/metrics` endpoint, no Prometheus/Grafana, no exporter, no monitoring container, and no alerting. Operational visibility comes from Docker healthchecks, container logs, and two UIs (Adminer and RabbitMQ Management).

## Healthchecks that exist

### Infrastructure (declared in `docker-compose.yml`)

| Service | Check | Interval / timeout / retries |
|---|---|---|
| `psql` | `pg_isready -U $POSTGRES_USER -d $POSTGRES_DB` (variables expanded inside the container) | 5s / 5s / 10 |
| `redis` | `redis-cli -a "$REDISCLI_AUTH" ping` piped to `grep -q PONG` | 5s / 5s / 10 |
| `rabbitmq` | `rabbitmq-diagnostics -q ping` | 10s / 10s / 12 |
| `adminer` | none; starts after `psql` is healthy via `depends_on` | — |

### Services (declared in each `Dockerfile`, not in Compose)

| Service | Check | Interval / timeout / retries | Start period |
|---|---|---|---|
| engine | `wget -q --spider http://localhost:1323/health` | 30s / 5s / 3 | 10s |
| spider | `pidof spider` succeeds | 30s / 5s / 3 | 30s |
| indexer | `pidof indexer` succeeds | 30s / 5s / 3 | 10s |
| ranking | `pidof main` succeeds | 30s / 5s / 3 | 30s |

!!! warning "Healthy does not mean ready"
    The three `pidof` checks prove only that the binary is running, not that Postgres, Redis, RabbitMQ, or queue consumers work. The engine's check only proves that an HTTP handler responds. A container can be reported healthy while the database is down.

Nothing gates startup on these checks: application service composes have no `depends_on`, and `just infra-up` waits only for Postgres and RabbitMQ. Redis is never waited on. See [Deployment](deployment.md#restart-and-upgrade-behavior).

## Engine `/health` and absent metrics

`GET /health` is registered on the engine and returns a static JSON body:

```json
{"status":"ok"}
```

It performs **no** database, aspell, or broker check. The engine's routes are `/`, `/public/*`, `/search`, `/health`, and `/feeling-lucky`; there is no `/metrics` route. The `monitor_state` table present in older documentation was dropped in `migration/03_index_completion.sql`.

!!! note "Monitoring screenshot is historical"
    `assets/monitoring.png` shows a `monitoring Service`, a scheduler, `LIVE 5s` telemetry and a `/metrics` link. None of those exist in the current code or Compose files. Treat the image as a concept mock, not as documentation of deployed behavior.

## What an operator can check today

| Question | Where to look | Practical check |
|---|---|---|
| Are the containers running and healthy? | Docker | `docker compose ps` for the root stack; the same command with `-f services/<svc>/docker-compose.yml` for a service. `docker logs --tail=100 <container>` or `just run-logs` for recent output. |
| Is Postgres answering? | Docker / Adminer | `docker compose exec -T psql pg_isready -U "${PG_USER:-admin}"`; Adminer at `http://127.0.0.1:8080` (log in to the `psql` server with configured credentials). |
| Is RabbitMQ answering? | Docker / Management UI | `docker compose exec -T rabbitmq rabbitmq-diagnostics -q ping`; UI at `http://127.0.0.1:15672`. Queue depths for `indexer.jobs` and `indexer.confirmations` are the closest thing to a pipeline backlog indicator. |
| Is the engine HTTP endpoint alive? | Engine | Request `http://localhost:1323/health`. Remember it is DB-agnostic; pair it with the Postgres check above. |
| Is Redis alive? | Docker | Health status in `docker compose ps` is the only built-in signal. There is no Redis UI, no published introspection command in this repo, and no persistence volume. |
| Is the index fresh? | Adminer / logs | Inspect `pages` rows (`indexed`, `indexed_at`, `updated_at`) and indexer/ranking logs. There is no freshness metric and no wall-clock ranking scheduler. |
| Why did a service fail? | Container logs | `just run-logs` follows all four services. Note that spider and indexer also write in-container JSON files; see [Logging](logging.md#streams-and-files). |

Port bindings and container names are listed in [Docker Compose layout](docker.md#root-infrastructure-stack).

## Known blind spots

- No metric history, no dashboards, no alerting, no retention configuration for logs.
- No check of the search path itself: `/search` correctness, aspell availability, ranking freshness, and candidate-window size are unmonitored.
- The aspell fallback emits no startup error signal, so a multi-word search collapse is only detectable by query behavior (review finding H-03; see [Known issues](../reference/known-issues.md)).
- Ranking failures have no retry path in the reviewed code (review finding M-18); a stalled confirmation backlog needs operator attention.
- No backup or restore verification exists for the `pgdata` volume.

For symptom-first guidance, see [Troubleshooting](troubleshooting.md).
