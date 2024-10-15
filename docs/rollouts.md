# Rollouts and schedules

SHA-256 receives each component with an 8-byte big-endian length prefix. Inputs are flag key, `project/environment`, and subject key. First 64 digest bits modulo 100000 produce the bucket. This format is part of the SDK compatibility contract; changing it reassigns users. Version is excluded so unrelated edits do not rebucket the population.

Weights are ordered and sum to 100000. For `on:10000, off:90000`, users in buckets 0–9999 get on. Increasing on to 50000 preserves that initial population. For a multivariation experiment, control:50000/a:25000/b:25000 is supported. Reordering variations or changing multiple boundaries can move subjects. Persistent assignment requires storage, identity retention, migration policies and privacy decisions; stateless hashing keeps local evaluation available without them.

Schedules store complete future configurations and an expected current flag revision. Workers claim a due environment with `FOR UPDATE SKIP LOCKED`, then its schedule; management and scheduler both lock environment before schedule. Config application, audit/outbox and schedule status commit together. A crash before commit rolls back everything; after commit, APPLIED rows are ineligible. Multiple workers cannot apply one schedule twice.

Gradual rollout is a chain of schedules. A successful predecessor updates its successor's expected revision to the actual committed revision (other flags may have advanced the environment counter). Conflict or cancellation leaves the dependent stage without a valid predecessor revision, so it becomes CONFLICT when due. Manual edits to the same flag deliberately stop progression rather than being overwritten.

PostgreSQL clock decides due time. A worker ticks once per second; stages may apply late during outages. Scheduled mutations retain the actor that authorized creation and continue even if that actor's token is revoked; cancel the schedule explicitly to withdraw a previously authorized change. Rollback validates historical config against current graph/type and writes a new version; it never rewinds the version counter or deletes history.
