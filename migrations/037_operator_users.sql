-- Operator user management: accounts can be disabled (no sign-in, sessions ended),
-- the last sign-in is recorded, and the operator sees a user's own audit trail.

ALTER TABLE users
    ADD COLUMN disabled_at     timestamptz,
    ADD COLUMN disabled_reason text NOT NULL DEFAULT '' CHECK (length(disabled_reason) <= 200),
    ADD COLUMN last_login_at   timestamptz;

-- Events made by the user or about the user, across organizations (names and actions
-- only: details can hold tenant data and are left out).
CREATE FUNCTION operator_user_audit(p_user uuid)
RETURNS TABLE (at timestamptz, org_slug text, action text, target text, ip text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT a.created_at, coalesce(o.slug, ''), a.action, a.target, coalesce(a.ip, '')
    FROM audit_events a LEFT JOIN orgs o ON o.id = a.org_id
    WHERE a.user_id = p_user OR a.target = 'user:' || p_user::text
    ORDER BY a.id DESC LIMIT 100;
$$;
REVOKE ALL ON FUNCTION operator_user_audit(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION operator_user_audit(uuid) TO platform_api;
