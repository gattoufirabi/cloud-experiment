-- Shared project variables (stage 3): set once on a project, passed to every service in
-- it (and its previews) as environment variables. A service's own variables and secrets
-- override a project variable of the same name. Values are always encrypted at rest;
-- a variable marked secret is never shown again (plain ones are shown to members).

CREATE TABLE project_variables (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    project_id  uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
    name        text NOT NULL CHECK (name ~ '^[A-Z_][A-Z0-9_]{0,63}$' AND name NOT LIKE 'PLATFORM\_%'),
    value_enc   bytea NOT NULL,
    secret      boolean NOT NULL DEFAULT true,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, name)
);
CREATE INDEX project_variables_org ON project_variables (org_id);

ALTER TABLE project_variables ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON project_variables
    USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON project_variables TO platform_api;
GRANT SELECT ON project_variables TO platform_agent;
