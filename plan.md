# Boogle Improvement Plan

## Phase 1 — Stabilize the Application

### Code quality
- [ ] Fix existing TODOs and placeholders
- [x] Fix naming inconsistencies (`craller` → `crawler`)
- [ ] Fix incorrect formatting/error handling
- [ ] Standardize service configuration
- [ ] Remove dead/commented-out code
- [ ] Validate environment variables at startup
- [ ] Document service dependencies

### Error handling
- [ ] Define consistent error types
- [ ] Add validation errors
- [ ] Add not-found errors
- [ ] Add database errors
- [ ] Standardize error responses across services
- [ ] Make errors observable through structured logs

---

# Phase 2 — Testing

## Unit tests

- [ ] Engine
  - [ ] Search query parsing
  - [ ] Result processing
  - [ ] Spell checking
  - [ ] Handlers/services

- [ ] Spider
  - [ ] URL parsing
  - [ ] Link extraction
  - [ ] Crawl logic
  - [ ] robots.txt handling
  - [ ] Error/retry logic

- [ ] Indexer
  - [ ] HTML parsing
  - [ ] Word extraction
  - [ ] Normalization
  - [ ] TF calculation

- [ ] Ranking
  - [ ] TF-IDF
  - [ ] PageRank
  - [ ] Score calculation

## Integration tests

- [ ] PostgreSQL integration tests
- [ ] Redis integration tests
- [ ] RabbitMQ integration tests
- [ ] Spider → Indexer
- [ ] Indexer → Ranking
- [ ] Search → PostgreSQL
- [ ] Database migration tests

## End-to-end tests

- [ ] Start complete system
- [ ] Crawl a test website
- [ ] Verify pages are stored
- [ ] Verify pages are indexed
- [ ] Verify ranking is generated
- [ ] Search for indexed content
- [ ] Verify expected results

## Testing infrastructure

- [ ] Add test fixtures
- [ ] Add test database
- [ ] Add test Docker Compose
- [ ] Add coverage reporting
- [ ] Define minimum coverage targets

---

# Phase 3 — Event-Driven Architecture

## RabbitMQ

- [ ] Add RabbitMQ
- [ ] Define exchanges
- [ ] Define queues
- [ ] Define routing keys
- [ ] Define event schemas
- [ ] Add message versioning

### Pipeline events

- [ ] `page.discovered`
- [ ] `page.indexed`
- [ ] `page.failed`
- [ ] `ranking.completed`
- [ ] `ranking.failed`

### Worker queues

- [ ] Spider → Indexer queue
- [ ] Indexer → Ranking queue
- [ ] Configure multiple consumers
- [ ] Configure prefetch
- [ ] Configure acknowledgements

### Reliability

- [ ] ACK/NACK handling
- [ ] Retry mechanism
- [ ] Dead-letter queues
- [ ] Idempotent consumers
- [ ] Message correlation IDs
- [ ] Graceful consumer shutdown
- [ ] Handle consumer crashes

### Pub/Sub

- [ ] Create event exchange
- [ ] Monitoring subscribes to events
- [ ] Collect page discovery metrics
- [ ] Collect indexing metrics
- [ ] Collect ranking metrics

### Database consistency

- [ ] Implement transactional outbox
- [ ] Create `outbox_events` table
- [ ] Implement outbox publisher
- [ ] Handle duplicate events

---

# Phase 4 — Observability

## Logging

- [ ] Standardize structured logs
- [ ] Add service name
- [ ] Add request ID
- [ ] Add correlation ID
- [ ] Add event/message ID
- [ ] Add log levels
- [ ] Remove sensitive information from logs

## Metrics

- [ ] Add Prometheus metrics
- [ ] HTTP request count
- [ ] HTTP latency
- [ ] Crawl rate
- [ ] Pages discovered
- [ ] Pages indexed
- [ ] Indexing failures
- [ ] Ranking duration
- [ ] Search latency
- [ ] RabbitMQ queue depth
- [ ] RabbitMQ unacknowledged messages

## Dashboards

- [ ] Create Grafana
- [ ] Application dashboard
- [ ] RabbitMQ dashboard
- [ ] PostgreSQL dashboard
- [ ] Kubernetes dashboard
- [ ] Service health dashboard

## Alerting

- [ ] Service unavailable
- [ ] High error rate
- [ ] Queue growing continuously
- [ ] High indexing latency
- [ ] Database unavailable
- [ ] High resource usage

---

# Phase 5 — Security

## Application

- [ ] Validate all user input
- [ ] Review SQL queries
- [ ] Verify HTML escaping
- [ ] Add security headers
- [ ] Configure CORS correctly
- [ ] Add rate limiting
- [ ] Review CSRF requirements
- [ ] Review authentication requirements

## Service communication

