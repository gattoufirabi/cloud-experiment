-- Managed Redis alongside PostgreSQL, in the same databases table.
ALTER TABLE databases DROP CONSTRAINT databases_engine_check;
ALTER TABLE databases DROP CONSTRAINT databases_version_check;
ALTER TABLE databases ADD CONSTRAINT databases_engine_version CHECK (
    (engine = 'postgres' AND version IN ('16', '17')) OR
    (engine = 'redis' AND version = '7'));
