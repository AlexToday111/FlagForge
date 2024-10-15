# Measured benchmarks

Measured on 2026-10-05 in Docker Desktop (Linux/amd64), Go 1.25.14, AMD Ryzen 7 8745HS, GOMAXPROCS=16. These observations describe this machine and small fixture snapshots, not service-level guarantees. Other local projects were running. Benchmarks include no synthetic pass/fail latency threshold.

## Go microbenchmarks

`go test -run '^$' -bench . -benchmem ./pkg/...`:

| Scenario | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| Evaluator, 20% deterministic rollout | 143.3 | 0 | 0 |
| SDK local boolean, default variation | 90.46 | 0 | 0 |

These tests use one prebuilt immutable snapshot and one repeated subject. They do not include JSON result copying, synchronization, analytics or many-rule/dependency graphs. Run the tests again on representative configurations before estimating capacity.

## Local workload

`go run ./cmd/loadtest -n <count>` evaluates an offline SDK with a 20% rollout. It samples up to 100,000 operations, reports monotonic clock percentiles and process allocation deltas. Total ns/op includes loop/sample overhead; latency samples include timer overhead and should not be compared directly with the Go harness.

| Evaluations | Elapsed | Operations/sec | ns/op | allocs/op | Sample p50 | p95 | p99 |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1,000,000 | 237 ms | 4,218,176 | 237.1 | 0 | 241 ns | 261 ns | 350 ns |
| 10,000,000 | 2.366 s | 4,227,273 | 236.6 | 0 | 241 ns | 331 ns | 490 ns |
| 100,000,000 | 22.930 s | 4,361,096 | 229.3 | 0 | 241 ns | 400 ns | 711 ns |

## Cross-instance propagation

Docker Compose ran API instance A, gateway instance B, PostgreSQL 16 and Redis 7. Admin updates went to A; all benchmark SDKs connected to B. Each run made 10 sequential toggle updates, waiting for every client before the next update. Outbox polling was 500 ms. Polling fallback was 60 seconds, longer than each individual update measurement. Healthy runs therefore exercised distribution via Redis/SSE. A fresh flag revision is fetched as a complete snapshot per SDK.

Timing uses a single load-runner monotonic clock: request start → observation of target SDK version. It includes API commit time, outbox wait, Redis/SSE, snapshot requests and observer sampling. It is broader than pure commit-to-install latency. SDK startup is excluded from updates/sec. The observer polls in a 1 ms loop; p99 is limited by sample count and this local setup.

| SDKs | Observations | Updates/sec | API p50 | API p95 | Propagation p50 | p95 | p99 |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 10 | 100 | 4.08 | 9.54 ms | 14.79 ms | 41.01 ms | 460.16 ms | 460.16 ms |
| 100 | 1,000 | 3.77 | 8.81 ms | 10.55 ms | 179.16 ms | 467.29 ms | 472.96 ms |
| 1,000 | 10,000 | 2.08 | 8.73 ms | 9.52 ms | 452.65 ms | 519.83 ms | 532.33 ms |

The broad p50 spread at low client count reflects the phase of sequential updates relative to the outbox timer. The 1000-client snapshot fan-out adds database/cache/network work. These runs do not establish maximum management throughput; updates/sec includes waiting for the slowest SDK. Increase independent writers and include realistic environment sizes for control-plane saturation measurements.

Reproduce using a disposable project/environment/flag and its SDK key:

```bash
export FLAGFORGE_ADMIN_TOKEN=...
export FLAGFORGE_SDK_KEY=...
go run ./cmd/loadtest -mode propagation -clients 1000 -updates 10 \
  -endpoint http://localhost:28080 -sdk-endpoint http://localhost:28082 \
  -project YOUR_PROJECT -env production -flag new-checkout
```

This runner toggles the selected flag and attempts to restore its original configuration as a new revision afterward. Use an isolated load-test flag without simultaneous operator edits. Profiling, multiple subjects, large segments/prerequisite graphs and multi-region delays are additional useful workloads. Configuration updates intentionally allocate/copy snapshots; evaluation hot paths remain independent of that network workload.
