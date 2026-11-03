-- Foundry control-plane schema (Postgres 17+). Reference DDL; migrations derive from this file.
-- Conventions: text ULID ids with type prefixes, org_id on every tenant table, timestamptz UTC.

CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS citext;

-- ───────────── Tenancy & identity ─────────────
CREATE TABLE orgs (
  id            text PRIMARY KEY,                       -- org_...
  name          text NOT NULL,
  slug          citext NOT NULL UNIQUE,
  plan          text NOT NULL DEFAULT 'pilot' CHECK (plan IN ('free','pilot','pro','business','enterprise')),
  region        text NOT NULL DEFAULT 'eu' CHECK (region IN ('eu','us','in')),
  expert_allowed boolean NOT NULL DEFAULT false,
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE users (
  id            text PRIMARY KEY,                       -- usr_...
  email         citext NOT NULL UNIQUE,
  display_name  text NOT NULL,
  oidc_subject  text,
  oidc_issuer   text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (oidc_issuer, oidc_subject)
);

CREATE TABLE memberships (
  org_id   text NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  user_id  text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role     text NOT NULL CHECK (role IN ('owner','admin','builder','reviewer','fleet_operator','viewer')),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, user_id)
);

CREATE TABLE idempotency_keys (
  org_id        text NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  key           text NOT NULL,
  request_hash  bytea NOT NULL,
  status_code   int NOT NULL,
  response      jsonb NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, key)
);

-- ───────────── Catalog ─────────────
CREATE TABLE catalog_versions (
  id           text PRIMARY KEY,                        -- git tree hash
  cnf_digest   bytea NOT NULL,
  published_at timestamptz NOT NULL DEFAULT now(),
  is_default   boolean NOT NULL DEFAULT false
);
CREATE UNIQUE INDEX catalog_one_default ON catalog_versions (is_default) WHERE is_default;

-- ───────────── Projects, specs, assets ─────────────
CREATE TABLE projects (
  id         text PRIMARY KEY,                          -- prj_...
  org_id     text NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  name       text NOT NULL,
  os_id      text NOT NULL CHECK (os_id ~ '^[a-z][a-z0-9-]{2,39}$'),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (org_id, os_id)
);

CREATE TABLE spec_versions (
  id              text PRIMARY KEY,                     -- spv_...
  org_id          text NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  project_id      text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  spec_digest     bytea NOT NULL,                       -- sha256(JCS(spec))
  api_version     text NOT NULL,
  spec            jsonb NOT NULL,                       -- canonical, defaults applied
  parent_id       text REFERENCES spec_versions(id),
  patch_from_parent jsonb,                              -- RFC 6902
  catalog_version text NOT NULL REFERENCES catalog_versions(id),
  has_expert      boolean NOT NULL,
  created_by      text NOT NULL REFERENCES users(id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (project_id, spec_digest)
);
CREATE INDEX spec_versions_project ON spec_versions (project_id, created_at DESC, id DESC);

CREATE TABLE assets (
  digest      bytea NOT NULL,                           -- sha256 of content
  org_id      text NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  kind        text NOT NULL CHECK (kind IN ('image','font','overlay','deb','flatpak','script','layout')),
  media_type  text NOT NULL,
  size_bytes  bigint NOT NULL CHECK (size_bytes > 0),
  validation  jsonb NOT NULL,                           -- validator report
  status      text NOT NULL CHECK (status IN ('uploaded','valid','rejected')),
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, digest)
);

