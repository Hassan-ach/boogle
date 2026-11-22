# Ranking Service - Detailed Guide

## Overview

The Ranking service is event-driven via RabbitMQ (indexer confirmations). It counts indexed pages and triggers IDF and PageRank computation once a threshold is reached, writing scores to Postgres for fast search result ordering.

## Architecture

```
Word Index → Calculate IDF → Update words table

Link Graph → Calculate PageRank → Update page_rank table

Search Query → Use both scores → Rank results
```

## Technology Stack

- **Python 3.9+** - Language
- **NumPy** - Numerical computing
- **SciPy** - Scientific algorithms
- **psycopg2** - PostgreSQL connection
- **APScheduler** - Scheduled tasks
- **python-dotenv** - Config

## Two Ranking Algorithms

### 1. TF-IDF (Term Frequency - Inverse Document Frequency)

Measures how relevant a page is for a specific query.

**Formula:**
```
TF-IDF(word, page) = TF(word, page) × IDF(word)

TF(word, page) = count of word / total words in page

IDF(word) = log(total pages / pages containing word)
```

**Example:**
```
Query: "python"
Page about Python
  TF("python") = 25 mentions / 500 words = 0.05
  IDF("python") = log(10000 / 800) = 2.51
  TF-IDF = 0.05 × 2.51 = 0.126

Page about programming
  TF("python") = 2 mentions / 400 words = 0.005
  IDF("python") = 2.51 (same)
  TF-IDF = 0.005 × 2.51 = 0.013

→ First page ranks higher
```

**Score Meaning:**
- Higher TF-IDF = More relevant to query
- Used to score results for specific searches

### 2. PageRank (Link Analysis)

Measures how "important" a page is based on links to it.

**Formula:**
```
PR(page) = (1 - d) / N + d × Σ(PR(source) / outlinks(source))

where:
  d = damping factor (0.85)
  N = total pages
  source = each page linking to current page
  outlinks(source) = number of links from source
```

**Intuition:**
```
If important pages link to you, you're probably important too.

Example:
        A (PageRank: 0.5)
       / \
      /   \
     B     C (PageRank: 0.3 each)
     |
     D (PageRank: 0.2 - no links to it)
     
D has low rank because no pages link to it.
```

**Iteration Process:**
```
Initial:  All pages PR = 1/N
Iter 1:   Redistribute based on links
Iter 2:   Recalculate with updated scores
...
Iter 20:  Converge to final scores
```

**Score Meaning:**
- Higher PageRank = More "important" page
- Used as global score for all queries
- Helps break ties when TF-IDF is similar

## Project Structure

```
ranking/
├── src/
│   ├── main.py          # Entry point, scheduling
│   ├── idf.py           # IDF calculation
│   ├── page_rank.py     # PageRank algorithm
│   └── psql.py          # Database operations
└── requirements.txt     # Python dependencies
```

## Database Integration

### Input Tables

#### words
```sql
SELECT id, word FROM words;
-- Returns: uuid1, "python" | uuid2, "search" | ...
```

#### page_word
```sql
SELECT page_id, word_id, tf FROM page_word;
-- Returns: page1, word1, 25 | page1, word2, 15 | ...
```

#### graph_edges
```sql
SELECT from_url, to_url FROM graph_edges;
-- Returns: page_a, page_b | page_a, page_c | ...
```

### Output Tables

#### words (updated)
```sql
UPDATE words SET idf = 2.51 WHERE id = word1;
```

#### page_rank (new)
```sql
INSERT INTO page_rank VALUES (url1, score=0.45);
```

## IDF Calculation

**Python Implementation:**
```python
def calculate_idf():
    total_pages = count_pages()
    for word in all_words:
        doc_freq = count_pages_with_word(word)
        idf = log(total_pages / doc_freq)
        update_word_idf(word, idf)
```

**Update Frequency:**
- Run once initially
- Update whenever new pages indexed
- Usually via scheduled job

## PageRank Calculation

**Algorithm Steps:**

1. **Build link matrix**
   ```
   If page A links to B and C:
   Matrix row A: [0, 0.5, 0.5, 0, ...]
   (divide 1.0 by number of outlinks)
   ```

2. **Initialize scores**
   ```
   All pages start with PR = 1 / total_pages
   ```

3. **Iterate** (typical 20 times)
   ```python
   for iteration in range(20):
       new_pr = {}
       for page in all_pages:
           # Damping factor: 15% random jump, 85% follow links
           new_pr[page] = (1 - 0.85) / N + 0.85 * sum_incoming_links
       pr = new_pr
   ```

4. **Converge**
   ```
   Stop when scores stabilize (continue until limit)
   ```

5. **Store results**
   ```sql
   INSERT INTO page_rank VALUES (url, score)
   ```

## Configuration

