-- API tokens for the REST API and the platformctl command-line tool. Each token belongs
-- to one organization, carries one role, and is stored only as a hash.

CREATE TABLE api_tokens (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    name          text NOT NULL CHECK (length(name) BETWEEN 1 AND 60),
    token_hash    bytea NOT NULL UNIQUE,
    role          text NOT NULL CHECK (role IN ('viewer', 'developer', 'admin')),
    created_by    uuid REFERENCES users ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz,
    expires_at    timestamptz
);
ALTER TABLE api_tokens ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON api_tokens
    USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid);

-- Resolves a token to its organization and role (valid, unexpired, org not suspended).
-- The API never gets cross-tenant read access to the tokens table.
CREATE FUNCTION api_token_check(p_hash bytea)
RETURNS TABLE (token_name text, role text, created_by uuid, org_id uuid, org_slug text, org_name text, plan_id text, scan_policy text)
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    UPDATE api_tokens t SET last_used_at = now()
    FROM orgs o
    WHERE t.token_hash = p_hash AND o.id = t.org_id AND NOT o.suspended
      AND (t.expires_at IS NULL OR t.expires_at > now())
    RETURNING t.name, t.role, t.created_by, o.id, o.slug, o.name, o.plan_id, o.scan_policy;
$$;
REVOKE ALL ON FUNCTION api_token_check(bytea) FROM PUBLIC;

GRANT SELECT, INSERT, DELETE ON api_tokens TO platform_api;
GRANT EXECUTE ON FUNCTION api_token_check(bytea) TO platform_api;
