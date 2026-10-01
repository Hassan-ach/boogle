#!/bin/sh

set -eu

: "${DEBUG:=false}"
: "${LOG_LEVEL:=info}"
: "${PG_HOST:? PG_HOST is not set}"
: "${PG_PORT:? PG_PORT is not set}"
: "${PG_DBNAME:? PG_DBNAME is not set}"
: "${PG_USER:? PG_USER is not set}"
: "${PG_PASSWORD:? PG_PASSWORD is not set}"
: "${PG_MAX_OPEN_CONNS:=20}"
: "${PG_MAX_IDLE_CONNS:=20}"
: "${PAGE_SIZE:=20}"
: "${RANKER_MAX_RESULTS:=100}"
: "${RANKER_WEIGHT_TF:=0.5}"

export DEBUG="${DEBUG:-false}"
export LOG_LEVEL="${LOG_LEVEL:-info}"
export PG_MAX_OPEN_CONNS="${PG_MAX_OPEN_CONNS:-20}"
export PG_MAX_IDLE_CONNS="${PG_MAX_IDLE_CONNS:-20}"
export PAGE_SIZE="${PAGE_SIZE:-20}"
export RANKER_MAX_RESULTS="${RANKER_MAX_RESULTS:-100}"
export RANKER_WEIGHT_TF="${RANKER_WEIGHT_TF:-0.5}"


exec ./boogle "$@"
