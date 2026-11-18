-- Observability: services can expose Prometheus metrics (scraped by the agent every
-- 30 seconds), and can turn on web analytics (a key for the page-view script).
ALTER TABLE services
    ADD COLUMN metrics_path  text NOT NULL DEFAULT '' CHECK (metrics_path = '' OR (metrics_path ~ '^/[A-Za-z0-9._~/-]*$' AND length(metrics_path) <= 200)),
    ADD COLUMN metrics_port  int  NOT NULL DEFAULT 0 CHECK (metrics_port BETWEEN 0 AND 65535),
    ADD COLUMN analytics_key text UNIQUE CHECK (analytics_key ~ '^[a-z0-9]{16,40}$');
GRANT UPDATE (analytics_key) ON services TO platform_api;

-- The page-view script's key identifies a service; the public collection endpoint
-- resolves it here (services are row-level protected) with the hosts the events may
-- come from.
CREATE FUNCTION analytics_site(p_key text)
RETURNS TABLE (org_id uuid, project_id uuid, service_id uuid, hosts text[], service_slug text, project_slug text, org_slug text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT s.org_id, s.project_id, s.id,
           coalesce(array_agg(DISTINCT dm.hostname) FILTER (WHERE dm.verified_at IS NOT NULL), '{}'), s.slug, p.slug, o.slug
    FROM services s JOIN orgs o ON o.id = s.org_id JOIN projects p ON p.id = s.project_id LEFT JOIN domains dm ON dm.service_id = s.id
    WHERE s.analytics_key = p_key AND NOT o.suspended
    GROUP BY s.id, p.slug, o.slug;
$$;
REVOKE ALL ON FUNCTION analytics_site(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION analytics_site(text) TO platform_api;
