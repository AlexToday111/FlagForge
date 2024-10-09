package repository

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/flagforge/flagforge/pkg/evaluation"
	"github.com/flagforge/flagforge/pkg/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

var (
	ErrNotFound  = errors.New("resource not found")
	ErrConflict  = errors.New("version conflict")
	ErrInvalid   = errors.New("invalid configuration")
	ErrForbidden = errors.New("permission denied")
)

type Principal struct{ Actor, Role, EnvironmentID string }
type Repository struct{ Pool *pgxpool.Pool }
type queryer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func New(ctx context.Context, dsn string) (*Repository, error) {
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return &Repository{Pool: p}, nil
}

func encode(v any) []byte { b, _ := json.Marshal(v); return b }
func token(prefix string) (string, []byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	key := prefix + hex.EncodeToString(b)
	hash := sha256.Sum256([]byte(key))
	return key, hash[:], nil
}
func mapError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return fmt.Errorf("%w: duplicate key", ErrConflict)
	}
	return err
}

func (r *Repository) BootstrapAdmin(ctx context.Context, key string) error {
	if len(key) < 24 {
		return fmt.Errorf("admin token must have at least 24 characters")
	}
	h := sha256.Sum256([]byte(key))
	_, err := r.Pool.Exec(ctx, `INSERT INTO api_tokens(hash,actor,role) VALUES($1,'bootstrap','Admin') ON CONFLICT(hash) DO NOTHING`, h[:])
	return err
}

func (r *Repository) Authenticate(ctx context.Context, key string, sdk bool) (Principal, error) {
	h := sha256.Sum256([]byte(key))
	var p Principal
	var err error
	if sdk {
		err = r.Pool.QueryRow(ctx, `SELECT environment_id::text FROM sdk_keys WHERE hash=$1 AND revoked_at IS NULL`, h[:]).Scan(&p.EnvironmentID)
	} else {
		err = r.Pool.QueryRow(ctx, `SELECT actor,role FROM api_tokens WHERE hash=$1 AND revoked_at IS NULL`, h[:]).Scan(&p.Actor, &p.Role)
	}
	return p, mapError(err)
}

func audit(ctx context.Context, q queryer, p Principal, action, project, env, flag string, before, after any) error {
	_, err := q.Exec(ctx, `INSERT INTO audit_logs(actor,action,project,environment,flag,before_state,after_state) VALUES($1,$2,$3,$4,$5,$6,$7)`, p.Actor, action, project, env, flag, encode(before), encode(after))
	return err
}

