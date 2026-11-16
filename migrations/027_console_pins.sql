-- Console: the products each user pins to the favorites bar and the console home, in
-- order (like the AWS console's favorites). Without rows, the console shows defaults.
CREATE TABLE user_pins (
    user_id    uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    service    text NOT NULL CHECK (service ~ '^[a-z]{2,20}$'),
    position   int  NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, service)
);
-- "customized": the user changed their pins at least once (an empty list then means none).
ALTER TABLE users ADD COLUMN pins_customized boolean NOT NULL DEFAULT false;

GRANT SELECT, INSERT, UPDATE, DELETE ON user_pins TO platform_api;
GRANT UPDATE (pins_customized) ON users TO platform_api;
