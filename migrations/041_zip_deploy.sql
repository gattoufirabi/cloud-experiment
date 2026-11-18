-- Zip deployment: upload the source of an application as a zip archive; the platform
-- builds it like a git build (Dockerfile, or Railpack without one) and deploys it.
-- The archive is kept with its build until the build finishes, then dropped.
ALTER TABLE builds
    ADD COLUMN source_zip    bytea,
    ADD COLUMN source_sha256 text NOT NULL DEFAULT '',
    ADD COLUMN source_name   text NOT NULL DEFAULT '' CHECK (length(source_name) <= 200);
ALTER TABLE builds DROP CONSTRAINT builds_trigger_check;
ALTER TABLE builds ADD CONSTRAINT builds_trigger_check CHECK (trigger IN ('console', 'api', 'webhook', 'pull_request', 'zip'));
GRANT UPDATE (source_zip) ON builds TO platform_agent;
