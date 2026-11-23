# Coding conventions

These are patterns observed in the current source, not a proposed style guide.
When in doubt, match the nearest service package and check its CI workflow.

## Small files organized by domain

Services are split by responsibility, then by concern within it:

| Service | Pattern |
|---|---|
| engine | `internal/handlers/*.handler.go`, `internal/service/{ranking,spellchecker}/`, `internal/store/`, `internal/apperror/`, `view/page/`, `view/component/` |
| spider | `internal/{spider,policy,state,store,parser,messaging,utils}/` |
| indexer | `src/core/{indexer,psql,messaging,config,errors,utils,text_sink}.rs` |
| ranking | `src/{main,calculator,messaging,psql}.py` |

Keep new logic in the owning domain package. Service behavior and configuration
changes belong in `services/<svc>/docs/` as well as the code; see
[Contributing](contributing.md) and [Documentation](documentation.md).

## Typed errors and boundary translation

- **engine** — `apperror.AppError` carries an HTTP `Code`, a client-safe
  `Message`, and a wrapped cause `Err`. `errors.Is` / `errors.As` still work;
  `HandleError` logs the cause and renders HTML, never the raw error text.
- **indexer** — `thiserror` enums (`DatabaseError`, `MessagingError`,
  `IndexerError`, aliased as `AppError`) classify failures. Requeue decisions
  depend on the variant: parse, serde, and not-found errors are dropped;
  database and messaging errors are requeued.
- **spider** — typed policy reasons and actions drive retries, parking, and
  permanent skips; exported sentinel errors (for example `state.ErrEmptyHost`)
  let callers distinguish expected cases.
- **ranking** — `retry_on_db_error` retries only `psycopg.OperationalError` and
  `psycopg.InterfaceError`; other exceptions are logged as non-retryable and
  re-raised. Startup failures exit nonzero.

Do not swallow errors at a layer where the caller can still act on them. The
code logs context at the point of failure and preserves the cause.

## Repository / store abstractions

Handlers and services depend on an interface or injected collaborator, not on
connection details:

- spider's `state.State` interface has Redis and in-memory implementations;
  its contract documents which fields counters own so a save cannot clobber
  concurrent updates.
- engine handlers receive a `PsqlStore` and ranking/spellchecker services;
  store methods own the SQL and wrap failures as `apperror.Internal`.
- indexer's `Indexer<Psql, RabbitMQ>` receives its DB and broker via `Arc`,
  with a shared `Semaphore` and `JoinSet`.
- ranking constructs `DatabaseManager`, `RankingCalculator`, and
  `MessagingService` once in `RankingService`.

## Immutable and copy-on-return patterns

The repository is not purely functional, but several boundaries deliberately
avoid leaking mutable state:

- engine's `PageMapper.GetValues()` / `WordMapper.GetValues()` copy the backing
  slice before returning it.
- ranking snapshots the pending confirmation count into a local `batch` before
  awaiting the pipeline, then subtracts only that snapshot on success; a recent
  change also stopped tests from mutating `os.environ`.
- spider bookkeeping uses a fresh context with a 5 s timeout for writes that
  must survive the crawl deadline; it does not mutate the caller's deadline.
- indexer passes owned values through `Arc` and returns new error values via
  `#[from]` conversions rather than mutating shared state.

## Parameterized SQL only

All observed production SQL uses bound parameters:

- engine uses `$1` with `pq.Array(words)` and `$2` for the candidate limit.
- spider uses `$n` placeholders; dynamic string assembly only builds
  placeholder lists for batch inserts, never injects user values.
- indexer uses compile-time checked `sqlx::query_as` / `query` with bound
  parameters and `FOR UPDATE SKIP LOCKED` for atomic claims.
- ranking calls `SELECT update_page_rank(%s, %s)` with psycopg parameters.

Never concatenate user input into SQL. Keep IDs as UUIDs and let the store
layer own statement construction.

## Context, cancellation, and deadlines

- engine passes `c.Request().Context()` into store and template rendering; a
  10 s boot ping and SQL `connect_timeout` / `statement_timeout` bound startup
  and queries. There is no per-request deadline.
- spider owns a cancellable root context, per-crawler timeouts, a fetch
  semaphore, and a `WaitGroup`; `Stop()` cancels and waits.
- indexer uses a `CancellationToken` plus a semaphore sized from
  `PG_MAX_CONNECTIONS` and drains tasks for up to 30 s on shutdown. It handles
  Ctrl-C only — see [Known issues](../reference/known-issues.md).
- ranking tracks background tasks in a set and cancels/drains them in
  `MessagingService.drain()` on shutdown.

When adding a blocking call, thread the existing context/token through rather
than creating an unrelated one.

## Structured logging and secrets

- ranking emits JSON with `service`, `timestamp`, `level`, `logger`, `message`,
  and optional `error` (traceback).
- indexer duplicates structured slog output to stderr and a JSON file with
  `service=indexer`.
- spider uses typed `Secret` values and a `Redacted()` config dump; its file
  sink is JSON and its console handler is plain text.
- engine's custom request logger logs method, path, status, and latency only —
  deliberately omitting search terms and full URIs.

!!! note "Residual ad-hoc output exists"
    There are a few plain prints outside the structured paths: the engine debug
    CLI, spider's `"Exiting gracefully"` line, indexer's startup/config
    `println!`/`eprintln!` notices, ranking's file-sink failure message, and one
    `console.log` in `static/js/main.js`. Do not cite those as precedent for new
    production code.

## Formatting, linting, and debt

Enforced in CI:

| Language | Check | Workflow |
|---|---|---|
| Go | `gofmt -l` check and `go vet ./...` | engine and spider CI |
| Rust | `cargo fmt --check` | indexer CI |
| Shell | `shellcheck scripts/*.sh services/*/entrypoint.sh` | infra CI |
| Python | 80% coverage gate only | ranking CI |

Not enforced: `golangci-lint` (its spider CI job is commented out) and
`cargo clippy -- -D warnings`. Both have known debt; do not add them as blocking
gates until that debt is cleared. See
[Known issues](../reference/known-issues.md).

Run `just test` before opening a PR, and `just docs-check` when documentation
changes. See [Testing](testing.md) for the test tiers.
