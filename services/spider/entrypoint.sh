#!/bin/sh
set -eu

# Required
: "${PG_HOST:?PG_HOST is not set}"
: "${PG_PORT:?PG_PORT is not set}"
: "${PG_USER:?PG_USER is not set}"
: "${PG_PASSWORD:?PG_PASSWORD is not set}"
: "${PG_DBNAME:?PG_DBNAME is not set}"
: "${REDIS_ADDR:?REDIS_ADDR is not set}"
: "${REDIS_PORT:?REDIS_PORT is not set}"
: "${RABBITMQ_USER:?RABBITMQ_USER is not set}"
: "${RABBITMQ_PASSWORD:?RABBITMQ_PASSWORD is not set}"
: "${RABBITMQ_HOST:?RABBITMQ_HOST is not set}"

export PG_MAX_OPEN_CONNS="${PG_MAX_OPEN_CONNS:-20}"
export PG_MAX_IDLE_CONNS="${PG_MAX_IDLE_CONNS:-20}"
export PG_MAX_CONN_LIFETIME="${PG_MAX_CONN_LIFETIME:-0}"
export PG_BATCH_SIZE="${PG_BATCH_SIZE:-30}"

export REDIS_PORT="${REDIS_PORT:-6379}"
export REDIS_PASSWORD="${REDIS_PASSWORD:-}"
export REDIS_DB="${REDIS_DB:-1}"
export REDIS_DELAY="${REDIS_DELAY:-5}"
export REDIS_MAX_RETRY="${REDIS_MAX_RETRY:-10}"

export MAX_CRAWLERS="${MAX_CRAWLERS:-20}"
export MAX_CONCURRENT_FETCH="${MAX_CONCURRENT_FETCH:-200}"

export HTTP_TIMEOUT="${HTTP_TIMEOUT:-60}"
export CRAWLER_TIMEOUT="${CRAWLER_TIMEOUT:-60}"
export CRAWLER_DELAY="${CRAWLER_DELAY:-200}"

export LOGS_PATH="${LOGS_PATH:-/app/logs/logs.json}"

# Ensure the log directory exists
mkdir -p "$(dirname "$LOGS_PATH")"

exec ./spider "$@"
