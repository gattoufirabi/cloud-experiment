-- Email alert channels, trial reminders, build minutes in billing.

-- Email channels store the address as "mailto:<address>" in url.
ALTER TABLE alert_channels DROP CONSTRAINT alert_channels_kind_check;
ALTER TABLE alert_channels ADD CONSTRAINT alert_channels_kind_check CHECK (kind IN ('webhook', 'ntfy', 'email'));
ALTER TABLE alert_channels DROP CONSTRAINT alert_channels_url_check;
ALTER TABLE alert_channels ADD CONSTRAINT alert_channels_url_check
    CHECK (length(url) <= 500 AND ((kind = 'email' AND url ~ '^mailto:[^@[:space:]]+@[^@[:space:]]+$') OR (kind <> 'email' AND url ~ '^https?://')));

-- Trial reminder: sent once, a few days before the end.
ALTER TABLE orgs ADD COLUMN trial_reminded_at timestamptz;
GRANT UPDATE (trial_reminded_at) ON orgs TO platform_agent;

-- Build minutes: priced per plan (cents per build minute), metered from builds.
ALTER TABLE plans ADD COLUMN price_build_minute_cents numeric(10,4) NOT NULL DEFAULT 0 CHECK (price_build_minute_cents >= 0);
UPDATE plans SET price_build_minute_cents = 0.5 WHERE id = 'starter';
UPDATE plans SET price_build_minute_cents = 0.4 WHERE id = 'pro';
