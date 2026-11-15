# Multi-tenant infrastructure platform — architecture and MVP plan

A self-hosted mini cloud. External clients sign in to a web console, create projects, deploy containers, attach domains and databases, and see their own logs, metrics, alerts, backups and security reports. They never see each other or the host.

> **Implementation status (2026-11-15):** Phase 0 scripts and the Phase 1 scope are built, plus all of Phase 2: private registry with Trivy scanning, per-client backups, alerts, managed PostgreSQL and Redis, API tokens with a CLI, public status pages and passkeys, covered by `scripts/e2e-dev.sh`; Phase 3 too: traffic protection, usage metering and invoices, preview environments, sandboxed git builds and self-signup with abuse controls. Still to verify on a Linux server: gVisor, the host firewall and ACME TLS. See README.md for what is built and what is not.

**Decisions taken**
| Decision | Choice |
|---|---|
| Approach | Own control plane (web console, API, node agent) that drives proven open-source tools |
| Scale | One server at first; the design must grow to several servers without a rewrite |
| Users | External clients (untrusted tenants) |
| Constraint | Open-source components only, self-hosted, lightweight |

---

## 1. The hard truth first: untrusted tenants on one server

Clients run **their own code** on **your kernel**. That one fact drives most of the design.

| Risk | Consequence if ignored | Answer in this design |
|---|---|---|
| Container escape (kernel exploit) | One client gets root on the host, and with it every other client's data | **gVisor** (`runsc`) sandbox runtime for every tenant container: an application kernel in user space between tenant code and the host kernel |
| Noisy neighbour | One client's runaway job slows or kills everyone | cgroup v2 limits per container **and** per tenant (CPU, memory, PIDs, disk I/O), plus per-tenant disk quotas |
| Lateral network movement | A tenant scans or attacks another tenant's database | One isolated network per tenant project. nftables blocks tenant traffic to the host, other tenants, private ranges and cloud metadata |
| Abuse | Crypto mining, spam, phishing pages on your IP, which gets the server blacklisted | Outbound SMTP (25) blocked, sustained-CPU detection, invite or verified signup, terms of service, fast suspend |
| Control-plane compromise | The web app is internet-facing; if it holds the Docker socket, one bug = full takeover | The web API has **no** host privileges. Only a local agent touches Docker, and it enforces a hard policy no matter what the database says |
| Single server = single point of failure | Every client is down at the same time | Honest SLA, tested restores, and an agent protocol already shaped for adding nodes later (phase 4) |

**Why not just use Coolify, Dokploy or CapRover?** They are good for *your own* apps, but they are not built for hostile tenants. They give the web app full Docker socket access, use the default `runc` runtime, have no per-tenant cgroup or disk quotas, and their "teams" are a UI concept, not a security boundary. **Why not Kubernetes with vCluster and Kata?** It solves the problem but needs several GB of RAM just for the control plane on one box, which conflicts with "lightweight".

---

## 2. Architecture

