# Project Review — Boogle

**Target**: `/home/bagi/Notes/dev/boogle`  
**Revision**: `51c29cd` (`docs/update-service-docs`; dirty: `README.md`, `services/README.md` modified, `docs/ARCHITECTURE.md` untracked)  
**Reviewed**: 2026-11-21  
**Depth**: `standard` · **Checks**: `enabled`  
**Reviewers**: 5 scoped read-only agents across 5 scopes  
**Verdict**: `CRITICAL` (operationally)

> **Contract status: MINOR VIOLATION.** The coverage check created one untracked artifact, `services/ranking/.coverage`. No tracked file changed: `git diff --stat` is still only the two README link lines. Per the read-only contract this report is marked **untrusted pending confirmation**; remove that artifact before committing. All findings remain evidence-backed, but the working tree is no longer byte-identical to preflight.

---

## 1. Executive Summary

Boogle is a four-service, polyglot learning search engine: a Go/ECHO web tier, a Go crawler, a Rust indexer, and a Python ranking worker, joined by RabbitMQ and a shared PostgreSQL database (Redis for spider state). The source code is above average for a learning project — typed error handling, parameterized SQL, a genuinely careful crawler-policy layer, a PageRank function with a documented conservation proof, and real unit/integration suites. The delivery and lifecycle layer is not: the documented `./deploy.sh` cannot complete, fresh env examples leave two services unable to start, the indexer un-indexes the entire corpus every 10–15 minutes, and the crawler lets any fetched page choose its own URL identity. The single most important action is to fix the index completion/sweep state machine before running the stack unattended; it currently deletes and rebuilds the search index continuously.

**Top three actions**

1. **Stop the index churn** — add an explicit index-completion marker (`indexed_at`, or clear `last_index_attempt_at` on success) and key the sweep off claims only. C-01 is destroying `page_word` for every successfully indexed page and re-queueing it forever.
2. **Repair deployment and the cross-service contract** — remove the deleted `services/monitoring` from `deploy.sh`, align `indexer.job` vs `indexer.jobs`, complete the env examples, add a Postgres volume, and make ranking log to stdout. As shipped, a fresh clone cannot bring up spider or ranking, and `./deploy.sh` aborts.
3. **Harden the crawler trust boundary** — store `rawUrl` (not `og:url`) as page identity, reject internal/loopback redirects and sitemap entries, and delete the two `%+v` config prints. These close index poisoning, SSRF into `psql`/`redis`, and plaintext credential leakage.

**Coverage**

- Scopes reviewed: **5/5** (engine, spider, indexer, ranking, delivery/docs)
- Production files read: **~85 of ~105** in scope (**≈81%**); all high-risk paths read; generated `*_templ.go`, `node_modules/`, `venv/`, `target/`, `dist/` excluded
- Dimensions covered: correctness, architecture, security, performance, reliability, testing, documentation, data/schema, API/contracts, dependencies/supply chain, infra/deployment, UI structure
- Dimensions not covered: WCAG/accessibility audit; live browser/DB/broker runtime; network verification of image tags
- Raw findings → unique → confirmed → disputed → refuted: **64 → 53 → 52 → 0 → 1** (the refuted `uuid` claim is in Appendix C)

---

## 2. Overall Project Health

| Dimension | Rating | Rationale |
|---|---|---|
| Architecture and boundaries | **C** | Clean per-service layering, but contracts are informal and drift (queue names, env keys, graph identity). |
| Correctness and defects | **D** | Index self-destruct loop, unordered graph edges, pagination count, NaN ranking, `og:url` identity. |
| Security | **D** | Credentials printed to logs, SSRF, exposed datastores, index poisoning; no auth is an explicit design choice. |
| Performance and scalability | **C** | Channel-per-crawl, N+1 admission, unindexed sweep/temp joins, full-table rewrites. |
| Reliability and error handling | **D** | At-most-once frontier, swallowed confirmations, consumer exits without drain, shutdown bugs. |
| Testing | **B** | Strong unit/integration intent and real assertions; gaps in messaging/config/pool and the success-state sweep. |
| Documentation | **C** | Detailed and mostly accurate per service; deployment, monitoring, queue names, and schema drift. |
| Maintainability | **B** | Small files, clear comments, dependency injection; some dead config and dead code. |
| Dependencies and supply chain | **C** | Floating tags, ignored `Cargo.lock`, unpinned tools, no `.dockerignore`. |

**Overall: D** — workable code with specific systemic weaknesses. It moves to C/B by fixing the index lifecycle, deploy/bootstrap contract, and crawler trust boundary; the test suites are already strong enough to protect those changes.

---

## 3. Architecture Review

**Actual structure** — four deployables plus shared infrastructure; no shared library, no API gateway:

```
engine (Go/Echo) ──reads──> PostgreSQL <──writes── indexer (Rust)
   │                          ▲   │
   │                          │   └──reads/writes── ranking (Python)
   │                          │
spider (Go) ──publish──> indexer.jobs ──consume──> indexer
spider ──writes──> urls/pages/graph_edges      indexer ──publish──> indexer.confirmations ──consume──> ranking
Redis: spider frontier/host state          PostgreSQL: urls, pages, words, page_word, graph_edges, page_rank
```

**Boundaries and dependency direction** — service boundaries hold: no service imports another's code; integration is RabbitMQ and the shared schema. That is the right shape for the project. The weakness is that every cross-service contract is unversioned and informal: queue names, JSON payload shape, `metadata` keys, `pages.indexed` semantics, and env variable names are duplicated by hand and have already drifted.

**Patterns in use** — handler → store interface → ranking service → templ (engine); a single `PolicyManager` over a `State` interface with Redis and memory implementations (spider); atomic claim with `FOR UPDATE SKIP LOCKED` (indexer); restricted-edge-set PageRank with temp tables and upsert (ranking). The patterns are applied consistently inside each service.

**Strengths**

- The crawler policy layer is a genuine single decision point (`Admit`, `Classify`, `Fetch`) with fail-closed state errors and injected clocks; its tests drive the real loop.
- The PageRank function derives one restricted `indexed → indexed` edge set and reuses it for numerator, denominator, and dangling mass; the comment names the exact rank leak it fixes.
- Engine error handling is centralized; SQL is parameterized; handler tests assert no DB error leakage.
- The indexer’s claim is a single atomic statement, and ack-after-success/nack policy is a small pure function with coverage of all variants.
- `02_pg_rank.sql` is unusually well-reasoned for a learning project, and the ranking integration suite pins conservation.

**Architectural risks**

- `services/indexer/src/core/psql.rs:59-70` — the sweep matches the post-success state, so success is indistinguishable from a dead worker. Impact: continuous corpus rebuild. Recommendation: explicit completion marker.
- `services/spider/internal/store/db.go:163-176` + `store.go:77-85` — edges are built from an unordered ID list and positional assumptions. Impact: corrupted link graph. Recommendation: `RETURNING id, url` and map by URL; one transaction.
- `services/spider/internal/parser/helpers.go:19-20` + `spider.go:225-227` — `og:url` becomes storage identity. Impact: index poisoning. Recommendation: identity is always the fetched URL.
- `services/ranking/src/messaging.py:23,75` + `services/engine/internal/store/store.go:96` — ranking only runs at 100 confirmations; engine inner-joins `page_rank`. Impact: small/fresh corpora return no results. Recommendation: configurable threshold plus a startup/periodic run, or a left join with a neutral score.

**Drift and documentation mismatch**

