-- +goose Up
CREATE TABLE projects (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), key text UNIQUE NOT NULL, name text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE environments (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), project_id uuid NOT NULL REFERENCES projects(id),
 key text NOT NULL, name text NOT NULL, protected boolean NOT NULL DEFAULT false,
 version bigint NOT NULL DEFAULT 0, updated_at timestamptz NOT NULL DEFAULT now(), UNIQUE(project_id,key)
);
CREATE TABLE feature_flags (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), project_id uuid NOT NULL REFERENCES projects(id),
 key text NOT NULL, name text NOT NULL, description text NOT NULL DEFAULT '', type text NOT NULL,
 status text NOT NULL DEFAULT 'ACTIVE' CHECK(status IN ('ACTIVE','DEPRECATED','ARCHIVED')),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), UNIQUE(project_id,key)
);
CREATE TABLE flag_environment_configs (
 flag_id uuid NOT NULL REFERENCES feature_flags(id), environment_id uuid NOT NULL REFERENCES environments(id),
 enabled boolean NOT NULL, default_variation text NOT NULL, off_variation text NOT NULL,
 targets jsonb NOT NULL DEFAULT '{}', excluded jsonb NOT NULL DEFAULT '[]',
 rollout jsonb NOT NULL DEFAULT '[]', prerequisites jsonb NOT NULL DEFAULT '[]', version bigint NOT NULL,
 PRIMARY KEY(flag_id,environment_id)
);
CREATE TABLE flag_variations (
 flag_id uuid NOT NULL, environment_id uuid NOT NULL, key text NOT NULL, value jsonb NOT NULL,
 PRIMARY KEY(flag_id,environment_id,key),
 FOREIGN KEY(flag_id,environment_id) REFERENCES flag_environment_configs(flag_id,environment_id) ON DELETE CASCADE
);
CREATE TABLE targeting_rules (
 flag_id uuid NOT NULL, environment_id uuid NOT NULL, id text NOT NULL, priority integer NOT NULL,
 variation text NOT NULL, segments jsonb NOT NULL DEFAULT '[]',
 PRIMARY KEY(flag_id,environment_id,id),
 FOREIGN KEY(flag_id,environment_id) REFERENCES flag_environment_configs(flag_id,environment_id) ON DELETE CASCADE
);
CREATE TABLE targeting_conditions (
 flag_id uuid NOT NULL, environment_id uuid NOT NULL, rule_id text NOT NULL, position integer NOT NULL,
 attribute text NOT NULL, operator text NOT NULL, values jsonb NOT NULL,
 PRIMARY KEY(flag_id,environment_id,rule_id,position),
 FOREIGN KEY(flag_id,environment_id,rule_id) REFERENCES targeting_rules(flag_id,environment_id,id) ON DELETE CASCADE
);
CREATE TABLE segments (
 environment_id uuid NOT NULL REFERENCES environments(id), key text NOT NULL, name text NOT NULL,
 included jsonb NOT NULL DEFAULT '[]', excluded jsonb NOT NULL DEFAULT '[]', PRIMARY KEY(environment_id,key)
);
CREATE TABLE segment_rules (
 environment_id uuid NOT NULL, segment_key text NOT NULL, position integer NOT NULL, definition jsonb NOT NULL,
 PRIMARY KEY(environment_id,segment_key,position),
 FOREIGN KEY(environment_id,segment_key) REFERENCES segments(environment_id,key) ON DELETE CASCADE
);
CREATE TABLE api_tokens (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), hash bytea UNIQUE NOT NULL, actor text NOT NULL,
 role text NOT NULL CHECK(role IN ('Viewer','Developer','Admin')), revoked_at timestamptz
);
CREATE TABLE sdk_keys (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), environment_id uuid NOT NULL REFERENCES environments(id),
 hash bytea UNIQUE NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), revoked_at timestamptz
);
CREATE TABLE audit_logs (
 id bigserial PRIMARY KEY, actor text NOT NULL, action text NOT NULL, project text NOT NULL,
 environment text NOT NULL DEFAULT '', flag text NOT NULL DEFAULT '', before_state jsonb, after_state jsonb,
 timestamp timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementBegin
CREATE FUNCTION immutable_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 RAISE EXCEPTION 'audit history is immutable';
END $$;
-- +goose StatementEnd
CREATE TRIGGER audit_immutable BEFORE UPDATE OR DELETE ON audit_logs FOR EACH ROW EXECUTE FUNCTION immutable_audit();
CREATE TABLE config_history (
 environment_id uuid NOT NULL REFERENCES environments(id), flag_key text NOT NULL, version bigint NOT NULL,
 config jsonb NOT NULL, PRIMARY KEY(environment_id,flag_key,version)
);
CREATE TABLE outbox_events (
 id bigserial PRIMARY KEY, environment_id uuid NOT NULL REFERENCES environments(id), version bigint NOT NULL,
 payload jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), published_at timestamptz
);
CREATE INDEX outbox_pending ON outbox_events(id) WHERE published_at IS NULL;
CREATE INDEX audit_filter ON audit_logs(project, environment, flag, timestamp DESC);
CREATE TABLE scheduled_changes (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), environment_id uuid NOT NULL REFERENCES environments(id),
 flag_key text NOT NULL, config jsonb NOT NULL, actor text NOT NULL,
 expected_version bigint NOT NULL, run_at timestamptz NOT NULL,
 status text NOT NULL DEFAULT 'PENDING' CHECK(status IN ('PENDING','APPLIED','CONFLICT','CANCELLED')),
 error text NOT NULL DEFAULT '', applied_at timestamptz
);
ALTER TABLE scheduled_changes ADD COLUMN predecessor_id uuid REFERENCES scheduled_changes(id);
CREATE INDEX schedule_due ON scheduled_changes(run_at) WHERE status='PENDING';
CREATE TABLE evaluation_usage (
 environment_id uuid NOT NULL REFERENCES environments(id), flag_key text NOT NULL, variation text NOT NULL,
 reason text NOT NULL, count bigint NOT NULL, last_evaluated timestamptz NOT NULL,
 PRIMARY KEY(environment_id,flag_key,variation,reason)
);

-- +goose Down
DROP TABLE evaluation_usage, scheduled_changes, outbox_events, config_history, audit_logs, sdk_keys, api_tokens,
 segment_rules, segments, targeting_conditions, targeting_rules, flag_variations, flag_environment_configs,
 feature_flags, environments, projects CASCADE;
DROP FUNCTION immutable_audit();
