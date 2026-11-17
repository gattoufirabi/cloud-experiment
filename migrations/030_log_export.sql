-- Stage 10: log retention per plan and continuous log export. Logs stay in VictoriaLogs
-- (31 days at most); each plan sees and exports its own retention window. Exports send
-- an organization's logs, in order and once, to one of its buckets (hourly NDJSON
-- files, gzipped) or to an HTTPS endpoint (NDJSON batches, signed).

ALTER TABLE plans ADD COLUMN log_retention_days int NOT NULL DEFAULT 7 CHECK (log_retention_days BETWEEN 1 AND 31);
UPDATE plans SET log_retention_days = 1 WHERE id = 'trial';
UPDATE plans SET log_retention_days = 30 WHERE id = 'pro';

CREATE TABLE log_exports (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    name        text NOT NULL CHECK (length(name) BETWEEN 1 AND 60),
    kind        text NOT NULL CHECK (kind IN ('bucket', 'https')),
    bucket_id   uuid REFERENCES buckets ON DELETE CASCADE,
    prefix      text NOT NULL DEFAULT 'logs/' CHECK (prefix ~ '^[A-Za-z0-9._/-]{0,100}$'),
    url         text NOT NULL DEFAULT '' CHECK (url = '' OR url ~ '^https?://'),
    secret_enc  bytea,           -- HTTPS: signing secret (X-Platform-Signature) and bearer token
    project_id  uuid REFERENCES projects ON DELETE CASCADE,  -- NULL = every project
    enabled     boolean NOT NULL DEFAULT true,
    cursor_at   timestamptz NOT NULL DEFAULT now(),          -- logs before this are exported
    last_run    timestamptz,
    last_status text NOT NULL DEFAULT '',
    last_error  text NOT NULL DEFAULT '',
    lines_total bigint NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CHECK ((kind = 'bucket') = (bucket_id IS NOT NULL)),
    CHECK (kind <> 'https' OR url <> '')
);
ALTER TABLE log_exports ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON log_exports
    USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON log_exports TO platform_api;
GRANT SELECT ON log_exports TO platform_agent;
GRANT UPDATE (cursor_at, last_run, last_status, last_error, lines_total) ON log_exports TO platform_agent;