- `deploy.sh` still starts `services/monitoring`, deleted in `51c29cd` (`docs/MONITORING.md | 513 --- services/monitoring/... | 31 -`).
- Docs say `indexer.jobs` (`docs/ARCHITECTURE.md:22,35,48`); spider defaults to `indexer.job` (`config.go:138,202`).
- `docs/SPIDER.md:95-117` publishes a schema three columns stale and omits the reverse unique index.
- `README.md:66` and `services/README.md:39` link an untracked `docs/ARCHITECTURE.md`.
- Root `.env.example` advertises `PR_SCHEDULE`, `IDF_SCHEDULE`, `BACK_LINKS_RUN_ON_STARTUP`, `INDEXER_COUNT`, `LOOP_DELAY_MS`, `MAX_GO_ROUTINES` — none are read by any service.

---

## 4. Findings by Severity

Counts: **CRITICAL 3** · **HIGH 17** · **MEDIUM 25** · **LOW 8**

### CRITICAL

#### C-01 The indexer sweep un-indexes every successfully indexed page and deletes its word index

- **Location**: `services/indexer/src/core/psql.rs:59-70` (reclaim), `:114-119` (claim), `:235-241` (undo); `services/indexer/src/core/indexer.rs:227-277` (success path); `migration/01_schema.sql:14-18`
- **Evidence class**: `Verified`
- **Dimensions**: correctness, data, reliability
- **Explanation**: Claiming sets `indexed = TRUE, last_index_attempt_at = NOW()`. Success performs no `pages` write, and there is no `indexed_at`/`confirmed` column. The reclaim CTE later matches exactly that state and flips it back to `indexed = FALSE`, deleting every `page_word` row for the page.
- **Evidence**:
  ```sql
  UPDATE pages SET indexed = FALSE
  WHERE indexed = TRUE AND last_index_attempt_at IS NOT NULL
    AND last_index_attempt_at < NOW() - make_interval(secs => $1)
  ```
  A repo-wide grep finds `last_index_attempt_at` only in `psql.rs`, the schema, and docs. The integration test at `psql.rs:857-864` constructs the same state a successful index leaves, so it cannot distinguish success from a dead worker.
- **Impact**: every page is un-indexed and rebuilt every `SWEEP_INTERVAL_SECONDS=300` + `SWEEP_GRACE_SECONDS=600`; search loses pages during each window; `page_word`/`words` churn continuously; PageRank’s `indexed=TRUE` node set flaps, so scores and IDF keep moving.
- **Reachability**: default configuration, no special condition — the sweep is enabled on every deployment.
- **Recommendation**: add `indexed_at TIMESTAMP` set in the same transaction as the last `page_word` write (or set `last_index_attempt_at = NULL` on success) and reclaim only rows where `indexed_at IS NULL`. Add an integration test that indexes a page, runs the sweep, and asserts `indexed` and `page_word` are unchanged.
- **Verification**: re-read the claim/success/undo paths; confirmed no success write to `pages`; confirmed the sweep predicate; confirmed the default grace/interval in `config.rs:59-70`.

#### C-02 `deploy.sh` is guaranteed to abort because `services/monitoring` was deleted

- **Location**: `deploy.sh:108,130,156,189,237`; `services/` tree; `git show --stat 51c29cd`
- **Evidence class**: `Verified`
- **Dimensions**: infra/deployment, docs-vs-reality
- **Explanation**: `bootstrap_env "$SVC/monitoring"` no-ops because there is no `.env.example`. Later, `upsert_env_var "$MONITORING_ENV" ...` runs `grep` on a missing file, then `printf ... >> "$file"` into a nonexistent directory. `set -euo pipefail` exits. Even if that line were fixed, the `--restart` teardown, monitoring start, and health check all reference the same missing compose file/container.
- **Evidence**: `51c29cd` is titled “remove monitoring” and deletes 26 files under `services/monitoring/` plus `docs/MONITORING.md`; `ls services/` returns only `engine indexer ranking spider`.
- **Impact**: the only documented deployment entry point fails for every user before bringing up the application services.
- **Reachability**: `./deploy.sh` on a clean clone; the root `.env` is bootstrapped first, so the failing block is always reached.
- **Recommendation**: delete the monitoring bootstrap/rewrite/start/health blocks and make `deploy.sh` assert that each compose file it will use exists.
- **Verification**: static trace of `bootstrap_env` and `upsert_env_var`; commit tree check; directory listing.

#### C-03 A fetched page chooses its own URL identity, enabling index poisoning and metadata overwrite

- **Location**: `services/spider/internal/parser/helpers.go:19-20`; `services/spider/internal/spider/spider.go:225-227`; `services/spider/internal/store/db.go:100-110`
- **Evidence class**: `Verified`
- **Dimensions**: security, correctness/data integrity
- **Explanation**: `<meta property="og:url">` is mapped to `page.MetaData.URL`; the spider only substitutes the real URL when that field is empty. `Persist` then uses `page.URL` to upsert `urls` and `pages`, so a page can create or rewrite a row under a URL it never owned — including URLs the policy layer would refuse to fetch.
- **Evidence**: `getMetaProperty` strips `og:` and returns `url`; `mergeMeta` copies it; `if page.URL == "" { page.URL = rawUrl }` leaves attacker content in place; `INSERT ... ON CONFLICT (url_id) DO UPDATE SET metadata = EXCLUDED.metadata` rewrites an existing page’s title/description/keywords/locale.
- **Impact**: index poisoning, URL spoofing, and bypass of the robots/scope gate at the identity layer. A page hosted anywhere can make the engine display its content under a trusted URL.
- **Reachability**: any crawled page; no privilege beyond getting a link crawled.
- **Recommendation**: use `rawUrl` (post-canonicalization) as storage identity; accept `og:url` only as display metadata, and only when it is same-host/same-path. Do not let an upsert of a fetched page rewrite metadata of an existing row.
- **Verification**: read the parser → `mergeMeta` → `Persist` chain; confirmed the fallback condition and the `ON CONFLICT` clause.

### HIGH

#### H-01 Spider publishes to `indexer.job`; the indexer consumes `indexer.jobs`

- **Location**: `services/spider/internal/config/config.go:138,202`; `services/indexer/src/core/config.rs:54,134`; `services/indexer/.env.example:33`; `docs/ARCHITECTURE.md:22,35,48`
- **Evidence class**: `Verified`
- **Dimensions**: API/contracts, reliability
- **Evidence**: spider defaults `RABBITMQ_JOB_QUEUE` to `"indexer.job"`; no spider/root env file sets it (`grep -c '^RABBITMQ_JOB_QUEUE=' services/spider/.env` = 0). Indexer defaults `RABBITMQ_QUEUE` to `"indexer.jobs"` and its example sets `indexer.jobs`. Docs say `indexer.jobs`.
- **Impact**: spider’s messages land in an unconsumed queue; the indexer’s sweep eventually rescues pages after `SWEEP_GRACE_SECONDS`, so indexing is delayed ~10–15 min and the dead queue grows without bound (broker disk/memory pressure).
- **Recommendation**: choose one name, define it in one shared place, and use `RABBITMQ_QUEUE` on both sides; add a startup assertion or an integration test that publishes from a spider-shaped payload and consumes it.
- **Verification**: read both config loaders; enumerated all env keys; grepped docs.

#### H-02 Fresh bootstrap cannot start spider or ranking, and root RabbitMQ credentials are undefined

- **Location**: `services/spider/entrypoint.sh:12-14`; `services/spider/.env.example:1-31`; `services/ranking/.env.example:4,15-16`; `.env.example:1-65`; `docker-compose.yml:27-28`; `deploy.sh:96-99`
- **Evidence class**: `Verified`
- **Dimensions**: infra/deployment, security
- **Evidence**: spider requires `RABBITMQ_USER/PASSWORD/HOST`, but its example has no RabbitMQ section. Ranking’s example sets `PG_HOST=localhost` and `BROKER_URL=amqp://...@localhost:5672`, while its compose injects the file into a container; `localhost` is the container. Root `.env.example` omits `RABBITMQ_DEFAULT_USER/PASS`, which `docker-compose.yml` interpolates.
- **Impact**: `deploy.sh` copies the examples verbatim; spider exits at PID 1, ranking cannot reach Postgres/RabbitMQ, and the broker starts with empty credentials. The documented Quick Start is not viable.
- **Recommendation**: complete each example with every required key, use `psql`/`rabbitmq` in Docker-oriented examples, add `RABBITMQ_DEFAULT_*` to the root example, and validate required keys after bootstrap.
- **Verification**: read entrypoints, examples, compose interpolation, and `bootstrap_env`.

