# Admin guide

For operators who install and run the platform. Commands assume the production stack in `deploy/compose.prod.yml` on a Debian 13 server, run from `/opt/platform/deploy`.

## How the platform is built

All platform services run as containers on one server. Only the agent can change what runs, and it is never reachable from the internet; the internet-facing API can only record requests in the database.

| Service | Role | Privileges |
| --- | --- | --- |
| caddy | Front door: TLS, routes to the console, the registry and client apps. Ports 80 and 443 only. | Binds ports; admin API on a private socket shared with the agent. |
| api | Console and API, registry tokens, internal port 8081 for TLS approval and registry webhooks. | Unprivileged, read-only, no Docker access. |
| postgres | Desired state. Row-level security limits the API to one organization per request. | Three roles: owner, API, agent. |
| registry | Per-organization image spaces; checks tokens signed by the API. | Unprivileged. |
| victoria-metrics / logs | Metrics and logs, readable only through the API's tenant filter. | Unprivileged. |
| trivy | Vulnerability database for image scans. | Unprivileged. |
| agent | Re-checks every spec, deploys, updates Caddy, runs scans and backups in short-lived containers, health probes, alerts. | Root and Docker socket; no open port. |

Client applications run under gVisor, each project on its own network (a /24 from `10.208.0.0/12`), with no route to other clients, private ranges or the host. Managed PostgreSQL and Redis databases are containers on the same project networks (`pd-<id>`), with their data in `pvdb-<id>` volumes and their dumps in `pbdb-<id>` volumes; a deleted database's volumes are kept until you remove them.

## Install a server

A fresh Debian 13 server becomes a running platform in about an hour. Do not onboard clients until the Phase 0 isolation test passes.

You need a Debian 13 server (8 vCPU and 32 GB RAM fit roughly 40 to 80 small client services), your SSH key, and three DNS records pointing to it: the console (`{{console}}`), the registry (`{{registry}}`) and a wildcard for apps (`*.{{apps}}`). Use a separate domain for client apps.

**Quickest:** point the DNS records at the server, then run `sudo bash host/deploy.sh --console {{console}} --apps {{apps}} --registry {{registry}}` from a copy of the code. It does steps 1 to 7 below, stops if the isolation gate fails, and is safe to re-run (`--help` lists the options). The steps, by hand:

1. **Harden the host** with `demo-env/scripts/bootstrap.sh`: SSH by key only, default-deny firewall, WireGuard, automatic security updates.
2. **Copy the code** to `/opt/platform`.
3. **Install gVisor:** `bash /opt/platform/host/install-gvisor.sh`
4. **Enable the tenant firewall:** copy `host/platform-tenant-firewall.service` to `/etc/systemd/system/`, then `systemctl enable --now platform-tenant-firewall`.
5. **Run the Phase 0 gate:** `bash /opt/platform/host/isolation-test.sh` must end with `Phase 0 gate: PASSED`.
6. **Configure:** copy `deploy/.env.example` to `deploy/.env` (mode 600) and fill in the hosts, the three database passwords, the registry and Trivy secrets, and `BACKUP_REST_URL`.
7. **Create the keys and start:**

```
mkdir -p secrets /srv/platform/{postgres,caddy,vlogs,vmdata,registry,trivy}
docker compose -f compose.prod.yml build
docker run --rm --user 0 -v "$PWD/secrets:/out" platform:local admin init-key --file /out/master.key
docker run --rm --user 0 -v "$PWD/secrets:/out" platform:local admin init-registry-key --file /out/registry-token.key --cert /out/registry-token.crt
chmod 444 secrets/master.key secrets/registry-token.key
docker compose -f compose.prod.yml up -d
```

8. **Back up `secrets/` and `.env` offline.** Without the master key, client secrets, two-step keys and backups cannot be decrypted.
9. **Check** that the console shows its login page with a valid certificate.

## Onboard a client

A client starts as an organization with a plan and one owner invitation; its own admins then invite the team.

1. **Create the organization:**

```
docker compose -f compose.prod.yml exec api /platform admin create-org --slug acme --name "Acme Ltd" --plan starter --owner-email owner@acme.example
```

