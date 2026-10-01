#!/bin/sh
set -eu

# Required
: "${PG_HOST:?PG_HOST is not set}"
: "${PG_PORT:?PG_PORT is not set}"
: "${PG_USER:?PG_USER is not set}"
: "${PG_PASSWORD:?PG_PASSWORD is not set}"
: "${PG_DBNAME:?PG_DBNAME is not set}"
: "${BROKER_URL:?BROKER_URL is not set}"
: "${RABBITMQ_URL:?RABBITMQ_URL is not set}"
: "${RABBITMQ_CONFIRMATION_QUEUE:?RABBITMQ_CONFIRMATION_QUEUE is not set}"

exec ./main "$@"
