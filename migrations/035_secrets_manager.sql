-- Secrets manager: organization-wide secrets, encrypted and versioned. Service and
-- function variables can reference one as ${secret:NAME}; the agent resolves the
-- current version at each deployment.

CREATE TABLE org_secrets (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    name            text NOT NULL CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'),
    description     text NOT NULL DEFAULT '' CHECK (length(description) <= 200),
    current_version int  NOT NULL DEFAULT 1,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);
CREATE TABLE org_secret_versions (
    secret_id  uuid NOT NULL REFERENCES org_secrets ON DELETE CASCADE,
    org_id     uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    version    int  NOT NULL,
    value_enc  bytea NOT NULL,
    created_by uuid REFERENCES users ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (secret_id, version)
);
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['org_secrets', 'org_secret_versions'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
            WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid)$p$, t);
    END LOOP;
END $$;

ALTER TABLE plans ADD COLUMN max_secrets int NOT NULL DEFAULT 50 CHECK (max_secrets >= 0);
UPDATE plans SET max_secrets = 10 WHERE id = 'trial';
UPDATE plans SET max_secrets = 500 WHERE id = 'pro';

GRANT SELECT, INSERT, UPDATE, DELETE ON org_secrets, org_secret_versions TO platform_api;
GRANT SELECT ON org_secrets, org_secret_versions TO platform_agent;
