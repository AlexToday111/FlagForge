# Implementation phases and verification

The repository was implemented and exercised in successive groups:

1. Bootstrap and domain: `go.mod`, `internal/config`, `migrations`, `pkg/types`, `internal/repository`. Embedded Goose migration lock, relational schemas, explicit pgx transactions.
2. Evaluation and rollout: `pkg/evaluation`. Pure local engine, deterministic length-delimited hash, operator/type/reference validation and cycles. Unit tests include 100,000-subject distribution and evaluation precedence.
3. SDK and streaming: `pkg/sdk`, `internal/streaming`, `internal/api/sdk.go`. Owned atomic snapshots, SSE/reconnect/poll reconciliation, bootstrap/offline and nonblocking analytics. SDK tests cover cache ownership, concurrent updates/reads, recovery, typed defaults and bounded buffers.
4. Distribution and control: `internal/eventbus`, `internal/repository/operations.go`, `internal/api`. Redis Pub/Sub, transactional outbox, coherent snapshots, authorization, immutable audit, rollback, key rotation, multi-worker scheduled/gradual rollout. Testcontainers tests verify real PostgreSQL/Redis behavior across independent gateways.
5. Runtime and operations: `cmd`, `internal/runtime`, `internal/observability`, Docker/Compose, CI, scripts and docs. Graceful cancellation, bounded workers, metrics/traces, CLI, full demo and measured load scenarios.

Verification uses unit tests, Go vet, golangci-lint, race unit/integration tests, container builds, runnable Compose demo and local/propagation benchmarks. Optional roadmap items are explicitly listed in README rather than represented by TODO implementations. Integration fixtures are disposable Testcontainers services, or explicitly configured dedicated CI databases.
