#!/usr/bin/env bash
# Generate services/<svc>/.env.docker: the container-network view of the root
# .env.
#
# A service .env is written for whichever environment the operator runs it in.
# A laptop run needs localhost; a container run needs the compose service names.
# Rather than making `just dev` and `just run` fight over one file, the compose
# files read .env first and this generated overlay second, so the overlay wins
# for addresses and keeps the credentials in sync with the root .env.
set -euo pipefail

cd "$(dirname "$0")/.."

if [ ! -f .env ]; then
  echo "missing .env; run 'just env' first" >&2
  exit 1
fi

set -a
# shellcheck disable=SC1091
. ./.env
set +a

: "${PG_USER:?PG_USER is not set in .env}"
: "${PG_PASSWORD:?PG_PASSWORD is not set in .env}"
: "${PG_DBNAME:?PG_DBNAME is not set in .env}"
: "${RABBITMQ_DEFAULT_USER:?RABBITMQ_DEFAULT_USER is not set in .env}"
: "${RABBITMQ_DEFAULT_PASS:?RABBITMQ_DEFAULT_PASS is not set in .env}"

# Escape a value for a compose env_file: a bare '#' would start a comment and a
# quote would end the value early.
emit() {
  local name="$1" value="$2" escaped
  escaped=$(printf '%s' "$value" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')
  printf '%s="%s"\n' "$name" "$escaped"
}

# Credentials embedded in a URL must be percent-encoded.
urlenc() {
  python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$1"
}

pg_user_enc=$(urlenc "$PG_USER")
pg_password_enc=$(urlenc "$PG_PASSWORD")
rabbit_user_enc=$(urlenc "$RABBITMQ_DEFAULT_USER")
rabbit_password_enc=$(urlenc "$RABBITMQ_DEFAULT_PASS")

{
  emit PG_HOST psql
  emit PG_PORT 5432
  emit PG_USER "$PG_USER"
  emit PG_PASSWORD "$PG_PASSWORD"
  emit PG_DBNAME "$PG_DBNAME"
} > services/engine/.env.docker

{
  emit PG_HOST psql
  emit PG_PORT 5432
  emit PG_USER "$PG_USER"
  emit PG_PASSWORD "$PG_PASSWORD"
  emit PG_DBNAME "$PG_DBNAME"
  emit REDIS_ADDR redis
  emit REDIS_PORT 6379
  emit REDIS_PASSWORD "${REDIS_PASSWORD:-}"
  emit RABBITMQ_HOST rabbitmq
  emit RABBITMQ_PORT 5672
  emit RABBITMQ_USER "$RABBITMQ_DEFAULT_USER"
  emit RABBITMQ_PASSWORD "$RABBITMQ_DEFAULT_PASS"
  emit LOGS_PATH /app/logs/logs.json
} > services/spider/.env.docker

{
  emit DATABASE_URL "postgres://${pg_user_enc}:${pg_password_enc}@psql:5432/${PG_DBNAME}"
  emit RABBITMQ_URL "amqp://${rabbit_user_enc}:${rabbit_password_enc}@rabbitmq:5672"
} > services/indexer/.env.docker

{
  emit PG_HOST psql
  emit PG_PORT 5432
  emit PG_USER "$PG_USER"
  emit PG_PASSWORD "$PG_PASSWORD"
  emit PG_DBNAME "$PG_DBNAME"
  emit BROKER_URL "amqp://${rabbit_user_enc}:${rabbit_password_enc}@rabbitmq:5672"
  emit RABBITMQ_URL "amqp://${rabbit_user_enc}:${rabbit_password_enc}@rabbitmq:5672"
} > services/ranking/.env.docker

echo "container env overlays written to services/*/.env.docker"
