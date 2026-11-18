-- Fine-grained access: a developer or viewer can be limited to chosen projects, with a
-- role per project (no rows = the whole organization with the member's role). An API
-- token can be limited to one project.
CREATE TABLE member_project_roles (
    org_id     uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    project_id uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
    role       text NOT NULL CHECK (role IN ('developer', 'viewer')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id, project_id)
);
ALTER TABLE member_project_roles ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON member_project_roles
    USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid);
-- The access check runs before the tenant context is set: a narrow lookup.
CREATE FUNCTION member_project_scope(p_org uuid, p_user uuid)
RETURNS TABLE (project_slug text, role text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT p.slug, mr.role FROM member_project_roles mr JOIN projects p ON p.id = mr.project_id
    WHERE mr.org_id = p_org AND mr.user_id = p_user;
$$;
REVOKE ALL ON FUNCTION member_project_scope(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION member_project_scope(uuid, uuid) TO platform_api;
GRANT SELECT, INSERT, UPDATE, DELETE ON member_project_roles TO platform_api;

ALTER TABLE api_tokens ADD COLUMN project_id uuid REFERENCES projects ON DELETE CASCADE;
DROP FUNCTION api_token_check(bytea);
CREATE FUNCTION api_token_check(p_hash bytea)
RETURNS TABLE (token_name text, role text, created_by uuid, org_id uuid, org_slug text, org_name text, plan_id text, scan_policy text,
               queue_id uuid, email_only boolean, project_slug text)
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    UPDATE api_tokens t SET last_used_at = now()
    FROM orgs o
    WHERE t.token_hash = p_hash AND o.id = t.org_id AND NOT o.suspended
      AND (t.expires_at IS NULL OR t.expires_at > now())
    RETURNING t.name, t.role, t.created_by, o.id, o.slug, o.name, o.plan_id, o.scan_policy, t.queue_id, t.email_only,
              (SELECT p.slug FROM projects p WHERE p.id = t.project_id);
$$;
REVOKE ALL ON FUNCTION api_token_check(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION api_token_check(bytea) TO platform_api;
