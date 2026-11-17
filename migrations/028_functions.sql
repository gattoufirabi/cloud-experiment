-- Serverless functions (like AWS Lambda): code (a zip archive) run on demand in a warm
-- container of its project, invoked through a function URL, the API, the console or a
-- schedule. Each invocation is a row: the API writes the event, the agent runs it and
-- writes the result back (payloads are cleared once delivered).

ALTER TABLE plans
    ADD COLUMN max_functions                   int NOT NULL DEFAULT 20 CHECK (max_functions >= 0),
    ADD COLUMN price_fn_gb_second_cents        numeric(12,8) NOT NULL DEFAULT 0 CHECK (price_fn_gb_second_cents >= 0),
    ADD COLUMN price_fn_million_requests_cents numeric(10,4) NOT NULL DEFAULT 0 CHECK (price_fn_million_requests_cents >= 0);
UPDATE plans SET max_functions = 3 WHERE id = 'trial';
UPDATE plans SET max_functions = 200 WHERE id = 'pro';
UPDATE plans SET price_fn_gb_second_cents = 0.0015, price_fn_million_requests_cents = 20 WHERE id = 'starter';
UPDATE plans SET price_fn_gb_second_cents = 0.0012, price_fn_million_requests_cents = 15 WHERE id = 'pro';

CREATE TABLE functions (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id           uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    project_id       uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
    slug             text NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,19}$'),
    runtime          text NOT NULL CHECK (runtime IN ('nodejs22', 'python3.13')),
    handler          text NOT NULL CHECK (length(handler) BETWEEN 3 AND 170),
    code             bytea NOT NULL,
    code_sha256      text NOT NULL,
    memory_mb        int NOT NULL DEFAULT 128 CHECK (memory_mb BETWEEN 128 AND 2048),
    timeout_seconds  int NOT NULL DEFAULT 10 CHECK (timeout_seconds BETWEEN 1 AND 300),
    concurrency      int NOT NULL DEFAULT 5 CHECK (concurrency BETWEEN 1 AND 50),
    env              jsonb NOT NULL DEFAULT '{}',
    url_enabled      boolean NOT NULL DEFAULT false,
    url_auth         text NOT NULL DEFAULT 'none' CHECK (url_auth IN ('none', 'token')),
    url_token_hash   bytea,
    schedule         text NOT NULL DEFAULT '' CHECK (length(schedule) <= 100),
    schedule_enabled boolean NOT NULL DEFAULT false,
    next_run_at      timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, slug)
);

CREATE TABLE function_invocations (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    function_id uuid NOT NULL REFERENCES functions ON DELETE CASCADE,
    trigger     text NOT NULL CHECK (trigger IN ('url', 'api', 'console', 'schedule')),
    async       boolean NOT NULL DEFAULT false,
    status      text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'timed_out')),
    event       bytea,          -- cleared once the run starts
    response    bytea,          -- the runtime's JSON line, cleared once delivered
    error_type  text NOT NULL DEFAULT '',
    error       text NOT NULL DEFAULT '',
    log         text NOT NULL DEFAULT '',
    duration_ms int,
    cold        boolean NOT NULL DEFAULT false,
    node_id     text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    started_at  timestamptz,
    finished_at timestamptz
);
CREATE INDEX function_invocations_fn ON function_invocations (function_id, created_at DESC);
CREATE INDEX function_invocations_queued ON function_invocations (created_at) WHERE status = 'queued';

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['functions', 'function_invocations'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
            WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid)$p$, t);
    END LOOP;
END $$;

-- Function URLs: https://<function>-<project>-<organization>.fn.<apps domain>/
CREATE FUNCTION function_by_host(p_label text)
RETURNS TABLE (org_id uuid, function_id uuid, url_auth text, url_token_hash bytea)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT f.org_id, f.id, f.url_auth, f.url_token_hash
    FROM functions f JOIN projects p ON p.id = f.project_id JOIN orgs o ON o.id = f.org_id
    WHERE f.url_enabled AND NOT o.suspended AND f.slug || '-' || p.slug || '-' || o.slug = p_label;
$$;
REVOKE ALL ON FUNCTION function_by_host(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION function_by_host(text) TO platform_api;

-- Usage for invoices: requests and memory x duration (MB-milliseconds).
ALTER TABLE usage_hourly DROP CONSTRAINT usage_hourly_kind_check;
ALTER TABLE usage_hourly ADD CONSTRAINT usage_hourly_kind_check CHECK (kind IN ('service', 'database', 'bucket', 'function'));
ALTER TABLE usage_hourly
    ADD COLUMN fn_requests bigint NOT NULL DEFAULT 0,
    ADD COLUMN fn_mb_ms    bigint NOT NULL DEFAULT 0;

GRANT SELECT, INSERT, UPDATE, DELETE ON functions, function_invocations TO platform_api;
GRANT SELECT ON functions TO platform_agent;
GRANT UPDATE (next_run_at) ON functions TO platform_agent;
GRANT SELECT, INSERT, UPDATE, DELETE ON function_invocations TO platform_agent;
