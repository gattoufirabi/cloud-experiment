-- Private connections between two projects of the same organization. Every web service
-- gets a private name, <service>.<project>.internal, served by the proxy on the project
-- networks; it answers requests from its own project and from connected projects only.
CREATE TABLE project_peerings (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    project_a  uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
    project_b  uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (project_a < project_b),                 -- one row per pair
    UNIQUE (project_a, project_b)
);
CREATE INDEX project_peerings_b ON project_peerings (project_b);
ALTER TABLE project_peerings ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON project_peerings
    USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid);
-- both projects must belong to the peering's organization
CREATE FUNCTION project_peering_same_org() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (SELECT count(*) FROM projects WHERE id IN (NEW.project_a, NEW.project_b) AND org_id = NEW.org_id) <> 2 THEN
        RAISE EXCEPTION 'both projects must belong to the organization';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER project_peerings_same_org BEFORE INSERT OR UPDATE ON project_peerings
    FOR EACH ROW EXECUTE FUNCTION project_peering_same_org();

GRANT SELECT, INSERT, DELETE ON project_peerings TO platform_api;
GRANT SELECT ON project_peerings TO platform_agent;
