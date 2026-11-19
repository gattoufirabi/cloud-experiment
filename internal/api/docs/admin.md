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
| stalwart | Mail server of hosted mailboxes: SMTP 25 (incoming), 587 (submission), IMAP 993, POP3 995, spam filter, DKIM signing. Its admin API (8080) is reached only by the API and the agent on the compose network. | Unprivileged (uid 2000), may bind ports below 1024; data in `/srv/platform/stalwart`. |
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
mkdir -p secrets /srv/platform/{postgres,caddy,vlogs,vmdata,registry,trivy,stalwart}
chown 2000:2000 /srv/platform/stalwart   # the mail server's user
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

## Hosted mailboxes (mail server)

Organizations can host mailboxes on their own domains. The mail server is Stalwart (open source), started by the production compose file as `platform-stalwart`; the platform drives it through its admin API and is the source of truth for domains and mailboxes.

- **Set up:** set `STALWART_ADMIN_PASSWORD` in `.env` (`openssl rand -hex 24`), create `/srv/platform/stalwart` owned by uid 2000, open ports 25, 587, 993 and 995 in the host firewall, and start the stack. The agent then names the server after the mail host (`MAIL_HOSTNAME`, by default the console host), adds the submission listener on port 587 (restarting the mail server once) and installs the mail host's certificate from the proxy, again whenever the proxy renews it.
- **Deliverability:** the server's reverse DNS (PTR) must name the mail host, and the mail host must resolve to the server. Outgoing port 25 must be open at the hosting provider.
- **Domains** go on the mail server only once their organization has verified them (a domain on the mail server is local for every mailbox). The agent removes domains of deleted organizations and turns off those of suspended ones.
- **Limits:** plans set the number of mailboxes and the total mailbox storage (**Plans and prices**).
- **Backups:** mail is in `/srv/platform/stalwart`; include it in the host backups.

## Several servers (nodes and clusters)

One server runs everything (the **primary**). Add **worker nodes** for capacity: each runs only the agent and the projects placed on it. A project is placed on one node with all its services and databases, so a project's private network never spans servers.

1. **Connect the servers with WireGuard** (a private network between them). Note each server's WireGuard address, for example 10.8.0.1 (primary) and 10.8.0.2 (new node).
2. **On the primary**, add `CONTROL_PLANE_ADDRESS=10.8.0.1` to `deploy/.env` and run `docker compose -f compose.prod.yml up -d`: the database, logs, metrics and scanner then also listen on that private address (and only there).
3. **On the primary**, create the join bundle: `sudo bash /opt/platform/host/node-bundle.sh --node-id node-2 --node-address 10.8.0.2 [--cluster eu-west]`. It contains secrets: copy it over SSH only.
4. **On the new server** (a copy of the code in /opt/platform): `sudo bash host/deploy.sh --worker --bundle <bundle>`. It hardens the server, installs gVisor and the tenant firewall, runs the isolation gate, then starts the agent.
5. The node appears in the operator console (**Nodes**) within a minute. New projects go to the online, uncordoned node with the most free memory.

- **Clusters:** a cluster is a group of nodes (for example a region). Set a node's cluster in the Nodes tab; pin an organization to a cluster on its operator page so its new projects only go there.
- **Cordon** a node to stop placing new projects on it (running ones stay). **Drain** it to also move its projects without persistent data to other nodes (each app restarts once, with no gap: the old container serves until the new one is live).
- **Moving projects with data:** tick **also move projects with data** when draining. For each such project the node stops it, snapshots every persistent folder and database data volume into the organization's encrypted backup repository, and hands it to the least loaded node of its cluster, which restores the volumes and starts it. The project is down while its data is copied (seconds to minutes, depending on size); the **Moves** list on the Nodes tab shows each step. A move that fails five times gives the project back to its source node, whose volumes were never changed. Moves need a backup repository every node reaches: set `BACKUP_REST_URL` (the local backup volume only works for nodes on one Docker host). The source node keeps its copy of the volumes until you clean them up.
- Apps on workers are published only on the worker's WireGuard address; the proxy on the primary reaches them there. Builds, scans, billing and alert delivery run on the primary.

