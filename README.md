# Boogle - Learning Search Engine

A 2000s-style search engine built for educational purposes.

<div align="center">
  <img src="https://github.com/Hassan-ach/boogle/blob/main/assets/search_page.png" alt="Search Page Screenshot" width="400">
  <img src="https://github.com/Hassan-ach/boogle/blob/main/assets/result_page.png" alt="Search Page Screenshot" width="400">
</div>

## Architecture Overview

<div align="center">
  <img src="https://github.com/Hassan-ach/boogle/blob/main/assets/arch_design.png" alt="Architecture Diagram" width="800">
</div>

## Quick Start

```bash
# Copy and configure environment
cp .env.example .env

# Deploy everything (creates network, builds images, starts all services)
./deploy.sh

# Open browser
open http://localhost:1323
```

## Services

| Service | Language | Purpose |
|---------|----------|---------|
| [Engine](services/engine/) | Go | Web interface, query handling, search results |
| [Spider](services/spider/) | Go | Web crawler, link discovery and frontier management |
| [Indexer](services/indexer/) | Rust | HTML parsing, word extraction, TF calculation |
| [Ranking](services/ranking/) | Python | TF-IDF and PageRank scoring |

## Pipeline

```
Web → Spider (RabbitMQ) → Indexer (RabbitMQ) → Ranking (RabbitMQ) → Engine → User
```

## Requirements

- Docker & Docker Compose V2
- Git

All other dependencies (PostgreSQL, Redis, language runtimes) run inside containers.

## Setup

1. Clone the repo and `cd` into it
2. Copy `.env.example` to `.env` and fill in real secrets
3. Copy each `services/<svc>/.env.example` to `services/<svc>/.env`
4. Run `./deploy.sh`

For a full teardown and clean rebuild:

```bash
./deploy.sh --pull --build --restart
```

## Docs

- [Services Overview](services/README.md) — service responsibilities and data flow
- [docs/ENGINE.md](docs/ENGINE.md) — search architecture and templates
- [docs/SPIDER.md](docs/SPIDER.md) — crawling algorithm and database schema
- [docs/INDEXER.md](docs/INDEXER.md) — TF calculation and word extraction
- [docs/RANKING.md](docs/RANKING.md) — PageRank math and IDF scoring


## Development

See individual service READMEs in [services/](services/) for language-specific build commands.

---

**Status**: Learning project — not for production
**License**: MIT
