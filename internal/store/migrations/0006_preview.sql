-- Live preview sessions (the VM runs on a previewd host; this row authorizes
-- the browser's WebSocket and is the audit trail).
CREATE TABLE preview_sessions (
    id          text PRIMARY KEY,
    org_id      text NOT NULL REFERENCES orgs(id),
    build_id    text NOT NULL REFERENCES builds(id),
    user_id     text NOT NULL REFERENCES users(id),
    remote_id   text NOT NULL,
    expires_at  timestamptz NOT NULL,
    stopped_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX preview_sessions_org ON preview_sessions (org_id, created_at DESC);
