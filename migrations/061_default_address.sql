-- A service with a verified custom domain can stop answering on its platform address
-- (<service>-<project>-<org>.<apps domain>): only its own domains are served. With no
-- verified domain left, the platform address answers again.
ALTER TABLE services ADD COLUMN default_address boolean NOT NULL DEFAULT true;
