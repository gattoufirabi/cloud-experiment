# User guide

For client teams who deploy applications on the platform. Console: `{{console}}`.

## Getting started

Accounts are created by invitation only, and every account must use two-step login with an authenticator app.

1. **Open your invitation link.** It works once and expires after 7 days. If it has expired, ask your organization's admin or the platform operator for a new one.
2. **Choose your name and a password** of at least 12 characters. If you already have an account on the platform, enter its password instead to join the new organization.
3. **Set up two-step login.** Add the key shown on screen to an authenticator app such as Aegis, 2FAS, FreeOTP or Google Authenticator, then type the 6-digit code it shows.
4. **Log in next time** with your email, password and the current 6-digit code.

- **No invitation?** If the platform allows it, choose **Create an account** on the login page: confirm your email address, then set up two-step login. You get your own organization on a free trial (see below).
- **Sessions** last up to 12 hours and end after 2 hours without activity.
- **Lockout:** after 5 wrong passwords, the account is locked for 15 minutes.
- **Recovery codes:** right after two-step setup you get 10 one-time codes. Keep them in a password manager or on paper. Each one replaces the 6-digit code once; create new ones under **Account**.
- **Lost phone:** log in with your password and a recovery code, then set up two-step login again. No codes left? Ask the platform operator.
- **Forgot your password:** choose **Forgot your password?** on the login page to get a one-hour link by email (or ask the operator for one). All your sessions end when you set the new password, and two-step login is still required.
- **Several organizations:** after logging in you see the list of organizations you belong to.

### Passkeys

A passkey lets you sign in with your fingerprint, face or device PIN instead of your password and 6-digit code. It works with phones, laptops, security keys and password managers that support passkeys.

1. Open **Account** (top bar) and choose **Add a passkey**. Give it a name you will recognize, such as "Work laptop".
2. Next time, on the login page, choose **Sign in with a passkey** and confirm on your device.

- Only a public key is stored on the platform; the private key never leaves your device or password manager.
- Your password and authenticator app keep working: use them if you lose the device, then remove the lost passkey from **Account**.
- A passkey only works on this platform's address, so a look-alike phishing site cannot use it.

## Free trial

Organizations created with **Create an account** start on the **Trial** plan: 2 services, half a CPU, 512 MB of memory and one database, free for 14 days. The organization page shows the days left.

- Not included in the trial: custom domains and builds from git (push an image instead).
- When the trial ends, your applications stop and changes are blocked; your data is kept. Contact the platform operator to choose a plan and everything starts again.
- Applications that use all their CPU for a long time (typical of crypto-mining) get a trial organization suspended automatically. The organization page says why; contact the operator if it was legitimate work.

## How things are organized

Your organization holds projects, projects hold services, and each service is one running container.

- **Organization:** members and roles, plan limits, audit log, private registry space, alert channels.
- **Project:** a group of related services, for example `shop`.
- **Service:** one container (a website, an API or a worker) with its own address: `<service>-<project>-<organization>.{{apps}}`.
- **For each service:** deployments (numbered revisions), settings and secrets, domains, backups, logs, metrics, health and alerts, and a vulnerability report.

Everything inside your organization is private to its members: other clients cannot see or reach it, through the console, the network or the registry. The left pane of the console lists every project and service, with the status of its latest deployment: ● live, × failed, ◌ deploying, ○ not deployed.

## Deploy your first application

An application is deployed from a container image and is usually live within a minute. The platform does not build source code: build the image yourself or in your CI, and push it to a registry.

1. **Create a project** on your organization page (admins). Its ID has 2 to 20 lowercase letters, digits or dashes, for example `shop`.
2. **Add a service** in the project (developers). Give it an ID such as `web` and, optionally, an image such as `nginx:1.27-alpine`.
3. **Open the Settings tab** of the service and check the image, the port the application listens on (nginx: `80`), the health check path, CPU and memory.
4. **Click Save and deploy.** The Overview tab shows each step: pulling, scanning, starting, health check.
5. **Open the address** shown at the top of the service page.

If the deployment fails, the reason and the last log lines appear under its status. A failed deployment never replaces a working one.

## Service settings

Settings take effect at the next deployment: use **Save and deploy**, or **Save only** and deploy later.

