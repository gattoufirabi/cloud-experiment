-- Per-client backups of service volumes (restic). Each organization has its own
-- encrypted repository and its own key; the key is stored encrypted with the master key.

ALTER TABLE orgs ADD COLUMN backup_key_enc bytea;

ALTER TABLE services
    ADD COLUMN backup_enabled       boolean NOT NULL DEFAULT true,
    ADD COLUMN backup_hour          int     NOT NULL DEFAULT 3 CHECK (backup_hour BETWEEN 0 AND 23),
    ADD COLUMN backup_attempted_at  timestamptz;

CREATE TABLE backup_jobs (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    service_id    uuid NOT NULL REFERENCES services ON DELETE CASCADE,
    kind          text NOT NULL CHECK (kind IN ('backup', 'restore')),
    trigger       text NOT NULL DEFAULT 'manual' CHECK (trigger IN ('manual', 'schedule')),
    snapshot_id   text NOT NULL DEFAULT '' CHECK (snapshot_id ~ '^[0-9a-f]{0,64}$'),
    status        text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'done', 'error')),
    message       text NOT NULL DEFAULT '',
    requested_by  uuid REFERENCES users ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    finished_at   timestamptz
);
CREATE INDEX backup_jobs_status ON backup_jobs (status);
CREATE INDEX backup_jobs_service ON backup_jobs (service_id, created_at DESC);

CREATE TABLE backup_snapshots (
    org_id       uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    service_id   uuid NOT NULL REFERENCES services ON DELETE CASCADE,
    snapshot_id  text NOT NULL CHECK (snapshot_id ~ '^[0-9a-f]{64}$'),
    taken_at     timestamptz NOT NULL,
    size_bytes   bigint NOT NULL DEFAULT 0,
    files        bigint NOT NULL DEFAULT 0,
    tags         text NOT NULL DEFAULT '',
    PRIMARY KEY (service_id, snapshot_id)
);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['backup_jobs', 'backup_snapshots'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format(
            'CREATE POLICY tenant_isolation ON %I
                USING (org_id = nullif(current_setting(''app.org_id'', true), '''')::uuid)
                WITH CHECK (org_id = nullif(current_setting(''app.org_id'', true), '''')::uuid)', t);
    END LOOP;
END $$;

GRANT SELECT, INSERT ON backup_jobs TO platform_api;
GRANT SELECT ON backup_snapshots TO platform_api;

GRANT SELECT, INSERT, UPDATE ON backup_jobs TO platform_agent;
GRANT SELECT, INSERT, UPDATE, DELETE ON backup_snapshots TO platform_agent;
GRANT UPDATE (backup_attempted_at) ON services TO platform_agent;
GRANT UPDATE (backup_key_enc) ON orgs TO platform_agent;
