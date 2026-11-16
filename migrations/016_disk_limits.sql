-- Disk limits per plan. The agent measures each organization's persistent storage
-- (service folders, database data and dump volumes) every 15 minutes; owners are
-- alerted at 90% and 100%, and above 110% new deployments and databases are refused.
-- Running applications are never stopped for disk use (that would risk their data).
ALTER TABLE plans ADD COLUMN max_disk_mb int NOT NULL DEFAULT 10240 CHECK (max_disk_mb > 0);
UPDATE plans SET max_disk_mb = 51200 WHERE id = 'pro';
UPDATE plans SET max_disk_mb = 2048 WHERE id = 'trial';

ALTER TABLE orgs
    ADD COLUMN disk_used_mb     int NOT NULL DEFAULT 0,
    ADD COLUMN disk_measured_at timestamptz,
    ADD COLUMN disk_alert_level int NOT NULL DEFAULT 0;   -- last level alerted: 0, 90, 100

ALTER TABLE alert_incidents DROP CONSTRAINT alert_incidents_kind_check;
ALTER TABLE alert_incidents ADD CONSTRAINT alert_incidents_kind_check
    CHECK (kind IN ('service_down', 'cpu_high', 'memory_high', 'deploy_failed', 'backup_failed',
                    'critical_vulns', 'test', 'disk_quota'));

GRANT UPDATE (disk_used_mb, disk_measured_at, disk_alert_level) ON orgs TO platform_agent;
