-- Signed job envelopes (Ed25519) issued by the API at build creation.
ALTER TABLE builds ADD COLUMN envelope jsonb;
