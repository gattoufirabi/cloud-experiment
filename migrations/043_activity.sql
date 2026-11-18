-- Activity: how many builds, all organizations together, run or wait before a queued
-- build (builds run one at a time). Only a count leaves the function.
CREATE FUNCTION build_queue_ahead(p_build uuid)
RETURNS int LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT count(*)::int FROM builds b, builds me
    WHERE me.id = p_build AND me.status = 'queued' AND b.id <> me.id
      AND (b.status = 'running' OR (b.status = 'queued' AND b.created_at < me.created_at));
$$;
REVOKE ALL ON FUNCTION build_queue_ahead(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION build_queue_ahead(uuid) TO platform_api;
