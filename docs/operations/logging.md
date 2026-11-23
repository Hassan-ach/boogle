# Logging

**Last updated:** 2026-11-23

Each service logs to its container's stdout/stderr. The spider and indexer also write JSON files **inside** the container, and ranking has an optional file sink. There is no central log aggregation, rotation, or shipping.

## Streams and files

| Service | Console stream | File sink | Format / notes |
|---|---|---|---|
| engine | stdout | none | Echo middleware logs `method`, `path`, `status`, `latency` for each request. The path-only logger deliberately avoids the query string, which would contain search terms. The error handler logs the internal cause and returns a generic HTML message. |
| spider | stdout | `LOGS_PATH` — forced to `/app/logs/logs.json` by the generated overlay | `slog` text handler on stdout plus a JSON handler writing to the file. If the file cannot be opened, it warns to stderr and continues with stdout only. Directory is created in the Dockerfile and entrypoint. |
| indexer | stderr | `LOG_PATH` — entrypoint default `./logs/indexer.log` | `slog` terminal drain on stderr plus a JSON drain to the file; the directory is created by the entrypoint/logger. |
| ranking | stdout | optional `LOG_PATH` (commented out in `services/ranking/.env.example`) | Structured JSON to stdout via a `JsonFormatter` on every log record. If a file sink is configured but cannot be opened, a warning is printed and logging continues to stdout only. |
| psql / redis / rabbitmq / adminer | Docker log streams | none | No logging configuration in the root Compose file; default Docker driver applies. |

`just run-logs` follows the four service containers with `docker logs -f --tail=100`; Ctrl-C stops only the log streams. See [Commands](../reference/commands.md).

!!! warning "File sinks are not durable"
    No Compose file mounts `/app/logs` or `./logs` from the host. Spider's `logs.json` and indexer's log file disappear when their containers are removed. Repo-local `*.log` and `logs.json` files are ignored by `.gitignore` and are not used by Compose.

## Docker log handling

- No Compose file contains a `logging:` key. The default Docker log driver and its default retention apply; no rotation, max-size, or max-file policy is configured.
- No log shipper, collector, or dashboard consumes these streams.
- Container removal also removes the Docker-held log stream, so use `just run-logs` while investigating a live incident or copy logs out before tearing down.

## Redaction and sensitive data

Verified behavior:

- Spider's config `Secret` type renders as `[redacted]` (and as an empty string when unset) in `slog` output, including nested JSON. The database layer explicitly avoids logging the raw libpq connection string and reveals the password only for the driver.
- The engine request log records only the URL **path**, not the query string; search terms are absent from request logs. Internal error causes are logged server-side, not returned to the client.
- Indexer/ranking log redaction is **not audited here** — do not assume their messages are free of URLs or query data.

Review-only caveat:

!!! note "Test helper echoes a DSN (review finding L-14)"
    `just test-integration` exports and then prints the full `TEST_DATABASE_URL`, including the test password (`justfile:128-129`). This is a test-runner message on the operator's terminal, not a service log. See [Known issues](../reference/known-issues.md).

## Where to look first

1. `just run-logs` — live stdout/stderr from all four service containers.
2. `docker logs --tail=100 <container>` — one container, including root infrastructure.
3. `docker compose ps` — confirm the process is still up before interpreting silence as success; healthchecks are liveness-oriented (see [Monitoring](monitoring.md#healthchecks-that-exist)).
4. RabbitMQ Management UI at `http://127.0.0.1:15672` — queue depth is often a better pipeline signal than log volume.

For symptom-specific guidance, see [Troubleshooting](troubleshooting.md).
