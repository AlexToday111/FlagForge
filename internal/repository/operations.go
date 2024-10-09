package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	auditdiff "github.com/flagforge/flagforge/internal/audit"
	"github.com/flagforge/flagforge/pkg/types"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

func (r *Repository) Environments(ctx context.Context, project string) ([]types.Environment, error) {
	rows, err := r.Pool.Query(ctx, `SELECT e.id::text,e.project_id::text,e.key,e.name,e.protected,e.version FROM environments e JOIN projects p ON p.id=e.project_id WHERE p.key=$1 ORDER BY e.key`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.Environment{}
	for rows.Next() {
		var e types.Environment
		if err = rows.Scan(&e.ID, &e.ProjectID, &e.Key, &e.Name, &e.Protected, &e.Version); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// UpdateFlag preserves metadata and archives instead of physically deleting history.
func (r *Repository) UpdateFlag(ctx context.Context, p Principal, project, key string, patch map[string]string) (types.FeatureFlag, error) {
	var out types.FeatureFlag
	if p.Role != "Admin" {
		return out, ErrForbidden
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var projectID string
	if err = tx.QueryRow(ctx, `SELECT id::text FROM projects WHERE key=$1 FOR UPDATE`, project).Scan(&projectID); err != nil {
		return out, mapError(err)
	}
	// Acquire environment locks in fixed order before loading or changing any flag.
	rows, err := tx.Query(ctx, `SELECT id::text,key,name,protected,version FROM environments WHERE project_id=$1 ORDER BY id FOR UPDATE`, projectID)
	if err != nil {
		return out, err
	}
	envs := []types.Environment{}
	for rows.Next() {
		e := types.Environment{ProjectID: projectID}
		if err = rows.Scan(&e.ID, &e.Key, &e.Name, &e.Protected, &e.Version); err != nil {
			rows.Close()
			return out, err
		}
		envs = append(envs, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	err = tx.QueryRow(ctx, `SELECT id::text,project_id::text,key,name,description,type,status,created_at,updated_at FROM feature_flags WHERE project_id=$1 AND key=$2 FOR UPDATE`, projectID, key).Scan(&out.ID, &out.ProjectID, &out.Key, &out.Name, &out.Description, &out.Type, &out.Status, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return out, mapError(err)
	}
	old := out
	for k, v := range patch {
		switch k {
		case "name":
			if v == "" {
				return out, ErrInvalid
			}
			out.Name = v
		case "description":
			out.Description = v
		case "status":
			if v != "ACTIVE" && v != "DEPRECATED" && v != "ARCHIVED" {
				return out, ErrInvalid
			}
			out.Status = v
		default:
			return out, ErrInvalid
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE feature_flags SET name=$1,description=$2,status=$3,updated_at=clock_timestamp() WHERE id=$4`, out.Name, out.Description, out.Status, out.ID); err != nil {
		return out, err
	}
	for _, env := range envs {
		s, err := snapshot(ctx, tx, env.ID)
		if err != nil {
			return out, err
		}
		if err := validate(s); err != nil {
			return out, err
		}
		if _, err = change(ctx, tx, p, project, env, key, "FLAG_METADATA_UPDATED", old, out); err != nil {
			return out, err
		}
	}
	if len(envs) == 0 {
		if err = audit(ctx, tx, p, "FLAG_METADATA_UPDATED", project, "", key, old, out); err != nil {
			return out, err
		}
	}
	return out, tx.Commit(ctx)
}

func (r *Repository) RotateSDKKey(ctx context.Context, p Principal, project, envKey string) (string, string, error) {
	if p.Role != "Admin" {
		return "", "", ErrForbidden
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	e, err := environment(ctx, tx, project, envKey, true)
	if err != nil {
		return "", "", err
	}
	key, hash, err := token("sdk_" + e.Key + "_")
	if err != nil {
		return "", "", err
	}
	var id string
	if err = tx.QueryRow(ctx, `INSERT INTO sdk_keys(environment_id,hash) VALUES($1,$2) RETURNING id::text`, e.ID, hash).Scan(&id); err != nil {
		return "", "", err
	}
	if err = audit(ctx, tx, p, "SDK_KEY_CREATED", project, envKey, "", nil, map[string]string{"id": id}); err != nil {
		return "", "", err
	}
	return id, key, tx.Commit(ctx)
}

func (r *Repository) RevokeSDKKey(ctx context.Context, p Principal, project, envKey, id string) error {
	if p.Role != "Admin" {
		return ErrForbidden
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	e, err := environment(ctx, tx, project, envKey, true)
	if err != nil {
		return err
	}
	res, err := tx.Exec(ctx, `UPDATE sdk_keys SET revoked_at=now() WHERE id::text=$1 AND environment_id=$2 AND revoked_at IS NULL`, id, e.ID)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err = audit(ctx, tx, p, "SDK_KEY_REVOKED", project, envKey, "", nil, map[string]string{"id": id}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *Repository) CreateToken(ctx context.Context, p Principal, actor, role string) (string, string, error) {
	if p.Role != "Admin" {
		return "", "", ErrForbidden
	}
	if actor == "" || (role != "Admin" && role != "Developer" && role != "Viewer") {
		return "", "", ErrInvalid
	}
	key, hash, err := token("api_admin_")
	if err != nil {
		return "", "", err
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id string
	if err = tx.QueryRow(ctx, `INSERT INTO api_tokens(hash,actor,role) VALUES($1,$2,$3) RETURNING id::text`, hash, actor, role).Scan(&id); err != nil {
		return "", "", err
	}
	if err = audit(ctx, tx, p, "API_TOKEN_CREATED", "", "", "", nil, map[string]string{"id": id, "actor": actor, "role": role}); err != nil {
		return "", "", err
	}
	return id, key, tx.Commit(ctx)
}

func (r *Repository) RevokeToken(ctx context.Context, p Principal, id string) error {
	if p.Role != "Admin" {
		return ErrForbidden
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	res, err := tx.Exec(ctx, `UPDATE api_tokens SET revoked_at=now() WHERE id::text=$1 AND revoked_at IS NULL`, id)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err = audit(ctx, tx, p, "API_TOKEN_REVOKED", "", "", "", nil, map[string]string{"id": id}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *Repository) History(ctx context.Context, project, env, key string, version int64) (types.FlagConfig, error) {
	var f types.FlagConfig
	var b []byte
	err := r.Pool.QueryRow(ctx, `SELECT h.config FROM config_history h JOIN environments e ON e.id=h.environment_id JOIN projects p ON p.id=e.project_id WHERE p.key=$1 AND e.key=$2 AND h.flag_key=$3 AND h.version=$4`, project, env, key, version).Scan(&b)
	if err != nil {
		return f, mapError(err)
	}
	return f, json.Unmarshal(b, &f)
}

type ScheduledChange struct {
	ID              string           `json:"id"`
	Flag            string           `json:"flag"`
	Config          types.FlagConfig `json:"config"`
	ExpectedVersion int64            `json:"expected_version"`
	RunAt           time.Time        `json:"run_at"`
	Status          string           `json:"status"`
	Error           string           `json:"error"`
}

func (r *Repository) Schedule(ctx context.Context, p Principal, project, envKey, key string, expected int64, at time.Time, f types.FlagConfig) (ScheduledChange, error) {
	changes, err := r.ScheduleStages(ctx, p, project, envKey, key, expected, []ScheduledChange{{RunAt: at, Config: f}})
	if err != nil {
		return ScheduledChange{}, err
	}
	return changes[0], nil
}

// ScheduleStages records a rollout progression atomically. Each stage expects the
// version written by its predecessor; intervening manual edits stop the progression.
func (r *Repository) ScheduleStages(ctx context.Context, p Principal, project, envKey, key string, expected int64, stages []ScheduledChange) ([]ScheduledChange, error) {
	if len(stages) == 0 || len(stages) > 100 {
		return nil, ErrInvalid
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	e, err := environment(ctx, tx, project, envKey, true)
	if err != nil {
		return nil, err
	}
	if err = authorize(p, e); err != nil {
		return nil, err
	}
	s, err := snapshot(ctx, tx, e.ID)
	if err != nil {
		return nil, err
	}
	if s.Flags[key].Version != expected {
		return nil, ErrConflict
	}
	var kind types.FlagType
	if err = tx.QueryRow(ctx, `SELECT type FROM feature_flags WHERE project_id=$1 AND key=$2 AND status!='ARCHIVED'`, e.ProjectID, key).Scan(&kind); err != nil {
		return nil, mapError(err)
	}
	previous := time.Now().UTC()
	for i := range stages {
		st := &stages[i]
		if !st.RunAt.After(previous) {
			return nil, fmt.Errorf("%w: stages must be future and strictly ordered", ErrInvalid)
		}
		previous = st.RunAt
		st.Config.Key = key
		if st.Config.Type != kind {
			return nil, ErrInvalid
		}
		s.Flags[key] = st.Config
		if err = validate(s); err != nil {
			return nil, err
		}
		st.Flag = key
		st.ExpectedVersion = expected
		st.Status = "PENDING"
		// Subsequent versions are resolved when a preceding stage commits, not
		// guessed from the environment counter (other flags can change meanwhile).
		if i > 0 {
			st.ExpectedVersion = -1
		}
		if err = tx.QueryRow(ctx, `INSERT INTO scheduled_changes(environment_id,flag_key,config,actor,expected_version,run_at) VALUES($1,$2,$3,$4,$5,$6) RETURNING id::text`, e.ID, key, encode(st.Config), p.Actor, st.ExpectedVersion, st.RunAt).Scan(&st.ID); err != nil {
			return nil, err
		}
	}
	// Link dependent stages explicitly so a failed predecessor never activates them.
	for i := 1; i < len(stages); i++ {
		if _, err = tx.Exec(ctx, `UPDATE scheduled_changes SET predecessor_id=$1 WHERE id=$2`, stages[i-1].ID, stages[i].ID); err != nil {
			return nil, err
		}
	}
	if err = audit(ctx, tx, p, "SCHEDULE_CREATED", project, envKey, key, nil, stages); err != nil {
		return nil, err
	}
	return stages, tx.Commit(ctx)
}

func (r *Repository) Schedules(ctx context.Context, project, envKey string) ([]ScheduledChange, error) {
	e, err := environment(ctx, r.Pool, project, envKey, false)
	if err != nil {
		return nil, err
	}
	rows, err := r.Pool.Query(ctx, `SELECT id::text,flag_key,config,expected_version,run_at,status,error FROM scheduled_changes WHERE environment_id=$1 ORDER BY run_at LIMIT 1000`, e.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ScheduledChange{}
	for rows.Next() {
		var s ScheduledChange
		var b []byte
		if err = rows.Scan(&s.ID, &s.Flag, &b, &s.ExpectedVersion, &s.RunAt, &s.Status, &s.Error); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &s.Config); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *Repository) CancelSchedule(ctx context.Context, p Principal, project, envKey, id string) error {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	e, err := environment(ctx, tx, project, envKey, true)
	if err != nil {
		return err
	}
	if err = authorize(p, e); err != nil {
		return err
	}
	res, err := tx.Exec(ctx, `UPDATE scheduled_changes SET status='CANCELLED' WHERE id::text=$1 AND environment_id=$2 AND status='PENDING'`, id, e.ID)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err = audit(ctx, tx, p, "SCHEDULE_CANCELLED", project, envKey, "", nil, map[string]string{"id": id}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RunSchedule holds a row lock until the configuration and status have committed.
// Multiple workers therefore cannot execute the same schedule twice.
func (r *Repository) RunSchedule(ctx context.Context) (bool, error) {
	ctx, span := otel.Tracer("flagforge/repository").Start(ctx, "schedule.claim_apply")
	defer span.End()
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id, key, actor, project string
	var expected int64
	var b []byte
	// Acquire the environment before schedules, matching management lock order.
	var e types.Environment
	err = tx.QueryRow(ctx, `SELECT e.id::text,e.project_id::text,e.key,e.name,e.protected,e.version,p.key
 FROM environments e JOIN projects p ON p.id=e.project_id WHERE EXISTS(
 SELECT 1 FROM scheduled_changes s WHERE s.environment_id=e.id AND s.status='PENDING' AND s.run_at<=now()
 AND (s.predecessor_id IS NULL OR EXISTS(SELECT 1 FROM scheduled_changes prev WHERE prev.id=s.predecessor_id AND prev.status!='PENDING')))
 ORDER BY e.id FOR UPDATE OF e SKIP LOCKED LIMIT 1`).Scan(&e.ID, &e.ProjectID, &e.Key, &e.Name, &e.Protected, &e.Version, &project)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	err = tx.QueryRow(ctx, `SELECT s.id::text,s.flag_key,s.actor,s.expected_version,s.config FROM scheduled_changes s
 WHERE s.environment_id=$1 AND s.status='PENDING' AND s.run_at<=now()
 AND (s.predecessor_id IS NULL OR EXISTS(SELECT 1 FROM scheduled_changes prev WHERE prev.id=s.predecessor_id AND prev.status!='PENDING'))
 ORDER BY s.run_at FOR UPDATE OF s SKIP LOCKED LIMIT 1`, e.ID).Scan(&id, &key, &actor, &expected, &b)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var f types.FlagConfig
	if err = json.Unmarshal(b, &f); err != nil {
		return false, err
	}
	status, message := "APPLIED", ""
	if expected < 0 {
		status, message = "CONFLICT", "previous stage did not apply"
	} else {
		// Savepoint allows recording terminal validation/conflict errors without
		// retaining any partial writes from a failed mutation.
		if _, err = tx.Exec(ctx, `SAVEPOINT schedule_apply`); err != nil {
			return false, err
		}
		f, err = updateConfig(ctx, tx, Principal{Actor: actor, Role: "Admin"}, project, e, key, expected, f)
		if err != nil {
			if err != ErrConflict && !isInvalid(err) && err != ErrNotFound {
				return false, err
			}
			if _, rollbackErr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT schedule_apply`); rollbackErr != nil {
				return false, rollbackErr
			}
			status, message = "CONFLICT", err.Error()
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE scheduled_changes SET status=$1,error=$2,applied_at=now() WHERE id=$3`, status, message, id); err != nil {
		return false, err
	}
	span.SetAttributes(attribute.String("schedule.status", status), attribute.String("environment", e.Key))
	if status == "APPLIED" {
		if _, err = tx.Exec(ctx, `UPDATE scheduled_changes SET expected_version=$1 WHERE predecessor_id=$2 AND status='PENDING'`, f.Version, id); err != nil {
			return false, err
		}
	}
	if err = audit(ctx, tx, Principal{Actor: actor}, "SCHEDULE_"+status, project, e.Key, key, nil, map[string]string{"id": id, "error": message}); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

type Publisher interface {
	Publish(context.Context, types.Event) error
}

// PublishOutbox is at-least-once: a crash after Redis accepts but before commit
// may duplicate a notification. Snapshot versions make consumers idempotent.
func (r *Repository) PublishOutbox(ctx context.Context, p Publisher, limit int) (int, error) {
	ctx, span := otel.Tracer("flagforge/repository").Start(ctx, "outbox.publish")
	defer span.End()
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT id,payload FROM outbox_events WHERE published_at IS NULL ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	type pending struct {
		id int64
		e  types.Event
	}
	batch := []pending{}
	for rows.Next() {
		var item pending
		var b []byte
		if err = rows.Scan(&item.id, &b); err != nil {
			rows.Close()
			return 0, err
		}
		if err = json.Unmarshal(b, &item.e); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, item := range batch {
		if err = p.Publish(ctx, item.e); err != nil {
			span.RecordError(err)
			return 0, fmt.Errorf("publish outbox %d: %w", item.id, err)
		}
		if _, err = tx.Exec(ctx, `UPDATE outbox_events SET published_at=now() WHERE id=$1`, item.id); err != nil {
			return 0, err
		}
	}
	span.SetAttributes(attribute.Int("outbox.batch_size", len(batch)))
	return len(batch), tx.Commit(ctx)
}

func (r *Repository) Audit(ctx context.Context, filters map[string]string, after int64) ([]types.Audit, error) {
	var from, to any
	if filters["from"] != "" {
		t, err := time.Parse(time.RFC3339, filters["from"])
		if err != nil {
			return nil, ErrInvalid
		}
		from = t
	}
	if filters["to"] != "" {
		t, err := time.Parse(time.RFC3339, filters["to"])
		if err != nil {
			return nil, ErrInvalid
		}
		to = t
	}
	rows, err := r.Pool.Query(ctx, `SELECT id,actor,action,project,environment,flag,before_state,after_state,timestamp FROM audit_logs
 WHERE id>$1 AND ($2='' OR project=$2) AND ($3='' OR environment=$3) AND ($4='' OR flag=$4) AND ($5='' OR actor=$5) AND ($6='' OR action=$6)
 AND ($7::timestamptz IS NULL OR timestamp>=$7) AND ($8::timestamptz IS NULL OR timestamp<=$8) ORDER BY id LIMIT 100`, after, filters["project"], filters["environment"], filters["flag"], filters["actor"], filters["action"], from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.Audit{}
	for rows.Next() {
		var a types.Audit
		var before, after []byte
		if err = rows.Scan(&a.ID, &a.Actor, &a.Action, &a.Project, &a.Environment, &a.Flag, &before, &after, &a.Timestamp); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(before, &a.Before); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(after, &a.After); err != nil {
			return nil, err
		}
		a.Diff = auditdiff.Diff(a.Before, a.After)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *Repository) RecordEvents(ctx context.Context, envID string, events []types.EvaluationEvent) error {
	if len(events) > 1000 {
		return ErrInvalid
	}
	s, err := r.Snapshot(ctx, envID)
	if err != nil {
		return err
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, e := range events {
		flag, ok := s.Flags[e.Flag]
		if !ok {
			continue
		}
		if _, ok = flag.Variations[e.Variation]; !ok && e.Reason != "ERROR" {
			continue
		}
		switch e.Reason {
		case "DEFAULT", "ERROR", "FLAG_DISABLED", "TARGET_MATCH", "RULE_MATCH", "ROLLOUT", "PREREQUISITE_FAILED":
		default:
			return ErrInvalid
		}
		// Use receipt time: SDK clocks and timestamps are untrusted.
		if _, err = tx.Exec(ctx, `INSERT INTO evaluation_usage VALUES($1,$2,$3,$4,1,now()) ON CONFLICT(environment_id,flag_key,variation,reason) DO UPDATE SET count=evaluation_usage.count+1,last_evaluated=now()`, envID, e.Flag, e.Variation, e.Reason); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (r *Repository) Usage(ctx context.Context, project string, stale bool) ([]map[string]any, error) {
	sql := `SELECT f.key,e.key,f.updated_at,max(u.last_evaluated),COALESCE(sum(u.count),0)::bigint,
 COALESCE(jsonb_agg(jsonb_build_object('variation',u.variation,'reason',u.reason,'count',u.count) ORDER BY u.variation,u.reason) FILTER(WHERE u.flag_key IS NOT NULL),'[]')
 FROM feature_flags f JOIN projects p ON p.id=f.project_id JOIN environments e ON e.project_id=p.id
 LEFT JOIN evaluation_usage u ON u.environment_id=e.id AND u.flag_key=f.key WHERE p.key=$1 GROUP BY f.id,e.id`
	if stale {
		sql += ` HAVING GREATEST(f.updated_at,COALESCE(max(u.last_evaluated),f.created_at))<now()-interval '90 days'`
	}
	rows, err := r.Pool.Query(ctx, sql, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var flag, env string
		var updated time.Time
		var last *time.Time
		var count int64
		var encoded []byte
		if err = rows.Scan(&flag, &env, &updated, &last, &count, &encoded); err != nil {
			return nil, err
		}
		var breakdown []map[string]any
		if err = json.Unmarshal(encoded, &breakdown); err != nil {
			return nil, err
		}
		var ruleMatches float64
		for _, entry := range breakdown {
			if count > 0 {
				entry["percentage"] = entry["count"].(float64) / float64(count) * 100
			}
			if entry["reason"] == "RULE_MATCH" {
				ruleMatches += entry["count"].(float64)
			}
		}
		out = append(out, map[string]any{"flag": flag, "environment": env, "updated_at": updated, "last_evaluated": last, "evaluations": count, "distribution": breakdown, "rule_matches": int64(ruleMatches)})
	}
	return out, rows.Err()
}
