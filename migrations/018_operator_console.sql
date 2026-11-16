-- Operator web console. Operators are users flagged by `platform admin make-operator`.
-- The API stays unprivileged: cross-tenant reads and changes go through these narrow
-- SECURITY DEFINER functions. The API checks users.is_operator (and a full two-step
-- session) before calling any of them, and audits every change.

ALTER TABLE users ADD COLUMN is_operator boolean NOT NULL DEFAULT false;

CREATE FUNCTION operator_overview()
RETURNS TABLE (orgs bigint, users bigint, live_services bigint, trials bigint, suspended bigint,
               open_incidents bigint, open_invoices bigint, builds_today bigint, disk_used_mb bigint)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT (SELECT count(*) FROM orgs),
           (SELECT count(*) FROM users),
           (SELECT count(*) FROM deployments WHERE status = 'live'),
           (SELECT count(*) FROM orgs WHERE plan_id = 'trial'),
           (SELECT count(*) FROM orgs WHERE suspended),
           (SELECT count(*) FROM alert_incidents WHERE status = 'firing'),
           (SELECT count(*) FROM invoices WHERE status = 'open'),
           (SELECT count(*) FROM builds WHERE created_at > now() - interval '1 day'),
           (SELECT coalesce(sum(disk_used_mb), 0) FROM orgs);
$$;

CREATE FUNCTION operator_orgs()
RETURNS TABLE (slug text, name text, plan_id text, owner text, services bigint, databases bigint,
               disk_used_mb int, max_disk_mb int, suspended boolean, suspend_reason text,
               trial_ends_at timestamptz, created_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT o.slug, o.name, o.plan_id,
           coalesce((SELECT u.email FROM memberships m JOIN users u ON u.id = m.user_id
                     WHERE m.org_id = o.id AND m.role = 'owner' ORDER BY m.created_at LIMIT 1), ''),
           (SELECT count(*) FROM services s WHERE s.org_id = o.id AND s.preview_of IS NULL),
           (SELECT count(*) FROM databases d WHERE d.org_id = o.id),
           o.disk_used_mb, p.max_disk_mb, o.suspended, o.suspend_reason, o.trial_ends_at, o.created_at
    FROM orgs o JOIN plans p ON p.id = o.plan_id
    ORDER BY o.created_at DESC;
$$;

-- Same rules as `platform admin set-plan`: leaving the trial lifts a trial-ended suspension.
CREATE FUNCTION operator_set_plan(p_slug text, p_plan text)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    UPDATE orgs SET plan_id = p_plan,
           trial_ends_at = CASE WHEN p_plan = 'trial' THEN coalesce(trial_ends_at, now() + interval '14 days') END,
           suspended = CASE WHEN suspend_reason = 'trial_ended' AND p_plan <> 'trial' THEN false ELSE suspended END,
           suspend_reason = CASE WHEN suspend_reason = 'trial_ended' AND p_plan <> 'trial' THEN '' ELSE suspend_reason END
    WHERE slug = p_slug AND EXISTS (SELECT 1 FROM plans WHERE id = p_plan);
    IF NOT FOUND THEN RETURN false; END IF;
    PERFORM pg_notify('deployments', '');
    RETURN true;
END $$;

CREATE FUNCTION operator_suspend(p_slug text, p_on boolean)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    UPDATE orgs SET suspended = p_on, suspend_reason = CASE WHEN p_on THEN 'operator' ELSE '' END WHERE slug = p_slug;
    IF NOT FOUND THEN RETURN false; END IF;
    PERFORM pg_notify('deployments', '');
    RETURN true;
END $$;

CREATE FUNCTION operator_invoices()
RETURNS TABLE (number text, org_slug text, period_start date, total_cents bigint, currency text, status text, issued_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT i.number, coalesce(o.slug, '(deleted)'), i.period_start, i.total_cents, i.currency, i.status, i.issued_at
    FROM invoices i LEFT JOIN orgs o ON o.id = i.org_id
    ORDER BY (i.status = 'open') DESC, i.issued_at DESC LIMIT 200;
$$;

-- open -> paid | void, like `platform admin invoice-paid / invoice-void`. Returns the org ID.
CREATE FUNCTION operator_invoice_set(p_number text, p_status text)
RETURNS uuid LANGUAGE sql SECURITY DEFINER SET search_path = public, pg_temp AS $$
    UPDATE invoices SET status = p_status, paid_at = CASE WHEN p_status = 'paid' THEN now() END
    WHERE number = p_number AND status = 'open' AND p_status IN ('paid', 'void')
    RETURNING org_id;
$$;

CREATE FUNCTION operator_audit()
RETURNS TABLE (at timestamptz, org_slug text, who text, action text, target text, ip text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT a.created_at, coalesce(o.slug, ''), coalesce(u.email, 'platform'), a.action, a.target, coalesce(a.ip, '')
    FROM audit_events a LEFT JOIN orgs o ON o.id = a.org_id LEFT JOIN users u ON u.id = a.user_id
    ORDER BY a.id DESC LIMIT 300;
$$;

DO $$
DECLARE f text;
BEGIN
    FOREACH f IN ARRAY ARRAY['operator_overview()', 'operator_orgs()', 'operator_set_plan(text, text)',
        'operator_suspend(text, boolean)', 'operator_invoices()', 'operator_invoice_set(text, text)', 'operator_audit()'] LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', f);
        EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO platform_api', f);
    END LOOP;
END $$;
