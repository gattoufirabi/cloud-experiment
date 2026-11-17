-- Message queues (like AWS SQS): at-least-once delivery with a visibility timeout, a
-- dead-letter queue after too many receives, and an optional function trigger that
-- consumes messages in batches. Services reach a queue with a token scoped to it.

ALTER TABLE plans
    ADD COLUMN max_queues int NOT NULL DEFAULT 10 CHECK (max_queues >= 0),
    ADD COLUMN price_queue_million_requests_cents numeric(10,4) NOT NULL DEFAULT 0 CHECK (price_queue_million_requests_cents >= 0);
UPDATE plans SET max_queues = 2 WHERE id = 'trial';
UPDATE plans SET max_queues = 100 WHERE id = 'pro';
UPDATE plans SET price_queue_million_requests_cents = 40 WHERE id = 'starter';
UPDATE plans SET price_queue_million_requests_cents = 30 WHERE id = 'pro';

CREATE TABLE queues (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id             uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    name               text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$'),
    visibility_seconds int  NOT NULL DEFAULT 30 CHECK (visibility_seconds BETWEEN 1 AND 43200),
    retention_seconds  int  NOT NULL DEFAULT 345600 CHECK (retention_seconds BETWEEN 60 AND 1209600),
    max_receives       int  NOT NULL DEFAULT 5 CHECK (max_receives BETWEEN 1 AND 1000),
    dlq_id             uuid REFERENCES queues ON DELETE SET NULL,
    function_id        uuid REFERENCES functions ON DELETE SET NULL,   -- trigger
    batch_size         int  NOT NULL DEFAULT 10 CHECK (batch_size BETWEEN 1 AND 10),
    trigger_enabled    boolean NOT NULL DEFAULT true,
    created_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, name),
    CHECK (dlq_id IS NULL OR dlq_id <> id)
);

CREATE TABLE queue_messages (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    queue_id      uuid NOT NULL REFERENCES queues ON DELETE CASCADE,
    body          text NOT NULL CHECK (octet_length(body) <= 262144),
    attributes    jsonb NOT NULL DEFAULT '{}',
    receive_count int  NOT NULL DEFAULT 0,
    receipt       text,                       -- changes at each receive
    visible_at    timestamptz NOT NULL DEFAULT now(),
    first_receive timestamptz,
    sent_at       timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL
);
CREATE INDEX queue_messages_ready ON queue_messages (queue_id, visible_at, sent_at);
CREATE INDEX queue_messages_receipt ON queue_messages (receipt) WHERE receipt IS NOT NULL;

-- Messages handed to a function invocation, settled when it finishes.
CREATE TABLE queue_deliveries (
    invocation_id uuid PRIMARY KEY REFERENCES function_invocations ON DELETE CASCADE,
    org_id        uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    queue_id      uuid NOT NULL REFERENCES queues ON DELETE CASCADE,
    message_ids   uuid[] NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['queues', 'queue_messages', 'queue_deliveries'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
            WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid)$p$, t);
    END LOOP;
END $$;

-- A token can be limited to one queue (given to a service by "Connect to a service").
ALTER TABLE api_tokens ADD COLUMN queue_id uuid REFERENCES queues ON DELETE CASCADE;
DROP FUNCTION api_token_check(bytea);
CREATE FUNCTION api_token_check(p_hash bytea)
RETURNS TABLE (token_name text, role text, created_by uuid, org_id uuid, org_slug text, org_name text, plan_id text, scan_policy text, queue_id uuid)
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    UPDATE api_tokens t SET last_used_at = now()
    FROM orgs o
    WHERE t.token_hash = p_hash AND o.id = t.org_id AND NOT o.suspended
      AND (t.expires_at IS NULL OR t.expires_at > now())
    RETURNING t.name, t.role, t.created_by, o.id, o.slug, o.name, o.plan_id, o.scan_policy, t.queue_id;
$$;
REVOKE ALL ON FUNCTION api_token_check(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION api_token_check(bytea) TO platform_api;

ALTER TABLE usage_hourly DROP CONSTRAINT usage_hourly_kind_check;
ALTER TABLE usage_hourly ADD CONSTRAINT usage_hourly_kind_check CHECK (kind IN ('service', 'database', 'bucket', 'function', 'queue'));
ALTER TABLE usage_hourly ADD COLUMN queue_requests bigint NOT NULL DEFAULT 0;

GRANT SELECT, INSERT, UPDATE, DELETE ON queues, queue_messages, queue_deliveries TO platform_api;
GRANT SELECT, INSERT, UPDATE, DELETE ON queue_messages, queue_deliveries TO platform_agent;
GRANT SELECT ON queues TO platform_agent;

-- Queue-triggered invocations.
ALTER TABLE function_invocations DROP CONSTRAINT function_invocations_trigger_check;
ALTER TABLE function_invocations ADD CONSTRAINT function_invocations_trigger_check CHECK (trigger IN ('url', 'api', 'console', 'schedule', 'queue'));

-- Queue requests are metered by the API as they happen.
GRANT SELECT, INSERT, UPDATE ON usage_hourly TO platform_api;