- [ ] Don't hardcode credentials
- [ ] Use Kubernetes Secrets
- [ ] Rotate credentials
- [ ] Define service authentication if needed

## Crawler

- [ ] Implement robots.txt
- [ ] Configure User-Agent
- [ ] Add crawl rate limits
- [ ] Prevent uncontrolled crawling
- [ ] Handle malicious/huge pages safely

## Dependency security

- [ ] `cargo audit`
- [ ] Go dependency scanning
- [ ] Python dependency scanning
- [ ] Container image scanning
- [ ] Dependabot/Renovate

---

# Phase 6 — Containerization

## Docker

- [ ] Production Dockerfile for Engine
- [ ] Production Dockerfile for Spider
- [ ] Production Dockerfile for Indexer
- [ ] Production Dockerfile for Ranking
- [ ] Production Dockerfile for Monitoring

## Image quality

- [ ] Multi-stage builds
- [ ] Minimal runtime images
- [ ] Non-root users
- [ ] Health checks
- [ ] Proper SIGTERM handling
- [ ] `.dockerignore`
- [ ] Pin important base image versions

## Registry

- [ ] Create GHCR repository
- [ ] Push images automatically
- [ ] Use immutable version tags
- [ ] Avoid relying only on `latest`

Example:

```text
ghcr.io/bagi/boogle-indexer:1.2.0
```

---

# Phase 7 — CI

Use GitHub Actions.

## Pull request pipeline

- [ ] Formatting
- [ ] Linting
- [ ] Unit tests
- [ ] Integration tests
- [ ] Build all services
- [ ] Dependency/security checks

```text
PR
 ↓
Lint
 ↓
Test
 ↓
Integration Test
 ↓
Build
 ↓
Security Scan
 ↓
PASS
```

## Main branch pipeline

- [ ] Run complete tests
- [ ] Build Docker images
- [ ] Scan images
- [ ] Push images to GHCR
- [ ] Generate version/tag
- [ ] Update deployment configuration

---

# Phase 8 — Kubernetes / k3s

## Cluster

- [ ] Create k3s cluster
- [ ] Configure namespaces
- [ ] Configure ingress
- [ ] Configure persistent storage
- [ ] Configure resource limits

## Services

- [ ] Engine Deployment
- [ ] Spider Deployment
- [ ] Indexer Deployment
- [ ] Ranking Deployment
- [ ] Monitoring Deployment

## Infrastructure

- [ ] PostgreSQL
- [ ] Redis
- [ ] RabbitMQ

## Kubernetes configuration

- [ ] Deployments
- [ ] Services
- [ ] ConfigMaps
- [ ] Secrets
- [ ] PersistentVolumeClaims
- [ ] Ingress
- [ ] Resource requests
- [ ] Resource limits
- [ ] Liveness probes
- [ ] Readiness probes

---

# Phase 9 — Helm

Create a Helm chart:

```text
deploy/
└── helm/
    └── boogle/
        ├── Chart.yaml
        ├── values.yaml
        └── templates/
            ├── engine.yaml
            ├── spider.yaml
            ├── indexer.yaml
            ├── ranking.yaml
            ├── monitoring.yaml
            ├── ingress.yaml
            └── config.yaml
```

- [ ] Create Helm chart
- [ ] Parameterize image tags
- [ ] Parameterize replicas
- [ ] Parameterize resources
- [ ] Parameterize environment
- [ ] Create dev values
- [ ] Create production values
- [ ] Validate Helm templates
- [ ] Helm lint in CI

---

# Phase 10 — GitOps

## Argo CD

- [ ] Install Argo CD
- [ ] Create Argo CD Application
- [ ] Connect deployment repository
- [ ] Enable automatic synchronization
- [ ] Enable self-healing
- [ ] Enable pruning where appropriate
- [ ] Configure deployment history

Target:

```text
Developer
   │
   ▼
Git
   │
   ▼
GitHub Actions
   │
   ├── Test
   ├── Build
   ├── Scan
   └── Push image
          │
          ▼
        GHCR
          │
          ▼
   Deployment Git repo
          │
          ▼
       Argo CD
          │
          ▼
        k3s
```

---

# Phase 11 — GitOps Repository

Prefer separating application and infrastructure:

```text
boogle/
├── engine/
├── spider/
├── indexer/
├── ranking/
└── monitoring/
```

```text
boogle-infra/
├── helm/
│   └── boogle/
├── environments/
│   ├── dev/
│   └── prod/
└── argocd/
```

- [ ] Separate application and infrastructure repositories
- [ ] Store desired cluster state in Git
- [ ] No manual `kubectl apply` for normal deployments
- [ ] Review infrastructure changes through PRs
- [ ] Keep deployment history in Git

---

# Phase 12 — Scaling

## Horizontal scaling

