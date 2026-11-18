-- Email: direct delivery to each recipient's mail server, attachments and SMTP
-- submission (messages stored as complete MIME), per-recipient results, a suppression
-- list fed by hard bounces, and delivery webhooks.

-- The complete message (headers and body) when it was composed with attachments or
-- received over SMTP; emptied once delivered, like the bodies.
ALTER TABLE email_messages ADD COLUMN raw bytea;
ALTER TABLE email_messages ADD COLUMN source text NOT NULL DEFAULT 'api' CHECK (source IN ('api', 'smtp', 'console'));
ALTER TABLE email_messages DROP CONSTRAINT email_messages_status_check;
ALTER TABLE email_messages ADD CONSTRAINT email_messages_status_check CHECK (status IN ('queued', 'sending', 'sent', 'partial', 'failed'));

-- One row per recipient: delivered, bounced (refused for good), suppressed (on the list),
-- failed (gave up after retries) or still queued.
CREATE TABLE email_recipients (
    message_id uuid NOT NULL REFERENCES email_messages ON DELETE CASCADE,
    org_id     uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    address    text NOT NULL CHECK (length(address) <= 320),
    status     text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'delivered', 'bounced', 'suppressed', 'failed')),
    detail     text NOT NULL DEFAULT '',            -- the receiving server's answer
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, address)
);
CREATE INDEX email_recipients_org ON email_recipients (org_id, updated_at DESC);

-- Addresses not to send to: hard bounces (added automatically) and addresses added by hand.
CREATE TABLE email_suppressions (
    org_id     uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    address    text NOT NULL CHECK (length(address) <= 320),
    reason     text NOT NULL CHECK (reason IN ('bounce', 'complaint', 'manual')),
    detail     text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, address)
);

-- Delivery webhook: one URL per organization, called with a signed JSON body.
CREATE TABLE email_webhooks (
    org_id     uuid PRIMARY KEY REFERENCES orgs ON DELETE CASCADE,
    url        text NOT NULL CHECK (url LIKE 'https://%' OR url LIKE 'http://%'),
    secret_enc bytea NOT NULL,
    events     text[] NOT NULL DEFAULT '{delivered,bounced,failed,suppressed}',
    enabled    boolean NOT NULL DEFAULT true,
    last_status text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Outbox of webhook calls (kept 7 days).
CREATE TABLE email_events (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    message_id      uuid NOT NULL,
    recipient       text NOT NULL,
    event           text NOT NULL CHECK (event IN ('delivered', 'bounced', 'failed', 'suppressed')),
    detail          text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'failed')),
    attempts        int  NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error      text NOT NULL DEFAULT ''
);
CREATE INDEX email_events_pending ON email_events (next_attempt_at) WHERE status = 'pending';

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['email_recipients', 'email_suppressions', 'email_webhooks', 'email_events'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
            WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid)$p$, t);
    END LOOP;
END $$;

-- Attachments: up to 10 MB per message.
ALTER TABLE plans ADD COLUMN max_email_bytes int NOT NULL DEFAULT 10485760 CHECK (max_email_bytes > 0);

GRANT SELECT, INSERT ON email_recipients TO platform_api;
GRANT SELECT, INSERT, DELETE ON email_suppressions TO platform_api;
GRANT SELECT, INSERT, UPDATE, DELETE ON email_webhooks TO platform_api;
GRANT SELECT ON email_events TO platform_api;
GRANT SELECT, INSERT, UPDATE ON email_recipients TO platform_agent;
GRANT SELECT, INSERT ON email_suppressions TO platform_agent;
GRANT SELECT, UPDATE (last_status) ON email_webhooks TO platform_agent;
GRANT SELECT, INSERT, UPDATE, DELETE ON email_events TO platform_agent;
