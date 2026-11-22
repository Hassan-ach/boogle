# Engine Service - Detailed Guide

## Overview

The Engine is the web server and user-facing interface. It handles HTTP requests, processes search queries, and returns ranked results from Postgres (pages, words, page_word, page_rank).

## Architecture

```
User Query → Echo HTTP Server → Store (PostgreSQL) → Ranking Service → Results → Templ Templates → HTML Response
                                      ↓
                                Spell Checker
```

## Technology Stack

- **Go 1.21+** - Language
- **Echo v5** - HTTP framework
- **Templ** - Type-safe templating
- **Tailwind CSS** - Styling
- **HTMX** - Dynamic interactions
- **Alpine.js** - Frontend logic

## Project Structure

```
engine/
├── cmd/server/main.go          # Entry point
├── internal/
│   ├── apperror/               # Error types & handling
│   ├── config/                 # Configuration loading
│   ├── handlers/               # HTTP request handlers
│   ├── middleware/             # HTTP middleware
│   ├── model/                  # Data models
│   ├── service/                # Business logic (ranking, spell-check)
│   ├── store/                  # Database queries
│   └── util/                   # Utilities
├── view/                       # Templ templates
├── public/                     # Compiled assets (CSS, JS)
├── static/                     # Source assets (CSS, JS)
└── justfile                    # Build commands
```

## Key Components

### Handlers
- `search.handler.go` - Main search query handler
- `home.handler.go` - Home page handler
- `error.handler.go` - Global error handler

### Services
- `ranking/` - Coordinates ranking service
- `spellchecker/` - Query suggestions

### Store
- Database operations for retrieving pages and word data
- Pagination support

## API Endpoints

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/` | GET | Home page |
| `/search` | GET | Search results |
| `/feeling-lucky` | GET | Random result |
| `/public/*` | GET | Static assets |

## Database Queries

### Get Search Results

```sql
WITH ranked AS (
    SELECT p.id, u.url, pr.score, p.metadata, 
           COUNT(DISTINCT w.id) AS word_count,
           json_agg(...) AS word_set
    FROM words w
    INNER JOIN page_word pw ON w.id = pw.word_id
    ...
    WHERE w.word = ANY($1)
    GROUP BY p.id, pr.score, u.url, p.metadata
)
SELECT * FROM ranked
ORDER BY word_count DESC, pr DESC
LIMIT $2 OFFSET $3
```

## Templates

### Component Structure
```
view/
├── component/          # Reusable components
│   ├── logo.templ
│   ├── page_card.templ
│   ├── pagination.templ
│   └── svgs.templ
├── layout/             # Page layouts
│   └── layout.templ
└── page/               # Full pages
    ├── home/
    ├── result/
    └── error/
```

## Frontend Assets

### CSS
- Input: `static/css/input.css` (Tailwind)
- Output: `public/css/styles.css` (built)
- Built with: `tailwindcss` CLI

### JavaScript
- Input: `static/js/main.js`
- Output: `public/js/index.js` (bundled)
- Bundled with: `esbuild`

## Error Handling

Custom `AppError` type:
```go
type AppError struct {
    Code    int    // HTTP status
    Message string // User message
    Err     error  // Internal error
}
```

Error types:
- `Internal()` - 500 errors
- (Future: Validation, NotFound, Unauthorized, etc.)

## Configuration

Loads from `../../.env`:
```env
PG_USER=postgres
PG_PASSWORD=password
PG_DBNAME=boogle
PG_PORT=5432
PG_HOST=localhost
REDIS_PORT=6379
```

## Development Workflow

```bash
# Install dependencies
go mod tidy
npm install

# Generate templates
just templ-generate

# Build assets
just tailwind-build
just js-build

# Run with hot reload
just dev

# Production build
just build
```

## Justfile Commands

```bash
just templ-generate    # Generate Go from Templ
just templ-watch       # Watch Templ changes
just tailwind-build    # Build CSS
just tailwind-watch    # Watch CSS changes
just js-build          # Bundle JavaScript
just js-watch          # Watch JS changes
just dev               # Development mode (all watches)
just build             # Production build binary
just cli <query>       # Run CLI with query
```

## Performance Considerations

1. **Response Caching** - Consider Redis for popular queries
2. **Database Indexing** - Queries use efficient indexes
3. **Pagination** - Default 10 results per page
4. **GZIP Compression** - Enabled via middleware

## Middleware Stack

```
Recover() → RequestID() → RequestLogger() → Gzip(level=5) → RateLimit(20/sec) → Routes
```

## Security Notes

⚠️ **Not for Production**
- CORS middleware commented out
- Secure middleware commented out
- No authentication
- Add proper validation before production use

## Spell Checker Integration

Uses aspell library:
- `spellchecker.NewAspellSpellingService()`
- Provides query suggestions
- Integrated into search handler

## HTMX Integration

Dynamic search updates without full page reload:
- `HX-Request` header detection
- Partial template rendering
- Optimized pagination load

## Testing Notes

Currently no test files. Consider adding:
- Handler tests (mock database)
- Template tests
- Search logic tests

## Common Issues

### Templates not generating
```bash
just templ-generate
```

### CSS not updating
```bash
just tailwind-build
```

### JavaScript bundle failing
```bash
npm install
just js-build
```

### Database not accessible
Check `.env` connection strings and network connectivity

## Future Improvements

- [ ] Complete images tab
- [ ] Complete graph visualization
- [ ] Query result caching
- [ ] Advanced search operators
- [ ] Search analytics
- [ ] Authentication layer
