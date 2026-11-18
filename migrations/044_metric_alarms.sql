-- Metric alarms: a PromQL query compared with a threshold, evaluated every minute by
-- the agent within the organization's own series. Breaching for the set number of
-- minutes opens an incident and notifies the channels; recovering resolves it.
CREATE TABLE alarm_rules (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    name          text NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    query         text NOT NULL CHECK (length(query) BETWEEN 1 AND 1000),
    op            text NOT NULL CHECK (op IN ('>', '>=', '<', '<=')),
    threshold     double precision NOT NULL,
    for_minutes   int  NOT NULL DEFAULT 5 CHECK (for_minutes BETWEEN 0 AND 120),
    enabled       boolean NOT NULL DEFAULT true,
    state         text NOT NULL DEFAULT 'ok' CHECK (state IN ('ok', 'pending', 'firing', 'nodata')),
    pending_since timestamptz,
    last_value    double precision,
    last_eval     timestamptz,
    last_error    text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);
ALTER TABLE alarm_rules ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON alarm_rules
    USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid);

ALTER TABLE alert_incidents ADD COLUMN alarm_id uuid REFERENCES alarm_rules ON DELETE CASCADE;
CREATE UNIQUE INDEX alert_incidents_open_alarm ON alert_incidents (alarm_id) WHERE status = 'firing';
ALTER TABLE alert_incidents DROP CONSTRAINT alert_incidents_kind_check;
ALTER TABLE alert_incidents ADD CONSTRAINT alert_incidents_kind_check
    CHECK (kind IN ('service_down', 'cpu_high', 'memory_high', 'deploy_failed', 'backup_failed',
                    'critical_vulns', 'test', 'disk_quota', 'job_failed', 'metric_alarm'));
ALTER TABLE alert_channels ALTER COLUMN events SET DEFAULT
    '{service_down,deploy_failed,backup_failed,critical_vulns,cpu_high,memory_high,job_failed,metric_alarm}';
UPDATE alert_channels SET events = array_append(events, 'metric_alarm') WHERE NOT ('metric_alarm' = ANY (events));

ALTER TABLE plans ADD COLUMN max_alarms int NOT NULL DEFAULT 20 CHECK (max_alarms >= 0);
UPDATE plans SET max_alarms = 5 WHERE id = 'trial';
UPDATE plans SET max_alarms = 100 WHERE id = 'pro';

GRANT SELECT, INSERT, UPDATE, DELETE ON alarm_rules TO platform_api;
GRANT SELECT ON alarm_rules TO platform_agent;
GRANT UPDATE (state, pending_since, last_value, last_eval, last_error) ON alarm_rules TO platform_agent;
