-- Egress attempts of build sandboxes (allowed and denied), for scanning and audit.
CREATE TABLE build_egress (
  id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  build_id  text NOT NULL REFERENCES builds(id) ON DELETE CASCADE,
  ts        timestamptz NOT NULL DEFAULT now(),
  method    text NOT NULL,
  host      text NOT NULL,
  port      int NOT NULL,
  allowed   boolean NOT NULL,
  reason    text,
  bytes     bigint NOT NULL DEFAULT 0
);
CREATE INDEX build_egress_build ON build_egress (build_id, allowed);
