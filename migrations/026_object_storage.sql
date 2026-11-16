-- Stage 8: S3-compatible object storage (served by Garage). Buckets and access keys
-- belong to an organization. A bucket is named "<org>-<name>" in the shared S3
-- namespace; each has a size quota, and the quotas of an organization's buckets stay
-- within its plan. Removing a bucket or key (or the organization) leaves a row in
-- storage_trash; the agent empties and deletes it in Garage.

ALTER TABLE plans
    ADD COLUMN max_buckets                  int NOT NULL DEFAULT 10 CHECK (max_buckets >= 0),
    ADD COLUMN max_storage_mb               int NOT NULL DEFAULT 10240 CHECK (max_storage_mb >= 0),
    ADD COLUMN price_storage_gb_month_cents numeric(10,4) NOT NULL DEFAULT 0 CHECK (price_storage_gb_month_cents >= 0);
UPDATE plans SET max_buckets = 2, max_storage_mb = 1024 WHERE id = 'trial';
UPDATE plans SET max_buckets = 100, max_storage_mb = 102400 WHERE id = 'pro';
UPDATE plans SET price_storage_gb_month_cents = 2.0 WHERE id = 'starter';
UPDATE plans SET price_storage_gb_month_cents = 1.5 WHERE id = 'pro';

CREATE TABLE buckets (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id             uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    name               text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$'),
    full_name          text NOT NULL UNIQUE CHECK (full_name ~ '^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$'),
    garage_id          text NOT NULL UNIQUE,
    quota_mb           int  NOT NULL CHECK (quota_mb BETWEEN 1 AND 10485760),
    website            boolean NOT NULL DEFAULT false,
    index_document     text NOT NULL DEFAULT 'index.html' CHECK (index_document ~ '^[A-Za-z0-9._/-]{1,200}$'),
    error_document     text NOT NULL DEFAULT '' CHECK (error_document = '' OR error_document ~ '^[A-Za-z0-9._/-]{1,200}$'),
    -- the console's own key on this bucket (object browser, emptying before deletion)
    console_key_id     text NOT NULL,
    console_secret_enc bytea NOT NULL,
    objects            bigint NOT NULL DEFAULT 0,
    bytes              bigint NOT NULL DEFAULT 0,
    usage_at           timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);

CREATE TABLE storage_keys (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    name          text NOT NULL CHECK (length(name) BETWEEN 1 AND 60),
    access_key_id text NOT NULL UNIQUE,
    -- set when the key was made for a service by "Connect to a service"
    service_id    uuid REFERENCES services ON DELETE SET NULL,
    created_by    uuid REFERENCES users ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE storage_grants (
    org_id    uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    key_id    uuid NOT NULL REFERENCES storage_keys ON DELETE CASCADE,
    bucket_id uuid NOT NULL REFERENCES buckets ON DELETE CASCADE,
    can_write boolean NOT NULL DEFAULT false,
    PRIMARY KEY (key_id, bucket_id)
);
CREATE INDEX storage_grants_bucket ON storage_grants (bucket_id);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['buckets', 'storage_keys', 'storage_grants'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (org_id = nullif(current_setting('app.org_id', true), '')::uuid)
            WITH CHECK (org_id = nullif(current_setting('app.org_id', true), '')::uuid)$p$, t);
    END LOOP;
END $$;

-- What to remove from Garage. Filled by triggers, so deleting an organization (which
-- cascades) cleans its storage too.
CREATE TABLE storage_trash (
    id                 bigserial PRIMARY KEY,
    kind               text NOT NULL CHECK (kind IN ('bucket', 'key')),
    garage_id          text NOT NULL,             -- bucket ID or access key ID
    console_key_id     text NOT NULL DEFAULT '',
    console_secret_enc bytea,
    seal_context       text NOT NULL DEFAULT '',  -- context the console secret is sealed with
    attempts           int NOT NULL DEFAULT 0,
    last_error         text NOT NULL DEFAULT '',
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION storage_trash_bucket() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    INSERT INTO storage_trash (kind, garage_id, console_key_id, console_secret_enc, seal_context)
    VALUES ('bucket', OLD.garage_id, OLD.console_key_id, OLD.console_secret_enc, 'bucket:' || OLD.id);
    RETURN OLD;
END $$;
CREATE TRIGGER buckets_trash AFTER DELETE ON buckets FOR EACH ROW EXECUTE FUNCTION storage_trash_bucket();

CREATE FUNCTION storage_trash_key() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
    INSERT INTO storage_trash (kind, garage_id) VALUES ('key', OLD.access_key_id);
    RETURN OLD;
END $$;
CREATE TRIGGER storage_keys_trash AFTER DELETE ON storage_keys FOR EACH ROW EXECUTE FUNCTION storage_trash_key();

-- Caddy asks whether to get a certificate for <bucket>.web.<apps domain>.
CREATE FUNCTION bucket_website_served(p_name text) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
    SELECT EXISTS (SELECT 1 FROM buckets b JOIN orgs o ON o.id = b.org_id
                   WHERE b.full_name = p_name AND b.website AND NOT o.suspended);
$$;
REVOKE ALL ON FUNCTION bucket_website_served(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION bucket_website_served(text) TO platform_api;

-- Stored bytes per bucket and hour, for invoices.
ALTER TABLE usage_hourly DROP CONSTRAINT usage_hourly_kind_check;
ALTER TABLE usage_hourly ADD CONSTRAINT usage_hourly_kind_check CHECK (kind IN ('service', 'database', 'bucket'));
ALTER TABLE usage_hourly ADD COLUMN storage_bytes bigint NOT NULL DEFAULT 0;

GRANT SELECT, INSERT, UPDATE, DELETE ON buckets, storage_keys, storage_grants TO platform_api;
GRANT SELECT ON buckets, storage_keys, storage_grants TO platform_agent;
GRANT UPDATE (objects, bytes, usage_at) ON buckets TO platform_agent;
GRANT SELECT, UPDATE, DELETE ON storage_trash TO platform_agent;
