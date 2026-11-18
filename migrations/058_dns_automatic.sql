-- Automatic DNS: when a service's custom domain or an email sending domain lies in a zone
-- the organization hosts on the platform, the platform writes the records it needs itself.
-- managed_by names what a record belongs to ("domain:<id>", "email:<id>"; empty for
-- records people added), so it is replaced or removed with it.
ALTER TABLE dns_records ADD COLUMN managed_by text NOT NULL DEFAULT '' CHECK (length(managed_by) <= 80);
CREATE INDEX dns_records_managed ON dns_records (managed_by) WHERE managed_by <> '';

-- When a zone's delegation is confirmed, the agent marks its automatic domains verified.
GRANT UPDATE (verified_at) ON domains TO platform_agent;
GRANT UPDATE (status, message, verified_at, checked_at) ON email_domains TO platform_agent;
