-- Operator visibility without intrusion. The operator sees infrastructure and usage:
-- nodes, capacity, health of platform services, each organization's resources,
-- consumption and costs. These functions never return secrets, environment values,
-- log lines, database contents or files.

-- Nodes: one row per server running an agent, refreshed every 30 seconds.
CREATE TABLE nodes (
    id             text PRIMARY KEY,                -- NODE_ID (defaults to the Docker host name)
    name           text NOT NULL,
    cluster        text NOT NULL DEFAULT 'default' CHECK (cluster ~ '^[a-z0-9][a-z0-9-]{0,30}$'),
    role           text NOT NULL DEFAULT 'primary' CHECK (role IN ('primary', 'worker')),
    address        text NOT NULL DEFAULT '',        -- private (WireGuard) address, for workers
    cpus           int  NOT NULL DEFAULT 0,
    mem_mb         bigint NOT NULL DEFAULT 0,
    mem_avail_mb   bigint NOT NULL DEFAULT 0,
    load1          numeric(8,2) NOT NULL DEFAULT 0,
    disk_total_mb  bigint NOT NULL DEFAULT 0,
    disk_free_mb   bigint NOT NULL DEFAULT 0,
    containers     int  NOT NULL DEFAULT 0,
    docker_version text NOT NULL DEFAULT '',
    agent_version  text NOT NULL DEFAULT '',
    services       jsonb NOT NULL DEFAULT '{}',     -- platform service -> "ok" | error text
    cordoned       boolean NOT NULL DEFAULT false,  -- no new projects placed here
    created_at     timestamptz NOT NULL DEFAULT now(),
    last_seen      timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT, INSERT, UPDATE ON nodes TO platform_agent;

-- Latest measured use of each live service (from container stats, every minute).
CREATE TABLE service_stats (
    service_id   uuid PRIMARY KEY REFERENCES services ON DELETE CASCADE,
    org_id       uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    node_id      text NOT NULL DEFAULT '',
    cpu_pct      numeric(7,2) NOT NULL DEFAULT 0,   -- % of the service's CPU limit
    mem_mb       int NOT NULL DEFAULT 0,
    updated_at   timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE service_stats ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON service_stats
    USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid);
GRANT SELECT ON service_stats TO platform_api;
GRANT SELECT, INSERT, UPDATE, DELETE ON service_stats TO platform_agent;

CREATE FUNCTION operator_nodes()
RETURNS SETOF nodes LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT * FROM nodes ORDER BY cluster, role, id;
$$;

CREATE FUNCTION operator_set_node(p_id text, p_cordoned boolean, p_cluster text)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    UPDATE nodes SET cordoned = p_cordoned, cluster = coalesce(nullif(p_cluster, ''), cluster) WHERE id = p_id;
    RETURN FOUND;
END $$;

-- Per-organization usage, this month: allocation and measured use, storage, traffic,
-- builds. Names and sizes only.
CREATE FUNCTION operator_org_usage()
RETURNS TABLE (slug text, cpu_alloc int, mem_alloc int, cpu_used_pct numeric, mem_used_mb bigint,
               vcpu_hours numeric, gb_hours numeric, gb_out numeric, build_minutes numeric, disk_used_mb int)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT o.slug,
           coalesce((SELECT sum((d.spec->>'cpu_millis')::int) FROM deployments d WHERE d.org_id = o.id AND d.status = 'live'), 0)::int
             + coalesce((SELECT sum(db.cpu_millis) FROM databases db WHERE db.org_id = o.id AND db.status = 'running'), 0)::int,
           coalesce((SELECT sum((d.spec->>'memory_mb')::int) FROM deployments d WHERE d.org_id = o.id AND d.status = 'live'), 0)::int
             + coalesce((SELECT sum(db.memory_mb) FROM databases db WHERE db.org_id = o.id AND db.status = 'running'), 0)::int,
           coalesce((SELECT avg(st.cpu_pct) FROM service_stats st WHERE st.org_id = o.id AND st.updated_at > now() - interval '5 minutes'), 0),
           coalesce((SELECT sum(st.mem_mb) FROM service_stats st WHERE st.org_id = o.id AND st.updated_at > now() - interval '5 minutes'), 0),
           round(coalesce((SELECT sum(cpu_milli_minutes) FROM usage_hourly u WHERE u.org_id = o.id AND u.hour >= date_trunc('month', now())), 0) / 60000.0, 2),
           round(coalesce((SELECT sum(mem_mb_minutes) FROM usage_hourly u WHERE u.org_id = o.id AND u.hour >= date_trunc('month', now())), 0) / 61440.0, 2),
           round(coalesce((SELECT sum(egress_bytes) FROM usage_hourly u WHERE u.org_id = o.id AND u.hour >= date_trunc('month', now())), 0) / 1e9, 3),
           round(coalesce((SELECT sum(extract(epoch FROM finished_at - started_at)) FROM builds b
                           WHERE b.org_id = o.id AND b.finished_at >= date_trunc('month', now())), 0) / 60.0, 1),
           o.disk_used_mb
    FROM orgs o ORDER BY o.slug;
$$;

-- One organization's resources: services (no env, no secrets), databases (no
-- credentials), domains, open incidents (titles only).
CREATE FUNCTION operator_org_services(p_slug text)
RETURNS TABLE (project text, service text, image text, preview text, status text, cpu_millis int, memory_mb int,
               cpu_pct numeric, mem_mb int, node_id text, waf boolean, has_volume boolean, deployed_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT p.slug, coalesce(ps.slug, s.slug), s.image, coalesce(s.preview_name, ''),
           coalesce((SELECT d.status FROM deployments d WHERE d.service_id = s.id ORDER BY d.revision DESC LIMIT 1), 'not deployed'),
           s.cpu_millis, s.memory_mb, st.cpu_pct, st.mem_mb, coalesce(st.node_id, ''), s.waf_enabled, s.volume_path <> '',
           (SELECT max(d.updated_at) FROM deployments d WHERE d.service_id = s.id AND d.status = 'live')
    FROM services s JOIN projects p ON p.id = s.project_id JOIN orgs o ON o.id = s.org_id
    LEFT JOIN services ps ON ps.id = s.preview_of
    LEFT JOIN service_stats st ON st.service_id = s.id AND st.updated_at > now() - interval '5 minutes'
    WHERE o.slug = p_slug ORDER BY p.slug, coalesce(ps.slug, s.slug), s.preview_name NULLS FIRST;
$$;

CREATE FUNCTION operator_org_databases(p_slug text)
RETURNS TABLE (project text, name text, engine text, version text, status text, cpu_millis int, memory_mb int, created_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT p.slug, d.slug, d.engine, d.version, d.status, d.cpu_millis, d.memory_mb, d.created_at
    FROM databases d JOIN projects p ON p.id = d.project_id JOIN orgs o ON o.id = d.org_id
    WHERE o.slug = p_slug ORDER BY p.slug, d.slug;
$$;

CREATE FUNCTION operator_org_domains(p_slug text)
RETURNS TABLE (hostname text, service text, verified boolean)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT dm.hostname, s.slug, dm.verified_at IS NOT NULL
    FROM domains dm JOIN services s ON s.id = dm.service_id JOIN orgs o ON o.id = dm.org_id
    WHERE o.slug = p_slug ORDER BY dm.hostname;
$$;

CREATE FUNCTION operator_org_incidents(p_slug text)
RETURNS TABLE (kind text, status text, service text, started_at timestamptz, resolved_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT i.kind, i.status, coalesce(s.slug, ''), i.started_at, i.resolved_at
    FROM alert_incidents i JOIN orgs o ON o.id = i.org_id LEFT JOIN services s ON s.id = i.service_id
    WHERE o.slug = p_slug ORDER BY i.started_at DESC LIMIT 30;
$$;

DO $$
DECLARE f text;
BEGIN
    FOREACH f IN ARRAY ARRAY['operator_nodes()', 'operator_set_node(text, boolean, text)', 'operator_org_usage()',
        'operator_org_services(text)', 'operator_org_databases(text)', 'operator_org_domains(text)', 'operator_org_incidents(text)'] LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', f);
        EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO platform_api', f);
    END LOOP;
END $$;