#### H-03 Ranking container cannot create `ranking.log`, so it exits before consuming

- **Location**: `services/ranking/src/main.py:42-50`; `services/ranking/Dockerfile:34-39`
- **Evidence class**: `Verified`
- **Dimensions**: reliability, deployment
- **Evidence**: `logging.FileHandler("ranking.log")` opens eagerly at `configure_logging()`. The image sets `WORKDIR /app` as root, copies only `main` with `--chown=app:app`, and runs `USER 10001:10001`; `/app` itself remains root-owned `0755`, so creating `ranking.log` raises `PermissionError`. `main()` catches it and exits 0.
- **Impact**: the shipped ranking artifact never runs; the container reports a clean exit and the `pgrep` healthcheck merely goes unhealthy.
- **Recommendation**: log to stdout only in the container (JSON stream handler), or gate a file sink on a writable, configurable directory; make sink failure loud or omit the sink.
- **Verification**: read `main.py`, Dockerfile ownership, and the eager `FileHandler` semantics.

#### H-04 No Postgres named volume; the documented `--restart` path destroys the index

- **Location**: `docker-compose.yml:5-6`; `deploy.sh:182-191`; `README.md:58-62`
- **Evidence class**: `Verified`
- **Dimensions**: data, infra/deployment
- **Evidence**: `psql` mounts only `./migration:/docker-entrypoint-initdb.d`; there is no `pgdata` volume. `deploy.sh --restart` runs `docker compose ... down --remove-orphans`, and README recommends `./deploy.sh --pull --build --restart` for a clean rebuild.
- **Impact**: the crawl corpus, words, `page_word`, and `page_rank` live in a container/anonymous volume and are not reliably preserved across recreate. The “clean rebuild” command is a data-loss command.
- **Recommendation**: add `pgdata:/var/lib/postgresql/data`, keep `down` (not `down -v`), and document the data-loss semantics of `--restart`.
- **Verification**: compose volume list; teardown path; README text. (Nuance: the Postgres image declares an anonymous volume, but Compose does not reliably reattach it on a new `up`; a named volume is required for deterministic persistence.)

#### H-05 Spider prints all datastore credentials to stdout on every start

- **Location**: `services/spider/internal/config/config.go:76`; `services/spider/internal/spider/spider.go:92`
- **Evidence class**: `Verified`
- **Dimensions**: security
- **Evidence**: `fmt.Printf("%+v\n", c)` prints `Config`, including `RabbitMq.Password`, `Store.DB.Password`, and `Store.Cache.Password`; `fmt.Printf("Spider initialized: %+v\n", s)` reprints the same struct through `s.config`. Both go to fd 1 (container logs), not the JSON logger.
- **Impact**: database, Redis, and broker credentials land in plaintext in `docker logs`, log shippers, and any operator/attacker with log access — a lateral-movement kit for the internal network.
- **Recommendation**: delete both prints; if diagnostics are needed, log a redacted struct at Debug.
- **Verification**: struct fields and `%+v` recursion; both call sites.

#### H-06 Graph edges are built from an unordered ID list and committed in two transactions

- **Location**: `services/spider/internal/store/db.go:163-176`; `store.go:65-93`; `store_test.go:34-44`
- **Evidence class**: `Verified`
- **Dimensions**: correctness/data, testing
- **Evidence**: `InsertURLs` ends with `SELECT u.id FROM input i JOIN urls u ON u.url = i.url` — no `ORDER BY`, no `url` column. `persistPage` assumes `ids[0]` is the page and `ids[1:]` are links. The fake DB fabricates order-preserving IDs, so the suite cannot catch it. The page is committed in one transaction and edges in a second; if edges fail, `Persist` still returns a non-nil `pageID`.
- **Impact**: the link graph — PageRank’s only input — is corrupted at an unknown rate; pages can be marked complete and published with missing edges.
- **Recommendation**: `RETURNING id, url` and map by URL in Go; put urls/pages/edges in one transaction and propagate the error.
- **Verification**: SQL text; positional use; mock behavior.

#### H-07 The crawler has no internal-address or redirect filtering (SSRF)

- **Location**: `services/spider/internal/policy/admit.go:21-89`; `services/spider/internal/utils/http.go:57-71`; `services/spider/internal/policy/hosts.go` (sitemap ingestion)
- **Evidence class**: `Verified`
- **Dimensions**: security
- **Evidence**: `Admit` validates scheme and URL shape only; there is no `ParseIP`/`IsPrivate`/`IsLoopback`/`DialContext` anywhere. `CheckRedirect` counts hops only. Sitemap entries are enqueued as absolute URLs. On the shared `search_net`, `https://redis:6379`, `https://psql:5432`, and broker/admin hosts are reachable.
- **Impact**: a crawled page or sitemap can make the spider issue requests into its own network, persist HTML responses into the index, and bypass the robots/scope gate for destinations. Canonicalization forcing HTTPS limits blind plaintext SSRF but not redirects.
- **Recommendation**: install a `DialContext` that rejects non-public IPs (re-resolve per connection to avoid rebinding), apply the same policy in `CheckRedirect`, and constrain sitemap/link hosts.
- **Verification**: grep for IP filtering; read `Admit`, `GetReq`, and sitemap resolution.

#### H-08 `Skip` verdicts carrying a future `Until` retire URLs permanently

- **Location**: `services/spider/internal/spider/spider.go:190-195`; `services/spider/internal/policy/policy.go:349-371`; `redis_state.go:40-57`
- **Evidence class**: `Verified`
- **Dimensions**: correctness, reliability
- **Evidence**: `hostGate` returns `Skip` with `Until` for dead/cold hosts; `admit.go:71-78` does the same for budget exhaustion. The spider ignores `verdict.Until` in the `Skip` branch and calls `Retire` → `MarkVisited`, an append-only `SAdd` with no TTL and no `SRem` anywhere.
- **Impact**: any URL popped while its host is transiently dead or over budget is blacklisted for the deployment’s lifetime; a 5,000-page host that hits the window loses its remaining URLs permanently.
- **Recommendation**: for `Skip` with a non-zero `Until`, `Park` instead of `Retire`; reserve retirement for terminal reasons.
- **Verification**: verdict constructors; `Skip` branch; `MarkVisited` implementation; absence of `SRem`.

#### H-09 Indexer publication is fire-and-forget: non-persistent, unconfirmed, and channel-per-crawl

- **Location**: `services/spider/internal/messaging/rabbitmq.go:60-76`; `spider.go:147-152,267-281`
- **Evidence class**: `Verified`
- **Dimensions**: reliability, messaging
- **Evidence**: `Publish` sets no `DeliveryMode` (transient) and no publisher confirms; returning `nil` only means the bytes reached the socket. The spider retires the URL before publishing, so a lost job is never retried. `crawl` opens and closes a new AMQP channel every round; 20 crawlers at 200 ms is ~100 channel objects/second.
- **Impact**: silent permanent index gaps; broker restart loses queued jobs; broker blips abort crawl rounds; channel churn pressures the broker.
- **Recommendation**: publisher confirms + `amqp.Persistent`, publish through an outbox (or before retire) so failure is retryable, and hold one long-lived channel per worker/process.
- **Verification**: publish options; call order in `crawl`; channel construction per call.

#### H-10 Indexer swallows confirmation-publish failure after acking the job

