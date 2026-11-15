#!/bin/sh
# Runs once, on first database start. Creates the two application roles:
#   platform_api   - subject to row-level security (tenant isolation in the database)
#   platform_agent - BYPASSRLS, used only by the agent, which is never internet-facing
set -eu
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<EOSQL
CREATE ROLE platform_api LOGIN PASSWORD '${API_DB_PASSWORD}';
CREATE ROLE platform_agent LOGIN BYPASSRLS PASSWORD '${AGENT_DB_PASSWORD}';
GRANT CONNECT ON DATABASE ${POSTGRES_DB} TO platform_api, platform_agent;
GRANT USAGE ON SCHEMA public TO platform_api, platform_agent;
EOSQL
