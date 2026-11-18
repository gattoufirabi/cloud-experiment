-- Lambda web apps: a function can run an ordinary HTTP server (Express, Flask,
-- FastAPI...) instead of a handler. The platform starts it with PORT set, forwards
-- function URL requests to it and POSTs other events to /events, like the AWS Lambda
-- Web Adapter. It scales to zero like any function.
ALTER TABLE functions
    ADD COLUMN web_command text NOT NULL DEFAULT '' CHECK (length(web_command) <= 300 AND web_command !~ '[\r\n]'),
    ADD COLUMN web_port int NOT NULL DEFAULT 8080 CHECK (web_port BETWEEN 1024 AND 65535);
