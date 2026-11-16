-- Several servers (Phase 4). A project is placed on one node with all its services and
-- databases, so project networks never span servers. The primary node runs the
-- control plane and the scheduler; each agent runs only the projects placed on its node.

ALTER TABLE projects ADD COLUMN node_id text REFERENCES nodes ON DELETE SET NULL;
CREATE INDEX projects_node ON projects (node_id);
GRANT UPDATE (node_id) ON projects TO platform_agent;

-- An organization may be pinned to a cluster (a group of nodes, e.g. a region).
ALTER TABLE orgs ADD COLUMN cluster text NOT NULL DEFAULT '' CHECK (cluster = '' OR cluster ~ '^[a-z0-9][a-z0-9-]{0,30}$');

-- Where the proxy reaches a live container on another node ("10.8.0.2:49153").
ALTER TABLE deployments ADD COLUMN upstream text NOT NULL DEFAULT '';

-- Disk usage measured on each node, summed per organization by the primary.
CREATE TABLE node_disk (
    node_id     text NOT NULL,
    org_id      uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    used_mb     int  NOT NULL DEFAULT 0,
    measured_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (node_id, org_id)
);
GRANT SELECT, INSERT, UPDATE, DELETE ON node_disk TO platform_agent;

-- Operator: load per node, drain, cluster pin.
CREATE FUNCTION operator_node_load()
RETURNS TABLE (node_id text, projects bigint, cpu_alloc bigint, mem_alloc bigint, stateful bigint)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT n.id,
           (SELECT count(*) FROM projects p WHERE p.node_id = n.id),
           coalesce((SELECT sum((d.spec->>'cpu_millis')::int) FROM deployments d JOIN services s ON s.id = d.service_id
                     JOIN projects p ON p.id = s.project_id WHERE p.node_id = n.id AND d.status = 'live'), 0)
             + coalesce((SELECT sum(db.cpu_millis) FROM databases db JOIN projects p ON p.id = db.project_id WHERE p.node_id = n.id), 0),
           coalesce((SELECT sum((d.spec->>'memory_mb')::int) FROM deployments d JOIN services s ON s.id = d.service_id
                     JOIN projects p ON p.id = s.project_id WHERE p.node_id = n.id AND d.status = 'live'), 0)
             + coalesce((SELECT sum(db.memory_mb) FROM databases db JOIN projects p ON p.id = db.project_id WHERE p.node_id = n.id), 0),
           (SELECT count(*) FROM projects p WHERE p.node_id = n.id AND (
                EXISTS (SELECT 1 FROM databases db WHERE db.project_id = p.id)
             OR EXISTS (SELECT 1 FROM services s WHERE s.project_id = p.id AND s.volume_path <> '')))
    FROM nodes n;
$$;

-- Drain: cordon the node and release its stateless projects so the scheduler places
-- them elsewhere. Projects with persistent data stay (volumes do not move by
-- themselves): the operator moves them with a backup and restore.
CREATE FUNCTION operator_drain_node(p_id text)
RETURNS TABLE (moved bigint, kept bigint)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE m bigint; k bigint;
BEGIN
    UPDATE nodes SET cordoned = true WHERE id = p_id;
    WITH stateless AS (
        SELECT p.id FROM projects p WHERE p.node_id = p_id
          AND NOT EXISTS (SELECT 1 FROM databases db WHERE db.project_id = p.id)
          AND NOT EXISTS (SELECT 1 FROM services s WHERE s.project_id = p.id AND s.volume_path <> '')
    ), upd AS (UPDATE projects SET node_id = NULL WHERE id IN (SELECT id FROM stateless) RETURNING 1)
    SELECT count(*) INTO m FROM upd;
    SELECT count(*) INTO k FROM projects WHERE node_id = p_id;
    PERFORM pg_notify('deployments', '');
    RETURN QUERY SELECT m, k;
END $$;

CREATE FUNCTION operator_set_org_cluster(p_slug text, p_cluster text)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    UPDATE orgs SET cluster = coalesce(p_cluster, '') WHERE slug = p_slug;
    RETURN FOUND;
END $$;

-- The operator org page shows where each project runs.
CREATE OR REPLACE FUNCTION operator_org_services(p_slug text)
RETURNS TABLE (project text, service text, image text, preview text, status text, cpu_millis int, memory_mb int,
               cpu_pct numeric, mem_mb int, node_id text, waf boolean, has_volume boolean, deployed_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT p.slug, coalesce(ps.slug, s.slug), s.image, coalesce(s.preview_name, ''),
           coalesce((SELECT d.status FROM deployments d WHERE d.service_id = s.id ORDER BY d.revision DESC LIMIT 1), 'not deployed'),
           s.cpu_millis, s.memory_mb, st.cpu_pct, st.mem_mb, coalesce(p.node_id, st.node_id, ''), s.waf_enabled, s.volume_path <> '',
           (SELECT max(d.updated_at) FROM deployments d WHERE d.service_id = s.id AND d.status = 'live')
    FROM services s JOIN projects p ON p.id = s.project_id JOIN orgs o ON o.id = s.org_id
    LEFT JOIN services ps ON ps.id = s.preview_of
    LEFT JOIN service_stats st ON st.service_id = s.id AND st.updated_at > now() - interval '5 minutes'
    WHERE o.slug = p_slug ORDER BY p.slug, coalesce(ps.slug, s.slug), s.preview_name NULLS FIRST;
$$;

CREATE FUNCTION operator_org_cluster(p_slug text)
RETURNS TABLE (cluster text) LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT cluster FROM orgs WHERE slug = p_slug;
$$;

DO $$
DECLARE f text;
BEGIN
    FOREACH f IN ARRAY ARRAY['operator_node_load()', 'operator_drain_node(text)', 'operator_set_org_cluster(text, text)',
        'operator_org_cluster(text)'] LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', f);
        EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO platform_api', f);
    END LOOP;
END $$;
