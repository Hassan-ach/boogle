# Spider Service - Detailed Guide

## Overview

The Spider crawls the web, discovers new URLs, and stores raw HTML content and link relationships for later processing. It publishes indexing jobs to RabbitMQ after persisting pages.

## Architecture

```
Seed URLs → Worker Pool → Fetch HTML → Parse Links → Store in DB → Keep crawling

           ↓ Concurrent Processing ↓
        Multiple crawlers processing URLs in parallel
```

## Technology Stack

- **Go 1.21+** - Language
- **net/http** - HTTP client with timeouts
- **Custom parser** - HTML parsing and link extraction
- **PostgreSQL** - Storage
- **Goroutines/Channels** - Concurrency

## Data Flow

```
Start URLs → Queue → Worker 1 → Fetch → Parse → Store URLs
                   → Worker 2 → Fetch → Parse → Store HTML
                   → Worker 3 → Fetch → Parse → Store Links
                   ...
```

## Project Structure

```
spider/
├── cmd/spider/main.go          # Entry point
├── internal/
│   ├── config/config.go        # Configuration loader
│   ├── entity/entity.go        # Data models
│   ├── parser/                 # HTML parsing logic
│   │   ├── html.go
│   │   ├── parser.go
│   │   ├── helpers.go
│   │   └── sitemap.go
│   ├── spider/spider.go        # Core crawling logic
│   ├── store/                  # Database operations
│   │   ├── db.go
│   │   ├── cache.go
│   │   └── store.go
│   └── utils/                  # Logging & helpers
└── justfile                    # Build commands
```

## Configuration

```go
type Config struct {
    App struct {
        MaxCrawlers         int
        MaxConcurrentFetch  int
        HttpTimeout         int
        CrawlerTimeout      int
        ClawlerDelay        int
        LogsPath            string
    }
    Store struct {
        // Database config
    }
}
```

## Crawling Process

1. **Initialize** - Load config, connect to database
2. **Start** - Spawn N worker goroutines
3. **Fetch** - Each worker:
   - Gets pending URL from database
   - Makes HTTP request
   - Parses HTML response
   - Extracts new URLs (adds to queue)
   - Saves HTML content
   - Records link relationships
4. **Repeat** - Until interrupted or queue exhausted

## Database Tables

### urls
```sql
CREATE TABLE urls (
    id UUID PRIMARY KEY,
    url TEXT UNIQUE NOT NULL
);
```

### pages
```sql
CREATE TABLE pages (
    id UUID PRIMARY KEY,
    url_id UUID REFERENCES urls(id),
    html TEXT NOT NULL,
    metadata JSONB NOT NULL
);
```

### graph_edges
```sql
CREATE TABLE graph_edges (
    id BIGSERIAL PRIMARY KEY,
    from_url UUID REFERENCES urls(id),
    to_url UUID REFERENCES urls(id),
    UNIQUE (from_url, to_url)
);
```

## Link Graph Example

```
Wikipedia Page A
    ├── Links to Page B
    ├── Links to Page C
    └── Links to Page D

graph_edges:
  (Page A, Page B)
  (Page A, Page C)
  (Page A, Page D)
```

Used later by PageRank to determine importance.

## URL Parsing

```
Input:  https://en.wikipedia.org/wiki/Search_engine
Output: Extracted links like:
        - https://en.wikipedia.org/wiki/Information_retrieval
        - https://en.wikipedia.org/wiki/Database
        - https://en.wikipedia.org/wiki/Crawling
```

## Metadata Extraction

```go
type Page struct {
    Title       string
    Description string
    Keywords    []string
    Language    string
    ContentType string
}
```

Stored in `pages.metadata` JSONB field.

## Worker Model

```
Worker 1 ─┐
Worker 2 ─┼─→ URL Channel ─→ Process ─→ Store
Worker 3 ─┤
...       │
         Concurrent fetch & parse
```

- Each worker runs in its own goroutine
- Shared database connection pool
- Channel-based work distribution

## Graceful Shutdown

```
os.Signal (SIGINT/SIGTERM)
    ↓
ctx.cancel() → All workers stop
    ↓
spider.Stop() → Wait for completion
    ↓
Spider.Close() → Cleanup resources
```

## Configuration Parameters

```env
SPIDER_MAX_CRAWLERS=5              # Number of worker goroutines
SPIDER_MAX_CONCURRENT_FETCH=10     # HTTP requests in parallel
SPIDER_HTTP_TIMEOUT=30             # Per-request timeout (seconds)
SPIDER_CRAWL_TIMEOUT=60            # Overall crawl timeout
SPIDER_CRAWL_DELAY=100             # Delay between requests (microseconds)
SPIDER_LOGS_PATH=./logs/spider.log # Log file path
```

## Performance Tuning

| Parameter | Effect | Tradeoff |
|-----------|--------|----------|
| MaxCrawlers | Higher = faster | Uses more memory/connections |
| MaxConcurrentFetch | Higher = faster | Higher server load |
| CrawlDelay | Lower = faster | May get blocked |

## Logging

Logs to file as JSON and console:

```json
{
  "time": "2026-03-02T10:00:00Z",
  "level": "info",
  "message": "Fetching URL",
  "crawler_id": 1,
  "url": "https://example.com"
}
```

View logs:
```bash
tail -f logs/spider.log
```

## Error Handling

Handles gracefully:
- Network timeouts
- Invalid HTML
- Connection failures
- Database errors
- Malformed URLs

## Parser Features

### HTML Parsing
- Extracts `<a href>` links
- Normalizes relative URLs to absolute
- Filters duplicate URLs

### robots.txt Support ✅
- Fetches and parses robots.txt from each domain
- Respects user-agent rules (disallow, allow)
- Reads crawl-delay from robots.txt
- Extracts sitemaps from robots.txt
- Falls back to default crawl delay if not specified

### Per-Domain Rate Limiting ✅
- Redis-based caching of host metadata
- Applies crawl-delay per domain
- Stores robots.txt rules per host
- Tracks delay and retry logic per domain

### Sitemap Support
- Parses sitemap.xml
- Extracts URLs from sitemaps
- Follows sitemap index files

### Link Extraction
- Respects `<base>` tag
- Handles encoded URLs
- Filters fragment URLs (#anchor)

## Important Limitations ⚠️

1. **No JavaScript** - Can't crawl JS-rendered content
2. **No redirect handling** - Limited redirect support
3. **No authentication** - Can't login to sites

## Example Crawl Session

```
1. Start with: https://en.wikipedia.org/wiki/Search_engine
2. Fetch & parse → Extract 47 links
3. Add to queue → URLs for pages like "Information_retrieval", "Database", etc.
4. Fetch each → Parse → Extract more links
5. Continue for hours/days until interrupted
```

## Database Growth

Expected database sizes:
- 1,000 pages: ~10MB
- 10,000 pages: ~100MB
- 100,000 pages: ~1GB

Includes HTML content, so varies by average page size.

## Common Issues

### Worker stuck
Check network connectivity and timeouts

### Slow crawling
- Increase `MaxCrawlers`
- Decrease `CrawlDelay`
- Check database performance

### Duplicate URLs
Database unique constraint prevents duplicates

### Out of memory
Reduce workers or crawl smaller domains

## Development

```bash
# Build
go build -o spider cmd/spider/main.go

# Run
./spider

# Debug
go run cmd/spider/main.go
# CTRL+C to stop

# Logs
tail -f logs/spider.log
```
