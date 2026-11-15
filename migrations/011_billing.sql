-- Usage metering and invoices. No payment provider: invoices are documents; the
-- operator records payments with `platform admin invoice-paid`.

-- Prices per plan, in cents of the installation's currency (BILLING_CURRENCY).
ALTER TABLE plans
    ADD COLUMN price_monthly_cents   int NOT NULL DEFAULT 0 CHECK (price_monthly_cents >= 0),
    ADD COLUMN price_vcpu_hour_cents numeric(10,4) NOT NULL DEFAULT 0 CHECK (price_vcpu_hour_cents >= 0),
    ADD COLUMN price_gb_hour_cents   numeric(10,4) NOT NULL DEFAULT 0 CHECK (price_gb_hour_cents >= 0),
    ADD COLUMN price_gb_out_cents    numeric(10,4) NOT NULL DEFAULT 0 CHECK (price_gb_out_cents >= 0);
-- Example prices; the operator sets real ones with `platform admin set-prices`.
UPDATE plans SET price_monthly_cents = 500,  price_vcpu_hour_cents = 1.0, price_gb_hour_cents = 0.5, price_gb_out_cents = 2.0 WHERE id = 'starter';
UPDATE plans SET price_monthly_cents = 2000, price_vcpu_hour_cents = 0.8, price_gb_hour_cents = 0.4, price_gb_out_cents = 1.5 WHERE id = 'pro';

-- Who the invoice is addressed to (edited by the organization's admins).
ALTER TABLE orgs
    ADD COLUMN billing_name    text NOT NULL DEFAULT '' CHECK (length(billing_name) <= 200),
    ADD COLUMN billing_address text NOT NULL DEFAULT '' CHECK (length(billing_address) <= 500),
    ADD COLUMN billing_tax_id  text NOT NULL DEFAULT '' CHECK (length(billing_tax_id) <= 50);

-- One row per metered minute: makes metering idempotent across agent restarts.
CREATE TABLE meter_ticks (minute timestamptz PRIMARY KEY);

-- Allocated resources and measured network traffic, per resource and hour.
CREATE TABLE usage_hourly (
    org_id            uuid NOT NULL REFERENCES orgs ON DELETE CASCADE,
    resource_id       uuid NOT NULL,
    kind              text NOT NULL CHECK (kind IN ('service', 'database')),
    name              text NOT NULL,          -- "project/service" when metered
    hour              timestamptz NOT NULL,
    cpu_milli_minutes bigint NOT NULL DEFAULT 0,
    mem_mb_minutes    bigint NOT NULL DEFAULT 0,
    egress_bytes      bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (resource_id, hour)
);
CREATE INDEX usage_org_hour ON usage_hourly (org_id, hour);

-- Invoices are kept even if the organization is deleted (accounting records).
CREATE SEQUENCE invoice_seq;
CREATE TABLE invoices (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id         uuid NOT NULL,
    number         text NOT NULL UNIQUE,
    period_start   date NOT NULL,
    period_end     date NOT NULL,              -- exclusive
    currency       text NOT NULL,
    seller         text NOT NULL,
    buyer          text NOT NULL,
    plan_name      text NOT NULL,
    subtotal_cents bigint NOT NULL,
    tax_percent    numeric(5,2) NOT NULL,
    tax_cents      bigint NOT NULL,
    total_cents    bigint NOT NULL,
    status         text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'paid', 'void')),
    issued_at      timestamptz NOT NULL DEFAULT now(),
    paid_at        timestamptz,
    UNIQUE (org_id, period_start)
);
CREATE TABLE invoice_lines (
    invoice_id       uuid NOT NULL REFERENCES invoices ON DELETE CASCADE,
    org_id           uuid NOT NULL,
    position         int NOT NULL,
    description      text NOT NULL,
    quantity         numeric(16,4) NOT NULL,
    unit             text NOT NULL,
    unit_price_cents numeric(12,4) NOT NULL,
    amount_cents     bigint NOT NULL,
    PRIMARY KEY (invoice_id, position)
);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['usage_hourly', 'invoices', 'invoice_lines'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY tenant_isolation ON %I
            USING (org_id = nullif(current_setting(''app.org_id'', true), '''')::uuid)
            WITH CHECK (org_id = nullif(current_setting(''app.org_id'', true), '''')::uuid)', t);
    END LOOP;
END $$;

GRANT SELECT ON usage_hourly, invoices, invoice_lines TO platform_api;
GRANT SELECT, INSERT, UPDATE, DELETE ON meter_ticks, usage_hourly TO platform_agent;
GRANT SELECT, INSERT ON invoices, invoice_lines TO platform_agent;
GRANT SELECT ON databases, deployments TO platform_agent;
GRANT USAGE ON SEQUENCE invoice_seq TO platform_agent;
