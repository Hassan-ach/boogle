# Monitoring Service

Watches the pipeline, manages service health, and exposes live metrics.

<div align="center">
  <img src="https://github.com/Hassan-ach/boogle/blob/main/assets/monitoring.png" alt="Monitoring Dashboard" width="600"/>
  <p><em>Live dashboard showing service status and metrics</em></p>
  </div>

**Language**: TypeScript (Node.js)
**Purpose**: Orchestrate containers, detect failures, serve a live dashboard


## Quick Start

```bash
npm install
npm run dev
```

Open http://localhost:7070

## Files

- `src/index.ts` - Entry point, graceful startup/shutdown
- `src/app.ts` - Express app factory, boot sequence
- `src/scheduler/scheduler.ts` - Tick loop — orchestrates all periodic checks
- `src/workers/engine-guardian.ts` - Engine watchdog with restart rate limiting
- `src/workers/cleanup-worker.ts` - Stale-container and orphan cleanup
- `src/docker/controller.ts` - Docker CLI wrappers (`docker compose`, `docker ps`)
- `src/redis/client.ts` - Redis singleton with reconnect strategy
- `src/db/queries.ts` - Metric queries against PostgreSQL
- `src/metrics/http.ts` - Express router: `/metrics`, `/health`, static dashboard
- `static/` - Live dashboard (HTML/CSS/JS, no build step)

## Commands

```bash
npm run dev    # Development with ts-node watch
npm run build  # Compile TypeScript → dist/
npm start      # Run compiled dist/index.js
npm test       # Run vitest unit tests
```

## Configuration

```env
MONITOR_PORT=7070
TICK_INTERVAL_MS=15000

ROOT_COMPOSE_FILE=../../docker-compose.yml
ENGINE_COMPOSE_FILE=../engine/docker-compose.yml
SPIDER_COMPOSE_FILE=../spider/docker-compose.yml
INDEXER_COMPOSE_FILE=../indexer/docker-compose.yml
RANKING_COMPOSE_FILE=../ranking/docker-compose.yml

DATABASE_URL=postgresql://admin:password@localhost:5432/se
REDIS_URL=redis://localhost:6379/1

ENGINE_HTTP_CHECK_URL=http://localhost:1323/
ENGINE_MAX_RESTARTS_PER_5M=5

SPIDER_MIN_INSTANCES=1
SPIDER_MAX_INSTANCES=4
SPIDER_QUEUE_HIGH_THRESHOLD=500

INDEXER_BACKLOG_THRESHOLD=100
RANKING_TRIGGER_DELTA=50
CLEANUP_EVERY_N_TICKS=4
```

## What It Monitors

| Service | Checks |
|---------|--------|
| Engine | HTTP health probe, restart rate limiting |
| Spider | Running instance count, queue depth, crawl rate |
| Indexer | Unindexed page backlog, running worker count |
| Ranking | Post-indexing delta trigger, running job count |
| Infra | Postgres connectivity, Redis connectivity |

## Metrics Endpoint

`GET /metrics` returns a JSON snapshot:

```json
{
  "timestamp": "2026-03-11T12:00:00.000Z",
  "spider_queue_size": 412,
  "spider_running": 2,
  "spider_crawl_rate_per_min": 17,
  "indexer_backlog": 34,
  "indexer_running": 1,
  "engine_running": 1,
  "engine_restarts_rate_limited": 0,
  "ranking_running": 0,
  "scheduler_tick_errors_total": 0,
  ...
}
```

`GET /health` returns `{ "status": "ok" }`.

## Dashboard

The compiled dashboard lives in `static/` and is served statically at `/`.
It polls `/metrics` every 5 seconds and renders live status cards for every service.

## Docker

```bash
# Build and start
docker compose up -d --build

# Follow logs
docker compose logs -f
```

> **Note**: The container requires `/var/run/docker.sock` mounted and
> `BOOGLE_PROJECT_ROOT` pointing to the project root (same absolute path as on the host).
> See [docker-compose.yml](docker-compose.yml) for a ready-made example.

## Details

See [docs/MONITORING.md](../../docs/MONITORING.md) for architecture, tick lifecycle, policy engine, and known issues.
