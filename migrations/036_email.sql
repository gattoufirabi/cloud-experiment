-- Outgoing email (SES-like): an organization verifies a sending domain through DNS
-- (an ownership TXT record and a DKIM key), then sends from any address of it with the
-- API. The agent signs each message with the domain's DKIM key and hands it to the
-- platform's SMTP relay.

CREATE TABLE email_domains (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    domain        text NOT NULL CHECK (length(domain) <= 253 AND domain ~ '^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$'),
    verify_token  text NOT NULL,
    dkim_selector text NOT NULL,
    dkim_key_enc  bytea NOT NULL,
    dkim_txt      text NOT NULL,
    status        text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'verified', 'failed')),
    message       text NOT NULL DEFAULT '',
    checked_at    timestamptz,
    verified_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, domain)
);

CREATE TABLE email_messages (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    domain_id       uuid REFERENCES email_domains ON DELETE SET NULL,
    from_addr       text NOT NULL,
    to_addrs        text[] NOT NULL CHECK (cardinality(to_addrs) BETWEEN 1 AND 50),
    reply_to        text NOT NULL DEFAULT '',
    subject         text NOT NULL CHECK (length(subject) <= 998),
    body_text       text NOT NULL DEFAULT '',
    body_html       text NOT NULL DEFAULT '',
    size_bytes      int  NOT NULL DEFAULT 0,
    status          text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'sending', 'sent', 'failed')),
    attempts        int  NOT NULL DEFAULT 0,
    error           text NOT NULL DEFAULT '',
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    created_at      timestamptz NOT NULL DEFAULT now(),
    sent_at         timestamptz
);
CREATE INDEX email_messages_queue ON email_messages (next_attempt_at) WHERE status = 'queued';
CREATE INDEX email_messages_org ON email_messages (org_id, created_at DESC);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['email_domains', 'email_messages'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
            WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid)$p$, t);
    END LOOP;
END $$;

ALTER TABLE plans
    ADD COLUMN max_email_domains int NOT NULL DEFAULT 3 CHECK (max_email_domains >= 0),
    ADD COLUMN max_emails_day int NOT NULL DEFAULT 500 CHECK (max_emails_day >= 0),
    ADD COLUMN price_email_thousand_cents numeric(10,4) NOT NULL DEFAULT 0 CHECK (price_email_thousand_cents >= 0);
UPDATE plans SET max_email_domains = 1, max_emails_day = 50 WHERE id = 'trial';
UPDATE plans SET max_email_domains = 20, max_emails_day = 20000 WHERE id = 'pro';
UPDATE plans SET price_email_thousand_cents = 10 WHERE id IN ('starter', 'pro');

-- A token can be limited to sending email (given to a service by "Connect to a service").
ALTER TABLE api_tokens ADD COLUMN email_only boolean NOT NULL DEFAULT false;
DROP FUNCTION api_token_check(bytea);
CREATE FUNCTION api_token_check(p_hash bytea)
RETURNS TABLE (token_name text, role text, created_by uuid, org_id uuid, org_slug text, org_name text, plan_id text, scan_policy text, queue_id uuid, email_only boolean)
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    UPDATE api_tokens t SET last_used_at = now()
    FROM orgs o
    WHERE t.token_hash = p_hash AND o.id = t.org_id AND NOT o.suspended
      AND (t.expires_at IS NULL OR t.expires_at > now())
    RETURNING t.name, t.role, t.created_by, o.id, o.slug, o.name, o.plan_id, o.scan_policy, t.queue_id, t.email_only;
$$;
REVOKE ALL ON FUNCTION api_token_check(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION api_token_check(bytea) TO platform_api;

ALTER TABLE usage_hourly DROP CONSTRAINT usage_hourly_kind_check;
ALTER TABLE usage_hourly ADD CONSTRAINT usage_hourly_kind_check CHECK (kind IN ('service', 'database', 'bucket', 'function', 'queue', 'email'));
ALTER TABLE usage_hourly ADD COLUMN emails bigint NOT NULL DEFAULT 0;

GRANT SELECT, INSERT, UPDATE, DELETE ON email_domains, email_messages TO platform_api;
GRANT SELECT, UPDATE ON email_domains, email_messages TO platform_agent;
GRANT DELETE ON email_messages TO platform_agent;
