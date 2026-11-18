-- Budgets: a monthly spending limit for the whole organization or one project. The agent
-- compares the month-to-date cost (and the forecast for the whole month) with the limit
-- every hour and notifies the alert channels when a threshold is first crossed.
CREATE TABLE budgets (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    project_id    uuid REFERENCES projects ON DELETE CASCADE,          -- NULL: the whole organization
    limit_cents   bigint NOT NULL CHECK (limit_cents > 0),
    thresholds    int[]  NOT NULL DEFAULT '{50,80,100}'
                  CHECK (cardinality(thresholds) BETWEEN 1 AND 5 AND 1 <= ALL (thresholds) AND 500 >= ALL (thresholds)),
    forecast      boolean NOT NULL DEFAULT true,                        -- also warn when the forecast passes the limit
    month         date,                                                 -- month the fields below refer to
    level         int    NOT NULL DEFAULT 0,                            -- highest threshold already notified that month
    forecast_sent boolean NOT NULL DEFAULT false,
    spent_cents   bigint NOT NULL DEFAULT 0,                            -- last evaluation (shown on the page)
    forecast_cents bigint NOT NULL DEFAULT 0,
    checked_at    timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX budgets_scope ON budgets (org_id, project_id) NULLS NOT DISTINCT;
ALTER TABLE budgets ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON budgets
    USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid);

ALTER TABLE alert_incidents ADD COLUMN budget_id uuid REFERENCES budgets ON DELETE CASCADE;
CREATE UNIQUE INDEX alert_incidents_open_budget ON alert_incidents (budget_id) WHERE status = 'firing';
ALTER TABLE alert_incidents DROP CONSTRAINT alert_incidents_kind_check;
ALTER TABLE alert_incidents ADD CONSTRAINT alert_incidents_kind_check
    CHECK (kind IN ('service_down', 'cpu_high', 'memory_high', 'deploy_failed', 'backup_failed',
                    'critical_vulns', 'test', 'disk_quota', 'job_failed', 'metric_alarm', 'budget'));
ALTER TABLE alert_channels ALTER COLUMN events SET DEFAULT
    '{service_down,deploy_failed,backup_failed,critical_vulns,cpu_high,memory_high,job_failed,metric_alarm,budget}';
UPDATE alert_channels SET events = array_append(events, 'budget') WHERE NOT ('budget' = ANY (events));

GRANT SELECT, INSERT, UPDATE, DELETE ON budgets TO platform_api;
GRANT SELECT ON budgets TO platform_agent;
GRANT UPDATE (month, level, forecast_sent, spent_cents, forecast_cents, checked_at) ON budgets TO platform_agent;
