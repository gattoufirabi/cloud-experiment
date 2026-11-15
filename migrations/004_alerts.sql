-- Alerts: per-organization notification channels, incidents and a delivery queue.

CREATE TABLE alert_channels (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id            uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    name              text NOT NULL CHECK (length(name) BETWEEN 1 AND 60),
    kind              text NOT NULL CHECK (kind IN ('webhook', 'ntfy')),
    url               text NOT NULL CHECK (url ~ '^https?://' AND length(url) <= 500),
    topic             text NOT NULL DEFAULT '' CHECK (topic ~ '^[A-Za-z0-9_-]{0,64}$'),
    secret_enc        bytea,  -- webhook signing secret or ntfy access token, encrypted
    events            text[] NOT NULL DEFAULT
                      '{service_down,deploy_failed,backup_failed,critical_vulns,cpu_high,memory_high}',
    enabled           boolean NOT NULL DEFAULT true,
    last_status       text NOT NULL DEFAULT '',
    last_delivery_at  timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE alert_incidents (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    service_id   uuid REFERENCES services ON DELETE CASCADE,
    kind         text NOT NULL CHECK (kind IN ('service_down', 'cpu_high', 'memory_high', 'deploy_failed',
                                               'backup_failed', 'critical_vulns', 'test')),
    status       text NOT NULL CHECK (status IN ('firing', 'resolved', 'event')),
    summary      text NOT NULL,
    started_at   timestamptz NOT NULL DEFAULT now(),
    resolved_at  timestamptz
);
-- At most one open incident per service and kind: repeated failures do not spam.
CREATE UNIQUE INDEX alert_incidents_open ON alert_incidents (service_id, kind) WHERE status = 'firing';
CREATE INDEX alert_incidents_org ON alert_incidents (org_id, started_at DESC);

CREATE TABLE alert_notifications (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id           uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    incident_id      uuid NOT NULL REFERENCES alert_incidents ON DELETE CASCADE,
    channel_id       uuid NOT NULL REFERENCES alert_channels ON DELETE CASCADE,
    transition       text NOT NULL CHECK (transition IN ('firing', 'resolved', 'event')),
    status           text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'failed')),
    attempts         int  NOT NULL DEFAULT 0,
    next_attempt_at  timestamptz NOT NULL DEFAULT now(),
    last_error       text NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now(),
    sent_at          timestamptz
);
CREATE INDEX alert_notifications_due ON alert_notifications (status, next_attempt_at);

-- Thresholds in % of the service's own limits; NULL = no alert.
ALTER TABLE services
    ADD COLUMN alert_cpu_pct int CHECK (alert_cpu_pct BETWEEN 1 AND 100),
    ADD COLUMN alert_mem_pct int CHECK (alert_mem_pct BETWEEN 1 AND 100);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['alert_channels', 'alert_incidents', 'alert_notifications'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format(
            'CREATE POLICY tenant_isolation ON %I
                USING (org_id = nullif(current_setting(''app.org_id'', true), '''')::uuid)
                WITH CHECK (org_id = nullif(current_setting(''app.org_id'', true), '''')::uuid)', t);
    END LOOP;
END $$;

GRANT SELECT, INSERT, UPDATE, DELETE ON alert_channels TO platform_api;
GRANT SELECT, INSERT ON alert_incidents, alert_notifications TO platform_api;

GRANT SELECT ON alert_channels TO platform_agent;
GRANT UPDATE (last_status, last_delivery_at) ON alert_channels TO platform_agent;
GRANT SELECT, INSERT, UPDATE ON alert_incidents, alert_notifications TO platform_agent;
