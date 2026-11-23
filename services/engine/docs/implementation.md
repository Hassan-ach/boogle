# Engine implementation

**Last updated:** 2026-11-23

This page documents the code paths behind [the HTTP interface](api.md): query normalization, the two SQL statements, in-memory ranking, view rendering, build inputs, and verified limitations.

## Source layout

| Path | Responsibility |
|---|---|
| `services/engine/cmd/server/main.go` | Startup, middleware, routes, `e.Start(":1323")` |
| `services/engine/cmd/cli/main.go` | One-shot ranking probe; not built into the image |
| `services/engine/internal/handlers/` | Home, search, health, and central error handling |
| `services/engine/internal/service/spellchecker/` | Aspell wrapper and dummy fallback |
| `services/engine/internal/service/ranking/` | TF-IDF vectors, cosine, normalization, blending, sort |
| `services/engine/internal/store/` | `PsqlStore`, SQL, connection string, row assembly |
| `services/engine/internal/config/` | Environment-backed config structs |
| `services/engine/internal/model/` | `Page`, `MetaData`, `Word`, `Edge` (JSON tags are internal-only) |
| `services/engine/internal/util/` | Env parsing and dense index mappers |
| `services/engine/view/` | templ sources (`*.templ`) and generated Go (`*_templ.go`) |
| `services/engine/static/`, `services/engine/public/` | Source assets / generated assets |

### Two binaries

| Binary | Source | Notes |
|---|---|---|
| `boogle` | `cmd/server/main.go` | The service. Built by `just build-emb` in the Dockerfile. |
| CLI/debug | `cmd/cli/main.go` | Takes `os.Args[1]`, uses a `mockSpeller` that only `strings.Split`s on spaces, applies a 5s timeout to `GetData` only, prints `query:`, timings, and one line per page. Local debugging only. |

## Query normalization

`queryTokens` in `internal/handlers/search.handler.go`:

1. `TrimSpace`; empty string → `nil` tokens.
2. Truncate to 200 runes by slicing `[]rune` (`maxQueryRunes`).
3. `strings.ToLower`, then `strings.Fields`.
4. Truncate to 10 tokens (`maxQueryTokens`).

The cap exists because every token can cost up to four aspell cgo calls and the query is attacker-controlled. `getPageNum` defaults invalid or non-positive `page` values to 1; `clampPage` bounds it to `[1, totalPages]` so a stale link renders the last real page.

## Spellchecker

`internal/service/spellchecker/spell-checker.go` holds one go-aspell speller created with `lang: "en_US"` (hardcoded, no env override) and guarded by a mutex. `suggestionsFor(q, speller, 3)`:

- splits on `" "`, always keeps the lower-cased original word;
- for misspelled words, keeps up to three suggestions **per word**;
- deduplicates via a map, drops blank and apostrophe-containing terms, and `sort.Strings` for deterministic output.

