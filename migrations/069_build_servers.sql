-- Build servers: an organization's own servers (068) can also run its builds, like
-- self-hosted CI agents. A service chooses where its builds run; a build for "servers"
-- waits until one of the organization's servers that runs builds, and has every label
-- the service asks for, claims it through the node API. The server clones, builds and
-- pushes with a push token valid for that build's repository only, while it runs.

ALTER TABLE nodes
    ADD COLUMN builds boolean NOT NULL DEFAULT false,
    ADD COLUMN labels text[] NOT NULL DEFAULT '{}'
        CHECK (cardinality(labels) <= 10 AND array_to_string(labels, ',') ~ '^([a-z0-9][a-z0-9._-]{0,29}(,[a-z0-9][a-z0-9._-]{0,29})*)?$');

ALTER TABLE services
    ADD COLUMN build_on text NOT NULL DEFAULT 'platform' CHECK (build_on IN ('platform', 'servers')),
    ADD COLUMN build_labels text[] NOT NULL DEFAULT '{}'
        CHECK (cardinality(build_labels) <= 10 AND array_to_string(build_labels, ',') ~ '^([a-z0-9][a-z0-9._-]{0,29}(,[a-z0-9][a-z0-9._-]{0,29})*)?$');

ALTER TABLE builds
    ADD COLUMN runner          text NOT NULL DEFAULT 'platform' CHECK (runner IN ('platform', 'servers')),
    ADD COLUMN runner_labels   text[] NOT NULL DEFAULT '{}',
    ADD COLUMN node_id         text REFERENCES nodes ON DELETE SET NULL,
    ADD COLUMN push_token_hash bytea UNIQUE,
    ADD COLUMN heartbeat_at    timestamptz;
CREATE INDEX builds_server_queue ON builds (org_id, created_at) WHERE status = 'queued' AND runner = 'servers';

-- Where a build runs is decided when it is queued, from its service's setting (every
-- way of queueing a build goes through this).
CREATE FUNCTION builds_set_runner() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    SELECT s.build_on, s.build_labels INTO NEW.runner, NEW.runner_labels FROM services s WHERE s.id = NEW.service_id;
    NEW.runner := coalesce(NEW.runner, 'platform');
    NEW.runner_labels := coalesce(NEW.runner_labels, '{}');
    RETURN NEW;
END $$;
CREATE TRIGGER builds_set_runner BEFORE INSERT ON builds FOR EACH ROW EXECUTE FUNCTION builds_set_runner();

-- The node API runs a server's build: claims it, stores its log, finishes it. Saving a
-- service's build settings moves its waiting builds along.
GRANT UPDATE (status, started_at, finished_at, log, error, commit, image, node_id, push_token_hash, heartbeat_at, source_zip, runner, runner_labels)
    ON builds TO platform_api;
GRANT UPDATE (build_on, build_labels) ON services TO platform_api;

-- Registry login of a build server: the push token of a running build gives push and
-- pull on that build's repository only (<org>/<service>).
CREATE FUNCTION build_push_check(p_hash bytea)
RETURNS TABLE (org_slug text, repository text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT o.slug, o.slug || '/' || s.slug
    FROM builds b JOIN services s ON s.id = b.service_id JOIN orgs o ON o.id = b.org_id
    WHERE b.push_token_hash = p_hash AND b.status = 'running' AND b.runner = 'servers' AND NOT o.suspended;
$$;
REVOKE ALL ON FUNCTION build_push_check(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION build_push_check(bytea) TO platform_api;

-- The queue position shown in Activity: a build for the organization's servers waits
-- only for that organization's server builds.
CREATE OR REPLACE FUNCTION build_queue_ahead(p_build uuid)
RETURNS int LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT count(*)::int FROM builds b, builds me
    WHERE me.id = p_build AND me.status = 'queued' AND b.id <> me.id AND b.runner = me.runner
      AND (me.runner = 'platform' OR b.org_id = me.org_id)
      AND (b.status = 'running' OR (b.status = 'queued' AND b.created_at < me.created_at));
$$;
