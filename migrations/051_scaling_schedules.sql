-- Scheduled scaling: at times given as cron expressions (UTC), a service is set to a number
-- of instances (for an autoscaled service, the autoscaling minimum is raised or lowered to
-- it). The primary agent applies them every minute.
CREATE TABLE scaling_schedules (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    service_id uuid NOT NULL REFERENCES services ON DELETE CASCADE,
    cron       text NOT NULL CHECK (length(cron) BETWEEN 9 AND 100),
    instances  int  NOT NULL CHECK (instances BETWEEN 1 AND 10),
    label      text NOT NULL DEFAULT '' CHECK (length(label) <= 60),
    last_run   timestamptz,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (service_id, cron)
);
ALTER TABLE scaling_schedules ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON scaling_schedules
    USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON scaling_schedules TO platform_api;
GRANT SELECT ON scaling_schedules TO platform_agent;
GRANT UPDATE (last_run, last_error) ON scaling_schedules TO platform_agent;

-- Node failover: when a node stops reporting, the primary cordons it and moves its
-- stateless projects to the other nodes (projects with data wait for the operator).
ALTER TABLE nodes ADD COLUMN failed_over_at timestamptz;
