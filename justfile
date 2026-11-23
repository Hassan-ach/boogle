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

# Copy every .env.example to .env when the file is missing. Existing files are
# never overwritten: they hold the credentials an operator set.
#
# Bootstrap missing .env files from the examples.
env:
    @[ -f .env ] || cp .env.example .env
    @for svc in engine spider indexer ranking; do \
        [ -f services/$svc/.env ] || cp services/$svc/.env.example services/$svc/.env; \
    done
    @./scripts/docker-env.sh
    @echo "env files ready (edit the change-me-* values before a real deployment)"

# Bring up Postgres/Redis/RabbitMQ.
infra-up:
    docker compose up -d psql redis rabbitmq adminer
    @echo "waiting for postgres..."
    @until docker compose exec -T psql pg_isready -U "${PG_USER:-admin}" >/dev/null 2>&1; do sleep 1; done
    @echo "waiting for rabbitmq..."
    @until docker compose exec -T rabbitmq rabbitmq-diagnostics -q ping >/dev/null 2>&1; do sleep 1; done
    @echo "infra ready"

# Fail early when a local engine (air/go run) already owns the only host port the
# stack publishes. Without this, `up` builds the image and only then fails with
# "address already in use", leaving a Created container behind.
#
# Check that the published engine port is free.
preflight:
    @if command -v ss >/dev/null 2>&1 \
        && ss -ltn 2>/dev/null | grep -q ':1323 ' \
        && ! docker ps -q -f 'name=^engine$' | grep -q .; then \
        echo "port 1323 is already in use; stop the local engine/air process first" >&2; \
        exit 1; \
    fi

# One-stop dev stack: bootstrap env files, start the datastores, apply
# migrations, then build and start all four services detached.
#
# Ctrl-C is not needed: the services run in the background. `just run-logs`
# follows their output and `just stop` tears everything down without deleting
# the pgdata volume.
#
# Start datastores, migrate, then build and run all four services.
run: env preflight infra-up migrate
    @for svc in engine spider indexer ranking; do \
        echo "==> building and starting services/$svc"; \
        docker compose -f services/$svc/docker-compose.yml up -d --build || exit 1; \
    done
    @echo ""
    @echo "boogle is up:"
    @echo "  engine  http://localhost:1323"
    @echo "  adminer http://127.0.0.1:8080"
    @echo "  broker  http://127.0.0.1:15672"
    @echo ""
    @echo "follow logs with: just run-logs"
    @echo "stop with:        just stop"

# Follow all four services in one terminal. Ctrl-C stops only the log streams.
run-logs:
    @docker logs -f --tail=100 engine & \
     docker logs -f --tail=100 spider & \
     docker logs -f --tail=100 indexer & \
     docker logs -f --tail=100 ranking & \
     wait

# Stop the services and the datastores. The pgdata volume is kept.
stop:
    @for svc in engine spider indexer ranking; do \
        docker compose -f services/$svc/docker-compose.yml down; \
    done
    docker compose down

infra-down:
    docker compose down

# Create (or reset) the isolated boogle_test database and apply the migrations.
#
# Never the live database: the PageRank and IDF tests write scores, and running
# them against the real index would move every stored ranking.
test-db:
    bash scripts/setup-test-db.sh

# Apply migration/*.sql to the running compose database. Idempotent.
migrate:
    ./scripts/migrate.sh

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

# === Documentation ===
# Markdown lives in docs/ and services/<svc>/docs/; scripts/docs.py aggregates
# the service pages, then builds or serves the MkDocs site in strict mode.
docs-install:
    python3 scripts/docs.py install

docs-build:
    python3 scripts/docs.py build

docs-serve:
    python3 scripts/docs.py serve

docs-service svc:
    python3 scripts/docs.py service {{svc}}

docs-check:
    python3 scripts/docs.py check
