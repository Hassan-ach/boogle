# Spider Service

Web crawler that discovers and downloads pages.

**Language**: Go 1.27  
**Purpose**: Crawl websites, extract links, store HTML, publish indexing jobs to RabbitMQ

## Quick Start

```bash
go mod tidy
go run cmd/spider/main.go
```

Press CTRL+C to stop gracefully.

## Features

- Policy-driven crawling with frontier management, host state, backoff, and rate limiting
- robots.txt parsing and enforcement
- Sitemap discovery
- Language filtering (English)
- Deduplication and canonicalization
- Publishes jobs to RabbitMQ (`indexer.jobs`) after persisting pages
- PostgreSQL + Redis for state and caching

## Project Structure

- `cmd/spider/main.go` - Entry point with signal handling
- `internal/spider/` - Core crawling logic
- `internal/policy/` - Admission, classification, host/frontier management
- `internal/parser/` - HTML parsing, sitemap, robots
- `internal/store/` - Database and cache operations
- `internal/messaging/` - RabbitMQ publishing
- `internal/config/` - Configuration

## Configuration

See root `.env.example` or set environment variables. Key variables include `SPIDER_MAX_CRAWLERS`, `SPIDER_MAX_CONCURRENT_FETCH`, `SPIDER_HTTP_TIMEOUT`, `SPIDER_CRAWL_DELAY`, Redis/Postgres/RabbitMQ settings, and extensive policy tuning (`SPIDER_*`).

## Output

Stores to PostgreSQL:
- `urls` table - Discovered URLs
- `pages` table - HTML content
- `graph_edges` table - Link relationships

Publishes indexing jobs to RabbitMQ after successful persistence.

## Details

See [docs/SPIDER.md](../../docs/SPIDER.md) for how it works, database schema, and troubleshooting.