func (r *Repository) CreateProject(ctx context.Context, p Principal, key, name string) (types.Project, error) {
	var out types.Project
	if p.Role != "Admin" {
		return out, ErrForbidden
	}
	if !evaluation.ValidKey(key) || name == "" {
		return out, ErrInvalid
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	err = tx.QueryRow(ctx, `INSERT INTO projects(key,name) VALUES($1,$2) RETURNING id::text,key,name,created_at,updated_at`, key, name).Scan(&out.ID, &out.Key, &out.Name, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return out, mapError(err)
	}
	if err = audit(ctx, tx, p, "PROJECT_CREATED", key, "", "", nil, out); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

func (r *Repository) Projects(ctx context.Context) ([]types.Project, error) {
	rows, err := r.Pool.Query(ctx, `SELECT id::text,key,name,created_at,updated_at FROM projects ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.Project{}
	for rows.Next() {
		var p types.Project
		if err := rows.Scan(&p.ID, &p.Key, &p.Name, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Repository) CreateEnvironment(ctx context.Context, p Principal, project string, env types.Environment) (types.Environment, string, error) {
	if p.Role != "Admin" {
		return env, "", ErrForbidden
	}
	if !evaluation.ValidKey(env.Key) || env.Name == "" {
		return env, "", ErrInvalid
	}
	if env.Key == "production" {
		env.Protected = true
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return env, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var projectID string
	if err = tx.QueryRow(ctx, `SELECT id::text FROM projects WHERE key=$1`, project).Scan(&projectID); err != nil {
		return env, "", mapError(err)
	}
	err = tx.QueryRow(ctx, `INSERT INTO environments(project_id,key,name,protected) VALUES($1,$2,$3,$4) RETURNING id::text,project_id::text,version`, projectID, env.Key, env.Name, env.Protected).Scan(&env.ID, &env.ProjectID, &env.Version)
	if err != nil {
		return env, "", mapError(err)
	}
	key, hash, err := token("sdk_" + env.Key + "_")
	if err != nil {
		return env, "", err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO sdk_keys(environment_id,hash) VALUES($1,$2)`, env.ID, hash); err != nil {
		return env, "", err
	}
	if err = audit(ctx, tx, p, "ENVIRONMENT_CREATED", project, env.Key, "", nil, env); err != nil {
		return env, "", err
	}
	return env, key, tx.Commit(ctx)
}

func environment(ctx context.Context, q queryer, project, key string, lock bool) (types.Environment, error) {
	var env types.Environment
	sql := `SELECT e.id::text,e.project_id::text,e.key,e.name,e.protected,e.version FROM environments e JOIN projects p ON p.id=e.project_id WHERE p.key=$1 AND e.key=$2`
	if lock {
		sql += ` FOR UPDATE OF e`
	}
	err := q.QueryRow(ctx, sql, project, key).Scan(&env.ID, &env.ProjectID, &env.Key, &env.Name, &env.Protected, &env.Version)
	return env, mapError(err)
}

func authorize(p Principal, env types.Environment) error {
	if p.Role == "Admin" || p.Role == "Developer" && !env.Protected && (env.Key == "development" || env.Key == "staging") {
		return nil
	}
	return ErrForbidden
}

func (r *Repository) Flags(ctx context.Context, project string) ([]types.FeatureFlag, error) {
	rows, err := r.Pool.Query(ctx, `SELECT f.id::text,f.project_id::text,f.key,f.name,f.description,f.type,f.status,f.created_at,f.updated_at FROM feature_flags f JOIN projects p ON p.id=f.project_id WHERE p.key=$1 ORDER BY f.key`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.FeatureFlag{}
	for rows.Next() {
		var f types.FeatureFlag
		if err := rows.Scan(&f.ID, &f.ProjectID, &f.Key, &f.Name, &f.Description, &f.Type, &f.Status, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (r *Repository) CreateFlag(ctx context.Context, p Principal, project string, f types.FeatureFlag) (types.FeatureFlag, error) {
	if p.Role != "Admin" {
		return f, ErrForbidden
	}
	if !evaluation.ValidKey(f.Key) || f.Name == "" || (f.Type != types.Boolean && f.Type != types.String && f.Type != types.Integer && f.Type != types.Float && f.Type != types.JSON) {
		return f, ErrInvalid
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return f, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = tx.QueryRow(ctx, `SELECT id::text FROM projects WHERE key=$1`, project).Scan(&f.ProjectID); err != nil {
		return f, mapError(err)
	}
	f.Status = "ACTIVE"
	err = tx.QueryRow(ctx, `INSERT INTO feature_flags(project_id,key,name,description,type) VALUES($1,$2,$3,$4,$5) RETURNING id::text,created_at,updated_at`, f.ProjectID, f.Key, f.Name, f.Description, f.Type).Scan(&f.ID, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		return f, mapError(err)
	}
	if err = audit(ctx, tx, p, "FLAG_CREATED", project, "", f.Key, nil, f); err != nil {
		return f, err
	}
	return f, tx.Commit(ctx)
}

func snapshot(ctx context.Context, q queryer, envID string) (types.Snapshot, error) {
	s := types.Snapshot{Flags: map[string]types.FlagConfig{}, Segments: map[string]types.Segment{}}
	err := q.QueryRow(ctx, `SELECT p.key,e.key,e.version,e.updated_at FROM environments e JOIN projects p ON p.id=e.project_id WHERE e.id=$1`, envID).Scan(&s.Project, &s.Environment, &s.Version, &s.UpdatedAt)
	if err != nil {
		return s, mapError(err)
	}
	rows, err := q.Query(ctx, `SELECT f.key,f.type,c.enabled,c.default_variation,c.off_variation,c.targets,c.excluded,c.rollout,c.prerequisites,c.version,
 COALESCE((SELECT jsonb_object_agg(v.key,v.value) FROM flag_variations v WHERE v.flag_id=c.flag_id AND v.environment_id=c.environment_id),'{}'),
 COALESCE((SELECT jsonb_agg(jsonb_build_object('id',r.id,'priority',r.priority,'variation',r.variation,'segments',r.segments,'conditions',
 COALESCE((SELECT jsonb_agg(jsonb_build_object('attribute',t.attribute,'operator',t.operator,'values',t.values) ORDER BY t.position) FROM targeting_conditions t WHERE t.flag_id=r.flag_id AND t.environment_id=r.environment_id AND t.rule_id=r.id),'[]')) ORDER BY r.priority,r.id)
 FROM targeting_rules r WHERE r.flag_id=c.flag_id AND r.environment_id=c.environment_id),'[]')
 FROM flag_environment_configs c JOIN feature_flags f ON f.id=c.flag_id WHERE c.environment_id=$1 AND f.status!='ARCHIVED'`, envID)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var f types.FlagConfig
		var targets, excluded, rollout, pre, vars, rules []byte
		if err = rows.Scan(&f.Key, &f.Type, &f.Enabled, &f.DefaultVariation, &f.OffVariation, &targets, &excluded, &rollout, &pre, &f.Version, &vars, &rules); err != nil {
			rows.Close()
			return s, err
		}
		for _, pair := range []struct {
			b []byte
			v any
		}{{targets, &f.Targets}, {excluded, &f.Exclude}, {rollout, &f.Rollout}, {pre, &f.Prerequisites}, {vars, &f.Variations}, {rules, &f.Rules}} {
			if err = json.Unmarshal(pair.b, pair.v); err != nil {
				rows.Close()
				return s, err
			}
		}
		s.Flags[f.Key] = f
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return s, err
	}
	rows, err = q.Query(ctx, `SELECT s.key,s.name,s.included,s.excluded,COALESCE((SELECT jsonb_agg(definition ORDER BY position) FROM segment_rules r WHERE r.environment_id=s.environment_id AND r.segment_key=s.key),'[]') FROM segments s WHERE s.environment_id=$1`, envID)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var seg types.Segment
		var inc, exc, rules []byte
		if err = rows.Scan(&seg.Key, &seg.Name, &inc, &exc, &rules); err != nil {
			return s, err
		}
		if err = json.Unmarshal(inc, &seg.Include); err != nil {
			return s, err
		}
		if err = json.Unmarshal(exc, &seg.Exclude); err != nil {
			return s, err
		}
		if err = json.Unmarshal(rules, &seg.Rules); err != nil {
			return s, err
		}
		s.Segments[seg.Key] = seg
	}
	return s, rows.Err()
}

func (r *Repository) Snapshot(ctx context.Context, envID string) (types.Snapshot, error) {
	tx, err := r.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return types.Snapshot{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	s, err := snapshot(ctx, tx, envID)
	if err != nil {
		return s, err
	}
	return s, tx.Commit(ctx)
}

func (r *Repository) Config(ctx context.Context, project, env, key string) (types.FlagConfig, error) {
	e, err := environment(ctx, r.Pool, project, env, false)
	if err != nil {
		return types.FlagConfig{}, err
	}
	s, err := r.Snapshot(ctx, e.ID)
	if err != nil {
		return types.FlagConfig{}, err
	}
	f, ok := s.Flags[key]
	if !ok {
		return f, ErrNotFound
	}
	return f, nil
}

// change serializes all mutations in an environment and creates version + outbox atomically.
func change(ctx context.Context, tx pgx.Tx, p Principal, project string, env types.Environment, flag, action string, before, after any) (int64, error) {
	var version int64
	var at time.Time
	if err := tx.QueryRow(ctx, `UPDATE environments SET version=version+1,updated_at=clock_timestamp() WHERE id=$1 RETURNING version,updated_at`, env.ID).Scan(&version, &at); err != nil {
		return 0, err
	}
	if err := audit(ctx, tx, p, action, project, env.Key, flag, before, after); err != nil {
		return 0, err
	}
	e := types.Event{EnvironmentID: env.ID, Version: version, CreatedAt: at}
	_, err := tx.Exec(ctx, `INSERT INTO outbox_events(environment_id,version,payload) VALUES($1,$2,$3)`, env.ID, version, encode(e))
	return version, err
}

func saveConfig(ctx context.Context, tx pgx.Tx, env types.Environment, key string, f types.FlagConfig) error {
	var flagID string
	var kind types.FlagType
	err := tx.QueryRow(ctx, `SELECT id::text,type FROM feature_flags WHERE project_id=$1 AND key=$2 AND status!='ARCHIVED'`, env.ProjectID, key).Scan(&flagID, &kind)
	if err != nil {
		return mapError(err)
	}
	if f.Type != kind {
		return fmt.Errorf("%w: flag type is immutable", ErrInvalid)
	}
	_, err = tx.Exec(ctx, `INSERT INTO flag_environment_configs(flag_id,environment_id,enabled,default_variation,off_variation,targets,excluded,rollout,prerequisites,version)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(flag_id,environment_id) DO UPDATE SET enabled=EXCLUDED.enabled,default_variation=EXCLUDED.default_variation,off_variation=EXCLUDED.off_variation,targets=EXCLUDED.targets,excluded=EXCLUDED.excluded,rollout=EXCLUDED.rollout,prerequisites=EXCLUDED.prerequisites,version=EXCLUDED.version`, flagID, env.ID, f.Enabled, f.DefaultVariation, f.OffVariation, encode(f.Targets), encode(f.Exclude), encode(f.Rollout), encode(f.Prerequisites), f.Version)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM flag_variations WHERE flag_id=$1 AND environment_id=$2`, flagID, env.ID); err != nil {
		return err
	}
	for k, v := range f.Variations {
		if _, err = tx.Exec(ctx, `INSERT INTO flag_variations VALUES($1,$2,$3,$4)`, flagID, env.ID, k, encode(v)); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM targeting_rules WHERE flag_id=$1 AND environment_id=$2`, flagID, env.ID); err != nil {
		return err
	}
	for _, rule := range f.Rules {
		if _, err = tx.Exec(ctx, `INSERT INTO targeting_rules VALUES($1,$2,$3,$4,$5,$6)`, flagID, env.ID, rule.ID, rule.Priority, rule.Variation, encode(rule.Segments)); err != nil {
			return err
		}
		for i, c := range rule.Conditions {
			if _, err = tx.Exec(ctx, `INSERT INTO targeting_conditions VALUES($1,$2,$3,$4,$5,$6,$7)`, flagID, env.ID, rule.ID, i, c.Attribute, c.Operator, encode(c.Values)); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO config_history VALUES($1,$2,$3,$4)`, env.ID, key, f.Version, encode(f))
	return err
}

func updateConfig(ctx context.Context, tx pgx.Tx, p Principal, project string, env types.Environment, key string, expected int64, f types.FlagConfig) (types.FlagConfig, error) {
	s, err := snapshot(ctx, tx, env.ID)
	if err != nil {
		return f, err
	}
	old := s.Flags[key]
	if old.Version != expected {
		return f, ErrConflict
	}
	f.Key = key
	f.Version = env.Version + 1
	s.Flags[key] = f
	if err = evaluation.ValidateSnapshot(s); err != nil {
		return f, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err = saveConfig(ctx, tx, env, key, f); err != nil {
		return f, err
	}
	_, err = change(ctx, tx, p, project, env, key, "FLAG_CONFIG_UPDATED", old, f)
	return f, err
}

func (r *Repository) UpdateConfig(ctx context.Context, p Principal, project, envKey, key string, expected int64, f types.FlagConfig) (types.FlagConfig, error) {
	ctx, span := otel.Tracer("flagforge/repository").Start(ctx, "flag.config.update")
	defer span.End()
	span.SetAttributes(attribute.String("project", project), attribute.String("environment", envKey), attribute.String("flag", key), attribute.Int64("expected_version", expected))
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return f, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	env, err := environment(ctx, tx, project, envKey, true)
	if err != nil {
		return f, err
	}
	if err = authorize(p, env); err != nil {
		return f, err
	}
	f, err = updateConfig(ctx, tx, p, project, env, key, expected, f)
	if err != nil {
		return f, err
	}
	return f, tx.Commit(ctx)
}

func (r *Repository) PutSegment(ctx context.Context, p Principal, project, envKey string, expected int64, seg types.Segment) (int64, error) {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	env, err := environment(ctx, tx, project, envKey, true)
	if err != nil {
		return 0, err
	}
	if err = authorize(p, env); err != nil {
		return 0, err
	}
	if env.Version != expected {
		return 0, ErrConflict
	}
	s, err := snapshot(ctx, tx, env.ID)
	if err != nil {
		return 0, err
	}
	old := s.Segments[seg.Key]
	s.Segments[seg.Key] = seg
	if err = evaluation.ValidateSnapshot(s); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO segments VALUES($1,$2,$3,$4,$5) ON CONFLICT(environment_id,key) DO UPDATE SET name=EXCLUDED.name,included=EXCLUDED.included,excluded=EXCLUDED.excluded`, env.ID, seg.Key, seg.Name, encode(seg.Include), encode(seg.Exclude)); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM segment_rules WHERE environment_id=$1 AND segment_key=$2`, env.ID, seg.Key); err != nil {
		return 0, err
	}
	for i, rule := range seg.Rules {
		if _, err = tx.Exec(ctx, `INSERT INTO segment_rules VALUES($1,$2,$3,$4)`, env.ID, seg.Key, i, encode(rule)); err != nil {
			return 0, err
		}
	}
	v, err := change(ctx, tx, p, project, env, "", "SEGMENT_UPDATED", old, seg)
	if err != nil {
		return 0, err
	}
	return v, tx.Commit(ctx)
}
