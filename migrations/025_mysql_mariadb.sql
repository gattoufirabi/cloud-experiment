-- Stage 7: managed MySQL 8.4 and MariaDB 11.4, in the same databases table as
-- PostgreSQL and Redis (one container per database, dump-based backups).
ALTER TABLE databases DROP CONSTRAINT databases_engine_version;
ALTER TABLE databases ADD CONSTRAINT databases_engine_version CHECK (
    (engine = 'postgres' AND version IN ('16', '17')) OR
    (engine = 'redis' AND version = '7') OR
    (engine = 'mysql' AND version = '8.4') OR
    (engine = 'mariadb' AND version = '11.4'));
