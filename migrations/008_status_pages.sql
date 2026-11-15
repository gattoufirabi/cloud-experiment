-- Public status pages: hourly uptime per service (from the agent's health probes) and an
-- opt-in flag per project. Public pages read only through the functions below, which
-- return nothing for projects that did not opt in.

ALTER TABLE projects ADD COLUMN status_public boolean NOT NULL DEFAULT false;

CREATE TABLE service_uptime (
    org_id      uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    service_id  uuid NOT NULL REFERENCES services ON DELETE CASCADE,
    hour        timestamptz NOT NULL,
    ok          int NOT NULL DEFAULT 0,
    total       int NOT NULL DEFAULT 0,
    PRIMARY KEY (service_id, hour)
);
ALTER TABLE service_uptime ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON service_uptime
    USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid);

-- Project header: NULL when the page is not public.
CREATE FUNCTION status_page_project(p_org text, p_project text)
RETURNS TABLE (org_name text, project_name text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT o.name, p.name FROM projects p JOIN orgs o ON o.id = p.org_id
    WHERE o.slug = p_org AND p.slug = p_project AND p.status_public AND NOT o.suspended;
$$;

-- One row per service and day (last 30 days) with probe counts, plus the current state.
CREATE FUNCTION status_page_services(p_org text, p_project text)
RETURNS TABLE (service text, state text, day date, ok bigint, total bigint)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT s.slug,
           CASE
               WHEN EXISTS (SELECT 1 FROM alert_incidents i WHERE i.service_id = s.id AND i.kind = 'service_down' AND i.status = 'firing') THEN 'down'
               WHEN EXISTS (SELECT 1 FROM deployments d WHERE d.service_id = s.id AND d.status = 'live') THEN 'up'
               ELSE 'none'
           END,
           (u.hour AT TIME ZONE 'UTC')::date, coalesce(sum(u.ok), 0), coalesce(sum(u.total), 0)
    FROM projects p JOIN orgs o ON o.id = p.org_id
    JOIN services s ON s.project_id = p.id
    LEFT JOIN service_uptime u ON u.service_id = s.id AND u.hour > now() - interval '30 days'
    WHERE o.slug = p_org AND p.slug = p_project AND p.status_public AND NOT o.suspended
    GROUP BY s.id, s.slug, (u.hour AT TIME ZONE 'UTC')::date
    ORDER BY s.slug, 3;
$$;

-- Outages of the last 14 days: times only, never internal error messages.
CREATE FUNCTION status_page_incidents(p_org text, p_project text)
RETURNS TABLE (service text, started_at timestamptz, resolved_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT s.slug, i.started_at, i.resolved_at
    FROM projects p JOIN orgs o ON o.id = p.org_id
    JOIN services s ON s.project_id = p.id
    JOIN alert_incidents i ON i.service_id = s.id AND i.kind = 'service_down'
    WHERE o.slug = p_org AND p.slug = p_project AND p.status_public AND NOT o.suspended
      AND i.started_at > now() - interval '14 days'
    ORDER BY i.started_at DESC LIMIT 20;
$$;

REVOKE ALL ON FUNCTION status_page_project(text, text), status_page_services(text, text), status_page_incidents(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION status_page_project(text, text), status_page_services(text, text), status_page_incidents(text, text) TO platform_api;
GRANT SELECT ON service_uptime TO platform_api;
GRANT SELECT, INSERT, UPDATE, DELETE ON service_uptime TO platform_agent;
