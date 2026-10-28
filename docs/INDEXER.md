# Indexer Service - Detailed Guide

## Overview

The Indexer consumes page jobs from RabbitMQ, processes crawled HTML pages and converts them into a searchable word index with term frequencies. It publishes indexing confirmations back to RabbitMQ.

## Architecture

```
HTML Pages → Parse → Extract Words → Calculate TF → Store Index

         ↓ Parallel Workers ↓
      Process multiple pages concurrently
```

## Technology Stack

- **Rust 1.70+** - Language
- **Tokio** - Async runtime
- **sqlx** - Async database
- **html5ever** - HTML parsing
- **slog** - Structured logging

## Data Flow

```
pages table (indexed=false)
    ↓
Worker 1 → Parse HTML → Extract words → Calculate TF → Store
Worker 2 → Parse HTML → Extract words → Calculate TF → Store
Worker 3 → Parse HTML → Extract words → Calculate TF → Store
...
    ↓
words table + page_word table (with TF values)
```

## Project Structure

```
indexer/
├── src/
│   ├── main.rs                 # Entry point, task spawning
│   └── core/
│       ├── mod.rs             # Module definitions
│       ├── config.rs          # Configuration loading
│       ├── indexer.rs         # Core indexing logic
│       ├── psql.rs            # Database operations
│       └── text_sink.rs       # Word extraction
├── Cargo.toml                 # Rust dependencies
└── justfile                   # Build commands
```

## Indexing Process

1. **Fetch** - Each worker queries unindexed pages
   ```sql
   SELECT * FROM pages WHERE indexed = false LIMIT 1
   ```

2. **Parse** - Extract text from HTML
   ```
   <html>
     <head><title>Example</title></head>
     <body><p>Hello world</p></body>
   </html>

   Extracted text: "Example Hello world"
   ```

3. **Tokenize** - Split into words
   ```
   "Example Hello world" → ["example", "hello", "world"]
   ```

4. **Calculate TF** - Term frequency per word
   ```
   "example" appears 1 time in 3 words = TF = 1
   "hello" appears 1 time in 3 words = TF = 1
   "world" appears 1 time in 3 words = TF = 1
   ```

5. **Store** - Write to database
   ```sql
   INSERT INTO words VALUES ('example', tf=1)
   INSERT INTO page_word VALUES (page_id, word_id, tf=1)
   ```

6. **Mark** - Update indexed flag
   ```sql
   UPDATE pages SET indexed = true WHERE id = page_id
   ```

## TF Calculation

```
TF(word, page) = count of word / total words in page

Example:
  Page has 100 words total
  Word "python" appears 5 times
  TF("python") = 5 / 100 = 0.05
```

## Database Tables

### words
```sql
CREATE TABLE words (
    id UUID PRIMARY KEY,
    word VARCHAR(25) UNIQUE NOT NULL,
    idf DOUBLE PRECISION DEFAULT 1,
    doc_frequency INTEGER DEFAULT 0
);
```

### page_word
```sql
CREATE TABLE page_word (
    page_id UUID NOT NULL REFERENCES pages(id),
    word_id UUID NOT NULL REFERENCES words(id),
    tf INTEGER NOT NULL CHECK (tf > 0),
    PRIMARY KEY (page_id, word_id)
);
```

### pages
```sql
CREATE TABLE pages (
    id UUID PRIMARY KEY,
    url_id UUID UNIQUE NOT NULL,
    html TEXT NOT NULL,
    metadata JSONB NOT NULL,
    indexed BOOLEAN NOT NULL DEFAULT FALSE  -- Set to true after indexing
);
```

## Parallel Processing

Configuration:
```env
INDEXER_COUNT=4  # Number of concurrent workers
```

Each worker:
- Has its own task
- Connects to database
- Processes independent pages
- Shares nothing (thread-safe)

## Word Normalization

Typical processing:
```
Raw word:     "Python"
Lowercase:    "python"
Stemming:     "python" (if implemented)
Stored as:    "python"
```

## Performance

Typical throughput:
- 1000 pages/minute (with 4 workers)
- Scales linearly with worker count

