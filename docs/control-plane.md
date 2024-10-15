# Control plane API

All `/api/` requests require a database-backed API token. Viewer reads; Developer modifies existing configuration only in unprotected development/staging; Admin owns project/environment/definition/key/token management and protected environments. A server-wide token bucket limits management requests to 100/sec with burst 200. It bounds load without an unbounded per-token map; distributed/global quotas are a future extension.

| Endpoint | Behavior |
|---|---|
| `POST/GET /api/v1/projects` | Create/list projects |
| `POST/GET /api/v1/projects/{project}/environments` | Create/list environments; creation returns one SDK key |
| `POST/GET /api/v1/projects/{project}/flags` | Create/list typed definitions |
| `GET/PATCH/DELETE /api/v1/projects/{project}/flags/{flag}` | Read/edit metadata/archive |
| `GET/PUT .../environments/{env}/flags/{flag}` | Get/update environment config |
| `POST .../flags/{flag}/enable` or `/disable` | Toggle while retaining rules and rollout |
| `POST .../flags/{flag}/rollback/{version}` | Restore history as a new revision |
| `GET .../environments/{env}/snapshot` | Inspect environment version, flags and segments |
| `PUT .../environments/{env}/segments/{segment}` | Upsert validated reusable segment |
| `POST/GET .../environments/{env}/sdk-keys` | Rotate key/list IDs without secrets |
| `DELETE .../environments/{env}/sdk-keys/{id}` | Revoke a key |
| `POST .../flags/{flag}/schedules` | Create scheduled complete config |
| `POST .../flags/{flag}/gradual-rollout` | Create linked percentage stages |
| `GET .../environments/{env}/schedules` | List schedule states |
| `DELETE .../environments/{env}/schedules/{id}` | Cancel pending schedule |
| `POST /api/v1/tokens` | Issue token with actor and role |
| `DELETE /api/v1/tokens/{id}` | Revoke token |
| `GET /api/v1/audit` | Filter immutable history and derived field diffs |
| `GET /api/v1/usage?project=...` | Aggregated evaluation use |
| `GET /api/v1/stale-flags?project=...` | No observed usage/update in 90 days |

Configuration updates and schedule creation require `If-Match` with the current flag config version; first PUT uses 0. Segment writes use the environment version. A mutation increases the environment revision, and changed flag configs receive that revision. Thus flag revisions increase monotonically but can have gaps. Metadata/segment changes increase snapshot revision without changing unrelated config revisions. Metadata writes are serialized; they currently do not expose an optimistic precondition.

Example config:

```json
{"type":"BOOLEAN","enabled":true,"variations":{"on":true,"off":false},"default_variation":"off","off_variation":"off","targets":{"user-1":"on"},"exclude":["user-999"],"rules":[{"id":"eu","priority":1,"conditions":[{"attribute":"country","operator":"in","values":["DE","FR"]}],"variation":"on"}],"rollout":[{"variation_key":"on","weight":10000},{"variation_key":"off","weight":90000}]}
```

Schedule body is `{"run_at":"2026-12-01T12:00:00Z","config":{...complete config...}}`. Gradual body is `{"variation":"on","stages":[{"percentage":10,"start_at":"2026-12-01T12:00:00Z"},{"percentage":50,"start_at":"2026-12-02T12:00:00Z"}]}`. Dates must be future, ordered RFC3339 times, and percentages must be nondecreasing. Gradual rollout keeps existing explicit targets/rules; these retain their precedence.

Audit filters: `project`, `environment`, `flag`, `actor`, `action`, `from`, `to` (RFC3339), and `after` (exclusive audit ID). Pages contain at most 100 rows. Before/after states support readable top-level diffs; arrays such as rollout show complete old/new values. Flags, schedules and keys use project/environment paths; permissions are role-based across this deployment, not tenant memberships.

JSON bodies are bounded to 2 MiB, reject unknown fields and trailing values. Errors use `{ "error": {"code":"...","message":"..."} }` for domain failures; authentication/rate-limit responses are simple error bodies. Key collisions/stale writes return 409, malformed config 400, missing resource 404, unauthorized 401, forbidden 403 and missing precondition 428.
