-- Admin panel: platform settings editable from the browser (NULL = use the server's
-- environment value), plan editing, and cross-tenant views for operators (metadata
-- only, through SECURITY DEFINER functions).

CREATE TABLE platform_settings (
    id                     int PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    signup_enabled         boolean,
    signup_daily_limit     int CHECK (signup_daily_limit >= 0),
    signup_trial_days      int CHECK (signup_trial_days BETWEEN 1 AND 365),
    signup_allowed_domains text[],
    signup_blocked_domains text[],
    banner_message         text NOT NULL DEFAULT '' CHECK (length(banner_message) <= 300),
    banner_level           text NOT NULL DEFAULT 'info' CHECK (banner_level IN ('info', 'warning')),
    deploys_paused         boolean NOT NULL DEFAULT false,
    updated_at             timestamptz NOT NULL DEFAULT now()
);
INSERT INTO platform_settings DEFAULT VALUES;
GRANT SELECT, UPDATE ON platform_settings TO platform_api;
GRANT SELECT ON platform_settings TO platform_agent;

-- Plans are platform configuration, not tenant data.
GRANT INSERT, UPDATE, DELETE ON plans TO platform_api;

-- Every resource of every organization: names, sizes and states, never contents.
CREATE FUNCTION operator_resources()
RETURNS TABLE (kind text, org_slug text, project text, name text, detail text, status text, created_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT 'service', o.slug, p.slug, s.slug,
           (CASE WHEN s.worker THEN 'worker · ' ELSE 'web · ' END) || s.instances || ' × ' || s.cpu_millis || 'm/' || s.memory_mb || 'MB',
           coalesce((SELECT d.status FROM deployments d WHERE d.service_id = s.id ORDER BY d.revision DESC LIMIT 1), 'new'), s.created_at
    FROM services s JOIN projects p ON p.id = s.project_id JOIN orgs o ON o.id = s.org_id WHERE s.preview_of IS NULL
    UNION ALL
    SELECT 'database', o.slug, p.slug, d.slug, d.engine || ' ' || d.version || ' · ' || d.memory_mb || 'MB', d.status, d.created_at
    FROM databases d JOIN projects p ON p.id = d.project_id JOIN orgs o ON o.id = d.org_id
    UNION ALL
    SELECT 'function', o.slug, p.slug, f.slug, f.runtime || ' · ' || f.memory_mb || 'MB', CASE WHEN f.url_enabled THEN 'url' ELSE 'private' END, f.created_at
    FROM functions f JOIN projects p ON p.id = f.project_id JOIN orgs o ON o.id = f.org_id
    UNION ALL
    SELECT 'bucket', o.slug, '', b.name, b.quota_mb || 'MB quota', CASE WHEN b.website THEN 'website' ELSE 'private' END, b.created_at
    FROM buckets b JOIN orgs o ON o.id = b.org_id
    UNION ALL
    SELECT 'domain', o.slug, p.slug, dm.hostname, s.slug, CASE WHEN dm.verified_at IS NULL THEN 'pending' ELSE 'verified' END, dm.created_at
    FROM domains dm JOIN services s ON s.id = dm.service_id JOIN projects p ON p.id = s.project_id JOIN orgs o ON o.id = dm.org_id
    UNION ALL
    SELECT 'queue', o.slug, '', q.name, '', '', q.created_at FROM queues q JOIN orgs o ON o.id = q.org_id
    UNION ALL
    SELECT 'email domain', o.slug, '', e.domain, '', e.status, e.created_at FROM email_domains e JOIN orgs o ON o.id = e.org_id
    ORDER BY 7 DESC
    LIMIT 2000;
$$;

-- Platform health: queues of work and recent failures, as counters.
CREATE FUNCTION operator_health()
RETURNS TABLE (name text, value bigint, bad boolean)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT 'Deployments waiting', count(*), count(*) > 0 AND min(created_at) < now() - interval '5 minutes' FROM deployments WHERE status = 'pending'
    UNION ALL SELECT 'Deployments failed, 24 h', count(*), count(*) > 0 FROM deployments WHERE status = 'failed' AND updated_at > now() - interval '1 day'
    UNION ALL SELECT 'Builds queued or running', count(*), count(*) > 0 AND min(created_at) < now() - interval '30 minutes' FROM builds WHERE status IN ('queued', 'running')
    UNION ALL SELECT 'Builds failed, 24 h', count(*), false FROM builds WHERE status = 'failed' AND created_at > now() - interval '1 day'
    UNION ALL SELECT 'Backups failed, 24 h', count(*), count(*) > 0 FROM backup_jobs WHERE status = 'error' AND created_at > now() - interval '1 day'
    UNION ALL SELECT 'Backups done, 24 h', count(*), false FROM backup_jobs WHERE status = 'done' AND created_at > now() - interval '1 day'
    UNION ALL SELECT 'Emails queued', count(*), count(*) > 0 AND min(created_at) < now() - interval '15 minutes' FROM email_messages WHERE status IN ('queued', 'sending')
    UNION ALL SELECT 'Emails failed, 24 h', count(*), count(*) > 0 FROM email_messages WHERE status = 'failed' AND created_at > now() - interval '1 day'
    UNION ALL SELECT 'Queue messages stored', count(*), false FROM queue_messages
    UNION ALL SELECT 'Function errors, 24 h', count(*), false FROM function_invocations WHERE status = 'error' AND created_at > now() - interval '1 day'
    UNION ALL SELECT 'Open incidents', count(*), count(*) > 0 FROM alert_incidents WHERE status = 'firing'
    UNION ALL SELECT 'Suspended organizations', count(*), false FROM orgs WHERE suspended;
$$;

-- Recent failures across the platform (system messages only).
CREATE FUNCTION operator_failures()
RETURNS TABLE (at timestamptz, kind text, org_slug text, what text, message text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT * FROM (
        SELECT d.updated_at, 'deployment', o.slug, s.slug || ' r' || d.revision, left(d.message, 300)
        FROM deployments d JOIN services s ON s.id = d.service_id JOIN orgs o ON o.id = d.org_id
        WHERE d.status = 'failed' AND d.updated_at > now() - interval '1 day'
        UNION ALL
        SELECT j.created_at, 'backup ' || j.kind, o.slug, coalesce(j.database_id::text, j.service_id::text, ''), left(j.message, 300)
        FROM backup_jobs j JOIN orgs o ON o.id = j.org_id WHERE j.status = 'error' AND j.created_at > now() - interval '1 day'
        UNION ALL
        SELECT b.created_at, 'build', o.slug, s.slug || ' #' || b.number, ''
        FROM builds b JOIN services s ON s.id = b.service_id JOIN orgs o ON o.id = s.org_id WHERE b.status = 'failed' AND b.created_at > now() - interval '1 day'
    ) x ORDER BY 1 DESC LIMIT 100;
$$;

DO $$
DECLARE f text;
BEGIN
    FOREACH f IN ARRAY ARRAY['operator_resources()', 'operator_health()', 'operator_failures()'] LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', f);
        EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO platform_api', f);
    END LOOP;
END $$;
