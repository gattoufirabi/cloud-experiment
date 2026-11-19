-- Feature switches: the operator turns platform services (compute, databases, mailboxes,
-- DNS...) off for everyone, for a plan or for one organization. A service that is off
-- disappears from the console's menus and its pages and API answer that it is off.
-- Values are product IDs (see products in internal/api/console.go).

ALTER TABLE platform_settings ADD COLUMN features_off text[] NOT NULL DEFAULT '{}';
ALTER TABLE plans ADD COLUMN features_off text[] NOT NULL DEFAULT '{}';
ALTER TABLE orgs ADD COLUMN features_off text[] NOT NULL DEFAULT '{}';

-- Every service that is off for an organization (its own switches, its plan's, the platform's).
CREATE FUNCTION org_features_off(p_org uuid) RETURNS text[]
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT coalesce(ARRAY(SELECT DISTINCT f FROM unnest(o.features_off || p.features_off || ps.features_off) f ORDER BY f), '{}')
    FROM orgs o JOIN plans p ON p.id = o.plan_id CROSS JOIN platform_settings ps
    WHERE o.id = p_org AND ps.id = 1
$$;

-- The operator console sets an organization's own switches.
CREATE FUNCTION operator_set_org_features(p_slug text, p_off text[]) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    UPDATE orgs SET features_off = coalesce(p_off, '{}') WHERE slug = p_slug;
    RETURN FOUND;
END $$;

REVOKE ALL ON FUNCTION org_features_off(uuid), operator_set_org_features(text, text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION org_features_off(uuid) TO platform_api, platform_agent;
GRANT EXECUTE ON FUNCTION operator_set_org_features(text, text[]) TO platform_api;

-- Restarting a platform service from the admin panel: the API records the request, the
-- agent on the primary restarts the container (compose service name) and notes the result.
CREATE TABLE platform_restarts (
    id           bigserial PRIMARY KEY,
    service      text NOT NULL CHECK (service IN ('api', 'caddy', 'stalwart', 'garage', 'registry', 'trivy', 'victorialogs', 'victoriametrics')),
    requested_by uuid REFERENCES users ON DELETE SET NULL,
    requested_at timestamptz NOT NULL DEFAULT now(),
    done_at      timestamptz,
    error        text NOT NULL DEFAULT ''
);
GRANT SELECT, INSERT ON platform_restarts TO platform_api;
GRANT USAGE ON SEQUENCE platform_restarts_id_seq TO platform_api;
GRANT SELECT, UPDATE ON platform_restarts TO platform_agent;

-- The admin panel's Mail and DNS pages: every organization's mail domains, hosted zones
-- and custom domains (names, status and counts only: never mailbox names, records or mail).
CREATE FUNCTION operator_mail_domains() RETURNS TABLE (org text, domain text, active boolean, mailboxes int, quota_mb bigint, created_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT o.slug, md.domain, md.stalwart_id IS NOT NULL,
           (SELECT count(*) FROM mailboxes b WHERE b.mail_domain_id = md.id)::int,
           (SELECT coalesce(sum(b.quota_mb), 0) FROM mailboxes b WHERE b.mail_domain_id = md.id)::bigint, md.created_at
    FROM mail_domains md JOIN orgs o ON o.id = md.org_id ORDER BY md.domain
$$;
CREATE FUNCTION operator_dns_zones() RETURNS TABLE (org text, domain text, status text, delegation text, records int, active_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT o.slug, z.domain, z.status, z.delegation, (SELECT count(*) FROM dns_records r WHERE r.zone_id = z.id)::int, z.active_at
    FROM dns_zones z JOIN orgs o ON o.id = z.org_id ORDER BY z.domain
$$;
CREATE FUNCTION operator_custom_domains() RETURNS TABLE (org text, hostname text, service text, verified boolean, created_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT o.slug, d.hostname, p.slug || '/' || s.slug, d.verified_at IS NOT NULL, d.created_at
    FROM domains d JOIN orgs o ON o.id = d.org_id JOIN services s ON s.id = d.service_id JOIN projects p ON p.id = s.project_id
    ORDER BY d.hostname
$$;
REVOKE ALL ON FUNCTION operator_mail_domains(), operator_dns_zones(), operator_custom_domains() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION operator_mail_domains(), operator_dns_zones(), operator_custom_domains() TO platform_api;