2. **Send the printed invite link privately.** It works once and expires in 7 days. Create a new one with `admin invite --org acme --email owner@acme.example --role owner`.
3. **Optionally** set `set-scan-policy --slug acme --policy block-critical`.
4. **Give the client** the console address, the registry host, the CNAME target for custom domains, and the [User guide](/docs/user).

| Plan | Services | CPU in total | Memory in total | Custom domains |
| --- | --- | --- | --- | --- |
| starter | 3 | 1000 millicores | 1024 MB | 3 |
| pro | 10 | 4000 millicores | 4096 MB | 20 |

Plans live in the `plans` table. Move a client with `set-plan`; lowering a plan does not stop running services but blocks changes that would exceed it.

## Operator commands

Run each as `docker compose -f compose.prod.yml exec api /platform admin <command>`.

| Command | What it does |
| --- | --- |
| create-org --slug S --name N --owner-email E [--plan P] | Creates an organization and prints the owner's invite link. |
| invite --org S --email E [--role R] | Prints a new invite link for an existing organization. |
| set-plan --slug S --plan P | Moves an organization to another plan. |
| set-scan-policy --slug S --policy warn or block-critical | Only reports, or refuses, images with critical vulnerabilities. |
| suspend-org --slug S [--undo] | Stops all of an organization's applications and blocks changes; --undo restores them. |
| backup-key --slug S | Prints the organization's backup repository and password, for disaster recovery. |
| init-key, init-registry-key | Create the master key and the registry signing key. Installation only. |
| totp-code --secret K | Prints the current two-step code for a key. Testing only. |

Creating, suspending, scan-policy changes and key exports are written to the audit log. Useful commands: `docker compose -f compose.prod.yml ps`, `docker compose -f compose.prod.yml logs -f agent api`, and `docker compose -f compose.prod.yml exec postgres psql -U platform_owner platform`.

## Security operations

The platform enforces isolation by itself; the operator reacts to abuse, helps locked-out users and guards the keys.

- **Suspend an abusive or unpaid client:** `suspend-org --slug acme` stops its containers within seconds, removes its routes and blocks changes; `--undo` restores everything.
- **Unlock an account:** it unlocks by itself after 15 minutes, or immediately with `UPDATE users SET failed_logins = 0, locked_until = NULL WHERE email = '…';`.
- **Reset two-step login** (lost phone), after verifying the person's identity out of band:

```
UPDATE users SET totp_enabled = false, totp_secret_enc = NULL, totp_last_step = 0 WHERE email = 'person@example.com';
DELETE FROM sessions WHERE user_id = (SELECT id FROM users WHERE email = 'person@example.com');
```

| Secret | If lost | If leaked |
| --- | --- | --- |
| secrets/master.key | Client secrets, two-step keys and backup keys are unreadable: restore it from the offline copy. | Treat client secrets as exposed and ask clients to rotate them. |
| secrets/registry-token.key | Generate a new pair, restart the API and registry. | Anyone could forge registry access: rotate immediately. |
| .env passwords and secrets | Set new ones and restart. | Rotate and restart. |