| Setting | Default | Rules and tips |
| --- | --- | --- |
| Container image | none | Public (`nginx:1.27-alpine`) or private (`{{registry}}/<organization>/<name>:<tag>`). Pin a version tag rather than `latest`. |
| Port | 8080 | 1 to 65535. Also passed to the application as the `PORT` environment variable. |
| Health check path | `/` | Any answer below HTTP 500 counts as healthy. The application has 90 seconds to answer after it starts. |
| CPU | 250 millicores | 50 to 8000; 1000 = one core. Counts against your plan. |
| Memory | 256 MB | 32 to 16384. The application is stopped if it uses more, so leave headroom. |
| Persistent folder | none | A folder kept across deployments and backed up, for example `/data`. System folders are refused. Everything else is reset at each deployment. |
| Environment variables | none | One `KEY=value` per line, up to 100. Names starting with `PLATFORM_` are reserved. Not for passwords. |
| Secrets | none | Name in capitals (`DATABASE_URL`), value up to 8192 characters. Encrypted at rest, passed as environment variables, never shown again. |

Every application runs in a sandbox with its own private network, a limit of 256 processes and no special system privileges.

## Updates and rollbacks

Every deployment is a numbered revision. The new version starts next to the old one, and traffic switches only after it passes its health check, so visitors see no downtime.

| Status | Meaning |
| --- | --- |
| pending | Waiting to start. |
| deploying | Pulling, scanning, starting or waiting for the health check. |
| live | Serving traffic. Only one revision per service is live. |
| superseded | A newer revision replaced it. It can be redeployed. |
| failed | It did not start or did not pass its health check. The previous live revision keeps serving. |

- **Update:** change the image tag in Settings and click **Save and deploy**.
- **Roll back:** click **Redeploy** next to an earlier revision on the Overview tab. Its exact settings come back as a new revision; secrets use their current values.
- **Delete a service:** at the bottom of the Settings tab, type the service ID to confirm.

## Builds from git

Instead of pushing images yourself, the platform can build your repository's **Dockerfile** for you.

1. In the service's **Builds** tab, enter the repository's https address, the branch and, if the Dockerfile is not at the root, its folder. For a private repository add a read-only access token (GitHub fine-grained token, GitLab/Gitea deploy token); it is stored encrypted and never shown again.
2. Click **Build now**, or run `platformctl build shop/web --wait` from CI. With **deploy when built** (the default) the service is redeployed with the new image and its current settings.
3. To build on every push, create a **webhook address** and add it to your repository's webhooks (push events, JSON). Pushes to other branches are ignored.

- Each build gets a page with its full log; images are stored in your registry space as `…/<organization>/<service>:git-<commit>` and scanned like any pushed image.
- Builds run in an isolated sandbox: no access to the platform's internal network, other customers or the server; the internet is reachable for base images and packages. Limits per build: 2 CPU, 2 GB memory, 15 minutes, 8 GB image (uncompressed).
- One build at a time per service: starting a build while one is waiting reuses the waiting one. At most 5 builds waiting or running per organization.
- Only Dockerfile builds are supported for now; the build fails clearly if there is no Dockerfile.

## Preview environments

A preview is a temporary copy of a service, with the same settings, secrets and traffic protection but another image, at its own address: `<name>.<service>-<project>-<organization>.<apps domain>`. Typical use: one preview per pull request, so reviewers can click through the change before it is merged.

- **Create one** in the service's **Previews** tab (name, image, lifetime), or from CI with `platformctl preview create shop/web pr-42 --image registry.example.com/acme/web:pr-42 --wait`. Running the same command again redeploys that preview with the new image and renews its lifetime, so it can run on every push.
- **Lifetime:** 72 hours by default, up to 720. The preview is deleted automatically when it expires; delete it earlier with the **Delete** button or `platformctl preview delete shop/web pr-42` (for example when the pull request is closed).
- **Limits:** at most 5 previews per service. They use your plan's CPU and memory (and are billed like services) but do not count as services.
- Previews never get a persistent folder or backups, and do not appear on public status pages. A preview's logs are under **logs and details** in the Previews tab.
- Secrets are copied when the preview is created; later changes to the service's secrets do not reach existing previews.

## Managed databases

Managed PostgreSQL and Redis run next to your services, on the project's private network, with nightly consistent backups.

1. **Create it** on the project's **Databases** tab (developers): give it an ID such as `main` and choose PostgreSQL 17, PostgreSQL 16 or Redis 7. It starts within a minute with 250 millicores and 256 MB, counted against your plan.
2. **Connect a service:** on the database's **Connect** tab, choose a service and click **Add secret**. The connection URL is stored as an encrypted secret of that service: `DATABASE_URL` for PostgreSQL, `REDIS_URL` for Redis. Redeploy the service to apply it.
3. **Use it** from your application through that variable, for example `postgres://app:…@pd-…:5432/app?sslmode=disable` or `redis://default:…@pd-…:6379/0`.

