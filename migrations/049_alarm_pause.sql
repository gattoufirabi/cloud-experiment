-- Pausing a metric alarm resolves its open incident from the console.
GRANT UPDATE (status, resolved_at) ON alert_incidents TO platform_api;
