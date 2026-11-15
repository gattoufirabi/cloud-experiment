-- Passkeys (WebAuthn). Only public keys are stored. A passkey sign-in requires user
-- verification on the device (fingerprint, face or PIN), so it replaces password + code.
CREATE TABLE passkeys (
    id                bytea PRIMARY KEY,               -- credential ID
    user_id           uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    name              text NOT NULL CHECK (length(name) BETWEEN 1 AND 60),
    public_key        bytea NOT NULL,                  -- COSE key
    attestation_type  text NOT NULL DEFAULT '',
    transports        text[] NOT NULL DEFAULT '{}',
    aaguid            bytea,
    sign_count        bigint NOT NULL DEFAULT 0,
    backup_eligible   boolean NOT NULL DEFAULT false,
    backup_state      boolean NOT NULL DEFAULT false,
    created_at        timestamptz NOT NULL DEFAULT now(),
    last_used_at      timestamptz
);
CREATE INDEX passkeys_user ON passkeys (user_id);
GRANT SELECT, INSERT, UPDATE, DELETE ON passkeys TO platform_api;
