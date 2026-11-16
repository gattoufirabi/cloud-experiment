# Platform — multi-tenant infrastructure on one server

A self-hosted mini cloud for external clients. Invited clients log in to a web console, create projects, deploy container images, attach domains, manage secrets and watch logs and metrics. Each client is isolated from the others and from the host.

Design: [ARCHITECTURE.md](ARCHITECTURE.md). Plain-language overview: the "Multi-tenant platform: how it works" doc.

---

## What is built (Phase 0 + first version)

| Area | Status |
|---|---|
| Console: login, **mandatory two-step codes (TOTP)**, invite-only accounts, roles (owner / admin / developer / viewer) | done, tested |
| Organizations, projects, services, plans with quotas (services, CPU, memory, domains) | done, tested |
| Deploy from a container image, settings, environment, rollback, zero-downtime switch, failed deploy keeps the old version | done, tested |
| Secrets encrypted at rest (XChaCha20-Poly1305, bound to their row) | done, tested |
| Custom domains with DNS TXT ownership check; on-demand HTTPS only for verified hosts | done, TLS path untested locally |
| Live logs (VictoriaLogs) and CPU/memory graphs (VictoriaMetrics), tenant filter enforced server side | done, tested |
| Hash-chained audit log of every action | done, tested |
| Tenant isolation: API checks, **PostgreSQL row-level security**, agent hard policy, per-project networks, container limits | done, tested |
| **Private image registry** (CNCF Distribution): one namespace per organization, `docker login` with console-issued tokens (push or pull-only, revocable, 1-year expiry) | done, tested |
| **Vulnerability scanning** (Trivy): every push, every deploy (cached 24 h per image), nightly rescan of running images; report per service | done, tested |
| Scan policy per organization: `warn` (default) or `block-critical` | done, tested |
| **Per-client backups** (restic) of each service's persistent folder: nightly + on demand, retention, self-service restore with a safety snapshot | done, tested |
| **Managed PostgreSQL** (16 or 17) **and Redis 7**: per-project private database, resources counted against the plan, one-click `DATABASE_URL` secret, consistent backups (`pg_dump` / Redis snapshot) with self-service restore | done, tested |
| **API tokens, REST API v1 and `platformctl`** (deploy from CI, status, logs, secrets), org- and role-scoped, audited | done, tested |
| **Passkeys** (WebAuthn, discoverable, user verification required) as an alternative to password + code; managed on the Account page | done, tested with a software authenticator |
| **Public status pages** per project (opt-in): live state, 30-day uptime, recent outages, nothing sensitive | done, tested |
| **Traffic protection** per service: WAF (Coraza + OWASP Core Rule Set), requests per minute per visitor, blocked IP ranges; enforced by Caddy | done, tested |
| **Usage metering and invoices**: allocated CPU/memory and network out metered per minute, per-plan prices, monthly invoices (printable), estimate so far, billing details, operator records payments (no payment provider) | done, tested |
| **Preview environments**: temporary copies of a service with another image at `<name>.<service>-…` (console, API, `platformctl preview`), secrets copied, auto-expiry | done, tested |
| **Builds from git**: Dockerfile builds in a rootless BuildKit sandbox (no Docker socket, isolated network), push to the org's registry space, optional deploy; console, API, `platformctl build`, push webhooks | done; rootless BuildKit verified under gVisor (`BUILD_RUNTIME=runsc`) on a Debian 12 server |
| **Self-signup with abuse controls**: email confirmation (any SMTP server), proof-of-work instead of CAPTCHA, honeypot, IP/daily limits, disposable-domain block, 14-day trial plan (no custom domains or builds), auto-suspension of expired trials and CPU-maxing (mining-like) trial apps, operator notices | done, tested |
| **Account recovery**: 10 one-time recovery codes for two-step login, password reset by email or operator link, `admin reset-totp` | done, tested |
| **Registry clean-up** (daily, unused tags older than 30 days) and weekly garbage collection; **nightly platform DB backup** timer | done, tested |
| **Disk limits per plan**: storage measured every 15 min, alerts at 90/100%, new deploys refused above 110% | done, tested |
| **Operator web console** (`/operator`): platform overview, organizations (plan, suspend/restore), invoices (paid/cancel), global audit log; `admin make-operator` | done, tested |
| **Railpack builds** for repositories without a Dockerfile (Node, Python, Go, PHP, Ruby, Java...), same sandbox | done, tested |
| **Email alert channel**, trial reminder email, build minutes on invoices, dark mode | done, tested |
| **Built-in docs** at `/docs/user` and `/docs/admin` (public, filled with this installation's addresses), linked from the header, the login page and a help link on every console tab | done |
| Console design: sidebar pane + tabs, monochrome style | done |
| **Alerts**: service down/recovered, CPU and memory thresholds, failed deploys and backups, critical CVEs in running images; ntfy push and signed webhooks per organization | done, tested |
| gVisor sandbox for tenant containers | done, verified on a Debian 12 server (Phase 0 gate) |
| Host firewall for tenant networks, isolation test (Phase 0 gate) | done, gate PASSED on a real server via `host/deploy.sh` |

The end-to-end test (`scripts/e2e-dev.sh`) runs 264 checks, including cross-tenant attempts on the console, database, network, registry, backups and alert channels, databases, the API, status pages, passkeys, traffic protection, invoicing, previews, git builds and self-signup. Unit tests: `go test ./...` (outbound request guard).

**Not built yet** (next phases in ARCHITECTURE.md): Phase 4 (multiple servers), hard filesystem disk quotas (XFS project quotas).

---

## Alerts

| Signal | How it is detected | Notification |
|---|---|---|
| `service_down` | The agent probes every live app's health path every `ALERT_INTERVAL_SECONDS` (30 s in production). 3 failures in a row, a stopped, paused or out-of-memory container opens an incident. | firing, then resolved automatically |
| `cpu_high`, `memory_high` | Usage stayed above the service's threshold (% of its own limit) for the whole last 5 minutes (VictoriaMetrics). Set per service; off by default. | firing, then resolved |
| `deploy_failed`, `backup_failed` | Recorded by the agent when the job fails. | event |
| `critical_vulns` | A nightly or push rescan finds critical CVEs in an image that is currently running. | event |

- **Deduplication**: at most one open incident per service and kind, so a flapping app does not flood the channel.
- **Channels** (per organization, admins): **ntfy** (any server, optional access token) or **webhook** (JSON, optional HMAC signature `X-Platform-Signature: sha256=<hex>` of the raw body). Each channel picks its events and has a "Send test" button. Its last delivery status is shown in the console.
- **Delivery**: queued in the database and retried 5 times with backoff (30 s to 4 min).
- **SSRF protection**: client URLs are checked when saved and again at connection time, against the IP actually dialled, after DNS resolution. Loopback, private, link-local, CGNAT and metadata ranges are refused, and redirects are not followed. Covered by unit tests. `ALERT_ALLOW_PRIVATE_TARGETS=true` exists only for the dev stack.
- The dev stack includes an ntfy server at http://localhost:8090 to watch notifications arrive.

---

## Per-client backups

| What | How |
|---|---|
| What is backed up | The persistent folder (volume) of each service. Stateless services have nothing to back up; settings live in the platform database (operator backup). |
| Isolation | One restic repository **per organization**, with its own random password, stored encrypted with the master key (bound to the org). Snapshots are grouped per service. A restore always targets the service's own volume, chosen by the agent. |
| Schedule | Nightly at the hour the client picks (UTC), plus "Back up now". Retention: 7 daily, 4 weekly, 6 monthly (`BACKUP_KEEP`). |
| Restore | Admins type the service ID to confirm. The agent takes a **safety snapshot** (always kept), stops the app, replaces the folder, then starts the app again. |
| Job containers | Short-lived `restic` containers: no network in local mode, read-only filesystem, read-only data mount for backups, only the capabilities needed, no Docker socket. |
| Where | Local mode: the `platform-backups` volume (fine for testing, **not off-site**). Production: `BACKUP_REST_URL` pointing to a [rest-server](https://github.com/restic/rest-server) on a machine you own, reached over WireGuard. |
| Disaster recovery | `platform admin backup-key --slug acme` prints the repository and password (audited), so the data can be restored with plain restic, without the platform. |

**Consistency**: backups are taken while the app runs (crash-consistent). For a database running inside a service, dump it to the persistent folder on a schedule (for example `pg_dump` from a cron job in the app), or wait for managed databases, which will get point-in-time recovery.

**Operator backup** (separate from client backups): the platform database (`pg_dump`), `deploy/.env` and `deploy/secrets/`. Use the `demo-env/scripts/backup.sh` pattern. Without the master key, neither client secrets nor client backup keys can be decrypted.

---

## Registry and scanning

```
docker push ──► Caddy (registry host) ──► registry ──(webhook)──► api ──► scan queued ──► agent
                     │ /token                ▲                                            │
                     ▼                       │ verifies signed token                      ▼
                    api: checks the docker-login token, signs a 5-minute token     short-lived Trivy container
                         limited to <org>/* (push only if the token allows it)     (no socket, read-only, limited)
                                                                                   ──► Trivy server (CVE database)
```

- **Namespaces**: organization `acme` can only reach `<registry-host>/acme/...`. The console, the API and the agent each refuse an image from another organization's namespace; host names are compared case-insensitively.
- **Platform-side pulls are org-scoped too**: the agent and the scanner log in as `agent/<org>` with a password derived (HMAC) from a secret. A deployment can therefore never pull another organization's image, even under a different spelling of the address, and a cached image is never reused without a fresh, authorized pull.
- **Deploy gate**: the image is scanned after the pull and before the container starts. With `block-critical`, an image with critical CVEs is refused and the client sees the CVE IDs; if the scanner is down, `block-critical` fails closed and `warn` deploys with a log entry.
- **Operator**: `platform admin set-scan-policy --slug acme --policy block-critical`.
- **External dependency**: the Trivy server downloads the open vulnerability database (from GitHub's container registry) daily. For an offline server, mirror it with `trivy --download-db-only` and serve it locally.

Client workflow (shown in the console):

```bash
docker login registry.example-cloud.net -u acme      # password: a token created in the console
docker tag myapp:1.0 registry.example-cloud.net/acme/myapp:1.0
docker push registry.example-cloud.net/acme/myapp:1.0
```

In the dev stack the registry is `localhost:8088`. Docker only allows plain HTTP for `localhost`, and scan containers use the Docker VM's network to reach it.

---

## How it fits together

```
Internet ──► Caddy (front door, TLS) ──► api  (Go, unprivileged, no Docker access)
                     │                     │ writes desired state only
                     │                     ▼
                     │                 PostgreSQL (row-level security per tenant)
                     │                     │ LISTEN/NOTIFY
                     ▼                     ▼
             tenant containers ◄──── agent (Go, privileged, not on the internet)
             (gVisor, own network,       re-validates every spec, starts containers,
              CPU/RAM/PID limits)        updates Caddy routes, ships logs and metrics
```

One binary, several roles: `platform api | agent | migrate | admin`.

| Path | What |
|---|---|
| `cmd/platform/` | entry point and operator commands |
| `internal/api/` | console + API, HTML templates, static assets |
| `internal/agent/` | reconcile loop, policy, Caddy config, log and metric shipping |
| `internal/docker/` | minimal Docker Engine API client (no SDK) |
| `internal/secure/` | argon2id passwords, TOTP, secret encryption, tokens |
| `internal/model/` | deployment spec and validation shared by API and agent |
| `migrations/` | database schema, RLS policies, audit function |
| `deploy/` | dev and production compose files, Caddy start config, DB role setup |
| `host/` | Phase 0: gVisor installer, tenant firewall, isolation test |
| `scripts/e2e-dev.sh` | end-to-end test: two tenants, deploys, cross-tenant attempts |

---

## Run it locally (Windows / macOS / Linux with Docker)

```bash
cd platform
mkdir -p deploy/dev-secrets
go run ./cmd/platform admin init-key --file deploy/dev-secrets/master.key
go run ./cmd/platform admin init-registry-key --file deploy/dev-secrets/registry-token.key --cert deploy/dev-secrets/registry-token.crt
docker compose -f deploy/compose.dev.yml up -d --build
```

Create an organization and open the invite link it prints:

```bash
docker compose -f deploy/compose.dev.yml exec api /platform admin create-org --slug demo --name "Demo Studio" --owner-email you@example.test
```

- Console: http://console.localhost:8088
- Apps: `http://<service>-<project>-<org>.apps.localhost:8088`
- Two-step codes: use any authenticator app, or for testing `docker compose -f deploy/compose.dev.yml exec api /platform admin totp-code --secret <key shown on screen>`

The dev stack runs tenant containers **without gVisor** (`ALLOW_INSECURE_RUNTIME=true`); the agent logs a warning and refuses to start that way unless the flag is set.

Run the end-to-end test (needs bash + curl; on Windows use Git Bash):

```bash
bash scripts/e2e-dev.sh
```

---

## Production (one Debian 13 server)

1. **Harden the host**: run `demo-env/scripts/bootstrap.sh` (SSH keys only, nftables, WireGuard, automatic security updates).
2. **Phase 0**, as root:
   ```bash
   cp -r platform /opt/platform
   bash /opt/platform/host/install-gvisor.sh
   cp /opt/platform/host/platform-tenant-firewall.service /etc/systemd/system/
   chmod +x /opt/platform/host/*.sh && systemctl enable --now platform-tenant-firewall
   bash /opt/platform/host/isolation-test.sh      # must print "Phase 0 gate: PASSED"
   ```
3. **DNS**: `A console.example.com`, `A registry.example-cloud.net` and `A *.apps.example-cloud.net` to the server.
4. **Configure**:
   ```bash
   cd /opt/platform/deploy
   cp .env.example .env && chmod 600 .env       # fill in hosts and three DB passwords
   mkdir -p secrets /srv/platform/{postgres,caddy,vlogs,vmdata,registry,trivy}
   docker compose -f compose.prod.yml build
   docker run --rm --user 0 -v "$PWD/secrets:/out" platform:local admin init-key --file /out/master.key
   docker run --rm --user 0 -v "$PWD/secrets:/out" platform:local admin init-registry-key \
     --file /out/registry-token.key --cert /out/registry-token.crt
   chmod 444 secrets/master.key secrets/registry-token.key   # read by the unprivileged API container (uid 65532)
   docker compose -f compose.prod.yml up -d
   ```
5. **Back up `secrets/master.key` offline.** Without it, client secrets and two-step keys cannot be decrypted.
6. Onboard the first client: `docker compose -f compose.prod.yml exec api /platform admin create-org ...`

Operator commands: `platform admin help` (create-org, invite, suspend-org, set-plan, init-key).

### Production notes
- Every app host gets its own Let's Encrypt certificate (on-demand, approved by the API). For many clients, a wildcard certificate for `*.APPS_DOMAIN` (DNS challenge) avoids rate limits; that needs a DNS-provider module in Caddy.
- The agent runs as root with the Docker socket, so it is effectively root on the host. It has no listening port, and only the database and the Caddy admin socket (shared with Caddy alone) can reach it.
- A tenant app can reach Caddy on its own network. That is equivalent to visiting other public apps over the internet, not a private path.
- Images with many CPU-based workers (nginx `worker_processes auto`) see the host's core count. Limits still apply, but clients may want to set worker counts explicitly.
- Each project network gets a /24 from `10.208.0.0/12` (4,096 projects), and the agent removes networks no deployment needs. Docker's default would give each a /16 and run out after about 30 projects.
- Volumes (`pv-<service>`) are kept when a service is deleted. Clean them up manually until volume lifecycle and quotas are added.

---

## Security model in one table

| Layer | Enforcement | Tested |
|---|---|---|
| Console | session + mandatory TOTP, CSRF token + Origin check, strict CSP, rate limits and account lockout | e2e |
| API → data | every handler resolves the org from the session; queries run with `app.org_id` set, so **RLS** filters rows | e2e (API role sees 0 rows without a tenant context) |
| API → host | none: no Docker socket, no capabilities, read-only filesystem | by design |
| Agent | re-validates every spec; only it builds container configs (no field exists for mounts, privileges, host network) | code review |
| Containers | gVisor (prod), cap-drop ALL + minimal set, no-new-privileges, memory/CPU/PID limits, per-project network | e2e (except gVisor) |
| Network | per-project bridges; host firewall blocks private ranges, metadata, host and SMTP | e2e (cross-tenant); host rules run on the server |
| Audit | every change recorded, hash-chained, append-only function | e2e |
| Registry | signed 5-minute tokens limited to the org namespace; push only with push tokens; agent pulls org-scoped | e2e (cross-namespace push, pull and deploy refused) |
| Supply chain | Trivy scan on push, deploy and nightly; optional block on critical CVEs | e2e |
| Backups | per-org repository and key; agent picks the target volume; restore needs admin + typed confirmation; safety snapshot first | e2e (cross-tenant backup/restore refused, encrypted at rest, round trip) |
| Alerts | channels and incidents per org (RLS); outbound requests refuse internal addresses after DNS resolution; secrets encrypted | e2e (cross-tenant channel use refused) + unit tests (SSRF guard) |
