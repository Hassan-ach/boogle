# Ranking Service

Calculates page importance & relevance scores.

**Language**: Python 3.9+  
**Algorithms**: TF-IDF, PageRank  
**Messaging**: FastStream + RabbitMQ

## Quick Start

```bash
python3 -m venv venv
source venv/bin/activate
pip install -r requirements.txt
python src/main.py
```

Press CTRL+C to stop.

## Features

- Event-driven: consumes indexer confirmations from RabbitMQ
- Triggers IDF and PageRank computation once a threshold of indexed pages is reached (default 100)
- Computes IDF via SQL over full corpus, PageRank via PL/pgSQL function
- Runs IDF and PageRank concurrently
- Resilient to failures, handles catch-up on bursts

## Project Structure

- `src/main.py` - Entry point, logging, env validation, lifecycle
- `src/calculator.py` - IDF and PageRank computation logic
- `src/messaging.py` - RabbitMQ consumption and trigger management
- `src/psql.py` - Database connection pool and retries

## Configuration

```env
# Database
DATABASE_URL=postgresql://...
# or PG_HOST/PORT/DBNAME/USER/PASSWORD

# Broker
BROKER_URL=amqp://...
# or RABBITMQ_URL / RABBITMQ_USER/PASSWORD/HOST/PORT

# Behavior (defaults)
RABBITMQ_CONFIRMATION_QUEUE=indexer.confirmations
# RANKING_DAMPING_FACTOR=0.85
# RANKING_ITERATIONS=20
```

## Input/Output

**Reads from**:
- `pages` table (corpus size)
- `words`, `page_word` tables (for TF-IDF)
- `graph_edges` table (for PageRank via DB function)

**Writes to**:
- `words` table (IDF values)
- `page_rank` table (page scores)

## Details

See [docs/RANKING.md](../../docs/RANKING.md) for algorithm details, math formulas, and examples.

## Notes

⚠️ Learning project - recomputes over full corpus/graph each run.
