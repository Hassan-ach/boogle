#!/bin/sh
set -eu

# Database: DATABASE_URL wins, otherwise every PG_* variable is required. The
# application accepts either form; requiring both used to reject a valid
# DATABASE_URL-only deployment (M-10).
if [ -z "${DATABASE_URL:-}" ]; then
  : "${PG_HOST:?DATABASE_URL or PG_HOST must be set}"
  : "${PG_PORT:?DATABASE_URL or PG_PORT must be set}"
  : "${PG_USER:?DATABASE_URL or PG_USER must be set}"
  : "${PG_PASSWORD:?DATABASE_URL or PG_PASSWORD must be set}"
  : "${PG_DBNAME:?DATABASE_URL or PG_DBNAME must be set}"
fi

# Broker: a URL, or the individual credentials the app can assemble into one.
if [ -z "${BROKER_URL:-}" ] && [ -z "${RABBITMQ_URL:-}" ]; then
  : "${RABBITMQ_USER:?BROKER_URL, RABBITMQ_URL, or RABBITMQ_USER and RABBITMQ_PASSWORD must be set}"
  : "${RABBITMQ_PASSWORD:?BROKER_URL, RABBITMQ_URL, or RABBITMQ_USER and RABBITMQ_PASSWORD must be set}"
fi

export RABBITMQ_CONFIRMATION_QUEUE="${RABBITMQ_CONFIRMATION_QUEUE:-indexer.confirmations}"

exec ./main "$@"