```
                                Internet
                                    │ 80/443 only (+ WireGuard for the operator)
┌───────────────────────────────────▼──────────────────────────────────────────┐
│  HOST (Debian 13, nftables default-drop, unattended security updates)        │
│                                                                              │
│  ┌─ EDGE ────────────────────────────────────────────────────────────────┐   │
│  │ Caddy: automatic TLS, on-demand certificates for client domains       │   │
│  │        (each domain is approved by the API before a certificate is    │   │
│  │        issued), rate limits, optional Coraza WAF                      │   │
│  └───────┬───────────────────────────────┬───────────────────────────────┘   │
│          │ console.example.com            │ *.apps.example.com + client domains
│  ┌───────▼─ CONTROL PLANE (runc, unprivileged) ─┐                            │
│  │ API + web console (Go binary + SPA)          │                            │
│  │  - auth: passwords (argon2id), TOTP, passkeys│                            │
│  │  - orgs, members, roles, API tokens          │                            │
│  │  - desired state, quotas, audit log          │                            │
│  │  - tenant-scoped log/metric query proxy      │                            │
│  └───┬───────────────┬──────────────────────────┘                            │
│      │ SQL (RLS)     │ queries (tenant filter injected)                      │
│  ┌───▼──────────┐ ┌──▼──────────────────────────┐                            │
│  │ PostgreSQL   │ │ VictoriaMetrics (metrics)   │                            │
│  │ (platform DB)│ │ VictoriaLogs   (logs)       │                            │
│  └───▲──────────┘ └──▲──────────────────────────┘                            │
│      │ LISTEN/NOTIFY │ push metrics + logs                                   │
│  ┌───┴──────────────┴─── NODE AGENT (root, NOT internet-facing) ──────────┐  │
│  │ reconcile loop: desired state (DB) → actual state (Docker/Caddy/nft)   │  │
│  │ hard policy: no privileged containers, no host mounts or host network, │  │
│  │ runsc only, limits required, images must pass the scan policy          │  │
│  │ also: cgroup stats, log shipping, health checks, backups, quotas       │  │
│  └───┬───────────────────────┬──────────────────────┬─────────────────────┘  │
│      │ Docker API (unix sock) │ Caddy admin API      │ restic                 │
│  ┌───▼───────────────────────▼──────────────────────┐ │                      │
│  │ DATA PLANE — tenant workloads (gVisor/runsc)     │ │                      │
│  │                                                  │ │                      │
│  │  tenant-a.slice (cgroup: 2 CPU, 2 GB, 200 PIDs)  │ │                      │
│  │   ├─ net tenant-a-p1: web, worker, postgres      │ │                      │
│  │   └─ volumes on XFS project quota (10 GB)        │ │                      │
│  │  tenant-b.slice ...                              │ │                      │
│  └──────────────────────────────────────────────────┘ │                      │
│  Registry (CNCF Distribution, per-tenant namespaces)   │                      │
│  Trivy (scan on push)    ntfy (alert push)             ▼                      │
│                                         encrypted backups → off-site rest-server│
└──────────────────────────────────────────────────────────────────────────────┘
```

### Trust zones
1. **Edge (Caddy)**: trusted code, exposed to the internet. Only proxies traffic; holds no client secrets other than TLS keys.
2. **Control plane (API)**: exposed to the internet through Caddy and treated as *compromisable*. It runs with no capabilities, a read-only root filesystem, and no Docker socket. It can only write *desired state* to Postgres.
3. **Agent**: privileged, reachable only via Postgres and a local Unix socket. **Even if the API is compromised, the agent refuses anything outside policy.** An attacker who controls the API can change tenants' apps but cannot escape to the host. This boundary is the most important one in the system.
4. **Tenant workloads**: untrusted. They run under gVisor in their own cgroup slice and network, without capabilities.

---

## 3. Components and choices

| Concern | Component (license) | Approx. RAM | Notes |
|---|---|---|---|
| API, console backend, agent | **Go** (own code) | 20–40 MB each | Static binaries, first-class Docker SDK, easy to audit and deploy |
| Console frontend | SvelteKit or React SPA (own code) | static files | Served by Caddy or embedded in the API binary |
| Platform DB | **PostgreSQL 17** | 60–150 MB | Row-level security as a second tenant-isolation layer, LISTEN/NOTIFY for the agent's work queue, transactional audit log |
| Edge / TLS | **Caddy** (Apache-2.0) | 30–60 MB | On-demand TLS with an `ask` endpoint (the API approves each custom domain); configured live through its admin API |
| WAF (optional) | **Coraza** + OWASP CRS (Apache-2.0) | +30 MB | Caddy plugin, per-project on/off |
| Sandbox runtime | **gVisor** `runsc` (Apache-2.0) | ~15 MB per sandbox | Syscall-heavy apps run 10–30% slower; databases work fine with the right flags |
| Containers | Docker Engine (Debian package) | ~60 MB | Later swappable for containerd directly |
| Image registry | **CNCF Distribution** (Apache-2.0) | ~20 MB | Token auth issued by the API; each tenant can push only to `tenant-id/*` |
| Image scanning | **Trivy** (Apache-2.0) | on demand | Scans on push and nightly. Policy per plan: warn, or block CRITICAL |
| Metrics | **VictoriaMetrics** single-node (Apache-2.0) | 50–150 MB | Prometheus-compatible. Never exposed directly: the API injects `tenant="<id>"` into every query |
| Logs | **VictoriaLogs** (Apache-2.0) | 50–150 MB | Same pattern. The agent ships container stdout/stderr tagged with tenant/project/service |
| Health checks | Built into the agent | — | HTTP/TCP checks per service, feeding status pages and alerts |
| Alerts to clients | Webhooks + self-hosted **ntfy** topics per tenant | ~15 MB | Email needs an SMTP server (self-hosted Postfix is possible but deliverability is hard); optional later |
| Backups | **restic** (BSD-2) + rest-server | on demand | One repository *per tenant* with its own key, so restores never cross tenants. Off-site copy to a machine you own |
| Operator access | WireGuard + SSH (from demo-env) | ~0 | The admin console is reachable only over WireGuard |
| Secrets at rest | Envelope encryption (own code, XChaCha20-Poly1305) | — | Per-tenant data keys wrapped by a host master key (file mode 400, TPM later). Injected as files, never logged |

