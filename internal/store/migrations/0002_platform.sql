-- Platform tables not in the reference schema: auth, queue, hosts, webhooks.

ALTER TABLE spec_versions ADD COLUMN source text;              -- original YAML/JSON as submitted
ALTER TABLE spec_versions ADD COLUMN warnings jsonb NOT NULL DEFAULT '[]';
ALTER TABLE builds ADD COLUMN catalog_version text;
ALTER TABLE builds ADD COLUMN tree_digest bytea;
ALTER TABLE builds ADD COLUMN platform jsonb;
ALTER TABLE builds ADD COLUMN priority smallint NOT NULL DEFAULT 0;
ALTER TABLE builds ADD COLUMN cancel_requested boolean NOT NULL DEFAULT false;
ALTER TABLE builds ADD COLUMN started_at timestamptz;
ALTER TABLE builds ADD COLUMN finished_at timestamptz;
ALTER TABLE builds ADD COLUMN predicted_minutes real NOT NULL DEFAULT 20;
ALTER TABLE builds ADD COLUMN test_report jsonb;

CREATE TABLE sessions (
  id          text PRIMARY KEY,                 -- random 256-bit, stored hashed
  token_hash  bytea NOT NULL UNIQUE,
  user_id     text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  csrf        text NOT NULL,
  expires_at  timestamptz NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  last_seen   timestamptz NOT NULL DEFAULT now(),
  user_agent  text
);
CREATE INDEX sessions_user ON sessions (user_id);

CREATE TABLE api_tokens (
  id          text PRIMARY KEY,                 -- tok_...
  org_id      text NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  user_id     text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name        text NOT NULL,
  token_hash  bytea NOT NULL UNIQUE,            -- sha256 of the secret
  scopes      text[] NOT NULL,
  expires_at  timestamptz NOT NULL,
  last_used   timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  revoked_at  timestamptz
);

CREATE TABLE login_codes (
  id          text PRIMARY KEY,
  email       citext NOT NULL,
  code_hash   bytea NOT NULL,
  attempts    smallint NOT NULL DEFAULT 0,
  expires_at  timestamptz NOT NULL,
  used_at     timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX login_codes_email ON login_codes (email, created_at DESC);

CREATE TABLE oidc_states (
  state       text PRIMARY KEY,
  verifier    text NOT NULL,
  nonce       text NOT NULL,
  redirect    text NOT NULL,
  expires_at  timestamptz NOT NULL
);

-- Job queue (internal/queue). FOR UPDATE SKIP LOCKED is used only there.
CREATE TABLE jobs (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  kind         text NOT NULL,
  org_id       text,
  payload      jsonb NOT NULL DEFAULT '{}',
  state        text NOT NULL DEFAULT 'available' CHECK (state IN ('available','running','done','failed','discarded')),
  priority     smallint NOT NULL DEFAULT 0,
  attempt      smallint NOT NULL DEFAULT 0,
  max_attempts smallint NOT NULL DEFAULT 3,
  run_at       timestamptz NOT NULL DEFAULT now(),
  locked_by    text,
  locked_until timestamptz,
  last_error   text,
  unique_key   text,
  created_at   timestamptz NOT NULL DEFAULT now(),
  finished_at  timestamptz
);
CREATE INDEX jobs_ready ON jobs (kind, priority DESC, run_at, id) WHERE state = 'available';
CREATE INDEX jobs_running ON jobs (locked_until) WHERE state = 'running';
CREATE UNIQUE INDEX jobs_unique ON jobs (kind, unique_key) WHERE unique_key IS NOT NULL AND state IN ('available','running');

CREATE TABLE hosts (
  id          text PRIMARY KEY,
  pool        text NOT NULL CHECK (pool IN ('build','test','preview','finalize')),
  arch        text NOT NULL,
  slots       int NOT NULL CHECK (slots > 0),
  weight      real NOT NULL DEFAULT 1,
  labels      jsonb NOT NULL DEFAULT '{}',
  last_seen   timestamptz NOT NULL DEFAULT now(),
  draining    boolean NOT NULL DEFAULT false
);

CREATE TABLE webhooks (
  id         text PRIMARY KEY,
  org_id     text NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  url        text NOT NULL,
  secret     text NOT NULL,
  events     text[] NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  disabled_at timestamptz
);

CREATE TABLE webhook_deliveries (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  webhook_id  text NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
  event       text NOT NULL,
  body        jsonb NOT NULL,
  status      int,
  attempts    smallint NOT NULL DEFAULT 0,
  next_try_at timestamptz NOT NULL DEFAULT now(),
  delivered_at timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE build_logs (
  build_id  text NOT NULL REFERENCES builds(id) ON DELETE CASCADE,
  seq       int NOT NULL,
  ts        timestamptz NOT NULL DEFAULT now(),
  stream    text NOT NULL DEFAULT 'build',
  line      text NOT NULL,
  PRIMARY KEY (build_id, seq)
);

-- Maintenance: create a monthly partition when needed (called by the scheduler daily).
CREATE OR REPLACE FUNCTION foundry_ensure_month_partition(parent text, month date) RETURNS void AS $$
DECLARE
  name text := parent || '_' || to_char(month, 'YYYY_MM');
BEGIN
  IF to_regclass(name) IS NULL THEN
    EXECUTE format('CREATE TABLE %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)', name, parent, month, month + interval '1 month');
  END IF;
END $$ LANGUAGE plpgsql;
