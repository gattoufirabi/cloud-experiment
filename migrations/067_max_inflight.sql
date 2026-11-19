-- Overload protection: at most this many requests in progress per instance; beyond it
-- the proxy answers 503 at once instead of queueing (0 = no limit).
ALTER TABLE services ADD COLUMN max_inflight int NOT NULL DEFAULT 0 CHECK (max_inflight BETWEEN 0 AND 100000);