-- ───────────── Builds ─────────────
CREATE TABLE builds (
  id              text PRIMARY KEY,                     -- bld_...
  org_id          text NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  project_id      text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  spec_version_id text NOT NULL REFERENCES spec_versions(id),
  arch            text NOT NULL CHECK (arch IN ('x86_64','aarch64')),
  state           text NOT NULL CHECK (state IN ('queued','leased','building','built','scanning','scanned',
                    'testing','tested','awaiting_review','approved','changes_requested','rejected',
                    'finalizing','signed','published','revoked','failed','cancelled')),
  tier            text CHECK (tier IN ('verified','reviewed','private-unverified','revoked')),
  risk_score      smallint CHECK (risk_score BETWEEN 0 AND 100),
  host_id         text,
  lease_expires_at timestamptz,
  attempt         smallint NOT NULL DEFAULT 0,
  failure         jsonb,                                -- {stage, reason, retryable}
  usr_roothash    bytea,
  manifest_digest bytea,
  reproduced      boolean,                              -- double-build match
  created_by      text NOT NULL REFERENCES users(id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX builds_project ON builds (project_id, created_at DESC, id DESC);
CREATE INDEX builds_active ON builds (state, created_at) WHERE state IN ('queued','leased','building','scanning','testing','finalizing');
CREATE INDEX builds_review ON builds (risk_score DESC, created_at) WHERE state = 'awaiting_review';
CREATE INDEX builds_org ON builds (org_id, created_at DESC);

CREATE TABLE build_events (
  build_id   text NOT NULL,
  org_id     text NOT NULL,
  seq        int  NOT NULL,
  ts         timestamptz NOT NULL DEFAULT now(),
  from_state text,
  to_state   text NOT NULL,
  actor      text NOT NULL,                             -- user id or service name
  detail     jsonb,
  PRIMARY KEY (build_id, seq, ts)
) PARTITION BY RANGE (ts);
CREATE TABLE build_events_default PARTITION OF build_events DEFAULT;

CREATE TABLE stage_runs (
  build_id    text NOT NULL REFERENCES builds(id) ON DELETE CASCADE,
  stage       text NOT NULL,
  cache_key   bytea NOT NULL,
  cache_hit   text NOT NULL CHECK (cache_hit IN ('l1','l2','miss')),
  started_at  timestamptz NOT NULL,
  finished_at timestamptz,
  exit_code   int,
  PRIMARY KEY (build_id, stage)
);
CREATE INDEX stage_runs_stats ON stage_runs (stage, started_at DESC);

CREATE TABLE artifacts (
  build_id   text NOT NULL REFERENCES builds(id) ON DELETE CASCADE,
  org_id     text NOT NULL,
  kind       text NOT NULL CHECK (kind IN ('disk.img.xz','disk.raw.zst','qcow2','vmdk','ova','iso','usr','usr-verity',
               'usr-verity-sig','uki','sbom','manifest','provenance','cidx','sources','sha256sums')),
  digest     bytea NOT NULL,
  size_bytes bigint NOT NULL,
  signed     boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (build_id, kind)
);
CREATE INDEX artifacts_digest ON artifacts (digest);

-- ───────────── Security pipeline ─────────────
CREATE TABLE scans (
  id          text PRIMARY KEY,
  build_id    text NOT NULL REFERENCES builds(id) ON DELETE CASCADE,
  scanner     text NOT NULL CHECK (scanner IN ('sbom','cve','malware','yara','script','diff','behavior')),
  tool_version text NOT NULL,
  db_version  text,
  status      text NOT NULL CHECK (status IN ('running','passed','flagged','blocked','error')),
  started_at  timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz,
  report_digest bytea
);
CREATE INDEX scans_build ON scans (build_id);

CREATE TABLE findings (
  id        text PRIMARY KEY,
  scan_id   text NOT NULL REFERENCES scans(id) ON DELETE CASCADE,
  build_id  text NOT NULL,
  rule_id   text NOT NULL,
  severity  text NOT NULL CHECK (severity IN ('info','low','medium','high','critical','blocker')),
  path      text,
  weight    smallint NOT NULL DEFAULT 0,
  detail    jsonb NOT NULL,
  accepted_by text REFERENCES users(id),
  accepted_reason text
);
CREATE INDEX findings_build ON findings (build_id, severity);

CREATE TABLE reviews (
  id        text PRIMARY KEY,
  build_id  text NOT NULL REFERENCES builds(id) ON DELETE CASCADE,
  reviewer  text NOT NULL REFERENCES users(id),
  decision  text NOT NULL CHECK (decision IN ('approve','reject','changes_requested')),
  comment   text,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (build_id, reviewer)
);

CREATE TABLE keys (
  id          text PRIMARY KEY,                         -- key_...
  org_id      text REFERENCES orgs(id),                 -- NULL = platform key
  purpose     text NOT NULL CHECK (purpose IN ('sb-db','uki-pcr-initrd','uki-pcr-main','verity','tuf-root','tuf-targets',
                'tuf-snapshot','tuf-timestamp','device-ca','policy','provenance','job-envelope')),
  algorithm   text NOT NULL,
  hsm_ref     text NOT NULL,                            -- PKCS#11 URI, never key material
  public_pem  text NOT NULL,
  not_before  timestamptz NOT NULL,
  not_after   timestamptz NOT NULL,
  revoked_at  timestamptz
);
CREATE INDEX keys_org_purpose ON keys (org_id, purpose) WHERE revoked_at IS NULL;

-- ───────────── Releases & rollouts ─────────────
CREATE TABLE releases (
  id          text PRIMARY KEY,                         -- rel_...
  org_id      text NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  project_id  text NOT NULL REFERENCES projects(id),
  build_id    text NOT NULL UNIQUE REFERENCES builds(id),
  version     text NOT NULL,
  arch        text NOT NULL,
  usr_roothash bytea NOT NULL,
  pcr11_predictions jsonb NOT NULL,
  tier        text NOT NULL,
  revoked_at  timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (project_id, version, arch)
);

CREATE TABLE channels (
  id          text PRIMARY KEY,
  org_id      text NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  project_id  text NOT NULL REFERENCES projects(id),
  name        text NOT NULL,
  release_id  text REFERENCES releases(id),
  UNIQUE (project_id, name)
);

CREATE TABLE rollouts (
  id          text PRIMARY KEY,
  channel_id  text NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  release_id  text NOT NULL REFERENCES releases(id),
  state       text NOT NULL CHECK (state IN ('draft','running','paused','halted','completed')),
  percent     numeric(5,2) NOT NULL DEFAULT 0 CHECK (percent BETWEEN 0 AND 100),
  stages      jsonb NOT NULL,                           -- [{percent, min_devices, min_hours}]
  gate_config jsonb NOT NULL,
  started_at  timestamptz,
  updated_at  timestamptz NOT NULL DEFAULT now()
);

-- ───────────── Fleet ─────────────
CREATE TABLE device_groups (
  id        text PRIMARY KEY,
  org_id    text NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  name      text NOT NULL,
  parent_id text REFERENCES device_groups(id),
  channel_id text REFERENCES channels(id),
  UNIQUE (org_id, name)
);

CREATE TABLE enrollment_tokens (
  id          text PRIMARY KEY,
  org_id      text NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  group_id    text NOT NULL REFERENCES device_groups(id),
  secret_hash bytea NOT NULL,                           -- argon2id
  uses_left   int NOT NULL CHECK (uses_left >= 0),
  expires_at  timestamptz NOT NULL,
  created_by  text NOT NULL REFERENCES users(id),
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE devices (
  id               text PRIMARY KEY,                    -- dev_...
  org_id           text NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  group_id         text NOT NULL REFERENCES device_groups(id),
  hostname         text,
  cert_fingerprint bytea NOT NULL UNIQUE,
  cert_not_after   timestamptz NOT NULL,
  ek_verified      boolean NOT NULL DEFAULT false,
  ak_pub           bytea,
  hw_model         text,
  release_version  text,
  booted_slot      text,
  update_stage     text,
  attestation      text CHECK (attestation IN ('attested','mismatch','unknown')),
  state_hash       bytea,
  state            jsonb,                               -- last heartbeat body (latest only)
  tags             text[] NOT NULL DEFAULT '{}',
  last_seen        timestamptz,
  enrolled_at      timestamptz NOT NULL DEFAULT now(),
  retired_at       timestamptz
);
CREATE INDEX devices_group ON devices (org_id, group_id, last_seen DESC);
CREATE INDEX devices_release ON devices (org_id, release_version);
CREATE INDEX devices_tags ON devices USING gin (tags);
CREATE INDEX devices_hostname_trgm ON devices USING gin (hostname gin_trgm_ops);

CREATE TABLE device_reports (
  device_id text NOT NULL,
  org_id    text NOT NULL,
  ts        timestamptz NOT NULL,
  seq       bigint NOT NULL,
  body      jsonb NOT NULL
) PARTITION BY RANGE (ts);
CREATE TABLE device_reports_default PARTITION OF device_reports DEFAULT;
CREATE INDEX device_reports_brin ON device_reports USING brin (ts);

CREATE TABLE device_commands (
  id          text PRIMARY KEY,
  org_id      text NOT NULL,
  device_id   text NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  type        text NOT NULL CHECK (type IN ('reboot','update-now','collect-logs','lock','unlock','wipe','set-url','screenshot')),
  args        jsonb NOT NULL DEFAULT '{}',
  issued_by   text NOT NULL REFERENCES users(id),
  approved_by text REFERENCES users(id),
  issued_at   timestamptz NOT NULL DEFAULT now(),
  expires_at  timestamptz NOT NULL,
  delivered_at timestamptz,
  result      jsonb
);
CREATE INDEX device_commands_pending ON device_commands (device_id) WHERE delivered_at IS NULL;

CREATE TABLE attestations (
  id         text PRIMARY KEY,
  device_id  text NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  nonce      bytea NOT NULL,
  result     text NOT NULL CHECK (result IN ('attested','mismatch','unknown')),
  pcrs       jsonb NOT NULL,
  release_id text REFERENCES releases(id),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX attestations_device ON attestations (device_id, created_at DESC);

CREATE TABLE policies (
  id         text PRIMARY KEY,
  org_id     text NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  scope_type text NOT NULL CHECK (scope_type IN ('org','group','device')),
  scope_id   text NOT NULL,
  version    int NOT NULL,
  document   jsonb NOT NULL,
  signature  bytea NOT NULL,
  created_by text NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (scope_type, scope_id, version)
);

-- ───────────── HCL ─────────────
CREATE TABLE hcl_models (
  id           text PRIMARY KEY,                        -- e.g. lenovo-thinkpad-t14-gen4
  dmi_vendor   text NOT NULL,
  dmi_product  text NOT NULL,
  status       text NOT NULL CHECK (status IN ('certified','works','issues','unsupported','unknown')),
  notes        text
);

-- ───────────── Audit ─────────────
CREATE TABLE audit_log (
  id         bigint GENERATED ALWAYS AS IDENTITY,
  org_id     text,
  ts         timestamptz NOT NULL DEFAULT now(),
  actor      text NOT NULL,
  action     text NOT NULL,
  target     text,
  detail     jsonb,
  prev_hash  bytea NOT NULL,
  hash       bytea NOT NULL,                            -- sha256(prev_hash || canonical(entry))
  PRIMARY KEY (id, ts)
) PARTITION BY RANGE (ts);
CREATE TABLE audit_log_default PARTITION OF audit_log DEFAULT;

