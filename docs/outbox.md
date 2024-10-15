# Transactional outbox

`repository.change` increments the environment revision and inserts audit/outbox rows in the same transaction that persists config. Nothing is published before commit. This removes the lost-update window between successful database mutation and failed event publication.

The publisher claims up to 100 pending rows using `FOR UPDATE SKIP LOCKED`, sends each to Redis and marks publication inside a transaction. A failed batch rolls back marks; durable rows retry on later ticks or another instance. A crash after publication but before commit can duplicate notifications. Consumers use monotonic revisions and complete snapshots, making duplicates safe.

Workers hold row locks while publishing, with a bounded 5-second batch context. This compact design avoids leases and supports competing instances, at the cost of keeping a transaction open across network publication. High-throughput production could move to claim leases and per-row retry timestamps, with explicit duplicate handling. Current rows have no cleanup worker; retain/partition published events and config history according to an operational retention policy. Audit must preserve required history.

Redis Pub/Sub acknowledges acceptance, not durable replay or SDK receipt. Recovery does not rely on outbox replay for disconnected clients: reconnect/full snapshot, gateway database heartbeat and SDK polling cover missing messages. Redis Streams would add durable consumer checkpoints and backlog management but still require full snapshot bootstrap and revision idempotence.
