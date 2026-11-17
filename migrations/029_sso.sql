-- Stage 9: single sign-on per organization (OpenID Connect). Members sign in through
-- the organization's identity provider (Google Workspace, Microsoft Entra ID, Okta,
-- Keycloak...). Accounts are created on first sign-in and their role can follow the
-- provider's groups. With "enforce", members whose email is in the organization's
-- domains can no longer sign in with a password.

CREATE TABLE org_sso (
    org_id            uuid PRIMARY KEY REFERENCES orgs ON DELETE CASCADE,
    enabled           boolean NOT NULL DEFAULT false,
    issuer            text NOT NULL CHECK (issuer ~ '^https?://[^ ]{3,250}$' AND length(issuer) <= 300),
    client_id         text NOT NULL CHECK (length(client_id) BETWEEN 1 AND 300),
    client_secret_enc bytea,
    domains           text[] NOT NULL DEFAULT '{}',   -- allowed email domains ('' = any)
    default_role      text NOT NULL DEFAULT 'developer' CHECK (default_role IN ('viewer', 'developer', 'admin')),
    groups_claim      text NOT NULL DEFAULT 'groups' CHECK (groups_claim ~ '^[A-Za-z0-9_:./-]{1,100}$'),
    admin_groups      text[] NOT NULL DEFAULT '{}',
    developer_groups  text[] NOT NULL DEFAULT '{}',
    viewer_groups     text[] NOT NULL DEFAULT '{}',
    enforce           boolean NOT NULL DEFAULT false,
    updated_at        timestamptz NOT NULL DEFAULT now()
);
-- Every domain belongs to at most one organization with SSO (it routes sign-ins by email).
CREATE TABLE org_sso_domains (
    domain text PRIMARY KEY CHECK (domain ~ '^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$'),
    org_id uuid NOT NULL REFERENCES orgs ON DELETE CASCADE
);

-- Pending sign-ins (state -> PKCE verifier and nonce), valid 10 minutes.
CREATE TABLE sso_logins (
    state         text PRIMARY KEY,
    org_id        uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    nonce         text NOT NULL,
    code_verifier text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- A user signed in through a provider: (issuer, subject) identifies them there.
CREATE TABLE user_identities (
    issuer     text NOT NULL,
    subject    text NOT NULL,
    user_id    uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    org_id     uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_login timestamptz,
    PRIMARY KEY (issuer, subject)
);

-- Accounts created by single sign-on have no usable password.
ALTER TABLE users ADD COLUMN sso_only boolean NOT NULL DEFAULT false;

GRANT SELECT, INSERT, UPDATE, DELETE ON org_sso, org_sso_domains, sso_logins, user_identities TO platform_api;
