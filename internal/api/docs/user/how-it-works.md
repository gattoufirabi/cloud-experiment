# How the platform works

What happens under the hood when you deploy, store data or send email: the pieces the platform is made of, how they talk to each other, and how one customer is kept away from another.

## Overview

One rule shapes the whole platform: customers run their own code on shared servers, so nothing one customer does may reach the server itself or another customer. The parts that face the internet hold no privileges on the server. A single local agent does the real work, and it checks every action against a fixed policy.

::: diagram architecture

The platform is split into four trust zones. Each zone assumes the one above it may be compromised.

| Zone | What runs there | What it may do |
| --- | --- | --- |
| 1 · Edge | Caddy proxy, the DNS server, the mail server | Listen on the internet, end HTTPS, route requests. It holds TLS keys and nothing else of yours. |
| 2 · Control plane | The API and console, PostgreSQL, the metrics and log stores, the registry, object storage | Check who you are and what you may do, then write what should exist into PostgreSQL. It has no access to Docker. |
| 3 · Node agent | One agent per server | The only privileged piece. It listens to nothing on the network, takes its work from PostgreSQL, and refuses anything outside its policy. |
| 4 · Your workloads | Your applications, databases, functions and jobs | Run sandboxed in gVisor, with their own limits and their own network per project. |

## Use cases

