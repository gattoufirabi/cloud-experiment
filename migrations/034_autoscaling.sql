-- Autoscaling (target tracking, like AWS): a service keeps its average CPU near a
-- target by running between a minimum and a maximum number of instances. The agent
-- measures every minute, scales up at once and down after a cool-down, each time with
-- a normal zero-downtime deployment.

ALTER TABLE services
    ADD COLUMN autoscale_min  int NOT NULL DEFAULT 0 CHECK (autoscale_min BETWEEN 0 AND 10),   -- 0 = off
    ADD COLUMN autoscale_max  int NOT NULL DEFAULT 0 CHECK (autoscale_max BETWEEN 0 AND 10),
    ADD COLUMN autoscale_cpu  int NOT NULL DEFAULT 70 CHECK (autoscale_cpu BETWEEN 10 AND 95),
    ADD COLUMN last_scaled_at timestamptz,
    ADD CONSTRAINT autoscale_range CHECK (autoscale_min = 0 OR (autoscale_min <= autoscale_max));

GRANT UPDATE (instances, last_scaled_at) ON services TO platform_agent;
