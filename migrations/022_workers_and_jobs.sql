-- Stage 4: background workers and scheduled jobs.
--
-- A worker is a service with no public address (a queue consumer, a bot): it is not
-- routed by the proxy and its health is "the container keeps running". Marked in the
-- deployment spec ("worker": true) and on the service.
--
-- A scheduled job runs a command on a cron schedule in a fresh container made from a
-- service's current deployment: same image, environment, secrets, project variables,
-- network and resource limits. Runs are recorded with their exit code and log.

ALTER TABLE services ADD COLUMN worker boolean NOT NULL DEFAULT false;

-- The node that runs a live deployment (set on promotion). Replaces guessing it from
-- the upstream address, which a worker service does not have.
ALTER TABLE deployments ADD COLUMN node_id text;

CREATE TABLE jobs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    service_id      uuid NOT NULL REFERENCES services ON DELETE CASCADE,
    slug            text NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,29}$'),
    schedule        text NOT NULL CHECK (length(schedule) BETWEEN 5 AND 100),
    command         text NOT NULL CHECK (length(command) BETWEEN 1 AND 2000),
    timeout_seconds int  NOT NULL DEFAULT 600 CHECK (timeout_seconds BETWEEN 10 AND 3600),
    enabled         boolean NOT NULL DEFAULT true,
    next_run_at     timestamptz,
    run_requested   boolean NOT NULL DEFAULT false,  -- "Run now"
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (service_id, slug)
);
CREATE INDEX jobs_due ON jobs (next_run_at) WHERE enabled;

CREATE TABLE job_runs (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    job_id       uuid NOT NULL REFERENCES jobs ON DELETE CASCADE,
    trigger      text NOT NULL CHECK (trigger IN ('schedule', 'manual')),
    status       text NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'succeeded', 'failed', 'timed_out', 'skipped')),
    exit_code    int,
    log          text NOT NULL DEFAULT '',
    node_id      text,
    container_id text,
    cpu_millis   int NOT NULL DEFAULT 0,
    memory_mb    int NOT NULL DEFAULT 0,
    started_at   timestamptz NOT NULL DEFAULT now(),
    finished_at  timestamptz
);
CREATE INDEX job_runs_job ON job_runs (job_id, started_at DESC);
CREATE INDEX job_runs_running ON job_runs (status) WHERE status = 'running';

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['jobs', 'job_runs'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format(
            'CREATE POLICY tenant_isolation ON %I
                USING (org_id = nullif(current_setting(''app.org_id'', true), '''')::uuid)
                WITH CHECK (org_id = nullif(current_setting(''app.org_id'', true), '''')::uuid)', t);
    END LOOP;
END $$;

GRANT SELECT, INSERT, UPDATE, DELETE ON jobs TO platform_api;
GRANT SELECT, DELETE ON job_runs TO platform_api;
GRANT SELECT, UPDATE ON jobs TO platform_agent;
GRANT SELECT, INSERT, UPDATE, DELETE ON job_runs TO platform_agent;
GRANT UPDATE (node_id) ON deployments TO platform_agent;

-- A failed job is an alert event, opt-in per channel like the others.
ALTER TABLE alert_incidents DROP CONSTRAINT alert_incidents_kind_check;
ALTER TABLE alert_incidents ADD CONSTRAINT alert_incidents_kind_check
    CHECK (kind IN ('service_down', 'cpu_high', 'memory_high', 'deploy_failed', 'backup_failed',
                    'critical_vulns', 'test', 'disk_quota', 'job_failed'));
ALTER TABLE alert_channels ALTER COLUMN events SET DEFAULT
    '{service_down,deploy_failed,backup_failed,critical_vulns,cpu_high,memory_high,job_failed}';
UPDATE alert_channels SET events = events || '{job_failed}' WHERE NOT ('job_failed' = ANY (events));