- **Lost passkey:** the person can still log in with password and code and remove it under Account. To remove all of someone's passkeys: `DELETE FROM passkeys WHERE user_id = (SELECT id FROM users WHERE email = 'person@example.com');`. Passkeys are bound to `CONSOLE_HOST`: changing the console address makes existing passkeys stop working, and users must add new ones.
- **Self-signup** (`SIGNUP_ENABLED=true`, needs `SMTP_*`): new organizations start on the `trial` plan (`SIGNUP_TRIAL_DAYS`, default 14). Abuse controls: honeypot, 5 signups per IP per hour, `SIGNUP_DAILY_LIMIT` per day, a browser proof-of-work (`SIGNUP_POW_BITS`, default 16, no third-party CAPTCHA), disposable domains refused (built-in list plus `SIGNUP_BLOCKED_DOMAINS`; or only `SIGNUP_ALLOWED_DOMAINS`), and a confirmation link that needs a click. The agent suspends expired trials (owners are emailed) and trial organizations whose containers stay above `ANOMALY_CPU_PERCENT` (90) of their CPU limit for `ANOMALY_MINUTES` (30); paid organizations only get an operator notice. Notices go to `OPERATOR_NTFY_URL`. Review with `platform admin trials`; upgrade with `set-plan --plan starter` (lifts a trial-ended suspension), restore an anomaly suspension with `suspend-org --undo`. For email, use your own SMTP server (Postfix, Stalwart, Mailcow…) with SPF, DKIM and DMARC set up, or a relay.
- **Builds from git** (`BUILDS_ENABLED=true`, needs the registry): the agent runs a git container (no capabilities except CHOWN) and a rootless BuildKit container (non-root; only SETUID/SETGID for its user namespace, so `no-new-privileges` cannot be set) on the network `pt-build`, which the tenant firewall's `pt-*` rules cover, with inter-container traffic off. Images are pushed as `builder/<org>`, which can push only to that organization's namespace. Limits: `BUILD_CPU_MILLIS`, `BUILD_MEMORY_MB`, `BUILD_TIMEOUT_MINUTES`, `BUILD_MAX_IMAGE_MB`. Per-service caches live in volumes `pbcache-*` (safe to delete). **Before enabling in production**, run a build with `BUILD_RUNTIME=runsc` and confirm rootless BuildKit works under gVisor on your kernel (verified on Debian 12; there plain runc refuses rootless BuildKit, so `runsc` is required anyway). Build minutes are not billed.
- **Preview environments** are hidden service rows (`services.preview_of`). The agent deletes expired ones every minute (audited as `preview.expired`); their containers and routes go away at the next reconcile. With ACME, each preview address gets its own certificate on demand, so wildcard DNS for the apps domain must cover two levels (`*.apps.example.com` does).
- **Traffic protection** runs in Caddy, built with two open-source modules (`deploy/caddy/Dockerfile`: Coraza WAF with the OWASP Core Rule Set, and caddy-ratelimit). All services with the WAF on share one rule-set instance, so Caddy needs about 100 MB more once any client turns it on. Rate limits are kept in memory and reset when Caddy restarts.
- **Status pages** are opt-in per project and read only through restricted database functions; a suspended client's pages return "not found". Uptime history older than 90 days is pruned by the agent.

The full audit log, including logins, is in the `audit_events` table; each row is chained to the previous one by a hash.

## Backups and disaster recovery

Client data is backed up by the platform; the platform's own state must be backed up by the operator.

- **Client backups, local mode** (`BACKUP_REST_URL` empty): repositories in the `platform-backups` volume on the same server. For testing only.
- **Client backups, off-site:** run rest-server on a machine you own, connect it over WireGuard, set `BACKUP_REST_URL=rest:http://platform:<password>@10.8.0.3:8000`. Retention: `BACKUP_KEEP`.
- **Platform backups:** nightly `pg_dump` of the database, plus `deploy/.env`, `deploy/secrets/` and, if needed, `/srv/platform/registry`.

To restore the platform on a new server:

1. Install the server (steps 1 to 5 of Install a server).
2. Put back `.env` and `secrets/`, then start the database: `docker compose -f compose.prod.yml up -d postgres`.
3. Load the dump: `docker compose -f compose.prod.yml exec -T postgres psql -U platform_owner platform < platform.sql`.
4. Start everything. The agent redeploys every live service by itself.
5. Restore services' persistent folders from their latest snapshots, from the console or with `backup-key` and plain restic.
6. Point DNS at the new server if its IP changed.

## Billing

Usage is metered by the agent every minute (allocated CPU and memory of live services and running databases, network out of services) into `usage_hourly`. With `BILLING_ENABLED=true`, the agent issues the previous month's invoices on the 1st (UTC). Invoices are documents only: there is no payment provider.

- **Prices** live on each plan, in cents (decimals allowed): `platform admin set-prices --plan starter --monthly 500 --vcpu-hour 1 --gb-hour 0.5 --gb-out 2`. New prices apply to invoices issued afterwards.
- **Seller block, currency and tax:** `BILLING_SELLER` (lines separated by `|`), `BILLING_CURRENCY` (default EUR) and `BILLING_TAX_PERCENT` in `.env`.
- **Issue invoices by hand** (catch-up after downtime, safe to rerun): `platform admin generate-invoices --month 2026-10`.
- **Record a payment:** `platform admin invoice-paid --number INV-2026-000042`; cancel one with `invoice-void`. Both are audited.
- Organizations with nothing to pay get no invoice. Invoices are kept when an organization is deleted. Minutes when the agent was down are not billed.

