# Root task runner for the boogle monorepo.
# Each service keeps its own justfile; this fans out across them.

services := "engine indexer ranking spider"

# Run every service's fast unit tests. No infrastructure required.
default: test

test:
    @for svc in engine spider; do \
        echo "==> go test: services/$$svc"; \
        (cd services/$$svc && go test ./internal/... -count=1) || exit 1; \
    done
    @echo "==> cargo test: services/indexer"
    @cd services/indexer && cargo test
    @echo "==> pytest: services/ranking"
    @cd services/ranking && venv/bin/python -m pytest tests -m "not integration"

test-cov:
    @for svc in engine spider; do \
        echo "==> coverage: services/$$svc"; \
        (cd services/$$svc && go test ./internal/... -count=1 -coverprofile=coverage.out && go tool cover -func=coverage.out | tail -1) || exit 1; \
    done
    @cd services/ranking && venv/bin/python -m pytest tests -m "not integration" --cov=src --cov-report=term-missing

# Bring up Postgres/Redis/RabbitMQ and create the boogle_test database.
infra-up:
    docker compose up -d psql redis rabbitmq
    @echo "waiting for postgres..."
    @until docker compose exec -T psql pg_isready -U $${PG_USER} >/dev/null 2>&1; do sleep 1; done
    @docker compose exec -T psql psql -U $${PG_USER} -d postgres -tc \
        "SELECT 1 FROM pg_database WHERE datname='boogle_test'" | grep -q 1 \
        || docker compose exec -T psql psql -U $${PG_USER} -d postgres -c "CREATE DATABASE boogle_test"
    @echo "infra ready"

infra-down:
    docker compose down

# Run the integration suites. Requires `just infra-up` (done automatically here).
test-integration:
    @just infra-up
    @export TEST_DATABASE_URL="postgres://$${PG_USER}:$${PG_PASSWORD}@localhost:$${PG_PORT}/boogle_test?sslmode=disable"
    @for svc in engine spider; do \
        echo "==> go integration: services/$$svc"; \
        (cd services/$$svc && TEST_DATABASE_URL="$$TEST_DATABASE_URL" go test -tags integration ./internal/... -count=1) || exit 1; \
    done
    @echo "==> rust integration: services/indexer"
    @cd services/indexer && DATABASE_URL="$$TEST_DATABASE_URL" cargo test -- --ignored
    @echo "==> python integration: services/ranking"
    @cd services/ranking && TEST_DATABASE_URL="$$TEST_DATABASE_URL" venv/bin/python -m pytest tests -m integration

test-all: test test-integration

clean:
    @for svc in engine spider; do rm -f services/$$svc/coverage.out; done
    @find services/ranking -name '__pycache__' -type d -prune -exec rm -rf {} +