- **Who can reach it:** only services of the same project. The database is not reachable from the internet or from other projects, so traffic stays on the private network.
- **Who can see the password:** developers and above. Viewers see the host, port, database and user only.
- **Resources:** change CPU and memory on the **Settings** tab. The database restarts with the new values; data is kept.
- **Backups:** every night at the hour you choose, the platform takes a consistent `pg_dump` (PostgreSQL) or snapshot (Redis), encrypts it with your organization's key and keeps 7 daily, 4 weekly and 6 monthly snapshots. **Back up now** takes one immediately.
- **Restore** (admins): type the database ID next to a snapshot and click **Restore**. A safety snapshot of the current data is taken first and always kept; the snapshot is then loaded with `pg_restore`, replacing existing tables; Redis is briefly stopped and restarted with the snapshot. Connected applications may see errors for a few seconds.
- **Redis memory:** Redis uses at most 80% of the memory you give it and then refuses new writes instead of evicting keys, so size it for your data.
- **Plans:** Starter includes 1 managed database, Pro includes 5 (PostgreSQL and Redis together).

## Public status page

Each project can have a public page that anyone can open, without an account, at `/status/<organization>/<project>`. Share it with your customers.

- Turn it on in the project's **Settings** tab (admins and owners). It is off by default.
- It shows each service's state (operational, down or not running), daily uptime over the last 30 days and outages from the last 14 days.
- It never shows logs, error messages, images, environment variables or secrets.
- Turn it off at any time; the address stops working immediately.

## Custom domains and HTTPS

Every service is always reachable at its platform address; you can add your own domains, with automatic HTTPS once you prove you own them.

1. On the **Domains** tab, enter the domain (for example `shop.example.com`) and click **Add domain**.
2. At your DNS provider, create a **TXT** record named `_platform-challenge.shop.example.com` with the value `platform-verify=<token>` shown on screen.
3. Create a **CNAME** (or A record) for `shop.example.com` pointing to the platform.
4. Click **Verify**. DNS changes can take a few minutes; try again if the record is not found yet.

Once verified, the domain serves the application within seconds, and an HTTPS certificate is issued on the first visit. Starter plans allow 3 custom domains, Pro plans 20. A domain can belong to only one organization.

## Private image registry

Your organization has a private image space at `{{registry}}/<organization>/`. Nobody else can read or write it, and every image pushed there is scanned for vulnerabilities.

1. **Create a token** (admins): on the organization page, open the **Registry** tab, name the token (for example `ci`) and choose whether it can push. Copy it immediately: it is shown once. Tokens expire after one year and can be revoked at any time.
2. **Log in, tag and push:**

```
docker login {{registry}} -u <organization>
docker tag myapp:1.4.2 {{registry}}/<organization>/myapp:1.4.2
docker push {{registry}}/<organization>/myapp:1.4.2
```

3. **Deploy it:** set the service image to `{{registry}}/<organization>/myapp:1.4.2`.

- Use a push token in CI and store it in your CI's secret store; use pull-only tokens elsewhere.
- An image from another organization's space is always refused.
- The **Last used** column shows whether a token is still in use.

- **Clean-up:** tags that no recent deployment uses and that nobody pushed in the last 30 days are deleted automatically. Tags your services run, or ran in their last 20 deployments, are always kept.

## Traffic protection

Each service's **Security** tab has three protections, applied by the platform's proxy before requests reach your app, on every address of the service. Changes apply within a few seconds, without a redeploy. Developers and above can change them.

- **Web application firewall:** the OWASP Core Rule Set blocks common attacks such as SQL injection, cross-site scripting, path traversal and known scanner probes. Blocked requests get `403 Forbidden`. It is off by default because some apps (for example ones that accept HTML or code in forms) can trigger false positives: turn it on, then click through your app.
- **Requests per minute per visitor:** each visitor IP address may send at most this many requests per rolling minute; above it they get `429 Too Many Requests` with a `Retry-After` header. Count every asset a page loads (scripts, images, API calls) when you choose the number.
- **Blocked addresses:** up to 100 IPv4/IPv6 addresses or ranges (for example `203.0.113.7` or `198.51.100.0/24`) that always get `403`.

Health checks and alerts are not affected: the platform checks your app directly, not through the proxy.

## Security scanning

Images are checked against a public database of known vulnerabilities (CVEs), at three moments:

- **On push** to your private registry.
- **On deployment**, before the application starts. A result from the last 24 hours for the same image is reused.
- **Every night**, for images that are running, because new vulnerabilities are published daily.

The **Security** tab of a service shows the scan of its latest deployment: counts per severity, then up to 25 findings, most severe first, with the package, installed version and fixed version.

