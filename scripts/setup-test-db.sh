#!/usr/bin/env bash
# Create (or reset) the isolated `boogle_test` database and apply the migrations.
#
# Integration tests must never touch the live `boogle` database: the PageRank
# and IDF tests write scores and would otherwise shift every stored ranking.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

PGHOST="${PG_HOST:-localhost}"
PGPORT="${PG_PORT:-5432}"
PGUSER="${PG_USER:-admin}"
PGPASSWORD="${PG_PASSWORD:-1234}"
TEST_DB="${BOOGLE_TEST_DB:-boogle_test}"

# Inside docker-compose the host is the service name ("psql"), which does not
# resolve outside the network. Fall back to localhost for local runs unless the
# caller has said otherwise.
if [[ -z "${BOOGLE_TEST_PGHOST:-}" && "${PGHOST}" != "localhost" && "${PGHOST}" != "127.0.0.1" ]]; then
  if ! getent hosts "$PGHOST" >/dev/null 2>&1; then
    echo "==> host ${PGHOST} does not resolve; using localhost"
    PGHOST="localhost"
  fi
fi

export PGPASSWORD

if [[ -f "$ROOT/.env" ]]; then
  # shellcheck disable=SC2046
  eval "$(grep -E '^(PG_|POSTGRES_)' "$ROOT/.env" | sed 's/^/export /')"
  PGHOST="${BOOGLE_TEST_PGHOST:-$PGHOST}"
  PGPASSWORD="${PG_PASSWORD:-${POSTGRES_PASSWORD:-$PGPASSWORD}}"
fi

export PGHOST PGPASSWORD PGUSER PGPORT

echo "==> dropping and recreating ${TEST_DB}"
psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d postgres -v ON_ERROR_STOP=1 <<SQL
DROP DATABASE IF EXISTS ${TEST_DB};
CREATE DATABASE ${TEST_DB};
SQL

echo "==> applying migrations to ${TEST_DB}"
for f in "$ROOT"/migration/*.sql; do
  echo "    $(basename "$f")"
  psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$TEST_DB" -v ON_ERROR_STOP=1 -q -f "$f"
done

echo "==> ${TEST_DB} is ready"
