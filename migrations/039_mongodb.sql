-- MongoDB as a managed database engine.
ALTER TABLE databases DROP CONSTRAINT databases_engine_version;
ALTER TABLE databases ADD CONSTRAINT databases_engine_version CHECK (
    (engine = 'postgres' AND version IN ('16', '17')) OR
    (engine = 'redis' AND version = '7') OR
    (engine = 'mysql' AND version = '8.4') OR
    (engine = 'mariadb' AND version = '11.4') OR
    (engine = 'mongodb' AND version = '8.0'));
