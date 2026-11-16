-- Self-service account recovery: one-time recovery codes for two-step login, and
-- password reset links (emailed, or printed by the operator). Only hashes are stored.
CREATE TABLE recovery_codes (
    user_id    uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    code_hash  bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    used_at    timestamptz,
    PRIMARY KEY (user_id, code_hash)
);

CREATE TABLE password_resets (
    token_hash bytea PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz
);
CREATE INDEX password_resets_user ON password_resets (user_id);

GRANT SELECT, INSERT, UPDATE, DELETE ON recovery_codes, password_resets TO platform_api;
