-- Where a service secret came from when the platform wrote it ("database:<id>",
-- "queue:<id>", "bucket:<id>", "email"; empty for secrets typed by people), so the
-- project map can draw which service uses which resource without reading any value.
ALTER TABLE secrets ADD COLUMN source text NOT NULL DEFAULT '' CHECK (length(source) <= 80);
