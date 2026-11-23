-- Customer-owned servers ("bring your own server"). An organization attaches its own
-- machine; a node agent on it runs only that organization's projects placed there.
-- That agent is NOT trusted: it never connects to PostgreSQL and never holds the master
-- key. It talks to the API over HTTPS with its own node token (only its hash is stored),
-- and the API hands it only what its projects need.

ALTER TABLE nodes DROP CONSTRAINT IF EXISTS nodes_role_check;
ALTER TABLE nodes ADD CONSTRAINT nodes_role_check CHECK (role IN ('primary', 'worker', 'customer'));
ALTER TABLE nodes ADD COLUMN org_id     uuid REFERENCES orgs ON DELETE CASCADE;
ALTER TABLE nodes ADD COLUMN token_hash bytea UNIQUE;
ALTER TABLE nodes ADD COLUMN os         text NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN arch       text NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN created_by uuid REFERENCES users ON DELETE SET NULL;
-- The server pulls the organization's images with its own registry token (pull only),
-- revoked with the server.
ALTER TABLE nodes ADD COLUMN registry_token_id uuid REFERENCES registry_tokens ON DELETE SET NULL;
-- A customer server always belongs to an organization, and platform nodes never do.
ALTER TABLE nodes ADD CONSTRAINT nodes_customer_org CHECK ((role = 'customer') = (org_id IS NOT NULL));
ALTER TABLE nodes ADD CONSTRAINT nodes_customer_id CHECK (role <> 'customer' OR id ~ '^c-[a-z0-9]{10}$');
CREATE INDEX nodes_org ON nodes (org_id) WHERE org_id IS NOT NULL;

-- The API sees only its organization's servers; platform nodes stay invisible to it
-- (the operator panel reads them through SECURITY DEFINER functions).
ALTER TABLE nodes ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON nodes
    USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid AND role = 'customer');
GRANT SELECT, INSERT, UPDATE, DELETE ON nodes TO platform_api;

-- One-time join tokens, shown once in the console, valid one hour.
CREATE TABLE server_join_tokens (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    name       text NOT NULL CHECK (length(name) BETWEEN 1 AND 60),
    token_hash bytea NOT NULL UNIQUE,
    created_by uuid REFERENCES users ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    node_id    text
);
ALTER TABLE server_join_tokens ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON server_join_tokens
    USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON server_join_tokens TO platform_api;

-- The API places a project on one of its organization's servers (or back on the platform).
GRANT UPDATE (node_id) ON projects TO platform_api;
-- A customer server reports the measured use of its applications through the API.
GRANT INSERT, UPDATE ON service_stats TO platform_api;

-- Join: consume a join token (once, before it expires). Returns the organization.
CREATE FUNCTION server_join_consume(p_hash bytea)
RETURNS TABLE (token_id uuid, org_id uuid, name text, created_by uuid)
LANGUAGE sql SECURITY DEFINER SET search_path = public, pg_temp AS $$
    UPDATE server_join_tokens t SET used_at = now()
    FROM orgs o
    WHERE t.token_hash = p_hash AND t.used_at IS NULL AND t.expires_at > now()
      AND o.id = t.org_id AND NOT o.suspended
    RETURNING t.id, t.org_id, t.name, t.created_by;
$$;

-- Node token check: which server, which organization.
CREATE FUNCTION customer_node_check(p_hash bytea)
RETURNS TABLE (node_id text, org_id uuid, org_slug text, suspended boolean)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT n.id, n.org_id, o.slug, o.suspended FROM nodes n JOIN orgs o ON o.id = n.org_id
    WHERE n.token_hash = p_hash AND n.role = 'customer';
$$;

-- The tunnel proxy: which server runs a live deployment, if it runs on a customer server.
CREATE FUNCTION customer_deployment_node(p_deployment uuid)
RETURNS TABLE (node_id text, org_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT n.id, n.org_id FROM deployments d
    JOIN services s ON s.id = d.service_id JOIN projects p ON p.id = s.project_id
    JOIN nodes n ON n.id = p.node_id AND n.role = 'customer' AND n.org_id = d.org_id
    WHERE d.id = p_deployment AND d.status = 'live';
$$;

DO $$
DECLARE f text;
BEGIN
    FOREACH f IN ARRAY ARRAY['server_join_consume(bytea)', 'customer_node_check(bytea)', 'customer_deployment_node(uuid)'] LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', f);
        EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO platform_api', f);
    END LOOP;
END $$;

-- The operator panel lists platform servers only; customer servers are the customer's.
CREATE OR REPLACE FUNCTION operator_nodes()
RETURNS SETOF nodes LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT * FROM nodes WHERE role <> 'customer' ORDER BY cluster, role, id;
$$;

CREATE OR REPLACE FUNCTION operator_set_node(p_id text, p_cordoned boolean, p_cluster text)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    UPDATE nodes SET cordoned = p_cordoned, cluster = coalesce(nullif(p_cluster, ''), cluster) WHERE id = p_id AND role <> 'customer';
    RETURN FOUND;
END $$;