## Operator console

Operators can manage the platform from the browser at `/operator`: a live overview (organizations, users, live services, trials, suspensions, open incidents and invoices, builds, storage), every organization with its owner, plan, services and disk use (change the plan, suspend or restore), all invoices (mark paid or cancel) and the global audit log.

- **Nodes:** each server's CPU load, memory, disk, containers, versions, the health of every platform service, the projects and resources placed on it; cordon, drain, set its cluster.
- **Usage:** every organization's allocated and measured CPU and memory, CPU- and memory-hours, network out, build minutes and disk this month. Each organization's page lists its services, databases, domains and incidents (titles) with the node they run on.
- **Admin panel:** a menu at the top of every operator page leads to Overview, Users, Resources, Plans and prices, Health and Settings.
  - **Settings** (`/operator/settings`): an announcement shown on every console page (information or warning), **pause deployments** for maintenance (new deployments wait in the queue and start on resume; running applications are untouched), and self-signup (open or closed, sign-ups per day, trial length, allowed and refused email domains). Each signup setting can stay on the server's value (environment) or be overridden here, without a restart.
  - **Plans and prices** (`/operator/plans`): every limit and price of each plan, editable; create a plan as a copy of another; delete an unused plan (the built-in trial, starter and pro stay).
  - **Resources** (`/operator/resources`): every service, database, function, bucket, domain, queue and email domain of every organization, filtered by kind or searched; names, sizes and states only.
  - **Health** (`/operator/health`): deployments and builds waiting or failed, backups, emails queued or failed, queue messages, function errors, incidents, server heartbeats and the failures of the last 24 hours.
  - **Invoices:** issue a past month's invoices from the Invoices tab (otherwise automatic on the 1st).
- **Users** (`/operator/users`): every account with its organizations, two-step login, status and last sign-in; search and filter (operators, disabled, locked out, without two-step login).
  - **Invite a user** into an existing organization with a role, or into a new organization they will own (with its plan). The person gets a one-time link (shown once, and emailed when a mail server is configured) and chooses their own password; someone who already has an account is added at once. Pending invitations can be revoked.
  - On a user's page: **disable** (signed out at once, cannot sign in by any means; the reason is kept) and enable, **sign out everywhere**, **unlock** after too many failed sign-ins, create a **password reset link**, **reset two-step login** (optionally removing passkeys), edit the name and sign-in email, add to an organization, change a role or remove a membership (an organization always keeps one owner), grant or revoke **operator** rights (operators need two-step login), and **delete** an account that is not the only owner of an organization. The Activity tab lists the last 100 events made by or about the account.
  - An operator cannot disable, sign out, reset or delete their own account, nor revoke their own rights: another operator does it.
- **No intrusion:** operator pages never show secrets, environment values, log lines or stored data, and operators cannot act inside a client's applications. Opening an organization's page is itself recorded in the audit log.
- Give access with `make-operator --email person@example.com` (remove it with `--undo`). The person needs an account first and a full two-step login session; everyone else gets "not found".
- Every change made here is audited under the operator's name. Rarer tasks (prices, invoices by month) stay on the command line below.

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
- **Lost phone:** users log in with one of their recovery codes. If they have none left, verify their identity out of band, then `reset-totp --email person@example.com`: they set up two-step login again at their next login.
- **Forgotten password:** users get a reset link by email from the login page (needs `SMTP_*`). Otherwise `reset-password --email person@example.com` prints a one-hour link to send privately. A reset ends all their sessions; two-step login is still required.