**Base footprint**: about 500–700 MB RAM for the whole platform. A server with 8 vCPU / 32 GB should host roughly **40–80 small client services** (web plus database), depending on traffic.

---

## 4. Tenant model

```
Organization (tenant, billing unit, plan + quotas)
 ├─ Members ── User (role: owner | admin | developer | viewer)
 ├─ ApiTokens (scoped, hashed, expiring)
 ├─ Projects
 │   ├─ Services (image@digest, env, secrets, resources, port, healthcheck, domains)
 │   │   └─ Deployments (immutable revisions → rollback = redeploy an older revision)
 │   ├─ Databases (managed Postgres/Redis: one container per instance, never a shared cluster)
 │   ├─ Volumes (size counted against quota)
 │   ├─ Domains (verified via DNS TXT before TLS issuance)
 │   ├─ Secrets (encrypted)
 │   ├─ AlertRules + Channels (webhook, ntfy)
 │   └─ BackupPolicies + Snapshots
 └─ AuditEvents (append-only, hash-chained: who, what, when, from which IP)
```

### Isolation is enforced at four layers (defense in depth)
1. **API authorization**: every handler resolves `org_id` from the session or token and checks the role.
2. **Postgres RLS**: the API connects as a role whose policies filter on `current_setting('app.org_id')`. A forgotten `WHERE` clause cannot leak another tenant's rows.
3. **Agent policy**: resources are labelled with `org_id`. The agent refuses to attach a container to another tenant's network or volume.
4. **Kernel and runtime**: gVisor, separate networks, cgroup slices, disk quotas.

---

## 5. Key flows

### Deploy
```
client ─(console/CLI/API)─► API: authenticate, check role, check quota, validate spec
   API ─► Postgres: insert Deployment(status=pending), NOTIFY agent
 agent ─► pull image@digest (from registry) ─► Trivy policy check
       ─► create container: runtime=runsc, cgroup-parent=tenant-<id>.slice,
          network=tenant-<id>-<project>, read-only rootfs, cap_drop ALL,
          limits from the plan, secrets as tmpfs files
       ─► wait for health check ─► switch Caddy route (blue/green, no downtime)
       ─► stop the old container ─► Deployment(status=live), audit event
   API ─► console: live progress stream (Server-Sent Events)
```

### Custom domain
The client adds `shop.client.com`. The API issues a TXT verification token. The client creates the DNS records. The API verifies the TXT record and marks the domain verified. When a request arrives, Caddy's on-demand TLS calls `ask` on the API, which only answers yes for verified domains. That stops attackers from making you request certificates for arbitrary domains.

### Logs and metrics query
The console calls `GET /api/projects/:id/logs?q=...`. The API checks membership, rewrites the query to force `org_id` and `project_id` filters, sends it to VictoriaLogs and returns the results. The storage backends are never reachable from outside.

---

## 6. What clients get (product surface)

| Area | MVP | Later |
|---|---|---|
| Compute | Deploy from image, env vars, secrets, resources, restart, rollback | Git builds (sandboxed), cron jobs, preview environments |
| Networking | `*.apps.<you>` subdomain, custom domains + automatic TLS | Per-project IP allowlist, WAF toggle, rate-limit settings |
| Data | Volumes | Managed Postgres/Redis, point-in-time recovery for Postgres |
| Observability | Live logs, CPU/RAM/network graphs, health status | Alert rules, uptime history, status page per project |
| Security | 2FA, roles, audit log, image scan report | Passkeys required per org, IP allowlist for the console, SSO via OIDC |
| Backups | Platform-level nightly backups (operator restore) | Client self-service snapshots and restore |
| Access | Web console | CLI + API tokens, Terraform provider |
| Account | Invite-only onboarding, plan quotas | Self-signup with verification, usage metering, invoices |

