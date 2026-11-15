-- Managed PostgreSQL: one container per database, on its project's private network,
-- with its data in a volume and consistent dump-based backups.

ALTER TABLE plans ADD COLUMN max_databases int NOT NULL DEFAULT 1;
UPDATE plans SET max_databases = 5 WHERE id = 'pro';

CREATE TABLE databases (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id               uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    project_id           uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
    slug                 text NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,19}$'),
    engine               text NOT NULL DEFAULT 'postgres' CHECK (engine = 'postgres'),
    version              text NOT NULL DEFAULT '17' CHECK (version IN ('16', '17')),
    cpu_millis           int  NOT NULL DEFAULT 250 CHECK (cpu_millis BETWEEN 50 AND 4000),
    memory_mb            int  NOT NULL DEFAULT 256 CHECK (memory_mb BETWEEN 128 AND 8192),
    db_name              text NOT NULL DEFAULT 'app',
    username             text NOT NULL DEFAULT 'app',
    password_enc         bytea NOT NULL,
    status               text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'failed')),
    message              text NOT NULL DEFAULT '',
    container_id         text,
    backup_enabled       boolean NOT NULL DEFAULT true,
    backup_hour          int NOT NULL DEFAULT 3 CHECK (backup_hour BETWEEN 0 AND 23),
    backup_attempted_at  timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, slug)
);
ALTER TABLE databases ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON databases
    USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid);

-- Backup jobs and snapshots now target either a service volume or a database.
ALTER TABLE backup_jobs
    ALTER COLUMN service_id DROP NOT NULL,
    ADD COLUMN database_id uuid REFERENCES databases ON DELETE CASCADE,
    ADD CONSTRAINT backup_jobs_one_target CHECK ((service_id IS NULL) <> (database_id IS NULL));

ALTER TABLE backup_snapshots
    DROP CONSTRAINT backup_snapshots_pkey,
    ALTER COLUMN service_id DROP NOT NULL,
    ADD COLUMN database_id uuid REFERENCES databases ON DELETE CASCADE,
    ADD PRIMARY KEY (org_id, snapshot_id),
    ADD CONSTRAINT backup_snapshots_one_target CHECK ((service_id IS NULL) <> (database_id IS NULL));
CREATE INDEX backup_snapshots_service ON backup_snapshots (service_id);
CREATE INDEX backup_snapshots_database ON backup_snapshots (database_id);

-- A new service or database gets its first nightly backup at the next scheduled hour,
-- not immediately (which also raced with "Back up now").
ALTER TABLE services ALTER COLUMN backup_attempted_at SET DEFAULT now();
ALTER TABLE databases ALTER COLUMN backup_attempted_at SET DEFAULT now();

GRANT SELECT, INSERT, UPDATE, DELETE ON databases TO platform_api;
GRANT SELECT ON databases TO platform_agent;
GRANT UPDATE (status, message, container_id, backup_attempted_at) ON databases TO platform_agent;
