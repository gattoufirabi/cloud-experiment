-- SRV records (service location: mail autodiscovery, SIP, XMPP...). Value: "weight port
-- target"; the priority column holds the SRV priority.
ALTER TABLE dns_records DROP CONSTRAINT dns_records_type_check;
ALTER TABLE dns_records ADD CONSTRAINT dns_records_type_check CHECK (type IN ('A', 'AAAA', 'CNAME', 'TXT', 'MX', 'CAA', 'NS', 'SRV'));
