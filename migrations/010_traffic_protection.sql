-- Per-service traffic protection, enforced by the proxy (Caddy):
--   waf_enabled     OWASP Core Rule Set via Coraza; blocked requests get 403
--   rate_limit_rpm  requests per minute per visitor IP; above it, 429
--   blocked_ips     addresses / ranges refused with 403 before anything else
ALTER TABLE services
    ADD COLUMN waf_enabled    boolean NOT NULL DEFAULT false,
    ADD COLUMN rate_limit_rpm int CHECK (rate_limit_rpm BETWEEN 10 AND 100000),
    ADD COLUMN blocked_ips    cidr[] NOT NULL DEFAULT '{}' CHECK (cardinality(blocked_ips) <= 100);