## Upgrades and maintenance

Client containers keep running while the platform services restart. Only an update of Caddy pauses client traffic, for up to a minute until the agent reconnects it.

1. Back up the database.
2. `cd /opt/platform && git pull`
3. `cd deploy && docker compose -f compose.prod.yml up -d --build` (migrations run automatically).
4. Check `docker compose -f compose.prod.yml ps` and the console.

| When | Task |
| --- | --- |
| Monthly | Bump the pinned third-party images after reading their release notes, then upgrade. |
| Monthly | Check disk space and remove unused images with `docker image prune -a`. |
| Quarterly | Rehearse a platform restore on a spare machine. |
| After any host change | Re-run `host/isolation-test.sh`. |

Deleted services keep their volume (`pv-<service id>`); list them with `docker volume ls --filter name=pv-` and remove those no longer needed.

## Monitoring the platform

Clients' alerts cover their applications; watch the server and the platform services separately.

- **Platform logs** are JSON: `docker compose -f compose.prod.yml logs -f agent api`. Warnings to act on: `push caddy config`, `attach to project network`, `notification failed`, `ship logs`, `scan failed`.
- **Server health:** watch disk, memory and CPU with an external checker, for example the `demo-env` stack (Gatus and ntfy).
- **Capacity:** keep allocated CPU and memory across clients under about 80% of the server.
- **Abuse:** clients with many `cpu_high` incidents or abuse reports about your IP can be suspended immediately.

## Configuration reference

| Variable | Default | Purpose |
| --- | --- | --- |
| CONSOLE_HOST, APPS_DOMAIN, REGISTRY_HOST | — | Console address, app domain, registry address. |
| RUNTIME | runsc | Container runtime for client apps; anything else needs ALLOW_INSECURE_RUNTIME=true (development only). |
| TLS_MODE | off | acme in production. |
| MAX_CPU_MILLIS, MAX_MEMORY_MB | 4000, 8192 | Largest single client container. |
| SCANNER_ENABLED | false | Scan images with Trivy. |
| BACKUPS_ENABLED, BACKUP_REST_URL, BACKUP_KEEP | false, empty, 7/4/6 | Client backups, off-site target, retention. |
| ALERTS_ENABLED, ALERT_INTERVAL_SECONDS | false, 30 | Alerts and health-check period. |
| ALERT_ALLOW_PRIVATE_TARGETS | false | Development only. Never in production. |
| COOKIE_SECURE | false | true in production. |

## Admin troubleshooting

| Symptom | Fix |
| --- | --- |
| Agent refuses to start: RUNTIME must be runsc | Run host/install-gvisor.sh, or set ALLOW_INSECURE_RUNTIME=true on a development machine only. |
| Deployments stay pending | Check `docker compose -f compose.prod.yml logs agent` and restart it; pending deployments resume. |
| "No application is published at this address" | The service has no live deployment, its organization is suspended, or the address is misspelled. |
| Apps unreachable right after a Caddy restart | Wait up to a minute: the agent re-attaches it. |
| Certificate errors on a client domain | Only verified domains get certificates; check DNS and Caddy logs. |
| docker push denied | Wrong, pull-only, expired or revoked token, or another organization's path. |
| Scans fail | Check `docker compose -f compose.prod.yml logs trivy`; block-critical organizations cannot deploy meanwhile. |
| Backup jobs fail | Check the job message and the rest-server (WireGuard, disk). |
| Alerts not delivered | The channel's last delivery line gives the reason. |

## Known limits

- **One server:** a hardware failure or a large traffic attack takes every client down until it is repaired or restored.
- **Backups are crash-consistent:** databases inside client containers need their own dumps.
- **Verify on the real server:** gVisor, the tenant firewall and Let's Encrypt were tested only by scripts on a development machine.
- **Not built yet:** operator dashboard, two-step reset command and passkeys, API tokens and CLI for clients, email notifications, managed databases, disk quotas, builds from source, billing, several servers.
