-- Pipeline: free-form artifact kinds (validated in Go), requested output
-- formats per build, PCR predictions captured at finalize.
ALTER TABLE artifacts DROP CONSTRAINT IF EXISTS artifacts_kind_check;
ALTER TABLE artifacts ADD CONSTRAINT artifacts_kind_check CHECK (kind ~ '^[a-z0-9][a-z0-9.-]{0,40}$');
ALTER TABLE builds ADD COLUMN formats text[] NOT NULL DEFAULT '{}';
ALTER TABLE builds ADD COLUMN pcr11 jsonb;
ALTER TABLE hosts ADD COLUMN version text NOT NULL DEFAULT '';
CREATE INDEX builds_host ON builds (host_id) WHERE state IN ('leased','building');
