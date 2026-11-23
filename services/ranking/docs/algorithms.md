# Algorithms

This page describes what the ranking service actually computes. Both passes rewrite whole tables; neither is incremental, and neither is part of a shared transaction with the other.

## IDF (inverse document frequency)

`compute_idf()` in `services/ranking/src/calculator.py` executes exactly one statement:

```sql
WITH corpus AS (
    SELECT GREATEST(COUNT(*), 1)::numeric AS n
    FROM pages
),
doc_freq AS (
    SELECT word_id, COUNT(DISTINCT page_id) AS df
    FROM page_word
    GROUP BY word_id
)
UPDATE words
SET idf = LOG(GREATEST(
    (SELECT n FROM corpus) / GREATEST(df.df, 1),
    1
))
FROM doc_freq df
WHERE words.id = df.word_id
```

Implemented behavior:

- **Corpus size** `n` is `COUNT(*)` from `pages`, clamped to at least 1. It counts **all pages**, indexed or not.
- **Document frequency** `df` is the number of distinct `page_id` values in `page_word` for each `word_id`. `page_word.tf` is not used.
- **Weight** is `LOG(GREATEST(n / GREATEST(df, 1), 1))`. The `GREATEST` clamps keep `df >= 1` and the ratio `>= 1`, so IDF is never negative and `LOG(0)` is impossible.
- Only words that appear in `page_word` are updated. A word with no occurrences keeps its previous `idf` (an integration test leaves `idf = 42.0` and expects `affected == 0`).
- `words.doc_frequency` is **not** updated, even though the column exists in `migration/01_schema.sql`.
- The method reads `cursor.rowcount`, commits, logs the count and duration, and returns the number of updated words. On error it rolls back and re-raises after the retry policy.

!!! warning "PostgreSQL `LOG()` is base 10"
    In PostgreSQL, `LOG(numeric)` is the base-10 logarithm. Integration tests assert `math.log10(378 / 352)` and `math.log10(4 / 3)`, not natural-log values. A worked example in older documentation (`log(10000 / 800) = 2.51`) uses the natural log; the base-10 result is about `1.097`.

!!! warning "This is not TF-IDF"
    The service stores only the IDF half. It never computes or stores term frequency-weighted values, and `page_word.tf` is ignored by the scoring SQL.

## PageRank

`update_pagerank()` calls a PL/pgSQL function with parameterized arguments:

```python
PAGERANK_UPDATE_SQL = "SELECT update_page_rank(%s, %s);"
await cursor.execute(PAGERANK_UPDATE_SQL, (iterations, damping_factor))
```

| Parameter | Python default | SQL default | Env-configurable? |
|---|---|---|---|
| `iterations` | `20` | `20` | No |
| `damping_factor` | `0.85` | `0.85` | No |

The defaults are function arguments only. `RANKING_ITERATIONS` and `RANKING_DAMPING_FACTOR` are not read anywhere in `services/ranking/src/`.

### Node and edge sets

- `N` is the count of rows in `pages` where `indexed = TRUE`. If `N = 0`, the function deletes all `page_rank` rows and returns without computing or returning scores.
- Every indexed page is seeded with `1.0 / N` in a temporary `temp_page_rank` table.
- Edges come only from `graph_edges` rows whose source **and** destination pages are both indexed. The out-degree table is built from that same restricted edge set, so denominators and rank transfers agree.
- Reciprocal edges are permitted: `migration/03_index_completion.sql` drops the old reverse-edge unique index, leaving only `UNIQUE (from_url, to_url)` in `migration/01_schema.sql`.

### Iteration

The function runs a fixed `FOR i IN 1..max_iterations LOOP`. Each pass recomputes all scores from the previous pass's values:

```sql
new_score =
      ((1.0 - damping_factor) / node_num)                    -- teleportation
    + (damping_factor * dangling_rank / node_num)            -- dangling redistribution
    + (damping_factor * COALESCE(SUM(tpr.score / od.cnt), 0.0)) -- incoming links
```

`dangling_rank` is the sum of scores of indexed pages with no indexed out-edge. Redistributing it across all nodes is what keeps total rank conserved.

!!! warning "No convergence check"
    There is no epsilon, delta, or early-stop condition. The loop always executes exactly `max_iterations` passes. With `iterations = 0`, the stored scores are just the initial uniform `1.0 / N` values.

### Persistence and return value

- Rows in `page_rank` whose `url_id` is no longer in the working set are deleted.
- Remaining scores are upserted with `ON CONFLICT (url_id) DO UPDATE SET score = EXCLUDED.score, updated_at = NOW()`.
- The function returns `(out_url_id, out_rank)` rows ordered by score descending; the Python caller uses `cursor.rowcount` as its returned count.

!!! warning "Corpus size differs between the two algorithms"
    IDF's `N` counts **all rows in `pages`**. PageRank's `N` counts only `indexed = TRUE` rows. The two stages are not computed over the same node set.
