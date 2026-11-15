-- Platform schema v1.
-- Roles platform_api (subject to row-level security) and platform_agent (BYPASSRLS)
-- must exist before this runs (see deploy/postgres-init/01-roles.sh).

-- ---------- plans ----------
CREATE TABLE plans (
    id              text PRIMARY KEY,
    name            text NOT NULL,
    max_services    int  NOT NULL,
    max_cpu_millis  int  NOT NULL,
    max_memory_mb   int  NOT NULL,
    max_domains     int  NOT NULL
);
INSERT INTO plans VALUES
    ('starter', 'Starter', 3, 1000, 1024, 3),
    ('pro',     'Pro',    10, 4000, 4096, 20);

-- ---------- identity (no RLS: accessed by user id, checked in code) ----------
CREATE TABLE users (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email            text NOT NULL UNIQUE CHECK (email = lower(email)),
    name             text NOT NULL,
    password_hash    text NOT NULL,
    totp_secret_enc  bytea,
    totp_enabled     boolean NOT NULL DEFAULT false,
    totp_last_step   bigint  NOT NULL DEFAULT 0,
    failed_logins    int     NOT NULL DEFAULT 0,
    locked_until     timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    token_hash  bytea PRIMARY KEY,
    user_id     uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    csrf        text NOT NULL,
    totp_ok     boolean NOT NULL DEFAULT false,
    ip          text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    last_seen   timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL
);
CREATE INDEX sessions_user ON sessions (user_id);

CREATE TABLE orgs (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,19}$'),
    name        text NOT NULL,
    plan_id     text NOT NULL REFERENCES plans,
    suspended   boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
    org_id      uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    user_id     uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    role        text NOT NULL CHECK (role IN ('owner', 'admin', 'developer', 'viewer')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id)
);

CREATE TABLE invites (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    email        text NOT NULL CHECK (email = lower(email)),
    role         text NOT NULL CHECK (role IN ('owner', 'admin', 'developer', 'viewer')),
    token_hash   bytea NOT NULL UNIQUE,
    created_by   uuid REFERENCES users ON DELETE SET NULL,
    expires_at   timestamptz NOT NULL,
    accepted_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- ---------- tenant resources (row-level security on org_id) ----------
CREATE TABLE projects (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    slug        text NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,19}$'),
    name        text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, slug)
);

CREATE TABLE services (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    project_id   uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
    slug         text NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,19}$'),
    image        text NOT NULL DEFAULT '',
    port         int  NOT NULL DEFAULT 8080 CHECK (port BETWEEN 1 AND 65535),
    cpu_millis   int  NOT NULL DEFAULT 250  CHECK (cpu_millis BETWEEN 50 AND 8000),
    memory_mb    int  NOT NULL DEFAULT 256  CHECK (memory_mb BETWEEN 32 AND 16384),
    health_path  text NOT NULL DEFAULT '/'  CHECK (health_path ~ '^/'),
    volume_path  text NOT NULL DEFAULT '',
    env          jsonb NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, slug)
);

CREATE TABLE secrets (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    service_id  uuid NOT NULL REFERENCES services ON DELETE CASCADE,
    name        text NOT NULL CHECK (name ~ '^[A-Z_][A-Z0-9_]{0,63}$'),
    value_enc   bytea NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (service_id, name)
);

CREATE TABLE deployments (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    service_id      uuid NOT NULL REFERENCES services ON DELETE CASCADE,
    revision        int  NOT NULL,
    spec            jsonb NOT NULL,
    status          text NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'deploying', 'live', 'failed', 'superseded')),
    message         text NOT NULL DEFAULT '',
    container_id    text,
    container_name  text,
    created_by      uuid REFERENCES users ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (service_id, revision)
);
CREATE INDEX deployments_status ON deployments (status);

