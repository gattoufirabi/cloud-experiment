-- Hosted mailboxes: an organization's verified email domain can receive mail on the
-- platform's mail server (Stalwart), with mailboxes (IMAP, POP3, SMTP submission),
-- aliases and a catch-all. The platform's tables are the source of truth; the mail
-- server holds the same domains and accounts (stalwart_id) and the passwords, which the
-- platform never stores.

CREATE TABLE mail_domains (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    email_domain_id uuid NOT NULL UNIQUE REFERENCES email_domains ON DELETE RESTRICT, -- its verification
    domain          text NOT NULL UNIQUE,
    stalwart_id     text,                      -- set once the domain is verified and on the mail server
    catch_all       text NOT NULL DEFAULT '',  -- local part of the mailbox receiving unknown addresses
    dkim            text NOT NULL DEFAULT '',  -- the mail server's DKIM records (zone file lines)
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE mailboxes (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id         uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    mail_domain_id uuid NOT NULL REFERENCES mail_domains ON DELETE CASCADE,
    local_part     text NOT NULL CHECK (local_part ~ '^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$'),
    display_name   text NOT NULL DEFAULT '' CHECK (length(display_name) <= 100),
    quota_mb       int  NOT NULL CHECK (quota_mb BETWEEN 50 AND 1048576),
    stalwart_id    text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (mail_domain_id, local_part)
);

CREATE TABLE mail_aliases (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id         uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    mailbox_id     uuid NOT NULL REFERENCES mailboxes ON DELETE CASCADE,
    mail_domain_id uuid NOT NULL REFERENCES mail_domains ON DELETE CASCADE,
    local_part     text NOT NULL CHECK (local_part ~ '^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$'),
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (mail_domain_id, local_part)
);
CREATE INDEX mail_aliases_mailbox ON mail_aliases (mailbox_id);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['mail_domains', 'mailboxes', 'mail_aliases'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
            WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid)$p$, t);
    END LOOP;
END $$;

ALTER TABLE plans
    ADD COLUMN max_mailboxes int NOT NULL DEFAULT 10 CHECK (max_mailboxes >= 0),
    ADD COLUMN max_mailbox_storage_mb int NOT NULL DEFAULT 10240 CHECK (max_mailbox_storage_mb >= 0);
UPDATE plans SET max_mailboxes = 2, max_mailbox_storage_mb = 2048 WHERE id = 'trial';
UPDATE plans SET max_mailboxes = 100, max_mailbox_storage_mb = 102400 WHERE id IN ('pro', 'demo');

GRANT SELECT, INSERT, UPDATE, DELETE ON mail_domains, mailboxes, mail_aliases TO platform_api;
GRANT SELECT ON mail_domains, mailboxes TO platform_agent;
