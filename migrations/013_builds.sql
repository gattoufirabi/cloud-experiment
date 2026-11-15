-- Builds from a git repository: the agent clones the repository and builds its
-- Dockerfile in a sandbox (rootless BuildKit, no Docker socket, own network), pushes
-- the image to the organization's registry namespace and, optionally, deploys it.
ALTER TABLE services
    ADD COLUMN source_repo      text NOT NULL DEFAULT ''
        CHECK (source_repo = '' OR (source_repo ~ '^https?://[A-Za-z0-9.-]+(:[0-9]+)?/[A-Za-z0-9._~/-]+$' AND length(source_repo) <= 300)),
    ADD COLUMN source_branch    text NOT NULL DEFAULT 'main' CHECK (source_branch ~ '^[A-Za-z0-9._/-]{1,100}$' AND source_branch !~ '\.\.'),
    ADD COLUMN source_dir       text NOT NULL DEFAULT '' CHECK (source_dir ~ '^([A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*)?$' AND source_dir !~ '\.\.'),
    ADD COLUMN source_token_enc bytea,            -- read token for private repositories (encrypted)
    ADD COLUMN build_hook_hash  bytea UNIQUE;     -- SHA-256 of the webhook token

CREATE TABLE builds (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    service_id  uuid NOT NULL REFERENCES services ON DELETE CASCADE,
    number      int  NOT NULL,
    status      text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'done', 'failed')),
    trigger     text NOT NULL CHECK (trigger IN ('console', 'api', 'webhook')),
    deploy      boolean NOT NULL DEFAULT true,
    commit      text NOT NULL DEFAULT '',
    image       text NOT NULL DEFAULT '',
    log         text NOT NULL DEFAULT '',
    error       text NOT NULL DEFAULT '',
    created_by  uuid REFERENCES users ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    started_at  timestamptz,
    finished_at timestamptz,
    UNIQUE (service_id, number)
);
CREATE INDEX builds_queue ON builds (created_at) WHERE status = 'queued';
ALTER TABLE builds ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON builds
    USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid);

-- Webhook: find the service a token belongs to, across tenants (the caller has no session).
CREATE FUNCTION build_hook_service(p_hash bytea)
RETURNS TABLE (org_id uuid, service_id uuid, branch text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT s.org_id, s.id, s.source_branch FROM services s JOIN orgs o ON o.id = s.org_id
    WHERE s.build_hook_hash = p_hash AND s.source_repo <> '' AND NOT o.suspended;
$$;
REVOKE ALL ON FUNCTION build_hook_service(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION build_hook_service(bytea) TO platform_api;

GRANT SELECT, INSERT ON builds TO platform_api;
GRANT SELECT, UPDATE ON builds TO platform_agent;
GRANT INSERT ON deployments TO platform_agent;
GRANT UPDATE (image) ON services TO platform_agent;
