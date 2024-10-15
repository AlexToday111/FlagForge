# Streaming and recovery

Every server owns an independent notification hub and Redis subscription. Updates from any server can therefore reach SDKs connected to another. The hub has a one-slot channel per connection; slow clients coalesce notifications and fetch the latest complete snapshot. Writers never block the Redis subscriber on client I/O. Unsubscribe removes the channel under the same mutex; no closed-channel send race is possible.

SSE events contain environment ID, environment revision and commit timestamp. The handler subscribes before flushing `: connected`; SDK then fetches the latest full configuration. An update between the original snapshot and stream establishment is therefore recovered. The SDK checks revision before fetching; CAS ignores out-of-order responses.

Reconnect uses exponential backoff with jitter from approximately 0.5–1 seconds to 15–30 seconds. Long-lived successful connections reset backoff. Polling runs independently even in streaming mode, so failed streams cannot leave configuration stale indefinitely once snapshot access recovers. Server heartbeats (15 seconds) revalidate key revocation and check PostgreSQL revision, covering lost Pub/Sub messages and subscriber reconnect gaps.

Each SSE write/flush has a 5-second deadline. The SDK idle timer defaults to 45 seconds; configure it above gateway heartbeat/proxy idle intervals. Cancellation closes HTTP response bodies and joins workers. Server shutdown cancels its stream context before draining HTTP. An invalid or revoked key ends the stream at the next heartbeat; subsequent snapshot requests fail immediately.

Redis Pub/Sub notifications are hints, not a replay log. `Last-Event-ID` is not used for replay; full snapshot resync is simpler and authoritative. A production proxy must preserve event streams, disable buffering, and support sufficiently long response durations.
