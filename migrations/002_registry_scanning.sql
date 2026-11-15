-- Private image registry (per-organization namespaces) and vulnerability scanning.

-- warn: deploy anyway and show findings · block-critical: refuse images with critical CVEs
ALTER TABLE orgs ADD COLUMN scan_policy text NOT NULL DEFAULT 'warn'
    CHECK (scan_policy IN ('warn', 'block-critical'));

-- Credentials for "docker login". Each token can only reach <org-slug>/* in the registry.
CREATE TABLE registry_tokens (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    name          text NOT NULL CHECK (length(name) BETWEEN 1 AND 60),
    token_hash    bytea NOT NULL UNIQUE,
    can_push      boolean NOT NULL DEFAULT false,
    created_by    uuid REFERENCES users ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz,
    expires_at    timestamptz
);

CREATE TABLE image_scans (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    image       text NOT NULL,
    image_id    text NOT NULL DEFAULT '',
    source      text NOT NULL CHECK (source IN ('push', 'deploy', 'rescan')),
    status      text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'done', 'error')),
    critical    int  NOT NULL DEFAULT 0,
    high        int  NOT NULL DEFAULT 0,
    medium      int  NOT NULL DEFAULT 0,
    low         int  NOT NULL DEFAULT 0,
    findings    jsonb NOT NULL DEFAULT '[]',
    error       text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    scanned_at  timestamptz
);
CREATE INDEX image_scans_status ON image_scans (status);
CREATE INDEX image_scans_org ON image_scans (org_id, created_at DESC);
CREATE INDEX image_scans_reuse ON image_scans (org_id, image_id, scanned_at DESC);

ALTER TABLE deployments ADD COLUMN scan_id uuid REFERENCES image_scans ON DELETE SET NULL;

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['registry_tokens', 'image_scans'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format(
            'CREATE POLICY tenant_isolation ON %I
                USING (org_id = nullif(current_setting(''app.org_id'', true), '''')::uuid)
                WITH CHECK (org_id = nullif(current_setting(''app.org_id'', true), '''')::uuid)', t);
    END LOOP;
END $$;

-- Token lookup for the registry login endpoint: returns the owning org only for a valid,
-- unexpired token of an active org. The API never gets cross-tenant read access.
CREATE FUNCTION registry_token_check(p_hash bytea)
RETURNS TABLE (org_id uuid, org_slug text, can_push boolean)
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    UPDATE registry_tokens t SET last_used_at = now()
    FROM orgs o
    WHERE t.token_hash = p_hash AND o.id = t.org_id AND NOT o.suspended
      AND (t.expires_at IS NULL OR t.expires_at > now())
    RETURNING t.org_id, o.slug, t.can_push;
$$;
REVOKE ALL ON FUNCTION registry_token_check(bytea) FROM PUBLIC;

-- Queue a scan for an image pushed to the registry (called by the registry webhook).
CREATE FUNCTION scan_enqueue_push(p_org_slug text, p_image text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    INSERT INTO image_scans (org_id, image, source)
    SELECT o.id, p_image, 'push' FROM orgs o
    WHERE o.slug = p_org_slug
      AND NOT EXISTS (SELECT 1 FROM image_scans s WHERE s.image = p_image AND s.status IN ('queued', 'running'));
    IF FOUND THEN
        PERFORM pg_notify('deployments', 'scan');
    END IF;
END $$;
REVOKE ALL ON FUNCTION scan_enqueue_push(text, text) FROM PUBLIC;

GRANT SELECT, INSERT, DELETE ON registry_tokens TO platform_api;
GRANT SELECT ON image_scans TO platform_api;
GRANT EXECUTE ON FUNCTION registry_token_check(bytea), scan_enqueue_push(text, text) TO platform_api;

GRANT SELECT, INSERT, UPDATE ON image_scans TO platform_agent;