| Policy | Effect |
| --- | --- |
| warn (default) | Findings are shown; deployments are never blocked. |
| block-critical | An image with a critical vulnerability is refused, and the deployment shows which ones. If the scanner is unavailable, deployments fail rather than go out unscanned. |

The platform operator sets the policy per organization. Most findings come from the base image: rebuild on its latest patch version, push, and redeploy.

## Logs and metrics

Everything your application writes to standard output and standard error is collected.

- **Logs tab:** the last 6 hours, up to 300 lines, refreshed every few seconds while the tab is open. Use the filter to search; untick **follow** to stop auto-scrolling.
- **Overview tab:** CPU (in % of one core) and memory (MB against the limit) over the last hour, about a minute after the first deployment.
- **Privacy:** your logs and metrics are stored separately from other clients' and are only readable through your organization.

Write logs to standard output rather than files: files are lost at each deployment unless they are in the persistent folder.

## Backups and restore

The persistent folder of each service is backed up every night, encrypted with a key unique to your organization. Services without a persistent folder hold no data to back up.

- **Schedule:** nightly at the hour you choose (UTC, 03:00 by default), on the **Backups** tab.
- **Back up now** (developers): takes a snapshot immediately, for example before a risky change.
- **Retention:** 7 daily, 4 weekly and 6 monthly snapshots.

To restore (admins):

1. In **Snapshots**, find the one to go back to.
2. Type the service ID next to it and click **Restore**.
3. The platform takes a **safety snapshot** of the current data, which is always kept, stops the application, replaces the folder, and starts the application again.
4. If the restore was a mistake, restore the snapshot marked *before restore*.

Backups are taken while the application runs. If your service runs its own database, dump it to the persistent folder on a schedule so the backup holds a consistent copy.

## Alerts

The platform watches your applications continuously and records incidents on the organization's **Alerts** tab and each service's **Alerts** tab.

| Event | When it happens |
| --- | --- |
| service_down | 3 health checks in a row fail, or the container stopped or ran out of memory. Resolves automatically when the application answers again. |
| cpu_high / memory_high | Usage stayed above your threshold for 5 minutes. Set thresholds in % of the service's limits; empty means off. |
| deploy_failed | A deployment failed. |
| backup_failed | A backup or restore job failed. |
| critical_vulns | The nightly scan found critical vulnerabilities in a running image. |

Add a notification channel (admins) on the organization's **Alerts** tab:

- **ntfy:** push notifications to a phone or desktop. Enter the server URL, a topic, and an access token if the topic is protected.
- **webhook:** an HTTPS endpoint receives JSON with `kind`, `status` (firing, resolved or event), `title`, `summary`, `organization`, `project`, `service`, `started_at` and `console_url`.

Pick the events for each channel and click **send test**. If you set a webhook secret, each request carries `X-Platform-Signature: sha256=<hex>`, the HMAC-SHA256 of the raw body with your secret:

```
import hmac, hashlib
expected = "sha256=" + hmac.new(secret.encode(), raw_body, hashlib.sha256).hexdigest()
valid = hmac.compare_digest(expected, request.headers["X-Platform-Signature"])
```

Channel URLs must be public HTTPS addresses; private and internal addresses are refused.

## API and command-line tool

Deploy from your CI pipeline with an API token, through the REST API or the `platformctl` tool.

1. **Create a token** (admins): on the organization page, open the **API** tab, name it (for example `github-actions`) and pick a role. Copy it immediately: it is shown once. Tokens expire after one year and can be revoked at any time.
2. **Set two variables** in your CI: `PLATFORM_URL={{console}}` and `PLATFORM_TOKEN=pla_…` (as a secret).
3. **Deploy:**

```
platformctl deploy shop/web --image {{registry}}/<organization>/web:1.4.2 --wait
```

| Command | What it does |
| --- | --- |
| `platformctl whoami` | Shows the token's organization and role. |
| `platformctl projects` | Lists projects. |
| `platformctl services <project>` | Lists services and the status of their last deployment. |
| `platformctl status <project>/<service>` | Settings, address and recent deployments. |
| `platformctl deploy <project>/<service> [--image IMAGE] [--wait]` | Deploys current settings, optionally with a new image; `--wait` follows it until live or failed and exits with an error on failure. |
| `platformctl logs <project>/<service> [--filter TEXT]` | Prints recent log lines. |
| `platformctl secret set <project>/<service> NAME` | Sets a secret; the value is read from standard input, never from the command line. |

`platformctl` is a single program with no dependencies; build it with `go build ./cmd/platformctl` from the platform's source, or ask your operator for a copy.

