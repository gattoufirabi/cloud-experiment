-- More function triggers: an object uploaded to or deleted from a bucket (from the
-- proxy's record of S3 requests), and a secret webhook URL that any service can call.
CREATE TABLE bucket_triggers (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    bucket_id   uuid NOT NULL REFERENCES buckets ON DELETE CASCADE,
    function_id uuid NOT NULL REFERENCES functions ON DELETE CASCADE,
    events      text NOT NULL DEFAULT 'created' CHECK (events IN ('created', 'removed', 'all')),
    prefix      text NOT NULL DEFAULT '' CHECK (length(prefix) <= 200),
    suffix      text NOT NULL DEFAULT '' CHECK (length(suffix) <= 50),
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (bucket_id, function_id)
);
ALTER TABLE bucket_triggers ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON bucket_triggers
    USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON bucket_triggers TO platform_api;
GRANT SELECT ON bucket_triggers TO platform_agent;

ALTER TABLE functions ADD COLUMN webhook_token_hash bytea UNIQUE;
-- The public webhook endpoint resolves its secret here (functions are row-level protected).
CREATE FUNCTION function_by_webhook(p_hash bytea)
RETURNS TABLE (org_id uuid, function_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT f.org_id, f.id FROM functions f JOIN orgs o ON o.id = f.org_id
    WHERE f.webhook_token_hash = p_hash AND NOT o.suspended;
$$;
REVOKE ALL ON FUNCTION function_by_webhook(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION function_by_webhook(bytea) TO platform_api;

ALTER TABLE function_invocations DROP CONSTRAINT function_invocations_trigger_check;
ALTER TABLE function_invocations ADD CONSTRAINT function_invocations_trigger_check
    CHECK (trigger IN ('url', 'api', 'console', 'schedule', 'queue', 'bucket', 'webhook'));
