-- Exposure: a web service can be private (only its .internal name), a service can publish
-- more HTTP ports (each on <label>.<its address>), a custom domain can send paths to
-- other services, and a database can be opened to the internet (TLS, IP allow-list).

ALTER TABLE services ADD COLUMN public boolean NOT NULL DEFAULT true;

CREATE TABLE service_ports (
    service_id uuid NOT NULL REFERENCES services ON DELETE CASCADE,
    org_id     uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    label      text NOT NULL CHECK (label ~ '^[a-z][a-z0-9]{0,19}$' AND label !~ '^pr[0-9-]'),
    port       int  NOT NULL CHECK (port BETWEEN 1 AND 65535),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (service_id, label),
    UNIQUE (service_id, port)
);

CREATE TABLE domain_paths (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    domain_id  uuid NOT NULL REFERENCES domains ON DELETE CASCADE,
    prefix     text NOT NULL CHECK (prefix ~ '^/[A-Za-z0-9._~/-]{1,200}$' AND prefix !~ '/$' AND prefix !~ '//'),
    service_id uuid NOT NULL REFERENCES services ON DELETE CASCADE,
    strip      boolean NOT NULL DEFAULT false,      -- remove the prefix before forwarding
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (domain_id, prefix)
);

ALTER TABLE databases ADD COLUMN public boolean NOT NULL DEFAULT false;
ALTER TABLE databases ADD COLUMN allow_cidrs text[] NOT NULL DEFAULT '{}';
-- The public endpoint's certificate (self-signed, per database, kept across restarts so
-- clients can pin it) and its key, encrypted with the master key.
ALTER TABLE databases ADD COLUMN proxy_cert text NOT NULL DEFAULT '';
ALTER TABLE databases ADD COLUMN proxy_key_enc bytea;
-- A database's public port comes from the same pool as services' TCP/UDP ports.
ALTER TABLE public_ports ALTER COLUMN service_id DROP NOT NULL;
ALTER TABLE public_ports ADD COLUMN database_id uuid REFERENCES databases ON DELETE CASCADE;
ALTER TABLE public_ports ADD CONSTRAINT public_ports_owner CHECK ((service_id IS NULL) <> (database_id IS NULL));
CREATE UNIQUE INDEX public_ports_database ON public_ports (database_id) WHERE database_id IS NOT NULL;

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['service_ports', 'domain_paths'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
            WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid)$p$, t);
    END LOOP;
END $$;

GRANT SELECT, INSERT, UPDATE, DELETE ON service_ports, domain_paths TO platform_api;
GRANT SELECT ON service_ports, domain_paths TO platform_agent;
GRANT UPDATE (public) ON services TO platform_api;
GRANT UPDATE (public, allow_cidrs, proxy_cert, proxy_key_enc) ON databases TO platform_api;

-- Certificates on demand: only public services, their extra ports (<label>.<address>) and
-- verified domains of public services.
CREATE OR REPLACE FUNCTION host_is_served(p_host text, p_apps_domain text)
RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT EXISTS (
        SELECT 1 FROM deployments d
        JOIN services s ON s.id = d.service_id
        LEFT JOIN services ps ON ps.id = s.preview_of
        JOIN projects p ON p.id = s.project_id
        JOIN orgs o ON o.id = d.org_id
        WHERE d.status = 'live' AND NOT o.suspended AND coalesce(ps.public, s.public)
          AND (CASE WHEN s.preview_of IS NULL
                   THEN s.slug || '-' || p.slug || '-' || o.slug || '.' || p_apps_domain
                   ELSE s.preview_name || '.' || ps.slug || '-' || p.slug || '-' || o.slug || '.' || p_apps_domain
              END = p_host
            OR (s.preview_of IS NULL AND EXISTS (SELECT 1 FROM service_ports sp WHERE sp.service_id = s.id
                AND sp.label || '.' || s.slug || '-' || p.slug || '-' || o.slug || '.' || p_apps_domain = p_host)))
    ) OR EXISTS (
        SELECT 1 FROM domains dm JOIN orgs o ON o.id = dm.org_id JOIN services s ON s.id = dm.service_id
        WHERE dm.hostname = p_host AND dm.verified_at IS NOT NULL AND NOT o.suspended AND s.public
    );
$$;
