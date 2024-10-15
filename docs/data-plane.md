# Data plane

`GET /sdk/v1/config` returns only the environment's evaluation configuration, segments, revision and commit timestamp. It omits raw credentials, administrative actors and audit metadata. The endpoint authenticates the key on every request and emits a private ETag of the snapshot revision. A matching `If-None-Match` returns 304 after checking PostgreSQL's current revision.

`GET /sdk/v1/stream` supplies small revision notifications. SDKs resynchronize the complete environment, rather than applying fragile partial patches. `POST /sdk/v1/events/batch` accepts at most 1000 usage events; no subject IDs or attributes are part of this payload.

Redis cache access is capped by a short context; misses and failures load PostgreSQL. Revision-qualified keys avoid a stale cache fill overwriting a newer revision. Full snapshot materialization currently issues relational queries and JSON aggregations: this favors correctness and inspectability. A future gateway deployment can precompute immutable snapshot artifacts per committed environment version.

Evaluation continues independently of server readiness, PostgreSQL, Redis and streaming availability. There is no per-evaluation HTTP request or shared server-side evaluator dependency.