```env
RANKING_DAMPING_FACTOR=0.85      # 85% follow links, 15% random jump
RANKING_ITERATIONS=20            # Number of PageRank iterations
RANKING_SCHEDULE_INTERVAL=3600   # Update every hour (seconds)
```

### Tuning Parameters

| Parameter | Effect | Notes |
|-----------|--------|-------|
| Damping Factor | Higher = more link-based | 0.85 is standard |
| Iterations | Higher = more accurate | 20 usually enough |
| Schedule | More often = fresher data | Every hour is reasonable |

## Scheduling

Uses APScheduler:

```python
scheduler.add_job(
    update_all_rankings,
    'interval',
    seconds=3600,  # Run hourly
    id='ranking_job'
)
scheduler.start()
```

Runs in background, doesn't block other services.

## Performance

Typical runtime:
```
1,000 pages:    2-3 seconds
10,000 pages:   5-10 seconds
100,000 pages:  1-2 minutes
1,000,000 pages: 5-10 minutes (with optimization)
```

Bottlenecks:
- Database queries (index optimization helps)
- NumPy operations (scales well with SciPy sparse matrices)

## Optimization Techniques

1. **Sparse matrices** - Don't store zeros
   ```python
   from scipy.sparse import csr_matrix
   ```

2. **Vectorized operations** - Use NumPy, not Python loops

3. **Database indexes** - Index graph_edges.from_url/to_url

4. **Batch updates** - Update many rows at once

## Logging

Example run:
```
2026-03-02 10:00:00 - Ranking job started
2026-03-02 10:00:02 - IDF calculation: 5,234 words updated
2026-03-02 10:00:15 - PageRank iteration 1/20
2026-03-02 10:00:16 - PageRank iteration 2/20
...
2026-03-02 10:00:30 - PageRank converged: 50,000 pages updated
2026-03-02 10:00:30 - Ranking job completed (30s elapsed)
```

## Combined Scoring for Search

When user searches for "python programming":

```
For each result page:
  1. Get TF-IDF scores for words ["python", "programming"]
  2. Sum TF-IDF scores: 0.126 + 0.089 = 0.215
  3. Multiply by PageRank: 0.215 × 0.45 = 0.097
  
Final score = 0.097
(Rank higher results with higher combined scores)
```

Search results ordered by combined score.

## Example Database State

**Before Ranking:**
```
words: 5000 rows, idf all = 1.0
page_rank: 0 rows (empty)
```

**After IDF Calculation:**
```
words: 5000 rows, idf values updated
  "the" → idf = 0.3 (common word)
  "python" → idf = 2.5 (less common)
  "cryptocurrency" → idf = 3.8 (rare word)
```

**After PageRank:**
```
page_rank: 50000 rows
  page1 → score = 0.75 (many links to it)
  page2 → score = 0.02 (few links to it)
  page3 → score = 0.35 (medium)
```

## Important Notes ⚠️

1. **IDF must run before searches** - Words need IDF values
2. **PageRank takes time** - Worst performance part
3. **More iterations = less improvement** - Diminishing returns after 20
4. **Outdated data** - Scores become stale if not updated regularly

## Development

```bash
# Install dependencies
pip install -r requirements.txt

# Run ranking job once
python src/main.py

# Run specific function
python -c "from src.idf import calculate_idf; calculate_idf()"
```

## Debugging

Check IDF values:
```python
import psycopg2
conn = psycopg2.connect("dbname=boogle user=postgres")
cur = conn.cursor()
cur.execute("SELECT word, idf FROM words LIMIT 10")
print(cur.fetchall())
```

Check PageRank:
```python
cur.execute("SELECT url_id, score FROM page_rank ORDER BY score DESC LIMIT 10")
print(cur.fetchall())  # Top-ranked pages
```

## Testing

Would test:
- IDF calculation correctness
- PageRank convergence
- Sparse matrix operations
- Database operations
- Scheduling trigger

## Future Improvements

- [ ] Incremental PageRank updates
- [ ] Personalized PageRank (user-specific)
- [ ] Topic-specific PageRank
- [ ] Temporal decay (weight recent links higher)
- [ ] Link spam detection
- [ ] Query-dependent ranking

## Common Issues

### PageRank takes too long
- Reduce RANKING_ITERATIONS
- Optimize database indexes
- Use sparse matrices

### IDF values all 1.0
- Run IDF calculation (usually automatic)
- Check database connection

### Rankings not updating
- Check scheduler is running
- Verify database writes working
- Check logs for errors

## Mathematical References

- [TF-IDF on Wikipedia](https://en.wikipedia.org/wiki/Tf%E2%80%93idf)
- [PageRank on Wikipedia](https://en.wikipedia.org/wiki/PageRank)
- [NumPy Documentation](https://numpy.org/)
- [SciPy Linear Algebra](https://docs.scipy.org/doc/scipy/reference/linalg.html)