- [ ] Scale Engine replicas
- [ ] Scale Indexer replicas
- [ ] Scale Ranking workers
- [ ] Test RabbitMQ work distribution

Example:

```text
Indexer replicas: 1
       ↓
Indexer replicas: 5
       ↓
RabbitMQ distributes jobs
```

## Autoscaling

- [ ] Configure HPA
- [ ] CPU/memory-based scaling
- [ ] Investigate RabbitMQ queue-depth scaling
- [ ] Test scale-up
- [ ] Test scale-down

---

# Phase 13 — Reliability

- [ ] Test service crashes
- [ ] Test worker crashes
- [ ] Test RabbitMQ restart
- [ ] Test PostgreSQL restart
- [ ] Test Redis restart
- [ ] Test network failures
- [ ] Test duplicate messages
- [ ] Test message retry
- [ ] Test dead-letter queues
- [ ] Test Kubernetes pod replacement

Document:

```text
What happens if Indexer crashes?
What happens if RabbitMQ crashes?
What happens if PostgreSQL crashes?
What happens if a message is processed twice?
What happens during deployment?
```

---

# Phase 14 — Deployment Strategy

- [ ] Rolling deployments
- [ ] Zero/minimal downtime deployment
- [ ] Versioned Docker images
- [ ] Database migration strategy
- [ ] Migration rollback strategy
- [ ] Kubernetes rollout monitoring
- [ ] Deployment rollback testing

Later:

- [ ] Canary deployment
- [ ] Blue/green deployment

---

# Phase 15 — Performance

- [ ] Benchmark crawler
- [ ] Benchmark indexer
- [ ] Benchmark ranking
- [ ] Benchmark search
- [ ] Profile PostgreSQL queries
- [ ] Add missing indexes
- [ ] Add Redis query caching
- [ ] Measure cache hit rate
- [ ] Load test Engine
- [ ] Load test RabbitMQ
- [ ] Load test indexing pipeline

Track:

```text
pages/sec
indexes/sec
searches/sec
messages/sec
search latency
indexing latency
queue latency
CPU
memory
DB connections
```

---

# Phase 16 — Documentation

- [ ] Architecture diagram
- [ ] Service documentation
- [ ] API documentation
- [ ] Event documentation
- [ ] RabbitMQ topology diagram
- [ ] Database diagram
- [ ] Deployment documentation
- [ ] Local development guide
- [ ] Kubernetes guide
- [ ] GitOps guide
- [ ] Troubleshooting guide
- [ ] Disaster recovery notes
- [ ] ADRs

Important ADRs:

```text
ADR-001: Why microservices?
ADR-002: Why RabbitMQ?
ADR-003: Events vs worker queues
ADR-004: Why Kubernetes/k3s?
ADR-005: Why GitOps?
ADR-006: Why Argo CD?
ADR-007: Transactional Outbox
ADR-008: PostgreSQL/Redis responsibilities
```

---

# Final Target

The final architecture should look roughly like:

```text
                         GitHub
                            │
                            ▼
                    GitHub Actions
                    ┌───────┴───────┐
                    │               │
                  Tests           Build
                    │               │
                    │               ▼
                    │             GHCR
                    │               │
                    └───────┬───────┘
                            │
                     Deployment Git
                            │
                            ▼
                         Argo CD
                            │
                            ▼
                    ┌─────────────────┐
                    │   k3s Cluster   │
                    │                 │
                    │ Engine × N      │
                    │ Spider × N      │
                    │ Indexer × N     │
                    │ Ranking × N     │
                    │ Monitoring      │
                    │                 │
                    │ RabbitMQ        │
                    │ PostgreSQL      │
                    │ Redis           │
                    │                 │
                    │ Prometheus      │
                    │ Grafana         │
                    └─────────────────┘
```

## Priority Order

If the goal is to actually finish this rather than accumulate technologies:

```text
[ ] 1. Fix existing code/issues
[ ] 2. Unit + integration tests
[ ] 3. RabbitMQ
[ ] 4. Event-driven pipeline
[ ] 5. ACK/retry/DLQ/idempotency
[ ] 6. Transactional outbox
[ ] 7. Structured logging + metrics
[ ] 8. Production Docker images
[ ] 9. GitHub Actions CI
[ ] 10. GHCR
[ ] 11. k3s
[ ] 12. Kubernetes manifests
[ ] 13. Helm
[ ] 14. Argo CD
[ ] 15. GitOps repository
[ ] 16. Prometheus + Grafana
[ ] 17. Scaling/HPA
[ ] 18. Security scanning
[ ] 19. Failure/load testing
[ ] 20. Documentation + ADRs
```

The key progression is **application reliability → messaging → containers → CI → Kubernetes → GitOps → observability → scaling**. That gives each new DevOps component a real problem to solve instead of adding infrastructure arbitrarily.
