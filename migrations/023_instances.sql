-- Stage 5: several instances per service. Each instance is the same container (same
-- image, settings and limits); the proxy spreads requests over the healthy ones and
-- updates replace all instances at once with no downtime. A service with a persistent
-- folder keeps a single instance (the folder cannot be shared safely).

ALTER TABLE services ADD COLUMN instances int NOT NULL DEFAULT 1 CHECK (instances BETWEEN 1 AND 10);

-- Every container of a live deployment: [{"id": "...", "name": "...", "upstream": "..."}].
-- container_id / container_name / upstream keep describing the first instance.
ALTER TABLE deployments ADD COLUMN instances jsonb NOT NULL DEFAULT '[]';

-- Most instances a service may have on each plan (CPU and memory are counted per instance).
ALTER TABLE plans ADD COLUMN max_instances int NOT NULL DEFAULT 5 CHECK (max_instances BETWEEN 1 AND 10);
UPDATE plans SET max_instances = 1 WHERE id = 'trial';

-- Allocation per node counts every instance.
CREATE OR REPLACE FUNCTION operator_node_load()
RETURNS TABLE (node_id text, projects bigint, cpu_alloc bigint, mem_alloc bigint, stateful bigint)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT n.id,
           (SELECT count(*) FROM projects p WHERE p.node_id = n.id),
           coalesce((SELECT sum((d.spec->>'cpu_millis')::int * greatest(1, coalesce((d.spec->>'instances')::int, 1)))
                     FROM deployments d JOIN services s ON s.id = d.service_id
                     JOIN projects p ON p.id = s.project_id WHERE p.node_id = n.id AND d.status = 'live'), 0)
             + coalesce((SELECT sum(db.cpu_millis) FROM databases db JOIN projects p ON p.id = db.project_id WHERE p.node_id = n.id), 0),
           coalesce((SELECT sum((d.spec->>'memory_mb')::int * greatest(1, coalesce((d.spec->>'instances')::int, 1)))
                     FROM deployments d JOIN services s ON s.id = d.service_id
                     JOIN projects p ON p.id = s.project_id WHERE p.node_id = n.id AND d.status = 'live'), 0)
             + coalesce((SELECT sum(db.memory_mb) FROM databases db JOIN projects p ON p.id = db.project_id WHERE p.node_id = n.id), 0),
           (SELECT count(*) FROM projects p WHERE p.node_id = n.id AND (
                EXISTS (SELECT 1 FROM databases db WHERE db.project_id = p.id)
             OR EXISTS (SELECT 1 FROM services s WHERE s.project_id = p.id AND s.volume_path <> '')))
    FROM nodes n;
$$;
