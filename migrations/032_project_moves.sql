-- Moving projects that keep data (persistent folders, databases) between nodes. Draining
-- a node can now move them too: the source node stops the project, snapshots each data
-- volume into the organization's backup repository (shared by the nodes), hands the
-- project over; the target node restores the volumes and starts it. While a move runs,
-- no node manages the project. A failed move gives the project back to its source node,
-- whose volumes were never changed.

CREATE TABLE project_moves (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
    org_id     uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    from_node  text NOT NULL,
    to_node    text NOT NULL,
    status     text NOT NULL DEFAULT 'snapshot' CHECK (status IN ('snapshot', 'restore', 'done', 'failed')),
    volumes    jsonb NOT NULL DEFAULT '[]',   -- [{"volume": "pv-...", "host": "move-...", "snapshot": "..."}]
    message    text NOT NULL DEFAULT '',
    attempts   int NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX project_moves_active ON project_moves (project_id) WHERE status IN ('snapshot', 'restore');

GRANT SELECT, UPDATE ON project_moves TO platform_agent;
GRANT UPDATE (node_id) ON projects TO platform_agent;

-- Drain: stateless projects are re-placed as before; with p_move_stateful, projects with
-- data get a move to the least loaded other node of their cluster.
DROP FUNCTION operator_drain_node(text);
CREATE FUNCTION operator_drain_node(p_id text, p_move_stateful boolean DEFAULT false)
RETURNS TABLE (moved bigint, kept bigint)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE m bigint; k bigint; r record; target text;
BEGIN
    UPDATE nodes SET cordoned = true WHERE id = p_id;
    WITH stateless AS (
        SELECT p.id FROM projects p WHERE p.node_id = p_id
          AND NOT EXISTS (SELECT 1 FROM databases db WHERE db.project_id = p.id)
          AND NOT EXISTS (SELECT 1 FROM services s WHERE s.project_id = p.id AND s.volume_path <> '')
    ), upd AS (UPDATE projects SET node_id = NULL WHERE id IN (SELECT id FROM stateless) RETURNING 1)
    SELECT count(*) INTO m FROM upd;
    IF p_move_stateful THEN
        FOR r IN SELECT p.id, p.org_id, o.cluster FROM projects p JOIN orgs o ON o.id = p.org_id
                 WHERE p.node_id = p_id AND NOT EXISTS (SELECT 1 FROM project_moves mv WHERE mv.project_id = p.id AND mv.status IN ('snapshot', 'restore'))
        LOOP
            SELECT n.id INTO target FROM nodes n
            WHERE n.id <> p_id AND n.last_seen > now() - interval '2 minutes' AND NOT n.cordoned AND (r.cluster = '' OR n.cluster = r.cluster)
            ORDER BY n.mem_mb * 0.8
                   - coalesce((SELECT sum(s.memory_mb * s.instances) FROM services s JOIN projects pp ON pp.id = s.project_id WHERE pp.node_id = n.id), 0)
                   - coalesce((SELECT sum(db.memory_mb) FROM databases db JOIN projects pp ON pp.id = db.project_id WHERE pp.node_id = n.id), 0)
                   DESC, n.id
            LIMIT 1;
            IF target IS NOT NULL THEN
                INSERT INTO project_moves (project_id, org_id, from_node, to_node) VALUES (r.id, r.org_id, p_id, target);
                m := m + 1;
            END IF;
        END LOOP;
    END IF;
    SELECT count(*) INTO k FROM projects p WHERE p.node_id = p_id
      AND NOT EXISTS (SELECT 1 FROM project_moves mv WHERE mv.project_id = p.id AND mv.status IN ('snapshot', 'restore'));
    PERFORM pg_notify('deployments', 'move');
    RETURN QUERY SELECT m, k;
END $$;
REVOKE ALL ON FUNCTION operator_drain_node(text, boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION operator_drain_node(text, boolean) TO platform_api;

-- The operator console lists moves (no tenant data: names and states only).
CREATE FUNCTION operator_moves() RETURNS TABLE (org text, project text, from_node text, to_node text, status text, message text, updated_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT o.slug, p.slug, m.from_node, m.to_node, m.status, m.message, m.updated_at
    FROM project_moves m JOIN projects p ON p.id = m.project_id JOIN orgs o ON o.id = m.org_id
    ORDER BY m.created_at DESC LIMIT 50;
$$;
REVOKE ALL ON FUNCTION operator_moves() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION operator_moves() TO platform_api;
