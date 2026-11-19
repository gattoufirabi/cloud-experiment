-- A mail domain can have its own server name (for example mail.theirshop.com): the
-- webmail answers there and mail apps connect there (IMAP, POP3, SMTP), each with its own
-- certificate. The name is inside the verified domain, so ownership is already proven.
ALTER TABLE mail_domains ADD COLUMN server_name text NOT NULL DEFAULT '';
CREATE UNIQUE INDEX mail_domains_server_name ON mail_domains (server_name) WHERE server_name <> '';

-- The agent serves these names (webmail route, certificates) for active mail domains of
-- organizations that are not suspended.
CREATE FUNCTION mail_server_names() RETURNS TABLE (server_name text, domain text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT md.server_name, md.domain FROM mail_domains md JOIN orgs o ON o.id = md.org_id
    WHERE md.server_name <> '' AND md.stalwart_id IS NOT NULL AND NOT o.suspended
$$;
REVOKE ALL ON FUNCTION mail_server_names() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mail_server_names() TO platform_agent, platform_api;
