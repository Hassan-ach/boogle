# Database functions

This page documents the SQL contracts the ranking service relies on. Schema objects live in `migration/`; the Python callers live in `services/ranking/src/calculator.py`.

## `update_page_rank(...)`

Defined by `migration/02_pg_rank.sql`:

```sql
CREATE OR REPLACE FUNCTION update_page_rank(
    max_iterations INTEGER DEFAULT 20,
    damping_factor FLOAT DEFAULT 0.85
)
RETURNS TABLE (
    out_url_id UUID,
    out_rank FLOAT
) AS $$ ... $$ LANGUAGE plpgsql;
```

The function body is elided above; the full definition is in `migration/02_pg_rank.sql`.

| Argument | Type | Default | Meaning |
|---|---|---|---|
| `max_iterations` | `INTEGER` | `20` | Exact number of update passes; no convergence check |
| `damping_factor` | `FLOAT` | `0.85` | Probability of following an indexed link; also scales teleport and dangling terms |

Returns one `(out_url_id, out_rank)` row per indexed page, ordered by score descending. Side effects: replaces the contents of `page_rank` with the computed scores.

### Phase 1 — zero-node guard

```sql
SELECT COUNT(*) INTO node_num FROM pages WHERE indexed = TRUE;

IF node_num = 0 THEN
    DELETE FROM page_rank;
    RETURN;
END IF;
```

Stale scores are cleared and no rows are returned. This guard prevents old ranks from surviving an empty indexed corpus.

### Phase 2 — temporary working tables

```sql
CREATE TEMP TABLE IF NOT EXISTS temp_page_rank (
    url_id UUID PRIMARY KEY,
    score FLOAT NOT NULL
) ON COMMIT DROP;
TRUNCATE temp_page_rank;

INSERT INTO temp_page_rank (url_id, score)
SELECT p.url_id, 1.0 / node_num
FROM pages p
WHERE p.indexed = TRUE;
```

`temp_page_rank`, `temp_out_degree`, and `temp_graph_edges` are all `ON COMMIT DROP`, created with `IF NOT EXISTS` and truncated at the start of each call. This makes repeated calls inside one transaction safe. `url_id` is used because `graph_edges` references `urls.id`, not `pages.id`.

### Phase 3 — indexed-only edge set

```sql
INSERT INTO temp_graph_edges (from_url, to_url)
SELECT ge.from_url, ge.to_url
FROM graph_edges ge
JOIN pages src ON src.url_id = ge.from_url AND src.indexed = TRUE
JOIN pages dst ON dst.url_id = ge.to_url   AND dst.indexed = TRUE;

INSERT INTO temp_out_degree (from_url, cnt)
SELECT from_url, COUNT(*)::FLOAT
FROM temp_graph_edges
GROUP BY from_url;
```

Both the transfer numerator and the out-degree denominator use this one restricted set. Unindexed sources and destinations do not participate in rank transfer.

### Phase 4 — fixed iteration and dangling mass

```sql
FOR i IN 1..max_iterations LOOP
    SELECT COALESCE(SUM(tpr.score), 0.0)
    INTO dangling_rank
    FROM temp_page_rank tpr
    LEFT JOIN temp_out_degree od ON tpr.url_id = od.from_url
    WHERE od.from_url IS NULL;

    WITH calculated AS (
        SELECT p.url_id,
            ((1.0 - damping_factor) / node_num)
          + (damping_factor * dangling_rank / node_num)
          + (damping_factor * COALESCE(SUM(tpr.score / od.cnt), 0.0)) AS new_score
        FROM pages p
        LEFT JOIN temp_graph_edges ge ON p.url_id = ge.to_url
        LEFT JOIN temp_page_rank tpr ON ge.from_url = tpr.url_id
        LEFT JOIN temp_out_degree od ON ge.from_url = od.from_url
        WHERE p.indexed = TRUE
        GROUP BY p.url_id
    )
    UPDATE temp_page_rank tpr
    SET score = c.new_score
    FROM calculated c
    WHERE tpr.url_id = c.url_id;
END LOOP;
```

The dangling term redistributes rank held by indexed pages with no indexed out-edge. There is no early stop.

### Phase 5 — persist and return

```sql
DELETE FROM page_rank
WHERE url_id NOT IN (SELECT url_id FROM temp_page_rank);

INSERT INTO page_rank (url_id, score, updated_at)
SELECT tpr.url_id, tpr.score, NOW()
FROM temp_page_rank tpr
ON CONFLICT (url_id)
DO UPDATE SET score = EXCLUDED.score, updated_at = NOW();

RETURN QUERY
SELECT tpr.url_id, tpr.score
FROM temp_page_rank tpr
ORDER BY tpr.score DESC;
```

## IDF statement (not a stored function)

There is no IDF function in `migration/`. `compute_idf()` sends the statement below directly; see [Algorithms](algorithms.md) for the semantics.

```sql
WITH corpus AS (
    SELECT GREATEST(COUNT(*), 1)::numeric AS n FROM pages
),
doc_freq AS (
    SELECT word_id, COUNT(DISTINCT page_id) AS df
    FROM page_word
    GROUP BY word_id
)
UPDATE words
SET idf = LOG(GREATEST((SELECT n FROM corpus) / GREATEST(df.df, 1), 1))
FROM doc_freq df
WHERE words.id = df.word_id
```

Only `words.idf` is assigned. `words.doc_frequency` and `page_word.tf` are not modified or read for weighting.

## Table access matrix

| Table | IDF statement | `update_page_rank()` | Schema source |
|---|---|---|---|
| `words` | write `idf` | — | `migration/01_schema.sql` |
| `page_word` | read `word_id`, `page_id` | — | `migration/01_schema.sql` |
| `pages` | read all rows for `COUNT(*)` | read `indexed`, `url_id` | `migration/01_schema.sql`, `03_index_completion.sql` |
| `graph_edges` | — | read `from_url`, `to_url` | `migration/01_schema.sql`, `03_index_completion.sql` |
| `page_rank` | — | delete stale + upsert scores | `migration/01_schema.sql` |
| `urls` | — | referenced by FK from `page_rank` and `graph_edges` | `migration/01_schema.sql` |

`page_rank.url_id` references `urls(id) ON DELETE CASCADE`, so deleting a URL also removes its stored rank. `page_rank` has a `score DESC` index in `migration/01_schema.sql`.

## Transactions and rowcounts

- Each stage obtains its own pooled connection, executes one statement, commits, and returns `cursor.rowcount`. The stages therefore commit **independently** even though `run_pipeline()` runs them in an `asyncio.TaskGroup`.
- On any exception a stage rolls back and re-raises; connection-class errors are retried first by `retry_on_db_error`.
- For IDF, `rowcount` is the number of updated words; it is `0` if no word has a `page_word` row.
- For PageRank, `rowcount` reflects the function's returned rows: integration tests assert `3` for three indexed pages and `0` when no pages are indexed.

!!! note "Graph schema detail"
    `migration/03_index_completion.sql` drops `idx_graph_edges_unique_revese`, the index that previously discarded reciprocal links. `UNIQUE (from_url, to_url)` from `migration/01_schema.sql` remains the only edge uniqueness rule.
