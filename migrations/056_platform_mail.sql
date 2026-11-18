-- The platform's own emails (invitations, password resets, sign-up confirmations,
-- alerts, trial notices) when there is no SMTP relay: queued here and delivered by the
-- primary agent straight to the recipients' mail servers, signed with the DKIM key of
-- the sender's domain when that domain is a verified sending domain on the platform.
-- Bodies are deleted once sent; rows are kept 30 days.
CREATE TABLE platform_mail (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    to_addr         text NOT NULL CHECK (length(to_addr) <= 320),
    subject         text NOT NULL CHECK (length(subject) <= 998),
    body            text NOT NULL DEFAULT '',
    status          text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'sending', 'sent', 'failed')),
    attempts        int  NOT NULL DEFAULT 0,
    error           text NOT NULL DEFAULT '',
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    created_at      timestamptz NOT NULL DEFAULT now(),
    sent_at         timestamptz
);
CREATE INDEX platform_mail_queue ON platform_mail (next_attempt_at) WHERE status = 'queued';

GRANT INSERT ON platform_mail TO platform_api;
GRANT SELECT, INSERT, UPDATE, DELETE ON platform_mail TO platform_agent;
