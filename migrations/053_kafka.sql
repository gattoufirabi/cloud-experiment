-- Apache Kafka (KRaft, single broker) as a managed streaming engine. Topics are a stream,
-- not a dataset: Kafka has no nightly backups.
ALTER TABLE databases DROP CONSTRAINT databases_engine_version;
ALTER TABLE databases ADD CONSTRAINT databases_engine_version CHECK (
    (engine = 'postgres' AND version IN ('16', '17')) OR
    (engine = 'redis' AND version = '7') OR
    (engine = 'mysql' AND version = '8.4') OR
    (engine = 'mariadb' AND version = '11.4') OR
    (engine = 'mongodb' AND version = '8.0') OR
    (engine = 'kafka' AND version = '3.9'));
ALTER TABLE databases ADD CONSTRAINT databases_kafka_no_backups CHECK (engine <> 'kafka' OR NOT backup_enabled);
