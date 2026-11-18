-- What the zone's delegation looks like from the internet: other name servers still listed
-- at the registrar (visitors then sometimes get their answers, without the platform's
-- records) or platform name servers missing. Empty when the delegation is complete.
ALTER TABLE dns_zones ADD COLUMN delegation text NOT NULL DEFAULT '' CHECK (length(delegation) <= 500);
GRANT UPDATE (delegation) ON dns_zones TO platform_agent;
