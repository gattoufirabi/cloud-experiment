-- Self-signup with abuse controls: a trial plan, verified email, and the reasons an
-- organization was suspended (operator, trial ended, CPU anomaly).

ALTER TABLE plans ADD COLUMN allow_builds boolean NOT NULL DEFAULT true;
-- Trial: small, free, no custom domains (phishing) and no git builds (mining).
INSERT INTO plans (id, name, max_services, max_cpu_millis, max_memory_mb, max_domains, max_databases,
                   price_monthly_cents, price_vcpu_hour_cents, price_gb_hour_cents, price_gb_out_cents, allow_builds)
VALUES ('trial', 'Trial', 2, 500, 512, 0, 1, 0, 0, 0, 0, false)
ON CONFLICT (id) DO NOTHING;

ALTER TABLE orgs
    ADD COLUMN trial_ends_at  timestamptz,
    ADD COLUMN suspend_reason text NOT NULL DEFAULT ''
        CHECK (suspend_reason IN ('', 'operator', 'trial_ended', 'cpu_anomaly'));

-- Pending signups, until the email address is confirmed (24 hours).
CREATE TABLE signups (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text NOT NULL,
    name          text NOT NULL,
    password_hash text NOT NULL,
    org_slug      text NOT NULL,
    org_name      text NOT NULL,
    token_hash    bytea NOT NULL UNIQUE,
    ip            text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    used_at       timestamptz
);
CREATE INDEX signups_created ON signups (created_at);
GRANT SELECT, INSERT, UPDATE, DELETE ON signups TO platform_api;
GRANT UPDATE (suspended, suspend_reason) ON orgs TO platform_agent;
-- The agent emails owners when their trial ends.
GRANT SELECT (id, email) ON users TO platform_agent;
GRANT SELECT ON memberships TO platform_agent;
