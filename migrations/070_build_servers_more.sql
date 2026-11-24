-- Build servers, continued (069): a server can run several builds at once, and can
-- start every build from an empty cache ("clean builds", like a throwaway CI agent).
-- The server's operating system and architecture (reported by its agent) count as
-- labels too, so a service can ask for "arm64" without anyone typing it on the server.

ALTER TABLE nodes
    ADD COLUMN max_builds int NOT NULL DEFAULT 1 CHECK (max_builds BETWEEN 1 AND 8),
    ADD COLUMN clean_builds boolean NOT NULL DEFAULT false;