- **Location**: `services/indexer/src/core/indexer.rs:250-277`, `:152-156`; test `:611-625`
- **Evidence class**: `Verified`
- **Dimensions**: reliability, data
- **Evidence**: the publish error is logged, then `return Ok(())`; the handler acks the delivery. The page is already `indexed = TRUE`, so the sweep’s second query (`indexed = FALSE`) never revisits it, and there is no confirmation outbox.
- **Impact**: ranking never learns about the page; the corpus can go stale indefinitely, with only an error log line. The test explicitly codifies the silent drop.
- **Recommendation**: make the confirmation durable (outbox row committed with the word write, published by the sweep) or only ack after a confirmed publish and let the message requeue.
- **Verification**: publish error branch; `return Ok(())`; ack; sweep predicate.

#### H-11 A consumer-stream error or cancel exits without draining in-flight work; no restart policy

- **Location**: `services/indexer/src/core/indexer.rs:119-125`; `main.rs:58-72`; `services/indexer/docker-compose.yml:1-8`
- **Evidence class**: `Verified`
- **Dimensions**: reliability, concurrency
- **Evidence**: `result?` propagates out of `index_loop`; `main` calls `process::exit(1)`, skipping the 30 s drain and `indx.close()`. The `None` branch returns `Ok`, cancels the token, and exits 0. Neither compose file sets `restart:`.
- **Impact**: one broker blip or consumer cancel stops indexing permanently (or loses in-flight writes); a clean exit 0 is not restarted.
- **Recommendation**: retry `consume` with backoff, reserve process exit for startup failures, keep the drain path for every exit, and add `restart: unless-stopped`.
- **Verification**: `?` propagation; `process::exit`; compose files.

#### H-12 The attempt cap never counts crashes, so a crash-inducing page retries forever

- **Location**: `services/indexer/src/core/psql.rs:81,114-119,235-241`; `Cargo.toml` `panic = "abort"`
- **Evidence class**: `Verified`
- **Dimensions**: reliability, data
- **Evidence**: `index_attempts` is incremented only in `undo_indexing` (failure path). The claim and the reclaim CTE do not increment it. With `panic = "abort"`, a panic/OOM kills the worker before undo.
- **Impact**: a page that reliably aborts the process becomes an unbounded crash loop: claim → abort → restart → sweep resets claim (attempt still 0) → repeat. The documented “gives up after three” only covers soft failures.
- **Recommendation**: increment `index_attempts` in the claim (or reclaim) statement so every delivery counts; add a terminal failed state.
- **Verification**: all `index_attempts` writers; claim statement; release profile.

#### H-13 One shared cgo aspell speller is used concurrently with no synchronization

- **Location**: `services/engine/cmd/server/main.go:25`; `internal/service/spellchecker/spell-checker.go:16-38`
- **Evidence class**: `Verified` (sharing) / `Likely` (corruption)
- **Dimensions**: security/availability, concurrency
- **Evidence**: a single `AspellSpeller` is constructed once and shared by all handlers; it holds one `*C.AspellSpeller` from go-aspell, whose `Check`/`Suggest` call the C API directly. The library has no mutex, and Echo handlers run concurrently.
- **Impact**: aspell keeps mutable per-speller state; concurrent use can corrupt suggestions, leak C allocations, or SIGSEGV the whole process under normal overlapping searches.
- **Recommendation**: serialize `Check`/`Suggest` with a mutex (microseconds) or pool N independent spellers.
- **Verification**: one construction site; shared value; library source has no lock. (External thread-safety contract is in Appendix A.)

#### H-14 `totalPages` is a result count, so pagination advertises non-existent pages and dead-ends

- **Location**: `services/engine/internal/store/store.go:195-208`; `internal/handlers/search.handler.go:58,77`; `view/component/pagination.templ:25,29`
- **Evidence class**: `Verified`
- **Dimensions**: correctness, UX
- **Evidence**: `GetTotalPages` returns matching web pages; the view prints `currentPage / totalPages` and enables Next until equality. `PageSize` is only used as `LIMIT/OFFSET`; nothing divides the count. `result.templ:22` hides pagination when `len(pages)==0`.
- **Impact**: every query with >20 matches shows e.g. “1 / 57”; paging past the real last page returns zero rows and no navigation — the user is stuck. The header also says “About 20 results” (this page’s count).
- **Recommendation**: compute `totalPages = ceil(total/PageSize)` (or pass both totals and label them), and clamp `currentPage`.
- **Verification**: SQL count; view math; hide condition.

#### H-15 Reverse-edge unique index silently discards reciprocal links

- **Location**: `migration/01_schema.sql:72`; `services/spider/internal/store/db.go:138-144`
- **Evidence class**: `Verified`
- **Dimensions**: data/schema
- **Evidence**: `CREATE UNIQUE INDEX idx_graph_edges_unique_revese ON graph_edges(to_url, from_url)` plus `ON CONFLICT DO NOTHING`. If `a → b` exists, inserting `b → a` violates the reverse index and is dropped without error.
- **Impact**: PageRank’s input is directionally incomplete; reciprocal links (navigation, blogrolls, cross-references) are common, and the loss is silent. Docs list only `UNIQUE(from_url,to_url)`, so the extra index is undocumented.
- **Recommendation**: drop the reverse index; keep `UNIQUE(from_url,to_url)` and, if dedupe is intended, count drops.
- **Verification**: schema line; insert statement; docs.

#### H-16 Every datastore is published on `0.0.0.0` with example credentials

- **Location**: `docker-compose.yml:11-12,18-19,29-31,38-39`; `.env.example:9-10`; `services/ranking/.env.example:15-16`; `deploy.sh:270-272`
- **Evidence class**: `Verified` (config) / `Unverified` (host firewall)
- **Dimensions**: security
- **Evidence**: Postgres `${PG_PORT}:5432`, Redis `${REDIS_PORT}:6379` with no password, RabbitMQ `5672/15672`, Adminer `8080`; defaults are `admin/1234` and `admin:admin`. `deploy.sh` prints `Adminer : http://<VPS_IP>:8080` as a feature and configures no TLS/firewall/reverse proxy.
- **Impact**: on the VPS deployment the script targets, Postgres, unauthenticated Redis, RabbitMQ management, and an arbitrary SQL UI are reachable from the internet with example credentials.
- **Recommendation**: bind Adminer/management to `127.0.0.1`, remove host port mappings for internal datastores, set a Redis password, generate non-default credentials during bootstrap, and put the dashboard behind TLS.
- **Verification**: port mappings; env examples; deploy output lines.

#### H-17 Engine inner-joins `page_rank`, but ranking only runs at 100 confirmations with no startup/timer path

- **Location**: `services/engine/internal/store/store.go:96`; `services/ranking/src/messaging.py:23,75`; `services/ranking/src/main.py:105`
- **Evidence class**: `Verified`
- **Dimensions**: API/contracts, correctness
- **Evidence**: `GetData` requires a `page_rank` row (`INNER JOIN page_rank`). `MessagingService` defaults `max_indexer_pages=100` and only calls `run_pipeline` from `record_confirmation`; there is no startup run, no timer, and `BACK_LINKS_RUN_ON_STARTUP`/`PR_SCHEDULE` are dead env vars.
- **Impact**: a fresh deployment with fewer than 100 indexed pages returns zero results even though `words`/`page_word` exist. Lost confirmations (H-10) can starve ranking permanently.
- **Recommendation**: make the threshold configurable, run an initial pipeline at startup (or on a timer), and/or `LEFT JOIN page_rank` with a neutral score so unranked pages remain searchable.
- **Verification**: join; threshold; only call site; dead-env grep.

### MEDIUM

