# Engine Service

Web server & search interface.

**Language**: Go 1.27  
**Framework**: Echo v5, Templ, Tailwind CSS, HTMX, Alpine.js

## Quick Start

```bash
go mod tidy
npm install
just dev
```

Open http://localhost:1323

## Features

- Full-text search over indexed pages with TF-IDF + PageRank blending
- Spelling suggestions (aspell with graceful fallback)
- Pagination
- HTMX partial updates
- Health endpoint at `/health`

## Project Structure

- `cmd/server/main.go` - Entry point and routing/middleware
- `cmd/cli/` - CLI for testing queries
- `internal/handlers/` - HTTP handlers (search, home, health, errors)
- `internal/service/` - Ranking and spellchecking logic
- `internal/store/` - Postgres data access
- `internal/config/` - Configuration
- `view/` - Templ templates
- `static/`, `public/` - Assets (built CSS/JS)

## Commands

```bash
just templ-generate  # Generate templates
just tailwind-build  # Build CSS
just js-build        # Build JavaScript
just dev             # Development mode (all watchers)
just build           # Production build
just test            # Run tests
```

## Configuration

Configuration loaded from environment (see root `.env.example`). Key variables: `PG_HOST/PORT/USER/PASSWORD/DBNAME`, `PAGE_SIZE`, `RANKER_MAX_RESULTS`, `RANKER_WEIGHT_TF`.

## Details

See [docs/ENGINE.md](../../docs/ENGINE.md) for architecture, configuration, and troubleshooting.

