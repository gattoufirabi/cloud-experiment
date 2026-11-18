-- Edge cache: the proxy keeps responses that explicitly allow shared caching
-- (Cache-Control public / max-age / s-maxage) and answers repeat requests itself.
ALTER TABLE services ADD COLUMN edge_cache boolean NOT NULL DEFAULT false;