| Secret | If lost | If leaked |
| --- | --- | --- |
| secrets/master.key | Client secrets, two-step keys and backup keys are unreadable: restore it from the offline copy. | Treat client secrets as exposed and ask clients to rotate them. |
| secrets/registry-token.key | Generate a new pair, restart the API and registry. | Anyone could forge registry access: rotate immediately. |
| .env passwords and secrets | Set new ones and restart. | Rotate and restart. |

- **Lost passkey:** the person can still log in with password and code and remove it under Account. To remove all of someone's passkeys: `DELETE FROM passkeys WHERE user_id = (SELECT id FROM users WHERE email = 'person@example.com');`. Passkeys are bound to `CONSOLE_HOST`: changing the console address makes existing passkeys stop working, and users must add new ones.
- **Self-signup** (`SIGNUP_ENABLED=true`, needs `SMTP_*`): new organizations start on the `trial` plan (`SIGNUP_TRIAL_DAYS`, default 14). Abuse controls: honeypot, 5 signups per IP per hour, `SIGNUP_DAILY_LIMIT` per day, a browser proof-of-work (`SIGNUP_POW_BITS`, default 16, no third-party CAPTCHA), disposable domains refused (built-in list plus `SIGNUP_BLOCKED_DOMAINS`; or only `SIGNUP_ALLOWED_DOMAINS`), and a confirmation link that needs a click. The agent suspends expired trials (owners are emailed) and trial organizations whose containers stay above `ANOMALY_CPU_PERCENT` (90) of their CPU limit for `ANOMALY_MINUTES` (30); paid organizations only get an operator notice. Notices go to `OPERATOR_NTFY_URL`. Review with `platform admin trials`; upgrade with `set-plan --plan starter` (lifts a trial-ended suspension), restore an anomaly suspension with `suspend-org --undo`. For email, use your own SMTP server (Postfix, Stalwart, Mailcow…) with SPF, DKIM and DMARC set up, or a relay.
- **Builds from git** (`BUILDS_ENABLED=true`, needs the registry): the agent runs a git container (no capabilities except CHOWN) and a rootless BuildKit container (non-root; only SETUID/SETGID for its user namespace, so `no-new-privileges` cannot be set) on the network `pt-build`, which the tenant firewall's `pt-*` rules cover, with inter-container traffic off. Images are pushed as `builder/<org>`, which can push only to that organization's namespace. Limits: `BUILD_CPU_MILLIS`, `BUILD_MEMORY_MB`, `BUILD_TIMEOUT_MINUTES`, `BUILD_MAX_IMAGE_MB`. Per-service caches live in volumes `pbcache-*` (safe to delete). **Before enabling in production**, run a build with `BUILD_RUNTIME=runsc` and confirm rootless BuildKit works under gVisor on your kernel (verified on Debian 12; there plain runc refuses rootless BuildKit, so `runsc` is required anyway). Build minutes are not billed.
- **Preview environments** are hidden service rows (`services.preview_of`). The agent deletes expired ones every minute (audited as `preview.expired`); their containers and routes go away at the next reconcile. With ACME, each preview address gets its own certificate on demand, so wildcard DNS for the apps domain must cover two levels (`*.apps.example.com` does).
- **Registry clean-up:** every day the agent deletes tags no recent deployment uses (last 20 revisions per service) and that were not pushed or built in the last `REGISTRY_KEEP_DAYS` (30) days. Every Sunday at 04:30, `platform-registry-gc.timer` stops the registry for about a minute and reclaims the freed layers (`host/registry-gc.sh`).
- **Platform database:** `platform-db-backup.timer` dumps it nightly to `/var/backups/platform` (14 days kept). Copy that folder off the server.
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

- **Build minutes** are billed at each plan's `price_build_minute_cents` (`set-prices --build-minute`), measured from each build's start to its end.
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
| STALWART_ADMIN_PASSWORD | — | Admin password of the mail server (hosted mailboxes); the API and the agent use it on the internal network. Required by the production compose file. |
| STALWART_URL | http://stalwart:8080 | The mail server's admin API. Empty turns hosted mailboxes off. |

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
