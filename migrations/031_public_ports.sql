-- Non-HTTP services: a service can expose TCP and UDP ports (game servers, MQTT, SMTP
-- relays, databases you run yourself...). Each gets a public port of the platform's
-- range, unique per protocol, reachable at <service>-<project>-<org>.<apps domain>.

ALTER TABLE services ADD COLUMN ports jsonb NOT NULL DEFAULT '[]';
ALTER TABLE plans ADD COLUMN max_public_ports int NOT NULL DEFAULT 2 CHECK (max_public_ports >= 0);
UPDATE plans SET max_public_ports = 0 WHERE id = 'trial';
UPDATE plans SET max_public_ports = 10 WHERE id = 'pro';

CREATE TABLE public_ports (
    public_port    int  NOT NULL CHECK (public_port BETWEEN 1 AND 65535),
    protocol       text NOT NULL CHECK (protocol IN ('tcp', 'udp')),
    org_id         uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    service_id     uuid NOT NULL REFERENCES services ON DELETE CASCADE,
    container_port int  NOT NULL CHECK (container_port BETWEEN 1 AND 65535),
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (public_port, protocol),
    UNIQUE (service_id, container_port, protocol)
);
ALTER TABLE public_ports ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON public_ports
    USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid);
-- Allocation must see every organization's ports (only the numbers, through this function).
CREATE FUNCTION public_port_taken(p_port int, p_protocol text) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT EXISTS (SELECT 1 FROM public_ports WHERE public_port = p_port AND protocol = p_protocol);
$$;
REVOKE ALL ON FUNCTION public_port_taken(int, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public_port_taken(int, text) TO platform_api;

GRANT SELECT, INSERT, UPDATE, DELETE ON public_ports TO platform_api;
GRANT SELECT ON public_ports TO platform_agent;