The same operations are available over HTTP with `Authorization: Bearer <token>`:

| Method and path | Role |
| --- | --- |
| `GET /api/v1/me` | viewer |
| `GET /api/v1/projects` | viewer |
| `GET /api/v1/projects/{project}/services` | viewer |
| `GET /api/v1/projects/{project}/services/{service}` | viewer |
| `POST /api/v1/projects/{project}/services/{service}/deploy` with optional `{"image": "…"}` | developer |
| `GET /api/v1/projects/{project}/services/{service}/deployments/{revision}` | viewer |
| `GET /api/v1/projects/{project}/services/{service}/logs?q=text` | viewer |
| `PUT /api/v1/projects/{project}/services/{service}/secrets/{NAME}` with `{"value": "…"}` | developer |

A token works only inside its organization and only with its role. Every change made with a token appears in the audit log, named after the token.

## Disk limits

Each plan includes an amount of persistent storage: service folders and managed databases (with their backup dumps). The organization page shows **disk used / allowed**; it is measured every 15 minutes.

- At 90% and 100% of the limit, your alert channels get a "Disk limit" notification.
- Above 110%, new deployments and new databases are refused until you delete data or move to a larger plan. Running applications keep running, and rolling back is always allowed.

## Billing and usage

Admins and owners find **Billing** in the organization's tabs.

- **What is billed:** your plan's monthly fee, plus the CPU and memory **allocated** to each running service and database (per hour, at the size you chose, not the share the app actually used), plus network traffic going out of your services. Stopped or deleted resources cost nothing.
- **This month so far:** the page shows the estimated amount up to now and the usage of each service and database. It updates every minute.
- **Invoices** cover one calendar month (UTC) and appear on the 1st of the next month. Open one to print it or save it as PDF from your browser. The plan fee of your first month is counted from the day the organization was created.
- **Billing details:** the company name, address and tax ID printed on your invoices. Changes apply to the next invoices, not to ones already issued.
- **Paying:** follow the payment instructions you received from the platform operator; an invoice shows **paid** once the operator has recorded your payment.

## Team members and roles

Each member has one role per organization; each role includes everything the roles above it can do.

| Role | Can do |
| --- | --- |
| viewer | See projects, services, deployments, logs, metrics, scan reports, backups and incidents. |
| developer | Create and delete services; change settings, secrets and domains; deploy and roll back; start a backup; set alert thresholds. |
| admin | Create and delete projects; invite and remove members; create registry tokens; manage alert channels; restore backups; read the audit log. |
| owner | Everything, including inviting or removing owners. An organization always keeps at least one owner. |

- **Invite:** on the **Members** tab, enter the email and role, then send the link shown. It is displayed once, works once, and expires in 7 days.
- **Audit log** (admins): every action, with who, when and from which IP address. Entries are chained, so they cannot be altered without detection.

## Limits and good practices

| Plan | Services | CPU in total | Memory in total | Custom domains |
| --- | --- | --- | --- | --- |
| Starter | 3 | 1000 millicores | 1024 MB | 3 |
| Pro | 10 | 4000 millicores | 4096 MB | 20 |

- **No direct email:** outgoing ports 25, 465 and 587 are blocked; use an email provider's HTTPS API.
- **No access to internal networks:** applications reach the internet, but not the server, other clients or private ranges.
- **Pin image versions** (`myapp:1.4.2`, not `latest`), so rollbacks are exact.
- **Keep the health check fast**, keep data in the persistent folder, use secrets for passwords, and add an alert channel.

## Troubleshooting

| Message | What to do |
| --- | --- |
| nothing answering on port N | Check the **Port** setting, and that the application listens on `0.0.0.0`, not only `127.0.0.1`. |
| health check returned HTTP 5xx | Point the health path to a lightweight page and read the logs. |
| container stopped (exit code N) | Read the log lines under the status: usually a missing environment variable or secret. |
| killed: out of memory | Raise **Memory**, or reduce the application's memory use. |
| could not pull image | Check the spelling and tag; push private images to your registry first. |
| images from the platform registry must be under … | Use `{{registry}}/<your organization>/…`. |
| blocked by security policy | Rebuild on an updated base image, push and redeploy. |
| CPU limit reached / Memory limit reached | Lower other services' resources, delete unused services, or ask for a bigger plan. |
| The TXT record … was not found yet | Check the record name and value, wait a few minutes, verify again. |
| invalid or missing CSRF token | Reload the page and try again. |
| Too many attempts | Wait 15 minutes. |

Still stuck? Send the platform operator the service address and the time of the problem.