---

## 7. Operator side (you)
- An admin console over WireGuard only: all tenants, usage, suspend/resume, impersonate in read-only mode (audited), capacity graphs.
- Host monitoring: the same VictoriaMetrics stores node metrics (node_exporter) under an operator-only tenant, with alerts to your ntfy.
- EagleEye can consume a read-only operator API for your global dashboard.
- Runbooks to write: restore one tenant, restore the whole host, suspend an abusive tenant, rotate the master key, handle a CVE in gVisor or Caddy.

---

## 8. Things that cannot be open-source or self-hosted (be explicit with clients)
- **The server, its IP and upstream network.** A volumetric DDoS attack cannot be absorbed by one server. Without a provider's filtering, the honest answer is "we null-route and recover".
- **Domain and DNS registrar**, and **Let's Encrypt** for certificates.
- **Payments**: card payments require a payment processor. MVP option: manual invoicing and bank transfer, with the platform only tracking usage.
- **Legal (EU clients)**: terms of service, acceptable-use policy, a data processing agreement (GDPR), a published data location, and real deletion when an account is closed (including backups after retention expires).

---

## 9. Roadmap

Rough effort for **one experienced developer**. The point is the order, not the dates.

### Phase 0 — Host foundation (≈1 week)
Reuse `demo-env/scripts/bootstrap.sh` (hardening, nftables, WireGuard, updates), then add:
- gVisor installed and registered as a Docker runtime; a test that a container under `runsc` cannot see host processes.
- An XFS data partition with project quotas for tenant volumes.
- An nftables `DOCKER-USER` policy: tenant bridges cannot reach the host, other tenants, RFC1918 ranges or 169.254.169.254; outbound 25 is dropped.
- **Done when**: an isolation test suite passes (escape attempts, cross-tenant ping, metadata access and fork bombs are all contained).

### Phase 1 — MVP: invite-only clients deploy containers (≈6–8 weeks)
- Auth: email + password (argon2id), mandatory TOTP, sessions, organization invites, roles.
- Organizations, projects, services, deployments, secrets, domains, audit log, plan quotas.
- Agent: reconcile loop, the hard policy, runsc, cgroup slices, per-project networks, Caddy routes, rollback.
- Logs (VictoriaLogs) and basic metrics (VictoriaMetrics) behind the tenant-scoped proxy.
- Registry with per-tenant tokens and a Trivy scan on push.
- Platform-level nightly backups and an operator restore runbook.
- **Done when**: two test tenants deploy apps on custom domains; neither can see or reach the other through the API, network, logs or metrics; a full host restore has been rehearsed.

### Phase 2 — Managed services and self-service (≈6 weeks)
Managed Postgres/Redis, per-tenant restic repositories with client restore, alert rules (webhook, ntfy), uptime checks and status pages, API tokens + CLI, passkeys.

### Phase 3 — Growth features (≈8 weeks)
Sandboxed git builds (Railpack/buildpacks under gVisor with strict limits), preview environments, WAF and rate-limit settings, usage metering and invoices, self-signup with abuse controls (verification, trial limits, CPU anomaly detection).

### Phase 4 — Multiple servers
Agents on additional servers connect over WireGuard with mTLS. The scheduler places services by capacity. Tenant networks span nodes (WireGuard mesh or VXLAN). Postgres gets a streaming replica, and the control plane runs on 2 nodes. The phase 1 agent design (desired state in the DB, agents converge) is what makes this possible without a rewrite.

---

## 10. Open questions before coding
1. **What do clients deploy?** Prebuilt images only (simplest, safest) or source code (needs sandboxed builds, which is a big risk and resource cost)? Recommendation: images only for the MVP.
2. **Plans and limits**: define 2–3 plans now (CPU, RAM, disk, services, domains) because quotas are wired in from phase 1.
3. **Server size**: 8 vCPU / 32 GB / NVMe is a good starting point for the estimates above.
4. **Brand and domains**: console domain, app wildcard domain (`*.apps.<you>`), and a separate domain for client content, so abuse on client apps cannot get your main domain blacklisted.
5. **Frontend stack preference**: SvelteKit (lighter) or React (bigger ecosystem).