`DummySpeller.GetSuggestions(q)` returns `[]string{q}`. See [the fallback caveat](api.md#suggestion-expansion).

!!! note "Concurrency"
    The mutex makes the shared speller race-safe but serializes all suggestion work. There is no timeout around the lock or the cgo calls: a slow request can block behind other requests in this section.

## Store: the only two SQL statements

Both are parameterized (`pq.Array` + positional params) and read-only. The driver is registered by the `lib/pq` import in `internal/store/store.go`. Neither statement filters on `pages.indexed`, applies an `OFFSET`, or references `graph_edges`.

### Candidate query — `PsqlStore.GetData`

```sql
WITH ranked AS (
    SELECT
        p.id,
        u.url,
        COALESCE(pr.score, 0) AS pr,
        p.metadata,
        COUNT(DISTINCT w.id) AS word_count,
        COALESCE(
            json_agg(
                json_build_object(
                    'word', w.word,
                    'idf',  w.idf,
                    'tf',   pw.tf
                )
            ),
            '[]'
        ) AS word_set
    FROM words w
    INNER JOIN page_word pw ON w.id = pw.word_id
    INNER JOIN pages p      ON pw.page_id = p.id
    LEFT JOIN page_rank pr  ON p.url_id = pr.url_id
    INNER JOIN urls u       ON p.url_id = u.id
    WHERE w.word = ANY($1)
    GROUP BY p.id, pr.score, u.url, p.metadata
)
SELECT *
FROM ranked
ORDER BY word_count DESC,
         pr DESC,
         id ASC
LIMIT $2
```

- `$1 = pq.Array(words)` (the suggestion list); `$2 = candidateLimit` (`RANKER_MAX_RESULTS`, floored at 100 in code when < 1).
- The `LEFT JOIN` on `page_rank` keeps unranked pages searchable with score `0`.
- `word_count` is a distinct matched-word count; it decides which rows fit the `LIMIT`, not the final order.
- `words.idf` and `page_word.tf` are aggregated into the JSON `word_set`.

### Count query — `PsqlStore.GetTotalResults`

```sql
SELECT COUNT(DISTINCT p.id)
FROM words w
JOIN page_word pw ON w.id = pw.word_id
JOIN pages p      ON pw.page_id = p.id
WHERE w.word = ANY($1)
```

An empty suggestion list short-circuits to `0` with no DB call. Otherwise this is an OR-match count across all suggestions, not the size of the candidate window.

The DSN is built with `url.URL` so passwords containing `@`, `:`, or `/` are escaped. It sets `sslmode` (forced to `disable` when empty), `connect_timeout=5`, and `statement_timeout=30000`. See [Configuration](configuration.md).

## In-memory data assembly

For each row, `GetData` scans the id, URL, `COALESCE`d PageRank, metadata JSONB, `word_count`, and `word_set` JSON. `word_count` is scanned but never used in Go. It then:

- unmarshals `metadata` into `model.MetaData` and `word_set` into `[]model.Word`;
- builds `Page{ID, URL, PRScore, Words: map[word]tf, MetaData}`;
- fills `Idf[word] = w.Idf` and `page.Words[word] = w.Tf` while walking the word set;
- builds a `PageMapper` from returned pages and a `WordMapper` from the **query words**, deduplicating.

The ranker relies on mapper/index consistency and deliberately **panics** on a miss (`tf-idf.go`, `util.go`); `Recover()` turns a request-time panic into a 500.

## Ranking math

`RankingService.Rank` runs `tfIdf` → `normalizeTFIDF` → `sort`.

1. **Vectors** — dimension `N = len(wordMapper.GetValues())` (unique suggestion words). For each page, `docVec[i] = tf(page word i) * idf(word i)`; TF is the integer from `page_word.tf`, IDF from `words.idf`. The query vector uses query TF × the same IDF map. Missing IDF keys read as Go map zero (`0`).
2. **Cosine** — `dot(A,B) / (|A|*|B|)`. Zero, NaN, or infinite denominator or score → `0`; NaN dot → `0`.
3. **Min–max normalization** — over the current candidate window, `score = (v - min) / (max - min)`. When `max == min`, every score becomes `0` and ordering falls entirely to PageRank. Scores are relative to the window, not absolute.
4. **Blend** — `GlobalScore = factor*normalizedTFIDF + (1-factor)*PRScore`, where `factor = RANKER_WEIGHT_TF` (default `0.5`). `PRScore` is the **raw** DB value; contrary to the comment in `ranking.go`, the engine does not normalize it.
5. **Sort** — `slices.SortStableFunc`, descending on `GlobalScore`, then matched-word count, title, URL, and finally `ID.String()`. The UUID key makes pagination deterministic.

!!! warning "Unbounded weight (review finding M-07)"
    `RANKER_WEIGHT_TF` is parsed with `strconv.ParseFloat`; NaN falls back to the default but negative, `>1`, and infinite values are accepted. A non-finite weight can produce NaN scores, for which the comparator is false in both directions — ordering then follows map iteration. No range check exists at config load.

## Pagination

`internal/handlers/search.handler.go` computes:

```text
pageSize   = PAGE_SIZE (default 20; <1 → 20)
reachable  = min(len(ranked), totalResults)
totalPages = max(1, ceil(reachable / pageSize))
currentPage = clamp(requested, 1, totalPages)
start = min((currentPage-1) * pageSize, len(ranked))
end   = min(start + pageSize, len(ranked))
```

The whole candidate window is re-fetched and re-ranked on every page; page N is just a Go slice. The raw DB count is not advertised — `reachable` is.

## Rendering

- `view/layout/layout.templ` — document shell, Google Fonts, `/public/css/styles.css`, footer, `/public/js/index.js` (`defer`).
- `view/page/home/home.templ` — logo, `GET /search` form with `name="query" required`, Alpine clear/submit state, hero buttons.
- `view/page/result/result.templ` — `ShowAll`, `ShowImages`, `ShowGraph` branch on `isHtmx`; full page renders layout + one `#results`, fragment renders only the tab content.
- `view/component/result_header.templ` — sticky search input (`hx-get="/search"`, 500 ms debounce on Enter) and the All/Images/Graph tabs.
- `view/page/result/tab/all.templ` — count, cards, `NoResults`, pagination.
- `view/page/result/tab/{images,graph}.templ` — both just render `NoResults()`.
- `view/component/page_card.templ` — `getIcon` accepts only absolute http(s) URLs or same-origin relative paths (`templ.URL` used for href/src); description truncated to 200 runes; title falls back to URL.
- `view/component/pagination.templ` — Alpine `goToPage` sets `page` in the URL and navigates.

Generated `*_templ.go` files are gitignored; a fresh checkout must run `templ generate` before building or testing.

## Build, generation, and tests

Verified recipes from `services/engine/justfile`:

| Command | Effect |
|---|---|
| `just templ-generate` / `just templ-watch` | Generate templ Go code |
| `just tailwind-build` / `-prod` | Build `public/css/styles.css` from `static/css/input.css` |
| `just js-build` / `js-prod` | Bundle `static/js/main.js` into `public/js/index.js` |
| `just dev` | Generate templates + dev assets, then `air` |
| `just build` | Generate templates + prod assets, then `go build -o ./bin/main ./cmd/server/main.go` |
| `just build-emb` | Generate templates, then static linux/amd64 build to `boogle` |
| `just cli "query"` | Run the debug binary |
| `just test` | `go test ./internal/... -count=1` (omits `view/page/result`) |
| `just test-cov` | Internal tests with coverage profile and `go tool cover -func` |
| `just test-integration` | `go test -tags integration ./internal/... -count=1` |

The fact sheet reports these verification runs: `go test ./... -count=1` passes (including view tests), `go vet ./...` clean, `go test -race ./internal/... -count=1` passes, and coverage of `apperror 71.4%`, `handlers 84.0%`, `ranking 91.8%`, `spellchecker 93.8%`, `store 66.2%`, `util 100%` (integration-tagged file excluded). Those results were **not re-run** while writing these docs. `staticcheck` is not installed on this machine. Integration tests live only in `internal/store/store_integration_test.go` (build tag `integration`), skip when no test DB is present, and use `TEST_DATABASE_URL` or a local `boogle_test` DSN.

!!! note "Tailwind config is not authoritative"
    `static/css/input.css` is Tailwind v4 CSS-first (`@import "tailwindcss"`). The v3-style `tailwind.config.js` has no `@config` directive pointing at it, so do not assume it controls scanning.

## Verified limitations

| Limitation | Where | Review ID |
|---|---|---|
| Aspell fallback returns the joined phrase; multi-word queries normally match nothing | `main.go`, `dummy-spell-checker.go`, `store.go` | H-03 |
| Rate-limiter identity from spoofable `RealIP`; port published on all interfaces | `main.go`, `docker-compose.yml` | H-02, M-26 |
| `/health` is DB-agnostic | `health.handler.go` | M-06 |
| `RANKER_WEIGHT_TF` not range-checked; NaN/Inf/blend nondeterminism | `dorenv.go`, `util.go` | M-07 |
| Count and data queries are non-atomic | `search.handler.go`, `store.go` | M-08 |
| No `pages.indexed` filter in either query | `store.go` | engine side of X-02 |
| Images/Graph tabs are placeholders | `view/page/result/tab/*`, `search.handler.go` | — |
| Hardcoded error timestamp | `view/page/error/error.templ` | L-11 |
| Unused `embed.FS`; no `go:embed`; assets from disk | `main.go`, `e.Static` | L-07 |
| Unlabeled inputs/buttons, no live region, keyboard/contrast issues | `view/**/*.templ` | M-09–M-15, L-08, L-09, L-12 |
| No caching, sessions, auth, analytics, advanced operators, or "did you mean" renderer | absence across engine code | — |
| `just test` skips view tests | `justfile` | — |
| Fail-fast DB boot; no graceful shutdown, HTTP timeouts, TLS, or CORS | `main.go`, `store.go` | M-26 (exposure) |

!!! note "Review IDs are supporting context only"
    The IDs in the table refer to the 2026-11-22 project review. They are included only where the fact sheet or current source confirms the underlying behavior. The review's runtime-unverified observations (for example whether a single click on the Images/Graph tabs fires both navigation paths in a real browser) are called out as such in [HTTP interface](api.md#response-shapes) and are not asserted as verified behavior here. Claims about other services are out of scope for this page.

## Related pages

- [Engine overview](index.md)
- [Architecture](architecture.md)
- [HTTP interface](api.md)
- [Configuration](configuration.md)
