# Observability

Prometheus exposes HTTP counters/durations, committed API flag updates, current SSE connections, written stream notifications/failures, pending outbox count, failed publication batches, reported evaluations and commit-to-gateway propagation histogram. Route labels aggregate mounted API families; no user IDs, flag keys, token values or raw URLs are metric labels. Evaluation reasons are validated/bounded.

`flagforge_propagation_latency_seconds` measures commit timestamp to gateway write for Redis notifications. SDK `Status.PropagationLatency` estimates commit to local snapshot install; it depends on synchronized clocks. `cmd/loadtest -mode propagation` measures update-request start to observed SDK revision using one monotonic clock, including management latency. These are distinct metrics.

OTel traces cover HTTP request handling, flag config mutations, scheduler claims and outbox publication when `OTEL_EXPORTER_OTLP_ENDPOINT` is set. The bundled collector accepts OTLP HTTP on 4318 and exports basic debug trace logs. External exporters can replace it. A long-lived SSE request creates a long-lived span; there is no per-evaluation span, keeping the hot path cheap and private. Distributed request context propagation is a future extension.

slog uses JSON and logs operational errors without request headers or bearer keys. pprof is optional and Admin-authenticated. For profiling, download `/debug/pprof/profile?seconds=10` using a Bearer header and open it with `go tool pprof`. Heap/goroutine/mutex endpoints are under `/debug/pprof/`; use `go tool pprof -http=localhost:8085 profile.pb.gz` to inspect saved CPU profiles. Enable runtime mutex profiling separately if collecting mutex contention samples.

Compose's observability profile provisions Prometheus, a Grafana datasource and a basic dashboard. Alert on pending outbox growth, publication failures, elevated propagation, API errors and database readiness. SDK usage telemetry is optional, lossy, and reflects reporting-enabled clients only; lack of analytics does not prove a flag is unused.