- **M-01** — `services/engine/view/page/result/result.templ:8-24`, `tab/all.templ:7` — the htmx branch wraps a second `<div id="results">`, and pagination is emitted as a sibling after the layout; header search nests duplicate IDs and detaches pagination. Impact: invalid swaps/ambiguous selectors. Recommendation: emit only the inner list for htmx and keep pagination inside the layout. `Verified` (static).
- **M-02** — `services/engine/internal/service/ranking/math-util.go:11-12`, `tf-idf.go:64-66` — a zero-magnitude cosine yields `NaN`; `normalizeTFIDF` propagates it and the comparator returns 0 for every pair, so results order by map iteration. Ranking clamps `idf` to 0 for ubiquitous words (`calculator.py:26-29`), so a single-term query for such a word triggers it. Recommendation: return 0 on zero magnitude and guard `NaN`/`dom==0`. `Verified` (path), `Likely` (trigger).
- **M-03** — `services/engine/internal/store/store.go:101-106`, `service/ranking/ranking.go:33` — the fine-grained blend runs after the SQL `LIMIT/OFFSET`; page 1 is chosen by `word_count`, not by the blend. Impact: globally better results can never surface. Recommendation: blend in SQL or widen the candidate window (`RANKER_MAX_RESULTS`, currently dead). `Verified`.
- **M-04** — `services/engine/internal/handlers/search.handler.go:41`, `spell-checker.go:33-38`, `cmd/server/main.go:41` — query length/token count is unbounded; each token costs up to four cgo calls; `fmt.Printf` logs the raw query and `RequestLogger` logs the full URI. Impact: CPU DoS from one IP and search terms in logs. Recommendation: cap query length/tokens, move spellcheck after tab dispatch, remove the `Printf`, log path only. `Verified`.
- **M-05** — `services/engine/internal/config/ranker/ranker.config.go:6-7`, `ranking.go:33` — `RANKER_WEIGHT_TF`/`RANKER_MAX_RESULTS` are loaded/exported/documented but never read; the blend is a literal `0.5`. `PAGE_SIZE=0` yields empty 200s. Recommendation: wire them and validate `PageSize >= 1`. `Verified`.
- **M-06** — `services/engine/internal/store/store.go:42-53` — DSN is built by `fmt.Sprintf` with unescaped password, `sslmode=disable` hardcoded, and `Ping`/queries have no timeout. Impact: special-character passwords break startup; no TLS; black-hole host blocks boot. Recommendation: `url.URL`/keyword escaping, `PG_SSLMODE`, `PingContext` + `connect_timeout`/`statement_timeout`. `Verified`.
- **M-07** — `services/ranking/src/main.py:80-92,108-111` — SIGTERM closes the DB pool but leaves `app.run()` awaiting the broker; faststream 0.7.7 installs no signal handler. Impact: process never exits, then consumes/acks with a closed pool. Recommendation: stop broker/app, drain tasks/lock, then close pool. `Verified`.
- **M-08** — `services/ranking/src/calculator.py:118-123`, `messaging.py:73-88,108-121` — `gather` does not cancel a sibling on failure, and the counter is zeroed before the work while confirmations are acked immediately. Impact: duplicate concurrent pipelines and permanently lost batches on failure/restart. Recommendation: `TaskGroup` or await both; persist the counter/reset after success; ack after completion or add a DLX. `Verified` (mechanism), `Likely` (impact).
- **M-09** — `migration/02_pg_rank.sql:41-46,130-149` — `temp_page_rank` is populated once, but iterations re-read mutable `pages`; under READ COMMITTED a page unindexed mid-run stays in the temp table with its initial score and is upserted. Impact: rank conservation only holds when indexing is quiescent. Recommendation: `REPEATABLE READ` or drive iterations off the captured temp sets. `Verified` (mechanism), `Likely` (reachability).
- **M-10** — `services/ranking/entrypoint.sh:5-12`, `psql.py:94-100` — entrypoint requires all `PG_*` and both broker vars, although the app accepts `DATABASE_URL` and either broker var; `pool.open()` defaults to `wait=False`, so a bad DSN reports “pool opened” and fails later. Recommendation: match the app contract and `open(wait=True)`. `Verified`.
- **M-11** — `services/ranking/src/messaging.py:25-29`, `psql.py:87-92`, `messaging.py:63` — broker falls back to `admin/admin`, Postgres to `postgres`/empty password; both URIs are unescaped; the full confirmation body is logged. Recommendation: fail closed, use `make_conninfo`/`quote`, log only message id/queue. `Verified`.
- **M-12** — `migration/02_pg_rank.sql:20-22,157-158` — `IF node_num = 0 THEN RETURN` skips the `DELETE FROM page_rank WHERE url_id NOT IN (...)`, so a fully unindexed corpus keeps stale ranks. Recommendation: clean before the early return. `Verified`.
- **M-13** — `services/indexer/src/core/psql.rs:180,213-221,429-440` — `batch_words` runs word upserts and each `page_word` chunk as independent autocommit statements; `undo_indexing` is two statements. Impact: partial page state if the process dies between chunks. Recommendation: one transaction per page. `Verified`.
- **M-14** — `services/indexer/src/core/psql.rs:59-69`; `indexer.rs:192-248` — the reclaim predicate is purely time-based; a page indexing longer than `SWEEP_GRACE_SECONDS` is reset and its `page_word` deleted mid-write, while a second worker can claim it. Recommendation: claim generation/token and conditional delete. `Verified` (mechanism), `Likely` (reachability).
- **M-15** — `services/indexer/src/core/indexer.rs:167,207,245` — requeue has no backoff/DLQ; a failed `undo_indexing` converts a permanent parse failure into a requeueable database error. Recommendation: retry claims with delay, preserve the original error, add a DLQ/terminal state. `Verified`.
- **M-16** — `services/indexer/src/core/psql.rs:59-89`; `migration/01_schema.sql:63-73` — the sweep filters/orders on unindexed `pages` columns twice per tick; there is no index on `(indexed, last_index_attempt_at)` or `updated_at`. No cap on HTML size or distinct vocabulary. Impact: sequential scans of a TOAST-heavy table and unbounded per-page memory/round trips. Recommendation: add partial indexes and caps. `Verified`.
- **M-17** — `services/spider/internal/policy/classify.go:40-48`, `utils/http.go:89-94` — a truncated body read returns body + error, but classification checks status first, so a 200 with a mid-stream reset is `ActSuccess`. Impact: truncated pages are indexed, hosts credited, never retried. Recommendation: check `out.Err` before status and require EOF under the cap. `Verified`.
- **M-18** — `services/spider/internal/policy/admit.go:177-196`, `hosts.go:95-146,393-435`; `redis_state.go:40-57` — per-link admission is a serial 3–4 round-trip loop that can fetch robots/sitemaps inline; `visited` and `stats` grow without TTL. Impact: link-dense pages exhaust the crawl deadline and lose links; Redis grows monotonically. Recommendation: batch gates, resolve robots/sitemaps off-path, retain/expire `visited`/`stats`. `Verified`.
- **M-19** — `migration/01_schema.sql:3`; `docker-compose.yml:5-6`; `deploy.sh` — schema is applied only by `docker-entrypoint-initdb.d` on an empty `PGDATA`; there is no migration runner, and `CREATE TABLE` is not idempotent. Impact: new migrations never apply to an existing DB; re-running by hand fails. Recommendation: versioned migrator or idempotent bootstrap + explicit deploy step. `Verified`.
- **M-20** — `services/engine/Dockerfile:1`; `services/spider/Dockerfile:9`; `.gitignore:15`; `docker-compose.yml:3,16,23,36`; no `.dockerignore` — floating tags (`node:current`, `adminer`, `rabbitmq:management-alpine`), unpinned `templ@latest`, ignored `Cargo.lock` for a binary, `>=` Python requirements, and multi-GB/`.env` build contexts. Impact: non-reproducible builds and credential-bearing context. Recommendation: pin tags/digests, track `Cargo.lock`, pin requirements, add per-service `.dockerignore`. `Verified`.
- **M-21** — `.github/workflows/*_ci.yaml` — no `golangci-lint`, `gofmt`, `cargo clippy`, `ruff`, coverage threshold, `docker compose config`, `shellcheck`, or SQL validation; CD builds `:latest` without running CI. Impact: the deploy/monitoring regression shipped because nothing validates it. Recommendation: add lint/format/coverage and an infra CI job; gate CD. `Verified`.
- **M-22** — `docker-compose.yml:2-21`; `services/indexer/docker-compose.yml:1-8`; `services/ranking/docker-compose.yml:1-8`; `deploy.sh:195-203` — only rabbitmq/adminer/engine/spider set restart; psql/redis/indexer/ranking do not; `up -d` has no health-gated dependency and the only post-start check is the removed monitoring endpoint. Impact: partial outages after reboot or a RabbitMQ race. Recommendation: restart every service, add healthchecks/`depends_on`, verify each container. `Verified` (config), `Likely` (race).
- **M-23** — `services/indexer/Dockerfile:10-13`; `indexer_cd.yaml` — `RUSTFLAGS="-C target-cpu=native"` in a CD-built image can emit instructions absent on the deploy host. Recommendation: baseline `x86-64-v2` or build per target. `Verified` (flag), `Likely` (SIGILL).
- **M-24** — `README.md:66`, `services/README.md:39`, untracked `docs/ARCHITECTURE.md` — the branch’s documentation addition links a file absent from git; if committed as-is, both links 404. Recommendation: add the file in the same commit and add a link checker. `Verified`.
- **M-25** — `services/spider/internal/spider/spider.go:162-177,261-265` — `PopFrontier` is `zpopmax`; any post-pop failure (Admit/Classify/Persist) returns without re-enqueueing, and the “leaving it crawlable” log is false. Impact: at-most-once delivery for the frontier; silent work loss. Recommendation: re-park on every error path or model an in-flight set with visibility timeout. `Verified`.

