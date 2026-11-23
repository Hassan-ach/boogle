#!/usr/bin/env bash
# Apply every migration/*.sql to the running database, in filename order.
# Migrations are idempotent, so this is safe on every deploy. Postgres only runs
# /docker-entrypoint-initdb.d on an empty PGDATA, so this handles later schema changes.
set -euo pipefail

cd "$(dirname "$0")/.."

if [ -f .env ]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

: "${PG_USER:?PG_USER is not set; copy .env.example to .env first}"
: "${PG_DBNAME:?PG_DBNAME is not set; copy .env.example to .env first}"

if ! docker compose ps --status running --services | grep -qx psql; then
  echo "psql container is not running; start it with: docker compose up -d psql" >&2
  exit 1
fi

for migration in migration/*.sql; do
  printf 'applying %s\n' "$migration"
  docker compose exec -T psql \
    psql -v ON_ERROR_STOP=1 -q -U "$PG_USER" -d "$PG_DBNAME" < "$migration"
done

echo "migrations applied"
