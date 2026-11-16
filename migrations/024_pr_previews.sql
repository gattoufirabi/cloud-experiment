-- Stage 6: a preview for each pull request. With "pr_previews" on, the service's build
-- webhook also accepts pull request events (GitHub, Gitea, Forgejo): opening or updating
-- a pull request builds its branch and deploys it as preview "pr-<number>"; closing it
-- deletes the preview. Pull requests from forks are ignored: a preview receives the
-- service's secrets.

ALTER TABLE services ADD COLUMN pr_previews boolean NOT NULL DEFAULT false;

ALTER TABLE builds
    ADD COLUMN branch             text NOT NULL DEFAULT '' CHECK (branch = '' OR (branch ~ '^[A-Za-z0-9._/-]{1,100}$' AND branch !~ '\.\.')),
    ADD COLUMN preview_service_id uuid REFERENCES services ON DELETE SET NULL,
    ADD COLUMN pr_number          int;
ALTER TABLE builds DROP CONSTRAINT builds_trigger_check;
ALTER TABLE builds ADD CONSTRAINT builds_trigger_check CHECK (trigger IN ('console', 'api', 'webhook', 'pull_request'));

DROP FUNCTION build_hook_service(bytea);
CREATE FUNCTION build_hook_service(p_hash bytea)
RETURNS TABLE (org_id uuid, service_id uuid, branch text, pr_previews boolean)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT s.org_id, s.id, s.source_branch, s.pr_previews FROM services s JOIN orgs o ON o.id = s.org_id
    WHERE s.build_hook_hash = p_hash AND s.source_repo <> '' AND NOT o.suspended;
$$;
REVOKE ALL ON FUNCTION build_hook_service(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION build_hook_service(bytea) TO platform_api;

GRANT UPDATE (branch) ON builds TO platform_api;
