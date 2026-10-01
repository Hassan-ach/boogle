# Root task runner for the boogle monorepo.
# Each service keeps its own justfile; this fans out across them.
#
# Two tiers, deliberately separated:
#   just test              fast, no infrastructure, safe on every save
#   just test-integration  needs Postgres; creates its own boogle_test database
#
# Syntax note: recipes run under /bin/sh, which on Debian is dash, where `$$` is
# the shell's PID rather than a literal `$`. Shell variables in these loops are
# therefore written with a single dollar sign.

# Interpolated by just, so it must not be a shell loop variable.
go_services := "engine spider"

# Run every service's fast unit tests. No infrastructure required.
default: test

test:
    @for svc in {{go_services}}; do \
        echo "==> go test: services/$svc"; \
        (cd services/$svc && go test ./internal/... -count=1) || exit 1; \
    done
    @echo "==> cargo test: services/indexer"
    @cd services/indexer && cargo test
    @echo "==> pytest: services/ranking"
    @cd services/ranking && venv/bin/python -m pytest tests -m "not integration"

test-cov:
    @for svc in {{go_services}}; do \
        echo "==> coverage: services/$svc"; \
        (cd services/$svc && go test ./internal/... -count=1 -coverprofile=coverage.out \
            && go tool cover -func=coverage.out | tail -1) || exit 1; \
    done
    @cd services/ranking && venv/bin/python -m pytest tests -m "not integration" \
        --cov=src --cov-report=term-missing

# Bring up Postgres/Redis/RabbitMQ.
infra-up:
    docker compose up -d psql redis rabbitmq
    @echo "waiting for postgres..."
    @until docker compose exec -T psql pg_isready -U "${PG_USER:-admin}" >/dev/null 2>&1; do sleep 1; done
    @echo "infra ready"

infra-down:
    docker compose down

# Create (or reset) the isolated boogle_test database and apply the migrations.
#
# Never the live database: the PageRank and IDF tests write scores, and running
# them against the real index would move every stored ranking.
test-db:
    bash scripts/setup-test-db.sh

# Run the integration suites. Resets the test database first so the schema
# matches the migrations rather than drifting with whatever was left behind.
#
# The body is one continued shell block: each recipe line would otherwise get its
# own shell, and the exported DSN would not survive to the next one.
test-integration: test-db
    @export TEST_DATABASE_URL="postgres://${PG_USER:-admin}:${PG_PASSWORD:-1234}@localhost:${PG_PORT:-5432}/boogle_test?sslmode=disable"; \
    echo "target: $TEST_DATABASE_URL"; \
    for svc in {{go_services}}; do \
        echo "==> go integration: services/$svc"; \
        (cd services/$svc && TEST_DATABASE_URL="$TEST_DATABASE_URL" \
            go test -tags integration ./internal/... -count=1) || exit 1; \
    done; \
    echo "==> rust integration: services/indexer"; \
    (cd services/indexer && DATABASE_URL="$TEST_DATABASE_URL" cargo test -- --ignored) || exit 1; \
    echo "==> python integration: services/ranking"; \
    (cd services/ranking && TEST_DATABASE_URL="$TEST_DATABASE_URL" \
        venv/bin/python -m pytest tests -m integration)

test-all: test test-integration

clean:
    @for svc in {{go_services}}; do rm -f services/$svc/coverage.out; done
    @find services/ranking -name '__pycache__' -type d -prune -exec rm -rf {} +