- **Security review**: explain to a customer or an auditor how their data is isolated from other customers. See [Isolation between customers](#isolation-between-customers).
- **Choosing a feature**: see what runs behind a queue, a function or a database before you rely on it. See [Feature by feature](#feature-by-feature).
- **Debugging a deployment**: know which step a failed deployment stopped at and what keeps your previous version online. See [Follow a deployment](#follow-a-deployment).
- **Planning capacity**: understand that autoscaling reserves its maximum against your plan, and that each database is its own container.

## How to use it

### Follow a deployment

Every change to an application follows the same path, whether it comes from the console, the [API](/docs/api-and-cli), the autoscaler or [Terraform](/docs/terraform). A deployment is an immutable revision, so rolling back means deploying an older one again.

1. **Check**: the API checks your role, your plan's limits and the settings, and that the image belongs to your registry namespace.
2. **Record**: it stores a deployment marked pending and notifies the agent.
3. **Pull and scan**: the agent pulls the image and applies the vulnerability policy (warn, or block critical findings).
4. **Start new instances**: containers start in gVisor on the project network, with their limits, secrets and variables, beside the running version.
5. **Health check**: each new instance must answer its health path. If one does not, the deployment is marked failed and the old version keeps serving.
6. **Switch**: the agent changes the proxy route, and traffic moves to the new instances (blue/green), spread between them in turn.
7. **Drain and stop**: old instances finish the requests in progress (up to 15 seconds), then stop. The deployment is marked live and the change is audited.

### Follow a visitor's request

1. The visitor's browser reaches **Caddy** over HTTPS. For a custom domain, Caddy asks the API whether the domain is verified before it requests a certificate.
2. Caddy applies the service's rules: [traffic protection](/docs/traffic-protection) limits, the [edge cache](/docs/cdn), and the limit of requests in progress per instance.
3. It forwards the request to one of the service's instances and writes an access log line, used for [analytics](/docs/analytics).
4. The application can call other services of the project privately at `<service>.<project>.internal`, and its databases on the project network. See [Private network](/docs/private-network).

### Follow a secret

1. You save a secret in the console or through the API. It is encrypted at once with the platform's master key and bound to its service and name, so the stored value cannot be moved to another service.
2. The console never shows it again.
3. When a container starts, the agent decrypts it and passes it to the container as an environment variable. See [Variables and secrets](/docs/variables).

## Technical reference

### Feature by feature

Each row names what runs the feature and the path a request or change takes through the zones.

| Feature | What runs it | Path |
| --- | --- | --- |
| [Applications](/docs/applications) | Agent, Docker with gVisor, Caddy | API → PostgreSQL → agent → containers → proxy route |
| Autoscaling | Agent, every 15 seconds, using CPU from VictoriaMetrics | at least doubles instances above 90 % CPU, removes them after 5 quiet minutes |
| [Builds from git](/docs/builds) | A git container, then a rootless BuildKit container | webhook or API → agent → build → registry → deployment |
| [Preview environments](/docs/previews) | A service linked to its parent, one instance | API or git webhook → agent → preview container → own address |
| [Scheduled jobs](/docs/scheduled-jobs) | Agent scheduler, a fresh container per run | agent → one-off container from the current deployment → run history |
| [Variables and secrets](/docs/variables) | XChaCha20-Poly1305 with the master key | API encrypts → PostgreSQL → agent decrypts → container environment |
| [Your own servers](/docs/servers) | A node agent on your machine, with no database access or master key | API → desired state over HTTPS → your server's Docker; visitors → proxy → tunnel your server opened → instance; builds set to your servers → claimed by a server → pushed with a one-build token |
| [Functions](/docs/functions) | Invocation queue in PostgreSQL, warm containers | URL or trigger → queue → agent → warm container (cold start on the first call) |
| [Managed databases](/docs/databases) | One container per database, on the project network | API → agent → database container → daily backup |
| [Object storage](/docs/object-storage) | Garage (S3-compatible), kept in line by the agent | application → Garage → bucket events → functions |
| [Queues](/docs/queues) | PostgreSQL, at-least-once delivery | producer → API → PostgreSQL → consumer or function; dead-letter queue after too many receives |
| [Transactional email](/docs/email) | Mail queue, DKIM signing and delivery by the agent | API or SMTP → queue → agent → recipient's mail server |
| [Mail hosting](/docs/mailboxes) | Stalwart mail server | API → Stalwart admin API → IMAP, SMTP and webmail |
| [Custom domains](/docs/domains) | TXT verification, certificates by Caddy | API verifies `_platform-challenge.<domain>` → Caddy asks the API → certificate |
| [DNS hosting](/docs/dns) | Authoritative DNS server in the agent | API → PostgreSQL → zones in memory, reloaded on change and every 30 seconds |
| [Edge cache](/docs/cdn) | Caddy | responses allowed by Cache-Control are kept and served again |
| [Private network](/docs/private-network) | One network per project, the proxy on each network | service → proxy → service of the same or a connected project |
| [Traffic protection](/docs/traffic-protection) | Rules in the service's proxy route | requests per minute per address, allow and block lists, 503 above the overload limit |
| [Logs and metrics](/docs/logs-and-metrics) | VictoriaLogs and VictoriaMetrics, fed by the agent | containers → agent → stores → API adds your organization's filter to every query |
| [Analytics](/docs/analytics) | Proxy access logs and a small page script | proxy → agent → VictoriaLogs → report |
| [Alerts](/docs/alerts) | Alarms evaluated by the agent | agent → incident → email or webhook |
| [Status page](/docs/status-page) | The API | deployments and health → public page |
| [Image registry](/docs/registry) | CNCF Distribution, tokens issued by the API | push and pull only under `registry/<organization>/` |
| [Security scanning](/docs/security-scanning) | A short-lived scan container and the Trivy server | agent → scan container → Trivy → findings and policy |
| [Backups](/docs/backups) | restic, one repository per organization | agent → repository with its own password → copy off the server |
| [Team and roles](/docs/team) | Role checks, then PostgreSQL row-level security | session or token → role → rows filtered on your organization |
| [Single sign-on](/docs/single-sign-on) | OpenID Connect per organization | console → your identity provider → session |
| [Account security](/docs/account) | argon2id passwords, TOTP, passkeys | sign-in → second factor or passkey → session |
| [Billing and usage](/docs/billing) | Metering by the agent once a minute | allocated CPU and memory, network, storage and builds → hourly usage → invoice and budgets |
| [API and CLI](/docs/api-and-cli) | API v1 with hashed, expiring tokens | script → API → the console's own checks → audit log |
| [Terraform and OpenTofu](/docs/terraform) | The open-source provider over the API | `tofu apply` → API → agent → resources |

### Several servers

Each project is placed on one server, with all its services and databases. A project can also run on one of [your own servers](/docs/servers): its agent talks to the API only, never to the database, and the proxy reaches it through tunnels the server opens. Every server runs an agent that only runs the projects placed on it; the first server also runs the control plane and the proxy, which routes to the others. Draining a server moves its projects, restoring their data from a snapshot, and a server that stops reporting is failed over.

### Isolation between customers

Four independent barriers keep one customer away from another. Getting past one still leaves the others.

| Barrier | How it works |
| --- | --- |
| API authorization | Every request resolves your organization from the session or token and checks your role before doing anything. |
| Database policy | The API connects as a role whose row-level security filters every row on the current organization, so a query that forgets its filter still sees only your rows. |
| Agent policy | Containers, networks and volumes carry their organization's labels. The agent refuses to attach anything to another organization's network or volume, and refuses privileged or unlimited containers, whatever the database says. |
| Kernel and runtime | gVisor sandboxes, one network per project, CPU, memory and process limits, disk quotas, and a server firewall that blocks the server itself, other customers, private address ranges, cloud metadata and outgoing mail ports. |

### Components

Everything is open source: the Go API and agent, Caddy, PostgreSQL, gVisor, Garage, Stalwart, VictoriaMetrics, VictoriaLogs, CNCF Distribution, Trivy and restic.
