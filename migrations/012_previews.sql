-- Preview environments: a temporary copy of a service (same settings and secrets,
-- another image) at <name>.<service>-<project>-<org>.<apps domain>, deleted by the
-- agent when it expires. Stored as a hidden service row so deploys, logs, metrics,
-- metering and cleanup work unchanged.
ALTER TABLE services
    ADD COLUMN preview_of   uuid REFERENCES services ON DELETE CASCADE,
    ADD COLUMN preview_name text CHECK (preview_name ~ '^[a-z0-9][a-z0-9-]{0,19}$'),
    ADD COLUMN expires_at   timestamptz,
    ADD CONSTRAINT preview_fields CHECK ((preview_of IS NULL) = (preview_name IS NULL) AND (preview_of IS NULL) = (expires_at IS NULL)),
    ADD CONSTRAINT preview_unique UNIQUE (preview_of, preview_name);
CREATE INDEX services_expiry ON services (expires_at) WHERE expires_at IS NOT NULL;

-- TLS: also approve preview hosts.
CREATE OR REPLACE FUNCTION host_is_served(p_host text, p_apps_domain text)
RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT EXISTS (
        SELECT 1 FROM deployments d
        JOIN services s ON s.id = d.service_id
        LEFT JOIN services ps ON ps.id = s.preview_of
        JOIN projects p ON p.id = s.project_id
        JOIN orgs o ON o.id = d.org_id
        WHERE d.status = 'live' AND NOT o.suspended
          AND CASE WHEN s.preview_of IS NULL
                   THEN s.slug || '-' || p.slug || '-' || o.slug || '.' || p_apps_domain
                   ELSE s.preview_name || '.' || ps.slug || '-' || p.slug || '-' || o.slug || '.' || p_apps_domain
              END = p_host
    ) OR EXISTS (
        SELECT 1 FROM domains dm JOIN orgs o ON o.id = dm.org_id
        WHERE dm.hostname = p_host AND dm.verified_at IS NOT NULL AND NOT o.suspended
    );
$$;

-- Public status pages never list previews.
CREATE OR REPLACE FUNCTION status_page_services(p_org text, p_project text)
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
    JOIN services s ON s.project_id = p.id AND s.preview_of IS NULL
    LEFT JOIN service_uptime u ON u.service_id = s.id AND u.hour > now() - interval '30 days'
    WHERE o.slug = p_org AND p.slug = p_project AND p.status_public AND NOT o.suspended
    GROUP BY s.id, s.slug, (u.hour AT TIME ZONE 'UTC')::date
    ORDER BY s.slug, 3;
$$;

-- The agent deletes expired previews (their deployments and secrets cascade).
GRANT DELETE ON services TO platform_agent;
