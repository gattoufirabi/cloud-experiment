-- DNS hosting: an organization adds a domain as a zone, delegates it to the platform's
-- name servers at its registrar, and manages its records here. A zone is served once
-- the delegation is seen (pending zones answer nothing), so nobody can take over a
-- domain they do not control; a pending zone is dropped after 14 days.
CREATE TABLE dns_zones (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    domain      text NOT NULL UNIQUE CHECK (length(domain) <= 253 AND domain ~ '^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$'),
    status      text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'active')),
    checked_at  timestamptz,
    active_at   timestamptz,
    serial      bigint NOT NULL DEFAULT (extract(epoch FROM now()))::bigint,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE dns_records (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    zone_id     uuid NOT NULL REFERENCES dns_zones ON DELETE CASCADE,
    name        text NOT NULL CHECK (name = '@' OR name ~ '^(\*\.)?([a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9])?\.)*[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9])?$' OR name = '*'),
    type        text NOT NULL CHECK (type IN ('A', 'AAAA', 'CNAME', 'TXT', 'MX', 'CAA', 'NS')),
    value       text NOT NULL CHECK (length(value) BETWEEN 1 AND 1000),
    priority    int  NOT NULL DEFAULT 0 CHECK (priority BETWEEN 0 AND 65535),
    ttl         int  NOT NULL DEFAULT 300 CHECK (ttl BETWEEN 60 AND 86400),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dns_records_zone ON dns_records (zone_id);
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['dns_zones', 'dns_records'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
            WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid)$p$, t);
    END LOOP;
END $$;
-- Zones are globally unique: the API checks a name is free across organizations here.
CREATE FUNCTION dns_zone_taken(p_domain text) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT EXISTS (SELECT 1 FROM dns_zones WHERE domain = p_domain);
$$;
REVOKE ALL ON FUNCTION dns_zone_taken(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION dns_zone_taken(text) TO platform_api;

ALTER TABLE plans ADD COLUMN max_dns_zones int NOT NULL DEFAULT 5 CHECK (max_dns_zones >= 0);
UPDATE plans SET max_dns_zones = 1 WHERE id = 'trial';
UPDATE plans SET max_dns_zones = 50 WHERE id = 'pro';

GRANT SELECT, INSERT, UPDATE, DELETE ON dns_zones, dns_records TO platform_api;
GRANT SELECT ON dns_zones, dns_records TO platform_agent;
GRANT UPDATE (status, checked_at, active_at) ON dns_zones TO platform_agent;
GRANT DELETE ON dns_zones TO platform_agent;