CREATE TABLE domains (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    service_id    uuid NOT NULL REFERENCES services ON DELETE CASCADE,
    hostname      text NOT NULL UNIQUE CHECK (hostname ~ '^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$'),
    verify_token  text NOT NULL,
    verified_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['projects', 'services', 'secrets', 'deployments', 'domains'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format(
            'CREATE POLICY tenant_isolation ON %I
                USING (org_id = nullif(current_setting(''app.org_id'', true), '''')::uuid)
                WITH CHECK (org_id = nullif(current_setting(''app.org_id'', true), '''')::uuid)', t);
    END LOOP;
END $$;

-- ---------- audit log (append-only, hash-chained) ----------
CREATE TABLE audit_events (
    id          bigserial PRIMARY KEY,
    org_id      uuid,
    user_id     uuid,
    action      text NOT NULL,
    target      text NOT NULL DEFAULT '',
    detail      jsonb NOT NULL DEFAULT '{}',
    ip          text,
    created_at  timestamptz NOT NULL,
    prev_hash   bytea,
    hash        bytea NOT NULL
);
CREATE INDEX audit_org ON audit_events (org_id, id DESC);
ALTER TABLE audit_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_read ON audit_events FOR SELECT
    USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid);

-- Only way to write audit events: computes the chain under a lock.
CREATE FUNCTION audit_append(p_org uuid, p_user uuid, p_action text, p_target text, p_detail jsonb, p_ip text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE
    v_prev bytea;
    v_now  timestamptz := clock_timestamp();
BEGIN
    PERFORM pg_advisory_xact_lock(7300451);
    SELECT hash INTO v_prev FROM audit_events ORDER BY id DESC LIMIT 1;
    INSERT INTO audit_events (org_id, user_id, action, target, detail, ip, created_at, prev_hash, hash)
    VALUES (p_org, p_user, p_action, coalesce(p_target, ''), coalesce(p_detail, '{}'::jsonb), p_ip, v_now, v_prev,
            sha256(coalesce(v_prev, '\x'::bytea) || convert_to(concat_ws('|',
                p_org, p_user, p_action, coalesce(p_target, ''), coalesce(p_detail, '{}'::jsonb)::text, p_ip, v_now), 'UTF8')));
END $$;
REVOKE ALL ON FUNCTION audit_append(uuid, uuid, text, text, jsonb, text) FROM PUBLIC;

-- Answers "does this host serve a live application?" for on-demand TLS, without giving
-- the API cross-tenant read access. Returns only a boolean.
CREATE FUNCTION host_is_served(p_host text, p_apps_domain text)
RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT EXISTS (
        SELECT 1 FROM deployments d
        JOIN services s ON s.id = d.service_id
        JOIN projects p ON p.id = s.project_id
        JOIN orgs o ON o.id = d.org_id
        WHERE d.status = 'live' AND NOT o.suspended
          AND s.slug || '-' || p.slug || '-' || o.slug || '.' || p_apps_domain = p_host
    ) OR EXISTS (
        SELECT 1 FROM domains dm JOIN orgs o ON o.id = dm.org_id
        WHERE dm.hostname = p_host AND dm.verified_at IS NOT NULL AND NOT o.suspended
    );
$$;
REVOKE ALL ON FUNCTION host_is_served(text, text) FROM PUBLIC;

-- ---------- grants ----------
GRANT EXECUTE ON FUNCTION host_is_served(text, text) TO platform_api;
GRANT SELECT, INSERT, UPDATE, DELETE ON users, sessions, orgs, memberships, invites,
    projects, services, secrets, deployments, domains TO platform_api;
GRANT SELECT ON plans, audit_events TO platform_api;
GRANT EXECUTE ON FUNCTION audit_append(uuid, uuid, text, text, jsonb, text) TO platform_api, platform_agent;

GRANT SELECT ON orgs, plans, projects, services, secrets, domains TO platform_agent;
GRANT SELECT, UPDATE ON deployments TO platform_agent;
