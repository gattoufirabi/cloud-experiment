-- Web pages allowed to use a bucket from the browser (CORS): origins such as
-- https://shop.example.com. The platform applies them with its own key for the bucket,
-- which therefore has owner rights on it (customers' keys never do).
ALTER TABLE buckets ADD COLUMN cors_origins text[] NOT NULL DEFAULT '{}'
    CHECK (cardinality(cors_origins) <= 20);