Optimization opportunities:
- Batch database inserts
- Streaming HTML parsing
- Connection pooling

## Logging

Structured logs to file:
```json
{
  "ts": "2026-03-02T10:00:00Z",
  "level": "info",
  "message": "Page indexed",
  "worker_id": 1,
  "page_count": 150
}
```

View logs:
```bash
tail -f logs/indexer.log | jq .
```

## Configuration

```env
PG_USER=postgres
PG_PASSWORD=password
PG_DBNAME=boogle
PG_PORT=5432
PG_HOST=localhost

INDEXER_COUNT=4
INDEXER_LOG_PATH=./logs/indexer.log
```

## Signal Handling

```
SIGINT/SIGTERM
    ↓
Cancel all workers
    ↓
Wait for in-flight operations
    ↓
Close connections
    ↓
Exit
```

30-second timeout for graceful shutdown.

## Error Handling

Recovers from:
- Malformed HTML (html5ever is forgiving)
- Database connection issues
- Invalid UTF-8
- Corrupted page data

Errors logged but don't stop workers.

## Example Index

After indexing Wikipedia page on "Information Retrieval":

```
words table:
  id: uuid1, word: "information", idf: 2.3, doc_frequency: 150
  id: uuid2, word: "retrieval", idf: 2.8, doc_frequency: 120
  id: uuid3, word: "search", idf: 3.1, doc_frequency: 200

page_word table:
  page_id: page123, word_id: uuid1, tf: 25
  page_id: page123, word_id: uuid2, tf: 18
  page_id: page123, word_id: uuid3, tf: 32
```

Later: Ranking service adds IDF values.

## Development

```bash
# Build
cargo build

# Run
cargo run

# Production build
cargo build --release

# Run with profiling
perf record ./target/release/indexer
perf report

# Format code
cargo fmt

# Lint
cargo clippy
```

## Testing

Currently no tests. Would add:
- HTML parsing tests
- Word extraction tests
- TF calculation tests
- Database integration tests

## HTML Parsing Details

Uses **html5ever** crate:
- Follows HTML5 spec
- Robust to malformed HTML
- Extracts text from all elements
- Handles encoding automatically

Text extraction example:
```html
<html>
  <script>var x = 1;</script>     <!-- Ignored -->
  <style>body { color: red; }</style> <!-- Ignored -->
  <p>Visible text</p>            <!-- Extracted -->
</html>

Result: "Visible text"
```

## Memory Usage

Typical per-worker:
- Page buffer: 1-10MB (depending on page size)
- Database connection: 1MB
- Task overhead: <1MB

Total for 4 workers: ~20-50MB

## Limitations ⚠️

1. **Simple tokenization** - No stemming/lemmatization
2. **English-only** - No multi-language support
3. **No partial updates** - Must reprocess entire page
4. **No incremental** - Ignores previously indexed content

## Future Improvements

- [ ] Implement stemming (Porter stemmer)
- [ ] Multi-language support
- [ ] Incremental indexing
- [ ] Custom tokenization rules
- [ ] Metadata extraction (author, date, etc.)
- [ ] Content type detection (news, blog, etc.)

## Integration with Ranking

After indexing:
```
Ranking service reads:
  - words table (to get word list)
  - page_word table (to get TF values)
  - Calculate IDF
  - Update words table with IDF

Then PageRank reads:
  - graph_edges table
  - Calculate PageRank scores
  - Update page_rank table
```

## Common Issues

### Workers not progressing
Check database queries are finding unindexed pages:
```sql
SELECT count(*) FROM pages WHERE indexed = false;
```

### Slow indexing
- Increase `INDEXER_COUNT`
- Check database performance
- Verify HTML parsing isn't bottleneck

### Memory growing
Might be database connection leak
- Check connection pooling
- Monitor active connections

## Dependencies

Key Rust crates:
- `tokio` - Async runtime
- `sqlx` - Database
- `html5ever` - HTML parsing
- `slog` - Logging
- `anyhow` - Error handling

See Cargo.toml for versions.
