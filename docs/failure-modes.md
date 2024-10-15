# Failure modes

| Failure | Impact and consistency | Recovery / application behavior |
|---|---|---|
| PostgreSQL down | Management and fresh snapshots fail; readiness 503; gateway heartbeat may close streams | DB reconnect via pgx pool; outbox/schedule transactions retry; SDK retains previous snapshot |
| Redis down | Publication remains pending and cache misses fall back to DB | Redis subscription reconnects; DB heartbeats and SDK polling reconcile revisions; evaluation unaffected |
| Server process crash | Open transactions roll back; clients lose streams | Replica/next startup publishes pending outbox and claims due schedules; SDK reconnects with jitter |
| Gateway crash | Its SSE connections close; other gateways continue | SDK fetches a complete snapshot after reconnect and keeps local values meanwhile |
| Network partition | SDK state may be stale with no freshness bound | Prefer availability; caller defaults before initialization, last-known config thereafter |
| Publisher failure | Committed config persists without prompt notification | Durable pending rows retry; transient Redis messages need not be replayed because snapshots reconcile |
| Scheduler failure | Due changes remain PENDING or transaction rolls back | Another worker claims due work; APPLIED changes do not repeat |
| Manual edit during schedule | Expected flag revision mismatch | Schedule becomes CONFLICT; dependent progression stops, preserving manual change |
| Invalid configuration | Management write rejected and transaction rolled back | Fix validation error; SDK also rejects invalid remote snapshots and retains prior version |
| Out-of-order snapshot | SDK receives an older version | Atomic CAS rejects it; one evaluation never mixes snapshots |
| Slow streaming client | Intermediate hints coalesce; socket writes time out | Reconnect and full resync; bounded channels avoid blocking other clients |
| Analytics outage/full buffer | Telemetry drops; usage may undercount | Dropped status counter increases; evaluation is still available |
| SDK key revoked | Fresh requests fail and stream closes at heartbeat | Deploy a replacement key; local configuration remains available |

Graceful SIGTERM/SIGINT cancels scheduler/subscriber/publisher and streams, drains HTTP with a deadline, joins workers, attempts one bounded final outbox batch, flushes traces and closes Redis/PostgreSQL. Pending durable rows survive shutdown. Snapshot history and audit survive flag archive. Current SDK cache is memory-only; application-owned bootstrap files are needed to preserve state across application restarts during a control-plane outage.
