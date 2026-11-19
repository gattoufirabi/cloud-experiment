-- Volumes left by deleted services and databases: the agent of each node notes when it
-- first saw one without an owner, and removes it once the retention (ORPHAN_VOLUME_DAYS,
-- default 7) has passed, so a deletion can still be undone by an operator for a while.
CREATE TABLE orphan_volumes (
    node_id    text NOT NULL,
    name       text NOT NULL,
    first_seen timestamptz NOT NULL DEFAULT now(),
    size_bytes bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (node_id, name)
);
GRANT SELECT, INSERT, UPDATE, DELETE ON orphan_volumes TO platform_agent;
GRANT SELECT ON orphan_volumes TO platform_api;