### LOW

| ID | Location | Issue | Recommendation |
|---|---|---|---|
| L-01 | `services/engine/cmd/cli/main.go:43-52` | Prints a query error then dereferences nil `*store.Data`; CLI panics when the DB is down. | `return` after the error; handle `config.LoadConfig` errors in `main`. |
| L-02 | `services/engine/view/component/page_card.templ:14,69-79` | `page.URL[0]` panics on an empty URL; icon URLs from crawled metadata are trusted for `<img src>`. | Guard empty URL; allow only validated HTTPS icons or drop remote icons. |
| L-03 | `services/spider/internal/policy/hosts.go:119-122`, `classify.go:287-293` | robots `Crawl-delay` is parsed and stored but never enforced per host; 20 workers can hit one origin concurrently. | Add per-host pacing/last-fetch gate and cap per-host concurrency. |
| L-04 | `services/ranking/src/main.py:121-124` | Fatal startup errors log and exit 0; no orchestrator restarts the service. | Re-raise or `sys.exit(1)`. |
| L-05 | `services/indexer/src/core/psql.rs:86,219,354`; `indexer.rs:27,315` | `MAX_RETRIES` only reaches one function; sweep cap is literal 500; `WORD_BATCH_SIZE=0` panics inside an async task with `panic=abort`. | Validate ranges at startup and route limits through config. |
| L-06 | `services/indexer/src/core/psql.rs:340` | Comment claims chunking is needed for the 65,535 bind-parameter limit, but the code binds arrays; the stated reason is false. | State the real constraint (statement size/memory). |
| L-07 | `migration/01_schema.sql:68` | `idx_page_word_word_page (word_id,page_id)` duplicates the `(page_id,word_id)` primary key, adding write cost. | Drop the redundant index. |
| L-08 | `migration/01_schema.sql:49-53`; root `.env.example:48-49,63-65`; `.gitignore:10-11` | `monitor_state` is dead; `INDEXER_COUNT`/`LOOP_DELAY_MS`/`PR_SCHEDULE`/`IDF_SCHEDULE`/`BACK_LINKS_RUN_ON_STARTUP`/`MAX_GO_ROUTINES` are unread; broad `*.txt`/`*.svg` ignores can hide real files. | Drop dead table/vars; narrow ignore patterns. |

---

## 5. Findings by Component

| Component / scope | Files | Critical | High | Medium | Low | Reviewer |
|---|---|---|---|---|---|---|
| Engine (Go/Echo) | 27 + 13 templ | 0 | 4 | 6 | 2 | `go-reviewer` |
| Spider (Go) | 31 / 52 | 1 | 5 | 4 | 1 | `go-reviewer` |
| Indexer (Rust) | 10 / 10 | 1 | 3 | 4 | 2 | `rust-reviewer` |
| Ranking (Python) | 18 / 18 | 0 | 3 | 6 | 1 | `python-reviewer` |
| Delivery / docs / schema | 38 / 40 | 1 | 5 | 5 | 2 | `code-reviewer` |
| Cross-component | — | 0 | 1 | 0 | 0 | orchestrator |

### Engine

- **Purpose**: public search UI over the ranked corpus.
- **Health**: strong error handling, SQL parameterization, and tests; pagination/htmx and shared-speller concurrency are the weak points.
- **Key findings**: H-13, H-14, H-17, M-01…M-06.
- **Notable strength**: typed `AppError` with client-safe messages; deterministic tie-breaking; real integration tests.

### Spider

- **Purpose**: crawl, parse, persist pages/edges, publish index jobs.
- **Health**: the policy layer is well designed; the seams (identity, graph edge ordering, frontier durability, publication) are not.
- **Key findings**: C-03, H-05…H-09, M-17, M-18, M-25.
- **Notable strength**: fail-closed state errors, Lua atomicity, overflow-safe backoff, strong policy tests.

### Indexer

- **Purpose**: claim pages, parse HTML, write `words`/`page_word`, confirm.
- **Health**: careful parsing and claim; the state machine has no success marker, and failure paths silently lose work.
- **Key findings**: C-01, H-10…H-12, M-13…M-16.
- **Notable strength**: `FOR UPDATE SKIP LOCKED` claim; ack-after-success; parameterized array SQL; SQLx offline cache.

### Ranking

- **Purpose**: compute IDF and PageRank from confirmations.
- **Health**: the PageRank function is the best-reasoned code in the repo; process lifecycle and fleet semantics are weak.
- **Key findings**: H-03, H-17, M-07…M-12.
- **Notable strength**: restricted edge set and dangling redistribution with a documented conservation proof.

### Delivery / docs / schema

- **Purpose**: deploy, configure, migrate, document.
- **Health**: broken against the current tree; examples do not boot the stack; security defaults are unsafe for a VPS.
- **Key findings**: C-02, H-01, H-02, H-04, H-15, H-16, M-19…M-24.
- **Notable strength**: non-root images, explicit entrypoint validation, isolated test DB, `.gitignore` blocks real secrets.

---

## 6. Cross-Component and Integration Issues

1. **Queue contract drift** (H-01): `indexer.job` vs `indexer.jobs`; no shared constant or integration test. The sweep masks it, so it presents as latency and queue growth rather than a clean failure.
2. **Env/config drift** (H-02, M-05, M-23): the same concepts are named differently across services (`RABBITMQ_QUEUE` vs `RABBITMQ_JOB_QUEUE`, `BROKER_URL` vs `RABBITMQ_URL`, `PG_*` vs `DATABASE_URL`), and root env vars advertise knobs no service reads.
3. **Index lifecycle ownership** (C-01, H-10, H-11, H-12): the indexer owns `pages.indexed`, but no other service can tell whether a page is “claimed”, “indexed”, or “failed”. Ranking and engine both depend on that distinction.
4. **Graph identity/integrity** (C-03, H-06, H-15): spider writes `urls/pages/graph_edges`; ranking consumes them. Identity spoofing, unordered edge construction, and the reverse unique index all corrupt the same input.
5. **Ranking trigger vs engine query** (H-17): engine requires `page_rank`; ranking only produces it at 100 confirmations. Small corpora are unsearchable.
6. **IDF corpus vs PageRank node set** (M-02, M-09): IDF uses `COUNT(*) FROM pages` (all pages) while PageRank uses `indexed=TRUE`. The two scores are computed over different populations; the engine blends them as if they were comparable.
7. **Deployment ordering** (M-22): indexer/ranking have no restart policy and no health-gated dependency on RabbitMQ; a broker race leaves them exited while `deploy.sh` reports success (once C-02 is fixed).
8. **Documentation/test coverage of seams**: no test starts spider→indexer→ranking together; the queue mismatch, `og:url` identity, and edge ordering are all invisible to the current suites.

---

## 7. Security and Performance Concerns

### Security posture

- **Trust boundaries**: the crawler consumes arbitrary web content; the engine is public; datastores are on a shared Docker network but also published to the host. There is no authn/authz layer by design; the risk is boundary enforcement, not missing login.
- **Authentication/authorization**: none on engine (public, read-only), Adminer, or RabbitMQ management. Adminer is a full SQL UI.
- **Input handling**: SQL is parameterized in all services; templ escapes text and uses `templ.URL` for result links. Untrusted paths remain: `og:url` as identity, icon URLs into `<img src>`, and crawler destinations.
- **Secrets/config**: no tracked secrets (`git ls-files` confirms; `.env` untracked). But spider prints all passwords; ranking falls back to `admin/admin`; examples ship `admin/1234`; datastores are exposed.
- **Dependency risk**: floating base images, ignored `Cargo.lock`, `templ@latest`, `>=` Python requirements, no `.dockerignore`.
- **Findings**: C-03, H-05, H-07, H-16, M-06, M-11, M-20.

### Performance and scalability

- **Hot paths**: engine search aggregation/`OFFSET`; indexer sweep full scans and full-corpus reindex; spider channel churn and per-link admission; ranking full-table rewrites.
- **Data access**: no index on `pages(indexed,last_index_attempt_at)`/`updated_at`; `temp_graph_edges` unindexed; `page_word(word_id,page_id)` redundant with the PK.
- **Resource limits**: crawler body cap 10 MB but no per-document token cap; engine query unbounded; ranking pool shared by IDF, PageRank, and the consumer; no `statement_timeout` in engine/ranking.
- **Observed/inferred bottlenecks**: ~100 AMQP channels/s at default spider settings; two full `pages` scans per sweep tick; 20 fixed PageRank passes over unindexed temp joins; engine aggregates every match before `LIMIT`.
- **Findings**: C-01, H-09, H-14, M-01, M-03, M-04, M-16, M-18, M-23.

---

## 8. Testing and Documentation Gaps

### Testing

- **Existing suites**: engine unit tests across handlers/ranking/spellchecker/store/util plus build-tagged DB integration; spider large policy/parser/store suites; indexer 70 unit + 19 DB-ignored; ranking 76 unit + integration.
- **Critical untested paths**: spider `messaging` (0%) and `store` (20%); engine `config` (0%); ranking `DatabaseManager`/DSN and the FastStream subscriber; indexer success-state sweep (the test constructs the same state as a dead worker); `deploy.sh` and compose/env validation.
- **Quality issues**: ranking `FakeDBManager` hands out the same connection, so the “concurrent gather” test cannot detect sequencing; `test_pagerankHandlesDuplicateEdges` inserts one edge; several IDF tests assert SQL substrings rather than behavior.
- **Recommended additions**: an end-to-end pipeline test (spider payload → indexer → confirmation → ranking → engine query); a sweep test that indexes successfully then asserts no reclaim; a subscriber ack/requeue test; a `deploy.sh`/`docker compose config` CI job; a query that matches a ubiquitous word to pin ranking determinism.
- **Checks run**:

| Command | Result |
|---|---|
| `go vet ./...` (engine, spider) | pass |
| `go test ./internal/... -count=1` (engine) | pass |
| `go test ./internal/... -count=1` (spider) | pass |
| `go test -cover ./internal/...` (engine) | handlers 83.6%, ranking 91.2%, store 61.2%, config 0% |
| `go test -cover ./internal/...` (spider) | policy 78.7%, spider 59.6%, store 20.0%, messaging 0% |
| `cargo fmt --check` | clean |
| `cargo test` | 70 passed, 19 DB-ignored |
| `pytest -m "not integration"` | 76 passed |
| `pytest --cov=src` | 81% total; `psql.py` 57% |

### Documentation

- **Accurate and useful**: per-service docs are detailed; the PageRank and indexer docs explain the hard parts; entrypoint/README setup is mostly clear.
- **Stale or contradicted**: `deploy.sh`/README still describe monitoring; docs say `indexer.jobs` while spider uses `indexer.job`; `docs/SPIDER.md` schema is stale; root env docs list dead variables; `services/README.md` still says `docker-compose up` (V1) while the project requires Compose V2.
- **Missing**: an operations runbook for deploy/rollback/migrations; a “data retention and re-crawl” policy; a security note on exposed ports/credentials; a contract document for queue/payload/env.
- **Recommended updates**: add `docs/ARCHITECTURE.md` to git; rewrite the deploy/monitoring sections; document the actual schema; add a DEPLOYMENT/OPERATIONS doc; add a link checker.

---

## 9. Recommendations

1. **Add an explicit index-completion state and repair the sweep** — resolves C-01, H-10, H-12, M-13, M-14 — effort **M** — risk **medium**.
2. **Repair the deploy/bootstrap contract** — remove monitoring, unify queue/env names, complete the examples, add a Postgres volume, log to stdout — resolves C-02, H-01, H-02, H-03, H-04 — effort **M** — risk **low**.
3. **Harden the crawler trust boundary** — `rawUrl` identity, `og:url` display-only, internal-IP/redirect filtering, remove credential prints — resolves C-03, H-05, H-07 — effort **M** — risk **medium**.
4. **Make publication and confirmation durable** — persistent messages, publisher confirms, outbox/DLQ, ack-after-confirm — resolves H-09, H-10, H-11 — effort **M** — risk **medium**.
5. **Fix engine relevance and pagination** — page count, htmx structure, blend before slice, NaN guard, query caps — resolves H-14, H-17, M-01…M-04 — effort **M** — risk **medium**.
6. **Fix graph and schema integrity** — drop the reverse index, order/map edge IDs, one transaction, migration runner, partial indexes — resolves H-06, H-15, M-16, M-19 — effort **M** — risk **medium**.
7. **Harden ranking lifecycle** — writable/stdout logging, signal shutdown, `TaskGroup`, persisted counter, `open(wait=True)`, entrypoint contract — resolves H-03, M-07…M-12 — effort **S/M** — risk **low**.
8. **Harden the delivery surface** — bind datastores to localhost, generate non-default credentials, pin images/deps, add `.dockerignore`, lint/coverage/infra CI, restart policies — resolves H-16, M-20…M-23 — effort **M** — risk **low**.

---

## 10. Prioritized Action Plan

| Priority | Action | Findings | Effort | Risk if deferred |
|---|---|---|---|---|
| **P0 — now** | Add `indexed_at`/success marker; reclaim only unfinished claims; stop deleting `page_word` on success. | C-01 | M | Continuous index destruction; search gaps every 10–15 min; rank/IDF churn. |
| **P0 — now** | Remove `services/monitoring` from `deploy.sh`; make missing compose files fatal. | C-02 | S | No documented deployment path works. |
| **P0 — now** | Use fetched `rawUrl` as page identity; treat `og:url` as display-only. | C-03 | S | Any crawled page can poison another URL’s index entry. |
| **P1 — this cycle** | Unify `indexer.jobs`; complete spider/ranking/root env examples; add `pgdata`; make ranking log to stdout. | H-01, H-02, H-03, H-04 | M | Fresh clone leaves spider/ranking dead; `--restart` destroys the index. |
| **P1 — this cycle** | Add persistent publishes + confirms + outbox; increment attempts on claim; retry consumer and drain on exit; add restart policies. | H-09, H-10, H-11, H-12, M-15 | M | Lost index jobs, permanent ranking starvation, crash loops. |
| **P1 — this cycle** | Block internal IPs/redirects; remove `%+v` prints; bind datastores to localhost; set Redis password. | H-05, H-07, H-16 | M | Credential leakage, SSRF into datastores, internet-exposed DB. |
| **P2 — next cycle** | Fix edge ID mapping/transaction; drop reverse unique index; add partial indexes/migration runner. | H-06, H-15, M-16, M-19 | M | Corrupted graph, missing reciprocal links, schema drift. |
| **P2 — next cycle** | Fix engine page count, htmx structure, NaN guard, blend-before-slice, query caps. | H-13, H-14, H-17, M-01…M-04 | M | Dead-end pagination, random results, relevance misses, speller crash risk. |
| **P2 — next cycle** | Pin images/deps, track `Cargo.lock`, add `.dockerignore`, lint/format/coverage/infra CI, gate CD. | M-20, M-21, M-23 | M | Non-reproducible builds; deploy regressions ship unnoticed. |
| **P3 — backlog** | Ranking snapshot/counter/pool hardening; docs truth pass; dead code/env cleanup; LOW items. | M-07…M-12, M-24, L-01…L-08 | S/M | Operational fragility and documentation drift. |

**Suggested sequencing** — fix C-01 first (it changes the data model), then C-02/H-01/H-02/H-03 (so the stack can start), then C-03/H-05/H-07 (so the crawler can be trusted). Do not run the indexer unattended until C-01 is fixed; do not expose the VPS ports until H-16 is fixed.

**Explicitly not recommended** — do not “fix” the queue mismatch by renaming only the indexer (the spider’s dead queue still grows); do not remove `page_word` deletes without adding a completion marker (the sweep will still reclaim); do not add a reverse-proxy auth layer before fixing SSRF/identity (it does not address index poisoning).

---

## 11. Appendices

### A. Risks to validate

| Risk | Location | Why suspected | How to settle |
|---|---|---|---|
| aspell C API is not thread-safe per speller | `spell-checker.go:16-38`; go-aspell source | Shared `*C.AspellSpeller`, no mutex | Load-test 50 parallel `/search?query=helo`; check aspell docs; run with `-race` (cgo race detection is limited). |
| `uuid` is a stdlib package only in the build toolchain | `spider` imports; `go.mod` | `go list` resolves to `/usr/lib/go/src/uuid`; header says Go 2026 | Build in a clean `golang:1.27` container in CI; if absent, pin `github.com/google/uuid` and update calls. |
| Sweep churn is observable in the live DB | `psql.rs:59-70` | No success marker | Count `page_word` rows for a page across two sweeps; watch `index_attempts=0 AND indexed=FALSE` growth. |
| `og:url` poisoning is reachable through `html.Parse` | `helpers.go:19-20` | Static chain | Craft `<meta property="og:url" content="https://victim/">`, parse, persist to a test DB, inspect `urls/pages`. |
| Unordered IDs actually mis-map on real Postgres | `db.go:163-176` | No `ORDER BY` | Insert a page + 50 links in a test DB; assert `ids[0]` matches `page.URL` across repeated runs/plans. |
| Host firewall mitigates exposed ports | `docker-compose.yml` | Config binds `0.0.0.0` | Inspect the VPS firewall/security group; do not assume. |
| Multiple ranking replicas / rolling restarts | `messaging.py` | In-memory counter | Check compose/K8s replica count; run two replicas and observe duplicate pipelines. |
| Large pages exceed `SWEEP_GRACE_SECONDS` | `psql.rs:59-69` | Time-based reclaim | Measure p99 indexing duration per page; compare to 600 s. |
| Registry tags resolve | Dockerfiles/compose | Floating tags | `docker manifest inspect` in a networked environment. |

### B. Disputed items

None. The only major disagreement (the `uuid` import) was resolved by direct inspection and is recorded as refuted.

### C. Refuted findings

| Finding | Why it was refuted |
|---|---|
| Spider `[CRITICAL]` “committed module cannot resolve its `uuid` import” (`spider.go:13`, `store.go:8`, `db.go:10`) | In this toolchain, `uuid` **is** a standard-library package: `go list -f '{{.ImportPath}} -> {{.Dir}}' uuid` returns `uuid -> /usr/lib/go/src/uuid`; `go vet ./...` and `go test ./internal/...` both pass. The reviewer assumed `uuid.Nil()`/`uuid.New()` require `github.com/google/uuid`. No `replace`/vendor is involved. **Caveat**: confirm the CI toolchain provides stdlib `uuid`; if it does not, pin `github.com/google/uuid` and adapt the calls. |

### D. Skill gaps

| Capability needed | Why no existing skill covered it | Suggested reusable skill |
|---|---|---|
| Frontier/queue at-least-once semantics (lease, visibility timeout, re-delivery) | `redis-patterns` covers TTL/atomicity, not work-queue durability | `distributed-queue-semantics` |
| Iterative PL/pgSQL algorithm review (snapshot isolation, temp-table lifecycle, conservation) | `postgres-patterns` covers indexes/transactions, not iterative functions | `postgres-plpgsql-review` |
| FastStream/psycopg_pool ack and lifecycle semantics | Version-dependent library behavior; no skill | `python-messaging-runtime` |
| Crawler compliance/trust boundaries (robots crawl-delay, sitemap trust, `og:canonical` identity) | No crawler-specific skill | `crawler-compliance-review` |
| Docker/compose/actionlint/hadolint validation | Docker execution prohibited; tools not assumed installed | `infra-static-validation` |

### E. Review methodology and limits

- **Decomposition**: workspace/services (one scope per deployable) plus one delivery/docs/schema scope; the orchestrator ran the cross-component pass.
- **Reviewer assignments**: engine `go-reviewer` (`golang-patterns`, `postgres-patterns`, `error-handling`); spider `go-reviewer` (`golang-patterns`, `redis-patterns`, `error-handling`); indexer `rust-reviewer` (`rust-patterns`, `rust-testing`, `postgres-patterns`); ranking `python-reviewer` (`python-patterns`, `python-testing`, `postgres-patterns`, `error-handling`); delivery `code-reviewer` (`docker-patterns`, `deployment-patterns`, `security-review`, `database-migrations`).
- **Sampling**: all high-risk paths read in full; some low-risk utility/test files were sampled via targeted grep rather than line-by-line (spider tests beyond `spider_test.go`/`store_test.go`, engine generated templ, vendored deps).
- **Commands executed**: `git rev-parse/status/diff/show`, `go vet`, `go test` (±cover), `cargo fmt --check`, `cargo test`, `pytest` (±cov), `grep/rg`, `wc`, `du`, `git check-ignore`, `git ls-tree`. No docker, no live DB/broker, no network.
- **Contract check**: HEAD unchanged (`51c29cd`); tracked diff unchanged (`README.md` +1, `services/README.md` +1); **new untracked `services/ranking/.coverage`** created by the coverage check → **minor contract violation**, report marked untrusted pending confirmation. No tracked project file was modified.
- **Known limitations**: no runtime verification of browser/htmx behavior, aspell thread safety, image-tag existence, or live SQL plans; findings depending on those are labeled `Likely`/`Unverified` and listed in Appendix A.

---

**Bottom line**: the codebase is a credible learning search engine with unusually good tests and comments, but it is **not operable as documented**. Fix the index completion/sweep state machine first, then the deploy/bootstrap contract, then the crawler trust boundary. Until then, treat `./deploy.sh`, unattended indexing, and any VPS deployment as unsafe.
