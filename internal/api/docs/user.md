# User guide

For client teams who deploy applications on the platform. Console: `{{console}}`.

This guide explains every part of the console: what each feature is for, how to use it step by step, and the details worth knowing (limits, plans, what happens behind the scenes). Each section matches a product of the console's **Services menu**.

## Getting started

Before you can use the console you need an account. Accounts are created by invitation (or, when the platform allows it, by signing up), and every account must use two-step login with an authenticator app, so a stolen password alone never opens your organization.

### Accept an invitation

1. **Open your invitation link** from the email you received. It works once and expires after 7 days. If it has expired, ask your organization's admin or the platform operator for a new one.
2. **Choose your name and a password** of at least 12 characters, and type it twice. If you already have an account on the platform, the page says so: enter its password instead to join the new organization.
3. **Set up two-step login.** Scan the QR code with an authenticator app such as Aegis, 2FAS, FreeOTP or Google Authenticator. If you cannot scan, type the key shown under **Can't scan?** (time-based, 6 digits, 30 seconds). Then type the 6-digit code the app shows and click **Confirm and continue**.
4. **Save your recovery codes** on the next page (**Print** them or copy them to a password manager), then click **I saved them, continue**.
5. **Log in next time** with your email and password, then the current 6-digit code.

### Create an account without an invitation

If the platform allows sign-up, the login page shows **Create an account**.

1. Enter your name, email, a password (at least 12 characters, typed twice), an **Organization name** (your company or project) and an **Organization ID** (2 to 20 lowercase letters, digits and dashes; it appears in your application addresses).
2. Click **Create account**. The page checks your browser for a few seconds (an anti-abuse check, no puzzle to solve).
3. Open the confirmation link sent to your email. It works once, within 24 hours. Nothing arrived? Check your spam folder, then sign up again.
4. Set up two-step login as above. You get your own organization on a free trial (see [Free trial](/docs/user#free-trial)).

### Sign-in rules

- **Sessions** last up to 12 hours and end after 2 hours without activity.
- **Lockout:** after 5 wrong passwords, the account is locked for 15 minutes ("Too many attempts").
- **Recovery codes:** right after two-step setup you get 10 one-time codes (format `abcde-fghij`). Each one replaces the 6-digit code once: type it in the **or a recovery code** field. To get a fresh set, open **Account and security** (account menu, top right) and click **Create new codes**; the old codes stop working.
- **Lost phone:** log in with your password and a recovery code, then set up two-step login again. No codes left? Ask the platform operator.
- **Forgot your password:** choose **Forgot your password?** on the login page to get a one-hour link by email (or ask the operator for one). All your sessions end when you set the new password, and two-step login is still required.
- **Single sign-on:** if your organization uses its own identity provider, click **Sign in with SSO** instead (see [Single sign-on](/docs/user#single-sign-on)).
- **Several organizations:** after logging in you see the list of organizations you belong to, with your role in each. Open one to enter its console.

### Passkeys

A passkey lets you sign in with your fingerprint, face or device PIN instead of your password and 6-digit code. It is faster than typing a code and cannot be phished. It works with phones, laptops, security keys and password managers that support passkeys.

1. Open the account menu (top right), choose **Account and security**, and find **Passkeys**.
2. Type a **Name** you will recognize, such as "Work laptop", and click **Add a passkey**. Confirm on your device.
3. Next time, on the login page, choose **Sign in with a passkey** and confirm on your device.

- The list shows each passkey with when it was added and last used. A **synced** tag means the passkey lives in a password manager or cloud keychain and is available on your other devices too.
- To remove one (a lost or replaced device), click **Remove** next to it.
- Only a public key is stored on the platform; the private key never leaves your device or password manager.
- Your password and authenticator app keep working: use them if you lose the device, then remove the lost passkey.
- A passkey only works on this platform's address, so a look-alike phishing site cannot use it.
- If your browser cannot create passkeys, the page says "This browser does not support passkeys".

## Free trial

The free trial lets you try the platform with real applications before choosing a paid plan. Organizations created with **Create an account** start on the **Trial** plan, free for 14 days.

What the trial includes (default values; your operator may set others):

| Resource | Trial |
| --- | --- |
| Services | 2 |
| CPU in total | 500 millicores (half a core) |
| Memory in total | 512 MB |
| Managed databases | 1 |
| Instances per service | 1 (no autoscaling) |
| Persistent disk | 2048 MB |
| Secrets in the secrets manager | 10 |
| Public TCP/UDP ports | none |
| Custom domains | none |
| Builds from git and zip deployments | not included: push an image instead |

- The console home shows a notice with the days left.
- You get an email 3 days before the trial ends (when the platform sends email).
- When the trial ends, your applications stop and changes are blocked; your data is kept. Contact the platform operator to choose a plan and everything starts again.
- Applications that use all their CPU for a long time (typical of crypto-mining) get a trial organization suspended automatically. The console home shows a **suspended** badge and says why; contact the operator if it was legitimate work.

## How things are organized

Everything you create lives in a simple hierarchy: your **organization** holds **projects**, and projects hold **services**, **databases** and **functions**. Knowing it helps you find things and decide what to group.

- **Organization:** your company's space. It holds the members and their roles (Access management), the plan and its limits, the audit log, your private registry space, notification channels (Monitoring), buckets (Object storage), queues, email domains and the secrets manager.
- **Project:** a group of related resources that make up one application, for example `shop`. Resources in a project share a private network and the project's shared variables.
- **Service:** one running container (a website, an API or a background worker) with its own address: `<service>-<project>-<organization>.{{apps}}`.
- **For each service:** deployments (numbered revisions), settings and secrets, domains, logs, metrics, health and alerts, a vulnerability report, and, when it has a persistent folder, backups.

Everything inside your organization is private to its members: other clients cannot see or reach it, through the console, the network or the registry.

### Projects

A project keeps one application's parts together, so you can see them at a glance and share settings between them.

1. Open **Projects** from the Services menu (or the **Projects** widget on the console home) and click **Create project** (admins).
2. Enter a **Project ID**: 2 to 20 lowercase letters, digits or dashes, for example `shop`. It is used in addresses and cannot be changed. Optionally add a **Display name** such as "Online shop".
3. Click **Create project**. API: `POST /api/v1/projects` with `{"slug": "shop", "name": "Online shop"}`. CLI: `platformctl projects` lists them.

The project page has these tabs:

| Tab | What it shows |
| --- | --- |
| **Overview** | Counters (live services, databases, shared variables, domains and jobs) and **All resources**: every service, database, domain and scheduled job of the project with its status, with a search box. |
| **Map** | The project map (below). |
| **Services** | The project's services with their image and last deployment, and an **Add a service** form. |
| **Databases** | The project's databases with engine, resources and status, and a **Create a database** form. |
| **Variables** | Shared variables (see [Project variables](/docs/user#project-variables)). |
| **Settings** (admins) | **Private network** connections to other projects (see [Private network between projects](/docs/user#private-network-between-projects)), the **Public status page** switch (see [Public status page](/docs/user#public-status-page)), and **Delete project**. |

**Delete a project** (admins): on the **Settings** tab, type the project ID and click **Delete project**. Everything in it is deleted (services, databases, functions, variables, jobs, previews) and its containers are stopped. This cannot be undone, so take the backups you need first.

### Project map

The **Map** tab shows how the project fits together, so a newcomer understands an application in seconds and you can spot a missing link.

- **Comes in from** (left): the internet, custom domains, schedules (jobs), function webhooks and connected projects.
- **Runs** (middle): services, workers and functions.
- **Uses** (right): databases, queues, buckets and the email sending service.
- Each box has a status color: running, deploying or starting, failed or stopped. A dashed line means a service calls another through a private name or public address found in its plain environment variables.
- Links come from what the platform set up (connection secrets, domains, jobs, triggers, project connections) and from host names in plain environment variables. Secret values are never read.
- Click a box to open that resource. The map refreshes on its own, every 3 seconds while something is changing.

### Getting around the console

The console works like a cloud provider's console: each product has its own area, and a few tools help you move around fast.

- **Services menu** (left side): every product, grouped as Compute, Data, Networking, Delivery, Operations and Management. Products the platform does not offer (for example object storage on an installation without it) are hidden.
- **Favorites:** open **All services** (bottom of the left menu) to see the whole catalog. Click **Pin** (the star) on a product to add it to the favorites bar at the top of the left menu and to the **Favorites** widget of the console home; click **Pinned** to remove it. Pins are personal to you. By default Compute, Functions, Databases, Object storage and Monitoring are pinned.
- **Area menu:** inside a product, its own pages appear under its name, for example Compute → Services, Scheduled jobs, Previews.
- **Search** (top bar, or press `/` anywhere outside a text field): type part of a name and press Enter. It finds projects, services (by name or image), databases (by name or engine), functions, queues, buckets and domains, and products by what they do ("backup", "email"). Up to 50 results.
- **Project filter** (the **Project** box in the top bar): narrows the lists of every area (services, databases, domains, scheduled jobs, previews) to one project. Choose **All projects** to see everything again. The choice is remembered in your browser for 6 months.
- **Project tree:** the left pane lists every project with its services, databases (`db`) and functions (`fn`), and the status of each service's latest deployment: ● live, × failed, ◌ deploying, ○ not deployed. Click a name to open it.
- **Breadcrumbs** above each page show where you are (Console › Compute › Online shop › storefront); click a part to go back up.
- **Lists:** every list spans all your projects (or the filtered one), shows a count, and has a search box that hides non-matching rows as you type.
- **Tabs** on a resource page (Overview, Settings…) can also be changed with the arrow keys.
- **Organization menu** (top right): switch to another organization you belong to.
- **Account menu** (top right): **Account and security** (passkeys, recovery codes), **Appearance** (**Light**, **Dark**, or **Auto** to follow your device), the user guide and **Log out**.
- **Small screens:** the menu button at the top left shows or hides the left menu.

### Console home

The console home (the logo, or **Console home** in the left menu) gives you the state of the organization at a glance:

- **Favorites:** your pinned products.
- **Projects:** each project with its number of services; **View all** opens the Projects list.
- **Health:** open incidents, or "healthy" when every live application answers its checks.
- **Usage of your plan:** services, CPU, memory, databases and disk used against your plan's limits.
- **Get started:** shortcuts to deploy a container, create a database, create a bucket and deploy from CI with an API token.

### Activity indicator

When something is in progress, the top bar shows a spinner with **N in progress**, so nothing seems to hang without a word.

- Click it to see each item with its state: builds (queued, with their place in the queue, or building, with the current step), deployments (waiting or deploying), databases starting, and backups or restores.
- When an item finishes, a notice appears for a few seconds ("build finished. See the result") with a link to it. If you are on that item's page, the page reloads to show the result.
- It checks every 3 seconds while something is in progress, and every 15 seconds otherwise. Members limited to some projects see only their projects' items.

### Create dialogs

The main create buttons at the top right of a page (**Create service**, **Create database**, **Create project**, **Create secret** and others) open their form in a dialog over the page. If the form is refused (a name already taken, a plan limit), the page comes back with the error and the dialog open again, so you can fix it. Without JavaScript, the button scrolls to the same form at the bottom of the page.

### Bulk actions on lists

Bulk actions let you act on many resources at once, for example redeploy every service after a base image update, or back up all databases before a migration.

1. In a list (services, databases, functions, buckets or domains), tick the checkbox of each row, or the checkbox in the header to select every row currently shown (rows hidden by the search box are not selected). The bar above the list shows how many are selected.
2. Choose an action in **Actions**.
3. For a delete, type `delete` in the confirmation box.
4. Click **Apply**. The page says what was done and lists anything skipped, with the reason (for example "no image" or "a backup is already running").

| List | Actions |
| --- | --- |
| Compute → Services | **Redeploy with current settings**, **Delete** (developers) |
| Databases | **Back up now** (developers), **Delete** (admins) |
| Functions | Turn off the function URL, delete (developers) |
| Object storage → Buckets | Delete (admins) |
| Domains | Remove (developers) |

Each bulk change is recorded in the audit log like a single one.

## Deploy your first application

A service runs your application from a container image and is usually live within a minute. You can push an image you built yourself or in your CI to any registry, or let the platform build it from git or a zip file (see [Builds from git](/docs/user#builds-from-git)).

1. **Create a project** (admins): **Projects** → **Create project**, for example `shop`.
2. **Create the service** (developers): open **Compute** and click **Create service**. The dialog has five steps:
3. **Name:** choose the **Project** and a **Service name** (2 to 20 lowercase letters, digits and dashes), such as `web`.
4. **Source:** the **Container image**, such as `nginx:1.27-alpine` or `ghcr.io/acme/api:1.4`. Leave it empty to build from a git repository later, on the service's **Builds** tab.
5. **Type and network:** **Web service** (public HTTPS address, health checks on the port) or **Worker** (background process, no address); the **Port** your app listens on (nginx: `80`) and the **Health check path**.
6. **Resources:** **CPU** in millicores, **Memory** in MB and **Instances**.
7. **Environment:** variables, one `NAME=value` per line (add secrets on the service page afterwards). Keep **Deploy now** ticked (it needs an image) and click **Create service**.
8. **Follow the deployment** on the service's **Overview** tab: each step appears (pulling, scanning, starting, health check), and the status updates on its own.
9. **Open the address** shown at the top right of the service page.

Shortcut: on a project's **Services** tab, **Add a service** creates one with just an ID and an image; then check its **Settings** tab and deploy. API: `POST /api/v1/projects/PROJECT/services` with `{"slug": "web", "image": "nginx:1.27-alpine"}`, then `POST /api/v1/projects/PROJECT/services/SERVICE/deploy`. CLI: `platformctl deploy shop/web --image nginx:1.27-alpine --wait`.

If the deployment fails, the reason and the last log lines appear under its status. A failed deployment never replaces a working one. See [Troubleshooting](/docs/user#troubleshooting) for the usual messages.

## Service settings

A service's settings say what to run (image), how to reach it (type, port, health check), how big it is (CPU, memory, instances) and what it receives (variables, secrets). Open the service and its **Settings** tab.

1. Change the values in the **Container** form.
2. Click **Save and deploy** to apply them now, or **Save only** to deploy later (with **Deploy current settings** on the **Overview** tab). Settings take effect at the next deployment.

API: `PUT /api/v1/projects/PROJECT/services/SERVICE/settings` with the form's fields as JSON (`image`, `kind`, `port`, `health_path`, `cpu_millis`, `memory_mb`, `instances`, `autoscale`, `autoscale_min`, `autoscale_max`, `autoscale_cpu`, `private`, `edge_cache`, `metrics_path`, `metrics_port`, `ports`, `volume_path`, `env` as `"A=1\nB=2"`, and `deploy: true` to deploy at once). CLI: `platformctl status shop/web` shows the current settings.

| Setting | Default | Rules and tips |
| --- | --- | --- |
| Container image | none | Public (`nginx:1.27-alpine`) or private (`{{registry}}/<organization>/<name>:<tag>`). Pin a version tag rather than `latest`, so rollbacks are exact. |
| Type | Web service | **Web service:** answers HTTP at a public address. **Worker:** runs in the background (a queue consumer, a bot) with no address, no port and no domains; it is healthy while it keeps running. Remove a service's custom domains before making it a worker. |
| Port the app listens on | 8080 | 1 to 65535. Also passed to the application as the `PORT` environment variable. Web services only. |
| Health check path | `/` | Any answer below HTTP 500 counts as healthy. The application has 90 seconds to answer after it starts. Web services only. |
| CPU | 250 millicores | 50 to 8000; 1000 = one core. Counts against your plan. |
| Memory | 256 MB | 32 to 16384. The application is stopped if it uses more, so leave headroom. |
| Instances | 1 | 1 to 10 identical copies, up to your plan's limit (5 on the default paid plans, 1 on Trial). See [Several instances](/docs/user#several-instances). |
| Autoscaling | off | Minimum, maximum and CPU target. See [Autoscaling](/docs/user#autoscaling). |
| Private: no public address | off | See [Public or private](/docs/user#public-or-private). |
| Edge cache (CDN) | off | The proxy keeps responses your app marks as cacheable. See [Edge cache (CDN)](/docs/user#edge-cache-cdn). |
| Metrics path and port | none | Your app's Prometheus endpoint, such as `/metrics`, scraped every 30 seconds; the port defaults to the service's port. See [Prometheus and Grafana](/docs/user#prometheus-and-grafana). |
| Public TCP/UDP ports | none | For non-HTTP traffic. See [TCP and UDP services](/docs/user#tcp-and-udp-services). |
| Persistent folder | none | A folder kept across deployments and backed up, for example `/data`. System folders are refused. Everything else in the container is reset at each deployment. A service with a persistent folder runs a single instance. |
| Environment variables | none | One `KEY=value` per line, up to 100. Names starting with `PLATFORM_` are reserved. Not for passwords: use secrets. A value can be a reference to the secrets manager, `${secret:NAME}`. |

Below the form, the **Settings** tab also has:

- **Scheduled scaling:** see [Scheduled scaling](/docs/user#scheduled-scaling).
- **Secrets:** values your app needs but nobody should read back, such as a database password or an API key. Enter a **NAME** in capitals (`DATABASE_URL`) and a value (up to 8192 characters), then click **Save secret**. They are encrypted at rest, passed as environment variables and never shown again; **delete** removes one. Deploy to apply changes. API: `PUT /api/v1/projects/PROJECT/services/SERVICE/secrets/NAME` with `{"value": "…"}`. CLI: `platformctl secret set shop/web NAME` (the value is read from standard input, never from the command line).
- **From the project:** the project's shared variables this service also receives (see [Project variables](/docs/user#project-variables)).
- **Delete service:** type the service ID and click **Delete service**. The container stops, and the service is deleted with its settings, secrets and domains. Data in the persistent folder is kept by the operator until it is cleaned up.

Every application runs in a sandbox with its own private network, a limit of 256 processes and no special system privileges.

The service page's other tabs are explained in their own sections: **Overview** ([Updates and rollbacks](/docs/user#updates-and-rollbacks)), **Logs** ([Logs and metrics](/docs/user#logs-and-metrics)), **Domains** ([Custom domains and HTTPS](/docs/user#custom-domains-and-https) and below), **Jobs** ([Scheduled jobs](/docs/user#scheduled-jobs)), **Security** ([Traffic protection](/docs/user#traffic-protection), [Security scanning](/docs/user#security-scanning)), **Backups** ([Backups and restore](/docs/user#backups-and-restore)), **Alerts** ([Alerts](/docs/user#alerts)), **Builds** ([Builds from git](/docs/user#builds-from-git)) and **Previews** ([Preview environments](/docs/user#preview-environments)).

### Public or private

Make a service private when only your other services should call it, for example an internal API or an admin back end: it is then unreachable from the internet.

1. On the service's **Settings** tab, tick **Private: no public address**.
2. Click **Save and deploy**.

- Its platform address and custom domains stop answering. Your other services in the same project, and in connected projects, still reach it at `http://<service>.<project>.internal` (plain HTTP, port 80; see [Private network between projects](/docs/user#private-network-between-projects)).
- The **Domains** tab then shows a **private** badge. Untick the box to publish the service again.
- A path of one of your public domains can still send traffic to it (see [Paths served by other services](/docs/user#paths-served-by-other-services)).
- Previews of a private service are private too.

### Platform address on or off

Every public web service answers on its platform address, `https://<service>-<project>-<organization>.{{apps}}`. Once your own domain works, you can turn that address off so visitors and search engines only use your domain.

1. Add and verify a custom domain on the service's **Domains** tab.
2. Under **Platform address**, click **Turn off the platform address** and confirm.
3. To undo it, click **Turn the platform address back on**.

- It applies within seconds, without a redeploy, and is recorded in the audit log.
- The button only appears once a custom domain is verified. If the service later has no verified domain left, the platform address answers again on its own, so the service is never left without an address.
- Private access at `http://<service>.<project>.internal` is not affected.
- API: `POST /api/v1/projects/PROJECT/services/SERVICE/default-address` with `{"on": false}` or `{"on": true}`.

### More public ports

Some applications serve a second web interface on another port, for example an app on port 80 and an admin interface on port 9000. You can publish that port on its own HTTPS address.

1. Open the service's **Domains** tab, section **More public ports**.
2. Enter a **label** (a lowercase letter, then up to 19 lowercase letters or digits, such as `admin`) and the **port** (1 to 65535).
3. Click **Publish port**. The address is `https://<label>.<service address>`, for example `https://admin.web-shop-acme.{{apps}}`, with HTTPS.
4. **unpublish** removes it.

- Up to 5 per service. Public web services only (not workers, private services or previews).
- API: `GET/POST /api/v1/projects/PROJECT/services/SERVICE/ports` (`{"label": "admin", "port": 9000}`), `DELETE …/ports/LABEL`.

## Project variables

Settings that every service of a project needs (an API key, a base address, a feature flag) can be set once on the project instead of on each service, so they never drift apart.

1. Open the project and its **Variables** tab.
2. Under **Add or change a variable**, enter a name in capitals (`API_BASE_URL`, up to 64 characters) and a value (up to 8192 characters). Keep **Secret** ticked to hide the value after saving; untick it for harmless values you want to read back later.
3. Tick **Redeploy the running services now to apply it** to apply it at once, then click **Save variable**. Or click **Redeploy all services** (section **Apply changes**) later. Each service switches over with no downtime.
4. To change a variable, save it again with the same name. **delete** removes one (redeploy to apply).

API: `GET /api/v1/projects/PROJECT/variables` (secret values omitted), `PUT /api/v1/projects/PROJECT/variables/NAME` with `{"value": "…", "secret": true}`, `DELETE …/variables/NAME`, `POST /api/v1/projects/PROJECT/redeploy`. CLI: `platformctl var list shop`, `platformctl var set shop NAME [--plain]` (value from standard input), `platformctl var unset shop NAME`, `platformctl redeploy shop`.

How they combine:

- Every service of the project, and its previews, receives the project variables as environment variables.
- A service's own environment variable or secret with the same name wins over the project variable.
- Values are encrypted at rest. Secret values are never shown again, not even to admins or the platform operator; plain values are visible to members of the organization.
- A service's **Settings** tab lists the project variables it receives, under **From the project**.
- Changes apply at the next deployment of each service. Rolling back to an earlier revision uses the current values.
- **Redeploy all services** redeploys only services that are live and not already deploying.
- `PORT` and names starting with `PLATFORM_` are reserved.

## Secrets manager

The secrets manager keeps secrets that several services or projects use (a payment provider key, a database password, a webhook signing key) in one place, encrypted and versioned. You change a value once, and every service that uses it is updated.

1. Open **Secrets manager** from the Services menu and click **Create secret**.
2. Enter a **Name** such as `stripe-key` (letters, digits, dots, dashes and underscores, up to 64 characters), an optional **Description**, and the **Value** (up to 64 KB). Click **Create secret**.
3. In a service's or function's environment variables, set a variable to the reference `${secret:stripe-key}`, for example `STRIPE_KEY=${secret:stripe-key}`. The whole value must be the reference.
4. Deploy. The service receives the secret's current value as `STRIPE_KEY`.

**Rotate a secret:** open it, enter the **New value** under **New version** and click **Save** (leave the value empty to only change the description). It becomes the current version, every live service using it is redeployed with no downtime, and functions pick it up at their next start. The last 10 versions are kept.

**Read a value:** on the secret's page, click **reveal** next to a version (developers and admins). The value shows until you leave the page, and each read is recorded in the audit log. Useful to check what an older version was before rolling back.

**Delete a secret:** type its name and click **Delete secret**. A secret in use cannot be deleted: the page lists the services and functions that use it under **Used by**.

Good to know:

- Values are never listed: the list shows name, description, current version, how many resources use it and when it changed.
- A deployment referencing a secret that does not exist fails with a clear message, and the previous revision keeps serving.
- Plan limits (defaults): 10 secrets on Trial, 50 on Starter, 500 on Pro. The list header shows how many you use.
- API: `GET/POST /api/v1/secrets`, `GET/PUT/DELETE /api/v1/secrets/NAME` (`?version=N` to read an older version). CLI: `platformctl vault list`, `vault create NAME VALUE|-`, `vault get NAME [--version N]`, `vault put NAME VALUE|-`, `vault delete NAME` (`-` reads the value from standard input).

## TCP and UDP services

Services that do not speak HTTP (game servers, MQTT brokers, voice servers, custom protocols) can expose TCP and UDP ports directly to the internet.

1. On the service's **Settings** tab, list the ports your container listens on in **Public TCP/UDP ports**, one per line: `25565/tcp`, `27015/udp`. Choose the type **Worker** if the service has no website; a web service can have public ports too.
2. Click **Save and deploy**. Each port gets a **public port** of the platform (for example `20003`), kept as long as the service lists it.
3. The **Overview** tab shows **Public TCP/UDP endpoints**: where to connect (`<service>-<project>-<organization>.{{apps}}:20003`), the protocol and the container port.

Good to know:

- A service with public ports runs a **single instance**, and a deployment replaces the running container: connections drop for a few seconds (the previous version is started again if the new one fails to start).
- Traffic reaches the container directly, without the proxy: the request limit and web application firewall do not apply, so your server must handle its own authentication and limits.
- Plans (defaults): Trial has no public ports, Starter 2, Pro 10. Public database endpoints count in the same total. Previews never get public ports.

## Several instances

Run more than one copy of a service to share the load and to keep it up if one copy fails.

1. On the service's **Settings** tab, set **Instances** (1 to 10, up to your plan's limit) and click **Save and deploy**.
2. Or from the command line: `platformctl scale shop/web 3 --wait`. API: `POST /api/v1/projects/PROJECT/services/SERVICE/scale` with `{"instances": 3}`.

- Every update starts all new instances, waits until each one passes its health check, then switches traffic at once: there is no downtime.
- The proxy sends requests to the instances in turn and stops sending to one that fails for 20 seconds. If an instance disappears, the platform replaces the whole set with a fresh revision.
- CPU and memory count for each instance: 3 instances of 250 millicores use 750 millicores of your plan and are billed that way.
- Instances do not share memory or local files: keep sessions and uploads in a database, Redis or object storage.
- A service with a persistent folder or public TCP/UDP ports runs a single instance; previews always run one. Trial organizations have one instance per service.

### Autoscaling

Autoscaling adds instances when your service is busy and removes them when it is quiet, so you pay for capacity only when you need it.

1. On the service's **Settings** tab, tick **Autoscaling**.
2. Choose the **Minimum instances**, **Maximum instances** (1 to 10) and a **CPU target** (10 to 95% of the CPU you give each instance; 70% by default).
3. Click **Save and deploy**.

- Every minute the platform compares the average CPU of the service's instances (over the last two minutes) with the target, and computes how many instances are needed to get back to it.
- It **scales out** within a minute when the CPU is above the target, and **scales in** only after 5 minutes without changes and when the CPU is well below it, so short spikes do not make it flap.
- Each change is a normal deployment with no downtime, recorded in the audit log with the measured CPU.
- Your plan's instance limit, CPU and memory are checked against the maximum, so the maximum must fit in your plan.
- Services with a persistent folder or public TCP/UDP ports run a single instance and cannot autoscale. Previews do not autoscale.

### Scheduled scaling

For load you can predict (office hours, a nightly batch, a weekly sale), set the number of instances at given times.

1. On the service's **Settings** tab, find **Scheduled scaling**.
2. Enter a time as a cron expression in UTC (`minute hour day month weekday`), the number of instances, and an optional label.
3. Click **Add schedule**. For example `0 8 * * 1-5` with 4 instances and `0 20 * * 1-5` with 1 instance gives 4 instances during office hours on weekdays.

- The table shows each schedule with its next time, when it was last applied and any error. **delete** removes one.
- The platform applies a schedule within a minute of its time, as a normal deployment with no downtime, and records it in the audit log.
- On an autoscaled service, a schedule moves the autoscaling **minimum** instead (raising the maximum if needed); the autoscaler still adds instances above it when the CPU needs them.
- A schedule runs at most every 15 minutes; a service has up to 10. The same rules as above apply: no persistent folder, no public ports.
- API: `GET/POST /api/v1/projects/PROJECT/services/SERVICE/schedules` (`{"cron": "0 8 * * 1-5", "instances": 4, "label": "office hours"}`), `DELETE …/schedules/ID`.

### Failover between servers

On an installation with several servers, a service with several instances stays up when one instance stops answering: the proxy retries the request on another instance and skips the failing one for 20 seconds (also when it answers 502, 503 or 504). There is nothing to set up.

When a whole server stops reporting for a few minutes (3 by default), the platform stops placing work on it and redeploys its projects without data on the other servers. Projects with a database or a persistent folder wait for the operator, who brings the server back or restores them from backups. Running several instances is the best way to ride out a server failure.

## Updates and rollbacks

Every deployment is a numbered revision, so you can always see what ran when and go back to a version that worked. The new version starts next to the old one, and traffic switches only after it passes its health check, so visitors see no downtime.

The service's **Overview** tab lists the revisions (**Deployments**) with their status, image, when and by whom; it updates on its own while a deployment runs.

| Status | Meaning |
| --- | --- |
| pending | Waiting to start. |
| deploying | Pulling, scanning, starting or waiting for the health check. |
| live | Serving traffic. Only one revision per service is live. |
| superseded | A newer revision replaced it. It can be redeployed. |
| failed | It did not start or did not pass its health check. The previous live revision keeps serving. |

- **Update:** change the image tag on the **Settings** tab and click **Save and deploy**. From CI: `platformctl deploy shop/web --image {{registry}}/acme/web:1.4.2 --wait` (API: `POST /api/v1/projects/PROJECT/services/SERVICE/deploy` with `{"image": "…"}`).
- **Redeploy as is:** **Deploy current settings** (top of the **Deployments** table) starts a new revision with the saved settings, for example to pick up a changed secret or a new image pushed under the same tag.
- **Roll back:** click **Redeploy** next to an earlier superseded or failed revision and confirm. Its exact settings come back as a new revision; secrets and project variables use their current values.
- **Follow a deployment:** `GET /api/v1/projects/PROJECT/services/SERVICE/deployments/REVISION` returns its status; `--wait` on the CLI does this for you and exits with an error if it fails.
- **Delete a service:** at the bottom of the **Settings** tab, type the service ID to confirm.

The **Overview** tab also shows **Resource usage** (CPU and memory over the last hour, about a minute after the first deployment) and **At a glance**: the address, verified domains, vulnerability counts, backup schedule and open incidents.

## Builds from git

Instead of building and pushing images yourself, let the platform build your repository: with its **Dockerfile** if it has one, otherwise with **Railpack**, which detects the language (Node, Python, Go, PHP, Ruby, Java, Rust, Elixir, static sites) and builds it without any configuration. Use it when you do not have a CI pipeline, or to keep it simple.

The **Builds** tab appears when the platform offers builds; the Trial plan does not include them.

1. On the service's **Builds** tab, under **Source**, enter the **Repository** https address (`https://github.com/acme/web.git`), the **Branch** and, if the app is not at the root, its **Folder** (`services/web`).
2. For a private repository, add an **Access token**: a read-only token (GitHub fine-grained token, GitLab/Gitea deploy token). It is stored encrypted and never shown again; leave the field empty to keep it, or tick **remove the saved token**. Click **Save source**.
3. Click **Build now**. With **deploy when built** (ticked by default) the service is redeployed with the new image and its current settings. From CI: `platformctl build shop/web --wait` (`--no-deploy` to only build); API: `POST /api/v1/projects/PROJECT/services/SERVICE/builds`.
4. To build on every push, click **Create webhook address** under **Build on every push**. Copy the address shown (it is shown once) and add it as a webhook in GitHub, Gitea, Forgejo or GitLab (push events, content type JSON). Pushes to other branches are ignored. **New webhook address** replaces it; the old one stops working.

- The **Builds** table lists each build with its status, commit, who or what started it (a person, the webhook, a pull request, a zip) and how long it took. Click a build number for its page with the full log; it refreshes while the build runs. API: `GET …/builds/N`.
- Images are stored in your registry space as `…/<organization>/<service>:git-<commit>` and scanned like any pushed image.
- Builds run in an isolated sandbox: no access to the platform's internal network, other customers or the server; the internet is reachable for base images and packages. Limits per build: 2 CPU, 2 GB memory, 15 minutes, 8 GB image (uncompressed).
- One build at a time per service: starting a build while one is waiting reuses the waiting one. At most 5 builds waiting or running per organization; the activity indicator shows a queued build's place in the queue.
- Without a Dockerfile, make your app listen on the `PORT` environment variable (the platform sets it to the service's port). If Railpack cannot detect the project, the build fails with its explanation: add a Dockerfile.
- Emptying the **Repository** field turns builds from git off. Build minutes are billed (see [Billing and usage](/docs/user#billing-and-usage)).

### Zip deployment

No git repository needed: upload your application's source as a zip archive and the platform builds it the same way (its Dockerfile, or Railpack without one), then deploys it. Handy for a quick test or a project kept on your laptop.

1. **Console:** on the service's **Builds** tab, under **Deploy a zip**, choose the archive (up to 100 MB), keep **deploy when built** ticked if you want it deployed, and click **Upload and build**.
2. **Command line:** `platformctl zip shop/web ./my-app --wait` zips the folder (leaving out `.git` and `node_modules`) and uploads it; a `.zip` file is uploaded as is. `--no-deploy` only builds and pushes the image.
3. **API:** `POST /api/v1/projects/PROJECT/services/SERVICE/zip` with the archive as the body (`Content-Type: application/zip`); add `?deploy=false` to only build, `?filename=app.zip` to name it in the log. The answer gives the build's `status_url`.

- An archive whose files all sit in one folder (a repository's **Download ZIP**) is built from that folder. Links and paths leaving the archive are refused; it may unpack to 500 MB and 20,000 files.
- The image is tagged `zip-<first 12 characters of the archive's SHA-256>`. The archive itself is deleted as soon as its build finishes; the build log keeps its name and SHA-256.

## Scheduled jobs

A job runs a command on a schedule, such as a nightly report, cleaning old records, sending reminder emails or warming a cache. It runs in a fresh container made from the service's current deployment: same image, environment variables, secrets, project variables and private network (so it reaches the project's databases). No extra service to keep running.

1. Open the service and its **Jobs** tab.
2. Under **Add or change a job**, enter a **Job ID** (`nightly-report`; an existing ID updates that job), a **Schedule** (suggestions appear as you type), a **Command** such as `python manage.py send_reminders` (up to 2000 characters), and a **Time limit** in seconds.
3. Click **Save job**.
4. In the jobs table: **run now** starts it at once, **pause** stops the schedule without deleting the job (**resume** starts it again), **delete** removes it and its run history.

The command runs with `/bin/sh -c`, so the image needs a shell. Schedules use the classic cron format, in UTC: `minute hour day-of-month month day-of-week`.

| Schedule | Runs |
| --- | --- |
| `*/15 * * * *` | every 15 minutes |
| `0 3 * * *` | every day at 03:00 UTC |
| `0 8 * * MON-FRI` | weekdays at 08:00 UTC |
| `0 4 * * SUN` | every Sunday at 04:00 UTC |
| `0 2 1 * *` | the 1st of each month at 02:00 UTC |
| `@hourly`, `@daily`, `@weekly`, `@monthly` | shortcuts |

**Recent runs** lists each run with its status (running, succeeded, failed, timed out, skipped), whether it was started by hand, when, how long it took, and **last lines** of its output with a link to the **full log**.

**Compute → Scheduled jobs** lists every job of the organization (or of the filtered project) with its schedule, last run and next run; click a job to open its service's **Jobs** tab.

Good to know:

- The service must have a running deployment; a job always uses the image deployed at that moment.
- A job never overlaps itself: a run due while the previous one is still going is recorded as **skipped**.
- Each run has a time limit (10 seconds to 1 hour, 10 minutes by default); a run over it is stopped and marked **timed out**.
- The job gets the service's CPU and memory, counted in your usage for the minutes it runs. It does not get the service's persistent folder.
- `PLATFORM_JOB` (the job ID) and `PLATFORM_JOB_RUN` (the run ID) are set in the job's environment.
- The last 50 runs of each job are kept, with their exit code and the end of their output. A failed or timed-out run sends a **Scheduled job failed** alert to channels that subscribe to it.
- A service can have up to 20 jobs. Previews have no jobs.
- API: `GET …/services/SERVICE/jobs`, `PUT …/jobs/JOB` with `{"schedule": "0 3 * * *", "command": "…", "timeout_seconds": 600, "enabled": true}`, `DELETE …/jobs/JOB`, `POST …/jobs/JOB/run`, `GET …/jobs/JOB/runs` and `…/runs/ID` (with the log). CLI: `platformctl job list shop/web`, `job run shop/web JOB [--wait]`, `job logs shop/web JOB`.

## Preview environments

A preview is a temporary copy of a service, with the same settings, secrets and traffic protection but another image, at its own address: `<name>.<service>-<project>-<organization>.{{apps}}`. Typical use: one preview per pull request, so reviewers can click through a change before it is merged, without touching the live service.

1. Open the service and its **Previews** tab.
2. Under **Create or update a preview**, enter a **Name** such as `pr-42` (lowercase letters, digits and dashes, up to 20), an **Image** (empty = the service's image) and a **Lifetime** in hours (1 to 720, 72 by default).
3. Click **Deploy preview**. Using an existing name redeploys that preview with the new image and renews its lifetime.
4. Click the preview's name to open it, **logs and details** for its own page (deployments, logs, settings), or **Delete** to remove it.

From CI: `platformctl preview create shop/web pr-42 --image {{registry}}/acme/web:pr-42 --wait` (`--ttl HOURS` for the lifetime). Running it again redeploys the preview and renews its lifetime, so it can run on every push. `platformctl preview list shop/web` and `platformctl preview delete shop/web pr-42` (for example when the pull request is closed). API: `GET/POST /api/v1/projects/PROJECT/services/SERVICE/previews`, `DELETE …/previews/NAME`.

**Compute → Previews** lists every preview of the organization (or of the filtered project) with its status and expiry.

- **Lifetime:** the preview is deleted automatically when it expires.
- **Limits:** at most 5 previews per service. They use your plan's CPU and memory (and are billed like services) but do not count as services.
- Previews run one instance, never get a persistent folder, backups, public TCP/UDP ports or jobs, and do not appear on public status pages.
- Secrets are copied when the preview is created; later changes to the service's secrets do not reach existing previews. Project variables are passed as for the service.

### A preview for each pull request

With builds from git set up, a service can get a preview for every pull request automatically, with nothing to script in CI.

1. On the service's **Builds** tab, under **Source**, tick **Preview every pull request** and click **Save source**.
2. In your repository's webhook settings (GitHub, Gitea or Forgejo), use the service's webhook address and add **pull request** events to push events.
3. Open a pull request: its branch is built and deployed as preview `pr-<number>`, at `pr-<number>.<service>-<project>-<organization>.{{apps}}`. Every push to the pull request rebuilds it; closing or merging the pull request deletes the preview.

Good to know:

- Pull requests from forks are ignored: a preview receives the service's secrets and project variables, so only branches of the repository itself are previewed.
- A pull request preview lives 14 days after its last push if it is never closed.
- The previews count against the limit of previews per service and your plan's CPU and memory. Builds of pull requests are listed on the **Builds** tab as "pull request #N".

## Managed databases

A managed database runs next to your services, on the project's private network, with nightly encrypted backups, so you do not have to install, secure or back up a database server yourself. The console offers:

| Engine | Versions | Port | Secret name when attached |
| --- | --- | --- | --- |
| PostgreSQL | 17, 16 | 5432 | `DATABASE_URL` |
| MySQL | 8.4 | 3306 | `DATABASE_URL` |
| MariaDB | 11.4 | 3306 | `DATABASE_URL` |
| MongoDB | 8.0 | 27017 | `MONGODB_URI` |
| Redis | 7 | 6379 | `REDIS_URL` |
| Apache Kafka (streaming) | 3.9 | 9092 | `KAFKA_URL` |

### Create a database

1. Open **Databases** and click **Create database** (developers), or use the **Create a database** form on a project's **Databases** tab.
2. Choose the **Project**, a **Database ID** (2 to 20 lowercase letters, digits and dashes, such as `main`) and the **Engine**.
3. Click **Create database**. It starts in under a minute (the activity indicator shows it) with 250 millicores and 256 MB (512 MB for MongoDB and Kafka), counted against your plan.

API: `POST /api/v1/projects/PROJECT/databases` with `{"slug": "main", "engine": "postgres:17"}` (engines: `postgres:17`, `postgres:16`, `mysql:8.4`, `mariadb:11.4`, `mongodb:8.0`, `redis:7`, `kafka:3.9`).

The **Databases** list shows every database with its project, engine and status, with a search box and bulk actions (**Back up now**, **Delete**). A database page has four tabs: **Overview** (status, who can reach it, backup schedule), **Connect**, **Backups** and **Settings**.

### Connect a service

1. Open the database's **Connect** tab, section **Add to a service**.
2. Choose a service of the same project and keep the suggested secret name (or type another, in capitals).
3. Click **Add secret**. The connection URL is stored as an encrypted secret of that service. Redeploy the service to apply it.
4. Use it from your application through that variable.

API: `POST /api/v1/projects/PROJECT/databases/DB/attach` with `{"service": "web", "name": "DATABASE_URL"}`.

Connection strings look like this (the host `pd-…` only resolves inside the project's network):

| Engine | Connection string |
| --- | --- |
| PostgreSQL | `postgres://app:…@pd-…:5432/app?sslmode=disable` |
| MySQL, MariaDB | `mysql://app:…@pd-…:3306/app` |
| MongoDB | `mongodb://app:…@pd-…:27017/app?authSource=admin` |
| Redis | `redis://default:…@pd-…:6379/0` |
| Kafka | `kafka://app:…@pd-…:9092?security.protocol=SASL_PLAINTEXT&sasl.mechanism=PLAIN` |

The **Connect** tab also shows the **Connection details** one by one (host, port, database and user, and for developers the password and the full **Connection URL**), for tools that want separate fields.

- **Who can reach it:** only services of the same project. The database is not reachable from the internet (unless you open public access, below) or from other projects, so traffic stays on the private network.
- **Who can see the password:** developers and above. Viewers see the host, port, database and user only.

### Public access

Public access opens a database to tools outside the platform (your laptop, a reporting tool, a server in another cloud) while keeping everyone else out. Available for PostgreSQL, Redis and MongoDB; admins only.

1. On the database's **Connect** tab, section **Public access**, list the addresses allowed to connect, separated by commas (IP addresses or ranges such as `198.51.100.0/24`). The field is prefilled with your current address.
2. Click **Open public access**.
3. Save the certificate shown under **Certificate (save as db.crt)**, then connect to the host `<db>-<project>-<organization>-db.{{apps}}` on the port shown. **Example** gives a ready command:

```
psql "postgres://app:PASSWORD@main-shop-acme-db.{{apps}}:PORT/app?sslmode=verify-full&sslrootcert=db.crt"
redis-cli -u "rediss://default:PASSWORD@cache-shop-acme-db.{{apps}}:PORT/0" --cacert db.crt
mongosh "mongodb://app:PASSWORD@docs-shop-acme-db.{{apps}}:PORT/app?authSource=admin&tls=true&tlsCAFile=db.crt"
```

4. **Update the allow-list** changes the addresses; **Close public access** removes the endpoint.

- TLS is required, with the database's own certificate (pass it as `sslrootcert` for `sslmode=verify-full`, `--cacert` for Redis, `tlsCAFile` for MongoDB). Plain-text connections are refused.
- Connections from any other address are dropped before they reach the database. Avoid `0.0.0.0/0`, which would allow everyone.
- MySQL, MariaDB and Kafka negotiate TLS inside their own protocols and have no public endpoint yet: connect from a service of the project.
- The public port counts against your plan's public ports (none on Trial).
- API: `POST /api/v1/projects/PROJECT/databases/DB/public` with `{"allow": "203.0.113.7"}` or `{"close": true}`.

### Database backups and restore

Every database except Kafka is backed up every night, so you can recover from a bad migration or a deleted table.

- **Schedule:** on the **Settings** tab, keep **nightly backup** ticked and choose the **Backup hour** (UTC), then **Save settings**.
- **How:** the platform takes a consistent `pg_dump` (PostgreSQL), `mysqldump` / `mariadb-dump` (MySQL, MariaDB), `mongodump` (MongoDB) or snapshot (Redis), encrypts it with your organization's key and keeps 7 daily, 4 weekly and 6 monthly snapshots.
- **Back up now** (developers), on the **Backups** tab, takes one immediately; the bulk action does it for several databases. Only one backup or restore runs at a time per database.
- **Snapshots** lists each snapshot with when it was taken and its size; **nightly** and **before restore** badges tell them apart. **Recent jobs** lists the latest backups and restores with their result.
- **Restore** (admins): type the database ID next to a snapshot and click **Restore**. A safety snapshot of the current data is taken first and always kept. PostgreSQL is loaded with `pg_restore`, replacing existing tables; MySQL and MariaDB recreate every table in the snapshot (tables created after it are left in place); MongoDB uses `mongorestore --drop`, replacing each collection in the snapshot (collections created after it are left in place); Redis is briefly stopped and restarted with the snapshot. Connected applications may see errors for a few seconds.

### Settings and deleting

- **Resources:** on the **Settings** tab, set **CPU** (50 to 4000 millicores) and **Memory** (128 to 8192 MB; at least 512 MB for MongoDB and Kafka) and click **Save settings**. The database restarts with the new values (a few seconds of downtime); data is kept in its persistent volume.
- **Delete** (admins): at the bottom of the **Settings** tab, type the database ID and click **Delete database**. The database stops and leaves the project; its data volume is kept by the operator until it is cleaned up. Take a backup first if you may need the data.

### Engine notes

- **PostgreSQL:** the database `app` and user `app` are created for you.
- **MySQL and MariaDB:** the database `app` and user `app` are created for you; `root` gets a random password that nobody is given. InnoDB gets half the memory you give MariaDB, a quarter for MySQL (which needs more for itself); give MySQL at least 256 MB. Up to 100 connections are accepted. Named time zones (`CONVERT_TZ(..., 'Europe/Paris')`) are not loaded; use offsets such as `+01:00` or UTC.
- **MongoDB:** the database `app` and user `app` (an administrator of this server only) are created for you. It starts with 512 MB, its minimum. The storage engine's cache gets 40% of the memory (0.25 GB at least).
- **Redis:** Redis uses at most 80% of the memory you give it and then refuses new writes instead of evicting keys, so size it for your data.
- **Apache Kafka:** a single-broker Kafka (KRaft mode, no separate coordination service) on the project's private network, for event streams between your services. Give your Kafka client the bootstrap server `pd-…:9092`, security protocol `SASL_PLAINTEXT`, mechanism `PLAIN`, user `app` and the password (all shown on the **Connect** tab). Topics are created on first use; messages are kept for 7 days. It needs at least 512 MB (60% for the JVM heap). Kafka is **not backed up**: keep data you need in a database or a bucket.

### Plans

Managed databases count against your plan (all engines together): Trial and Starter include 1, Pro includes 5 (default plans). Their CPU and memory share the plan's totals with your services, and their data and backup dumps count against your disk limit (see [Disk limits](/docs/user#disk-limits)).

## Functions

Functions run your code on demand, without a server you keep running. Use them for small jobs that happen now and then: an API endpoint with little traffic, a webhook receiver, an image resize after each upload, a nightly cleanup. Your code starts when something calls it and stops when it is done, and you pay only while it runs. Open **Functions** in the Services menu.

The **Functions** page lists every function of the organization (or of the current project) with its runtime, whether its URL or schedule is on, and its invocations and errors over the last 24 hours. Type in **Find functions** to filter the list.

### Create a function

1. On the **Functions** page, click **Create function** (developers).
2. Choose the **Project**. The function runs on that project's private network, next to its databases and services.
3. Enter a **Function name**, such as `resize-image`: 2 to 20 lowercase letters, digits and dashes.
4. Choose the **Kind**: **Function** (a handler called with each event) or **Web app** (an ordinary HTTP server that scales to zero, see [Web apps that scale to zero](/docs/user#web-apps-that-scale-to-zero)).
5. Choose the **Runtime**: **Node.js 22** or **Python 3.13**.
6. Click **Create function**. It starts with a working example you can run right away from the **Test** tab.

API: `PUT /api/v1/projects/PROJECT/functions/NAME` creates the function if it does not exist, or updates it (see [Manage functions from the API and CLI](/docs/user#manage-functions-from-the-api-and-cli)). Your plan sets how many functions you can have.

### Write the code

A handler is a function that receives two arguments, the **event** (the input, as JSON) and the **context** (facts about this call), and returns a result.

Node.js, in `index.mjs` (handler `index.handler`):

```
export const handler = async (event, context) => {
  console.log("got", JSON.stringify(event));
  return { message: "Hello from " + context.functionName };
};
```

With CommonJS, write `exports.handler = async (event, context) => { … }` in `index.js`. The runtime looks for `.mjs`, then `.js`, then `.cjs`.

Python, in `main.py` (handler `main.handler`):

```
def handler(event, context):
    print("got", event)
    return {"message": "Hello from " + context.function_name}
```

The **Handler** setting (Configuration tab) is `file.function`: the file name without its extension, a dot, and the function's name. A file in a folder is written `src/app.main`. The handler in the page header must match your file and function names; when you rename one, change the handler too.

The context gives:

| Node.js | Python | Meaning |
| --- | --- | --- |
| `context.functionName` | `context.function_name` | the function's name |
| `context.memoryLimitInMB` | `context.memory_limit_in_mb` | its memory setting |
| `context.getRemainingTimeInMillis()` | `context.get_remaining_time_in_millis()` | time left before the timeout |

What your code can rely on:

- Everything it prints (`console.log`, `print`, errors) becomes the invocation's **log**, shown on the **Test** and **Monitor** tabs (up to 32 KB per call).
- It runs as an unprivileged user, in UTC (`TZ=UTC`), in its own sandbox. Its code is read-only; `/tmp` is writable.
- The variable `PLATFORM_FUNCTION` holds `project/function`.
- Return any value that can be turned into JSON. Throwing an error (or raising an exception) marks the call as failed, with the error's name and message.

### Edit in the browser or upload a zip

- **Code** tab: edit the handler's file and click **Deploy**. Saved code runs on the next invocation. When the archive has several files, **All files** lists them. A file that is too large or not text cannot be edited here: upload a new archive instead.
- **Upload a zip archive** (same tab): choose a `.zip` with your files and their dependencies (`node_modules`, Python packages installed into the folder, for example with `pip install -t . requests`), then click **Upload and deploy**. It replaces all the code. Limits: 10 MB zipped, 50 MB unzipped, 2000 files. The handler's file must be at the top level of the archive, or match the handler's path.
- CLI: `platformctl fn deploy shop/resize-image --zip build.zip` or `--file index.mjs` (add `--runtime python3.13` and `--handler main.handler` when needed).

### Test events

Use the **Test** tab to run the deployed code with an event of your choice before anything real calls it.

1. Type a JSON event in **Test event**, such as `{"key": "photos/a.jpg"}`.
2. Click **Test**.
3. Read the **Result**: the status, the duration, whether it was a cold start (a new sandbox was started), the request id, the returned value, the error type and message if it failed, and the **Log**.

### Function settings

On the **Configuration** tab, under **Settings**, then **Save configuration**:

| Setting | What it does |
| --- | --- |
| **Memory** | 128, 256, 512, 1024, 1536 or 2048 MB. CPU grows with memory, so a slow function often gets faster with more. |
| **Timeout** | 1 to 300 seconds. A call running longer is stopped and marked timed out. |
| **Concurrency** | 1 to 50 invocations running at the same time. |
| **Handler** | `file.function`, see above. Unused by a web app. |
| **Web app start command** and **Web app port** | Only for web apps, see below. |
| **Function URL** and **Who may call the URL** | Turn the HTTPS endpoint on, and choose who may call it. |
| **Schedule** and **run on the schedule** | A cron expression in UTC; the next run is shown once it is on. |
| **Environment variables** | `NAME=value`, one per line (lines starting with `#` are ignored). |

About environment variables:

- Names use uppercase letters, digits and underscores; 4 KB in total. Names starting with `PLATFORM_`, and a few names the runtime uses itself, are reserved.
- The project's shared variables are added too (see [Project variables](/docs/user#project-variables)).
- Set a value to `${secret:NAME}` to use a secret of the [Secrets manager](/docs/user#secrets-manager). The function picks up a new secret version at its next start.
- Use the connection URLs of the project's databases as variables: the function reaches them over the private network, as well as services by their private names (`http://api.shop.internal`).

**Delete function** (bottom of the Configuration tab): type the function's name to confirm. Its URL, schedule, triggers and history go with it. On the **Functions** page you can also select several functions and choose **Turn off the URL** or **Delete** (type `delete` to confirm), then **Apply**.

### How it runs

- **Cold and warm starts:** the first call after a while starts a fresh sandbox, a **cold start** of about a second (the very first call of a runtime on a server also downloads it). The sandbox then stays warm for 10 minutes after the last call, and warm calls start in a few milliseconds. Changing the code or the settings starts a new sandbox on the next call.
- **No state between calls:** each call runs your handler as a new process inside the sandbox, so variables at the top of your file are set up again every time. Keep data in a database or a bucket.
- **Queueing and throttling:** when more calls arrive than the concurrency allows, they wait their turn. Once four times the concurrency are waiting or running, new calls are refused with HTTP 429. Raise **Concurrency** if this happens in normal traffic.
- **Sizes:** an event and a response are limited to 6 MB each.
- **History:** invocations, their logs and errors are kept 7 days.
- **Billing:** per million requests and per GB-second (memory times run time), at your plan's prices. See [Billing and usage](/docs/user#billing-and-usage).

### Function URL

A function URL gives the function its own HTTPS address, for browsers, apps and other services.

1. On the **Configuration** tab, tick **Function URL** and click **Save configuration**. The address is shown under **Function URL**: `https://<function>-<project>-<organization>.fn.{{apps}}/`.
2. Choose **Who may call the URL**: **anyone (public)** needs no authentication; **callers with the token** requires a token.
3. With **callers with the token**, click **Create a token** under **Function URL** and copy the token: it is shown once. Callers send it in the `X-Function-Token` header; other calls get `403 Forbidden`. **Replace the token** makes a new one and stops the old one at once.

The request your handler receives as `event`:

| Field | Content |
| --- | --- |
| `rawPath` | the path, such as `/orders/42` |
| `rawQueryString` | the query string without `?`, such as `page=2&sort=asc` |
| `queryStringParameters` | the query as an object (repeated keys joined with commas); absent when there is none |
| `headers` | the request headers, names in lowercase, repeated headers joined with commas (the token header is removed) |
| `cookies` | a list of `name=value` strings; absent when there is none |
| `requestContext.http` | `method`, `path`, `protocol`, `sourceIp` and `userAgent` |
| `requestContext.requestId`, `requestContext.timeEpoch` | a request id and the time in milliseconds |
| `body` | the body as text, or base64 for binary bodies |
| `isBase64Encoded` | `true` when `body` is base64 |
| `version`, `routeKey` | always `"2.0"` and `"$default"` |

The response is built from what the handler returns:

- An object with a `statusCode` is the HTTP response: `statusCode`, `headers` (an object), `cookies` (a list of `Set-Cookie` values), `body` (a string) and `isBase64Encoded` (`true` when `body` is base64, for images or files). The content type is `application/json` unless you set one.
- A string is sent as the body, with status 200.
- Any other value (an object without `statusCode`, a list, a number) is sent as JSON with status 200.

Example, Node.js:

```
export const handler = async (event) => {
  const name = event.queryStringParameters?.name ?? "world";
  return { statusCode: 200, headers: { "content-type": "text/plain" }, body: `Hello ${name}` };
};
```

When things go wrong, the caller gets: `502` if the handler failed, `504` if it timed out, `429` when the function is throttled, `413` for a body over 6 MB, `403` for a missing or wrong token. Every answer carries `X-Platform-Request-Id`, which you find again on the **Monitor** tab.

### Triggers

Besides its URL and the API, a function can be started by a schedule, a bucket, a queue or a webhook. All of these run it in the background: nobody waits for the result, which you find on the **Monitor** tab. Failed background runs are not retried, except queue batches.

**Schedule**: run the function at fixed times, for reports, cleanups or syncs.

1. On the **Configuration** tab, enter a cron expression in UTC in **Schedule**, such as `0 3 * * *` (every day at 03:00) or `*/15 * * * *` (every 15 minutes). At most once a minute.
2. Tick **run on the schedule** and click **Save configuration**. The next run time appears next to it.

The event has `detail-type` set to `"Scheduled Event"`, `source` set to `"platform.scheduler"`, `time` (the scheduled time, ISO 8601), an `id`, and empty `resources` and `detail`.

**Bucket events**: run the function each time a file is uploaded to or deleted from a bucket, for thumbnails, virus scans or indexing.

1. Open the **Triggers** tab. Under **Bucket events**, choose the **Bucket** (create one in Object storage first).
2. Choose **On**: **uploads**, **deletes**, or **uploads and deletes**.
3. Optionally narrow it with a **Key prefix** (`images/`) and a **Key suffix** (`.jpg`).
4. Click **Add bucket trigger**. It applies within about 20 seconds; **remove** deletes it.

The event holds a `Records` list with one record:

| Field | Content |
| --- | --- |
| `eventName` | `ObjectCreated:Put`, `ObjectCreated:CompleteMultipartUpload`, `ObjectCreated:Copy` or `ObjectRemoved:Delete` |
| `eventTime` | when it happened (ISO 8601, UTC) |
| `eventSource`, `eventVersion` | `"platform:s3"` and `"2.1"` |
| `s3.bucket.name` | the bucket's S3 name, such as `acme-uploads` |
| `s3.object.key` | the object's key |
| `s3.object.size` | its size in bytes (uploads only) |

Read the key with `event.Records[0].s3.object.key`. Batch deletes (`DeleteObjects`) are not reported. If the function falls far behind (20 times its concurrency waiting), new events are dropped rather than piling up. A function that writes into the bucket it listens to should use a prefix or suffix that its own files do not match, or it calls itself forever. API: `POST /api/v1/projects/PROJECT/functions/NAME/triggers` with `{"bucket": "uploads", "events": "created", "prefix": "images/", "suffix": ".jpg"}` (`events`: `created`, `removed` or `all`).

**Queue**: let the function process the messages of a queue in batches. Set it up on the queue's **Settings** tab (see [Queues](/docs/user#queues)).

**Webhook**: a secret address for services that send webhooks, such as GitHub, Stripe or a form provider.

1. On the **Triggers** tab, under **Webhook**, click **Create a webhook URL**.
2. Copy the URL (`{{console}}/hooks/fn/whk_…`): it is shown once. Anyone with it can start the function.
3. Give it to the other service.

Each `POST` to it answers `202` at once with an invocation id and runs the function in the background with the request as the event, in the function URL format above (`rawPath` is always `/`; the `authorization` and `cookie` headers are removed). Bodies up to 1 MB; at most 120 calls a minute from one address. Check the sender's signature header in your code before trusting the body. **Replace the webhook URL** revokes the old one; **remove the webhook** turns it off. API: `POST /api/v1/projects/PROJECT/functions/NAME/webhook` (the answer carries the new `url`).

### Web apps that scale to zero

A function can also be an ordinary web application (Express, Fastify, Flask, FastAPI, a plain HTTP server) that starts on the first request and stops when idle. Use it for a small site or API that gets little traffic, without paying for an always-on service.

1. Create a function and choose **Web app**. It starts with a small HTTP server (`server.mjs` or `app.py`) and its start command (`node server.mjs`, `python3 -u app.py`).
2. Your server must listen on the port in the `PORT` variable (8080 unless you change **Web app port**, 1024 to 65535).
3. Change the **Web app start command** on the Configuration tab when needed, for example `node dist/index.js` or `python3 -m uvicorn main:app --host 0.0.0.0 --port 8080`. Include the dependencies in the zip.
4. Turn on the **Function URL**: each request (method, path, query, headers, cookies, body) is forwarded to your server, with `X-Forwarded-For` (the caller's address), `X-Forwarded-Proto: https` and `X-Platform-Request-Id`. Its answer (status, headers, cookies, body) is returned as is.

Good to know:

- Other events (API invoke, console test, schedule, queue, bucket) are sent to your server as `POST /events` with the event as JSON; its answer (JSON or text) is the result. Webhook calls arrive as ordinary `POST /` requests. A status of 400 or more counts as an error.
- The server starts on the first request (it has 60 seconds to listen) and stays warm for 10 minutes after the last one. It handles concurrent requests itself. A server that crashes is started again on the next request. A server that never listens fails the request with `Platform.StartFailed` and the reason.
- Timeout, memory, concurrency, variables and billing work as for any function. Unlike a handler, a web app's server keeps its memory while it is warm.

### Monitor and invocations

The **Monitor** tab shows, for the last 24 hours, the number of **Invocations**, **Errors**, the **Average duration** and the number of **Cold starts**. Under **Recent invocations**, each call shows when it ran, its trigger (`url`, `api`, `test`, `schedule`, `bucket`, `queue`, `webhook`), its status (with a **cold** badge for cold starts), its duration, its error, and its **log**.

CLI: `platformctl fn logs shop/resize-image`. API: `GET /api/v1/projects/PROJECT/functions/NAME/invocations` (the last 50).

### Errors

A failed call has an error type and a message, shown on the Test and Monitor tabs and returned by the API:

| Error type | Meaning |
| --- | --- |
| the error's own name (`Error`, `TypeError`, `ValueError`…) | your handler threw an error or raised an exception |
| `Runtime.ImportModuleError` | the handler's file was not found or could not be loaded |
| `Runtime.HandlerNotFound` | the file has no function with the handler's name |
| `Runtime.UserCodeSyntaxError` | the file has a syntax error |
| `Runtime.MarshalError` | the Python result could not be turned into JSON |
| `Runtime.ExitError` | the process ended without a result; exit code 137 usually means out of memory |
| `Sandbox.Timedout` | the call ran past the timeout |
| `Function.ResponseSizeTooLarge` | the result is larger than 6 MB |
| `Platform.StartFailed` | the sandbox (or the web app's server) could not start |
| `Platform.InvokeFailed`, `WebApp.RequestFailed` | the call could not reach the code, for example after a crash |

When you call the function through the API, a failed call still answers `200`, with the header `X-Platform-Function-Error: Unhandled` and the body `{"errorType": "…", "errorMessage": "…"}`. Always check that header before using the body.

### Manage functions from the API and CLI

- **Run it:** `POST /api/v1/projects/PROJECT/functions/NAME/invoke` with the event as the JSON body (empty means `{}`). The answer is the handler's result, with `X-Platform-Invocation-Id` and `X-Platform-Duration-Ms`. Add `X-Log-Type: Tail` to get the last 4 KB of the log, base64 encoded, in `X-Platform-Log-Result`. Add `X-Invocation-Type: Event` to queue it without waiting: the answer is `202` with `{"invocation_id": "…", "status": "queued"}`. A throttled call gets `429` with `Retry-After: 1`. CLI: `platformctl fn invoke shop/resize-image --event '{"key": "a.jpg"}'` (`--event -` reads it from standard input, `--async` queues it).
- **Create or update:** `PUT /api/v1/projects/PROJECT/functions/NAME` with any of `runtime` (`nodejs22` or `python3.13`), `handler`, `code` (the handler file's source) or `zip_base64` (a whole archive), `memory_mb`, `timeout_seconds`, `concurrency`, `env` (an object), `url_enabled`, `url_auth` (`none` or `token`), `schedule`, `schedule_enabled`, `web_command`, `web_port`. Fields you leave out keep their value.
- **List and delete:** `GET /api/v1/projects/PROJECT/functions`, `DELETE /api/v1/projects/PROJECT/functions/NAME`. CLI: `platformctl fn list shop`, `platformctl fn delete shop/resize-image`.

## Queues

A queue holds messages between the part of your system that creates work and the part that does it. A producer (your website, say) sends a message such as "order 1042 was paid"; a consumer (a worker service or a function) receives it, does the work and deletes it. If the consumer is slow, busy or down, messages wait safely in the queue instead of being lost. Open **Queues** in the Services menu.

The **Queues** page lists each queue with its **Available** messages (waiting), **In flight** messages (received, not yet deleted) and its **Consumer** (a function, or your code through the API). Type in **Find queues** to filter.

### Create a queue

1. On the **Queues** page, enter a name such as `orders` (3 to 40 lowercase letters, digits and dashes) and click **Create queue** (developers).
2. It starts with these defaults: a received message is hidden for 30 seconds, messages are kept 4 days, and a message can be received at most 5 times. Change them on the queue's **Settings** tab.

API: `POST /api/v1/queues` with `{"name": "orders"}`, optionally with `visibility_timeout_seconds`, `retention_seconds`, `max_receives` and `dead_letter_queue`. CLI: `platformctl queue create orders`. Your plan sets the number of queues.

### How delivery works

1. A producer **sends** a message. It becomes available at once, or after a delay you choose.
2. A consumer **receives** it. The message is not removed: it is hidden for the **visibility timeout** and comes with a **receipt**.
3. When the work is done, the consumer **deletes** the message with its receipt.
4. If the consumer never deletes it (it crashed, or the work failed), the message becomes available again after the visibility timeout and someone receives it again.

Delivery is therefore *at least once*: a message can occasionally be processed twice, so make your processing safe to repeat (for example, check whether order 1042 was already handled). Messages are handed out roughly oldest first.

### Send, receive and delete over the API

All calls take an API token (or a queue token, see below) in `Authorization: Bearer`.

- **Send:** `POST /api/v1/queues/QUEUE/messages` with `{"body": "…"}`. A body is text of 1 byte to 256 KB (send JSON as a string). Add `"attributes": {"source": "shop"}` for string attributes, and `"delay_seconds"` (0 to 900) to hide the message for up to 15 minutes. Send up to 10 at once with `{"messages": [{"body": "…"}, …]}`. The answer is `201` with `{"message_ids": […]}`. CLI: `platformctl queue send orders '{"order": 1042}'` (`-` reads the body from standard input).
- **Receive:** `POST /api/v1/queues/QUEUE/receive?max=10&wait=20`. `max` is 1 to 10 (1 by default). `wait` is 0 to 20 seconds: when the queue is empty, the call waits that long for a message to arrive (long polling), which saves requests. `visibility=SECONDS` overrides the visibility timeout for this receive. The answer is `{"messages": [...]}`; each message has `id`, `receipt`, `body`, `attributes`, `receive_count`, `sent_at` and `first_received_at`. CLI: `platformctl queue receive orders --max 10 --wait 20`.
- **Delete:** `DELETE /api/v1/queues/QUEUE/messages/RECEIPT`. A receipt works only until the message is received again; an old receipt gets `404`. CLI: `platformctl queue delete orders RECEIPT`.
- **Purge** (delete every message): `POST /api/v1/queues/QUEUE/purge`, `platformctl queue purge orders`, or **purge** on the **Messages** tab.
- **List and delete queues:** `GET /api/v1/queues` (with counts), `DELETE /api/v1/queues/QUEUE` (admins). CLI: `platformctl queue list`.

A worker loop, in short: receive with `wait=20`; for each message, do the work, then delete it; repeat.

### The Messages tab

- **Send a message**: type a body and click **Send**, handy for testing a consumer.
- **Oldest messages**: the first waiting messages with when they were sent and how many times they were received. Viewing does not receive them.
- **purge**: deletes every message after you confirm.

### Queue settings

On the queue's **Settings** tab, then **Save settings**:

| Setting | What it does |
| --- | --- |
| **Visibility timeout** | Seconds a received message stays hidden, 1 to 43,200 (12 hours). Set it longer than your slowest processing, or messages come back while still being worked on. |
| **Retention** | Hours a message is kept if nobody deletes it, 1 to 336 (14 days). |
| **Max receives** | 1 to 1000. After that many receives without a delete, the message moves to the dead-letter queue. |
| **Dead-letter queue** | Another queue of the organization that collects messages that failed too often. |
| **Function trigger** | A function that processes the messages (see below). |
| **Batch size** | Messages per function call, 1 to 10. |
| **trigger on** | Untick to pause the function without losing messages. |

**Dead-letter queue:** without one, a message that always fails ("poison message") keeps coming back until its retention ends. With one, it moves aside after **Max receives**, so the other messages keep flowing, and you can look at it later on the dead-letter queue's **Messages** tab. Create the dead-letter queue first (for example `orders-failed`), then choose it here.

**Delete queue** (admins, Settings tab): type the queue's name to confirm. Its messages are deleted.

### Connect a service with a scoped token

Instead of giving a service a full API token, give it a token that only works for one queue.

1. On the queue's **Connect** tab, under **Connect to a service**, choose the service and click **Connect**.
2. The service gets two secrets: `QUEUE_URL` (the queue's API address) and `QUEUE_TOKEN`.
3. Redeploy the service.

The token can only send, receive and delete this queue's messages: it cannot purge, change settings or reach anything else. The **From your code** box on the same tab shows the three `curl` calls ready to copy. API: `POST /api/v1/queues/QUEUE/attach` with `{"service": "project/service"}`.

### Process messages with a function

1. Create a function (see [Functions](/docs/user#functions)).
2. On the queue's **Settings** tab, choose it as **Function trigger**, set the **Batch size**, tick **trigger on**, and click **Save settings**.

The platform then receives messages for you and calls the function with batches, as many batches at once as the function's concurrency allows. Each call's event is `{"Records": [...]}`, one record per message:

| Field | Content |
| --- | --- |
| `messageId` | the message's id |
| `receiptHandle` | its receipt |
| `body` | the body, as text (parse JSON yourself) |
| `attributes.ApproximateReceiveCount` | how many times it has been received, including this time |
| `attributes.SentTimestamp` | when it was sent, in milliseconds since 1970 |
| `attributes.ApproximateFirstReceiveTimestamp` | when it was first received, in milliseconds |
| `messageAttributes` | your attributes, each as `{"stringValue": "…", "dataType": "String"}` |
| `eventSourceARN` | `plat:queue:<organization>:<queue>` |

Records also carry `md5OfBody` (empty), `eventSource` and a region field, which you can ignore.

What happens next:

- The function **succeeds**: all the batch's messages are deleted.
- It succeeds but returns `{"batchItemFailures": [{"itemIdentifier": "<messageId>"}]}`: only the listed messages stay, and come back after the visibility timeout; the others are deleted.
- It **fails or times out**: the whole batch comes back after the visibility timeout, and moves to the dead-letter queue after **Max receives**.

Example, Node.js:

```
export const handler = async (event) => {
  const failed = [];
  for (const r of event.Records) {
    try { await handleOrder(JSON.parse(r.body)); }
    catch (e) { failed.push({ itemIdentifier: r.messageId }); }
  }
  return { batchItemFailures: failed };
};
```

Keep the function's timeout shorter than the queue's visibility timeout, or a batch can be handed out again while it is still running.

### Queue billing

Each API call to a queue (a send, even with 10 messages, a receive, a delete) counts as one request; queue requests are billed per million.

## Email

Send transactional email (receipts, password resets, sign-up confirmations, notifications) from your own domain, through an API or SMTP. Messages are signed with your domain's DKIM key so mail providers trust them and they do not land in spam. Open **Email** in the Services menu. The top of the page shows how many emails you sent in the last 24 hours, out of your plan's daily limit.

The page has five tabs: **Sending log**, **Domains**, **Use from code**, **Suppressions** and **Webhook**.

### Add a sending domain

You can send only from domains you have proven you own.

1. On the **Domains** tab, under **Add domain**, enter a domain such as `example.com` or `mail.example.com` and click **Add domain** (developers).
2. Open the domain and create, at your DNS provider, the TXT records its page lists (see the table below).
3. The platform checks the records every minute, or click **Check now**. DNS changes can take a few minutes to be visible.
4. Once the domain shows **verified**, send from any address at it.

| Record | Name | Why |
| --- | --- | --- |
| TXT | `_platform-verify.<domain>` | proves you own the domain (required) |
| TXT | `<selector>._domainkey.<domain>` | publishes the DKIM key receivers use to check each message's signature (required) |
| TXT (SPF) | `<domain>` | says this platform may send for your domain (recommended); if you already have a `v=spf1` record, merge the value into it instead of adding a second one |
| TXT (DMARC) | `_dmarc.<domain>` | tells receivers what to do with mail that fails the checks (recommended) |

If the domain's DNS is hosted on the platform (see [DNS hosting](/docs/user#dns-hosting)), the records are written for you and the domain is verified at once: the page says **automatic**.

The **Domains** tab shows each domain's status (**verified**, **waiting for DNS**, or **records removed**) and when it was last checked. The platform rechecks verified domains daily: if the records disappear, sending from the domain pauses until they are back.

**Delete domain** (bottom of the domain's page): type the domain to confirm. Its messages still queued fail. API: `GET/POST /api/v1/email/domains` (`{"domain": "example.com"}`), `DELETE /api/v1/email/domains/DOMAIN`. Your plan sets the number of sending domains.

### Connect a service to Email

The simplest way to give your application what it needs to send:

1. On the **Use from code** tab, under **Connect to a service**, choose the service and click **Connect**.
2. The service gets the secrets `EMAIL_API_URL` and `EMAIL_TOKEN` (and, when SMTP is offered, `SMTP_HOST`, `SMTP_PORT`, `SMTP_USERNAME`, `SMTP_PASSWORD`).
3. Redeploy the service.

The token can only send email and read the status of messages: nothing else. API: `POST /api/v1/email/attach` with `{"service": "project/service"}`.

### Send with the API

```
curl -X POST -H "Authorization: Bearer $EMAIL_TOKEN" -H 'Content-Type: application/json' $EMAIL_API_URL \
  -d '{"from": "Shop <hello@example.com>", "to": ["buyer@example.org"], "reply_to": "support@example.com",
       "subject": "Your order", "text": "Thanks!", "html": "<p>Thanks!</p>"}'
```

The endpoint is `POST /api/v1/email/send`. Fields:

| Field | Content |
| --- | --- |
| `from` | an address at a verified domain, with an optional name: `Shop <hello@example.com>` |
| `to` | a list of 1 to 50 recipients |
| `reply_to` | optional, where replies go |
| `subject` | one line, up to 998 characters |
| `text`, `html` | the body: give one or both (256 KB together) |
| `attachments` | optional, see below |

The answer is `202` with the message's id. `GET /api/v1/email/messages/ID` gives its status (`queued`, `sent`, `partial` when only some recipients got it, or `failed`) and each recipient's result with the receiving server's answer.

**Attachments:** add `"attachments": [{"filename": "invoice-1042.pdf", "content_type": "application/pdf", "content": "<base64>"}]`. Up to 20 files and 10 MB per message in total (your plan may set a lower size). Programs and scripts (`.exe`, `.js`, `.bat`…) are refused, as mail providers would refuse them.

### Send with SMTP

For software that only speaks SMTP (WordPress, Django, Rails, Nodemailer…), the **Use from code** tab shows the settings, when the platform offers SMTP:

| Setting | Value |
| --- | --- |
| host | the host shown on the tab |
| port | **465**, with TLS from the start ("SSL/TLS", not STARTTLS) |
| user name | any value, such as `token` |
| password | your email token (`EMAIL_TOKEN`) |

The same rules apply as with the API: the From address must use one of your verified domains, and the same limits count. Bcc headers are removed before sending. Messages sent this way show **SMTP** in the sending log.

### Send a test email

At the bottom of the **Sending log** tab (once a domain is verified), **Send a test email** sends a short message: fill in **From**, **To**, **Subject** and **Text**, then click **Send**. Use it to check that a domain works before you write any code.

### Sending log

The **Sending log** tab lists the last 100 messages: when they were queued, from, to, subject, status, and how many recipients were delivered, bounced, suppressed or failed. Type in **Find messages** to filter. A message still being retried shows why. Bodies are deleted as soon as a message is done; the log keeps 30 days.

### Suppressions

When a recipient's mail server refuses an address for good (an unknown mailbox, a domain that does not exist: a "hard bounce"), sending to it again would hurt your domain's reputation. So the address goes on your organization's **Suppression list**, and later messages skip it: that recipient is marked `suppressed`.

On the **Suppressions** tab:

- See each address, why it was added (**bounced** with the server's answer, or added by hand) and since when.
- Add an address yourself (someone asked not to get mail, a known bad address): type it and click **Suppress**.
- Click **remove** once you know an address works again.

API: `GET/POST /api/v1/email/suppressions`, `DELETE /api/v1/email/suppressions/ADDRESS`.

### Delivery webhook

Get told when each recipient's email is delivered, bounced, failed or suppressed, for example to flag a customer's bad address in your database.

1. On the **Webhook** tab, enter your endpoint's **URL** and tick the **Events** you want: `email.delivered`, `email.bounced`, `email.failed`, `email.suppressed`.
2. Click **Save webhook**. Copy the **signing secret** shown at the top: it is shown once.

For each recipient's result, the platform sends a `POST` with a JSON body:

```
{"id": "…", "type": "email.bounced", "message_id": "…", "recipient": "buyer@example.org",
 "detail": "550 5.1.1 no such user", "created_at": "2026-11-18T10:00:00Z"}
```

Each call carries `X-Platform-Signature: sha256=<hex>`, the HMAC-SHA256 of the raw body made with your signing secret. Compute it yourself and compare before trusting the body. Answer with a 2xx status; failed calls are retried for about a day. The tab shows the result of the last call. **remove the webhook** stops the calls. API: `GET/PUT/DELETE /api/v1/email/webhook`.

### Limits and billing

- The platform delivers your messages itself, straight to each recipient's mail server, signed with your domain's DKIM key. Keep the SPF and DMARC records so receivers know this platform may send for your domain.
- Your plan sets the number of emails per 24 hours (each recipient counts) and of sending domains.
- Temporary refusals (a full mailbox, a busy server) are retried for about half an hour, then the recipient is marked failed with the reason.
- Emails are billed per delivered recipient, per thousand.
- To receive mail at your domain, see [Mailboxes](/docs/user#mailboxes).

## Mailboxes

Host real mailboxes on your own domains, such as `anna@example.com`, and use them from any mail app (Outlook, Apple Mail, Thunderbird, Gmail's app, phones) or in the browser. Mailboxes come with aliases, a catch-all and spam filtering. Use them for your team's everyday email; for messages your application sends, use [Email](/docs/user#email). Open **Mailboxes** in the Services menu. The page shows how many mailboxes and how much storage you use out of your plan.

### Add a mail domain

1. On the **Mailboxes** page, click **Add a domain**, enter it (`example.com`) and click **Add domain** (developers).
2. The domain is verified like a sending domain. If its DNS is hosted on the platform (**DNS**), it is verified at once and every record is written for you. Otherwise create the verification record shown on its sending domain page; the domain is checked every few minutes.
3. Once it is verified, open the domain and click **Turn on mail**. Its mail is then delivered here.

The domain list shows each domain's status: **waiting for DNS verification**, **verified: ready to turn on**, or **receiving mail**.

### Mail DNS records

The domain's **DNS records** tab lists what mail needs:

- **MX**: where mail for the domain is delivered.
- **DKIM**: keys that sign outgoing mail (they appear once mail is turned on).
- **SPF** and **DMARC**: tell other servers which senders to trust (recommended).
- **SRV**: let mail apps find the servers by themselves from an address.

In a hosted zone they are written automatically, and your own SPF or DMARC record is kept. Otherwise create them at your DNS provider, and merge the SPF value into an existing SPF record. Mail goes to one place: remove MX records that point elsewhere (for example your previous provider). The page warns you while they are there. In a hosted zone, remove them, then click **Write them again**.

### Create mailboxes

1. On the domain's **Mailboxes** tab, under **Create a mailbox**, enter the **Address** (the part before `@`, such as `anna`).
2. Optionally a **Name** shown to recipients (`Anna Martin`).
3. A **Storage** size in MB (at least 50; 1024 by default).
4. A **Password** of at least 12 characters, or leave it empty to get a strong one, shown once.
5. Click **Create mailbox**, and give the address and password to its owner.

Each mailbox shows its aliases and its storage used. Its **manage** menu lets you:

- change the **Name** and **Storage**, then **Save**;
- **Add alias**: another address delivered to the same mailbox, such as `sales@example.com` (click **×** next to an alias to remove it);
- **Set new password** (leave it empty to generate one): mail apps must sign in again;
- **Delete with its mail**: type the address to confirm. This cannot be undone.

Type in **Find a mailbox** to filter the list.

### Catch-all

A catch-all receives mail sent to addresses of the domain that do not exist (a typo, an old address), instead of refusing it. On the domain's **Settings** tab, under **Catch-all**, choose a mailbox and click **Save**; choose **Off: refuse unknown addresses** to stop. Catch-alls attract spam: use one only if you need it.

### Your own server name

By default, mail apps and the webmail use the platform's mail server name. You can use a name of your own domain instead, such as `mail.example.com`, so your users see your brand.

1. On the domain's **Settings** tab, under **Your own server name**, enter the first part (`mail`) and click **Use this name**.
2. Point that name to the platform with an A record (the IP is shown; in a hosted zone the record is written for you).
3. Its HTTPS certificate is issued within minutes. The webmail then opens at `https://mail.example.com` with your domain on its sign-in page, and IMAP, POP3 and SMTP answer under that name.

**Back to the platform's name** undoes it. API: `PUT /api/v1/mail/domains/DOMAIN/server-name` with `{"name": "mail"}` (empty to go back).

### Webmail

Anyone with a mailbox can read and send mail in the browser, without installing anything and without a platform account. The webmail address is shown on the **Mailboxes** page and on the **Mail app settings** tab: share it with your users. They sign in with the full address and the mailbox's password.

In the webmail they can:

- open folders and **search** their mail;
- read messages (remote images and scripts in messages stay blocked) and download attachments;
- write, **reply** and **forward**, with Cc, Bcc and attachments;
- star messages, mark them read or unread, and move them to **Archive**, **Spam** or **Delete** (the trash), one by one or several at once;
- in the trash, **Restore** messages or **Delete for good**;
- switch between light and dark themes.

### Mail app settings

Most mail apps find the settings by themselves from the address. If yours asks, the domain's **Mail app settings** tab shows:

| | Port | Security |
| --- | --- | --- |
| Incoming (IMAP) | 993 | SSL/TLS |
| Incoming (POP3) | 995 | SSL/TLS |
| Outgoing (SMTP) | 587 | STARTTLS, with sign-in |

All on the server shown on the tab (or your own server name). The user name is the full address, such as `anna@example.com`; the password is the mailbox's password.

### Remove a domain

On the domain's **Settings** tab, **Remove the domain**: type the domain to confirm. Every mailbox of the domain is deleted with its mail, and mail for it is no longer received. The domain stays a sending domain for [Email](/docs/user#email).

### Plans and API

Your plan sets the number of mailboxes and the total storage you can give them; the page shows what you use.

API, with `DOMAIN` such as `example.com`:

- `GET/POST /api/v1/mail/domains` (`{"domain": "example.com"}`), `GET` or `DELETE /api/v1/mail/domains/DOMAIN`, `POST /api/v1/mail/domains/DOMAIN/activate` (turn on mail).
- `GET/POST /api/v1/mail/domains/DOMAIN/mailboxes`: create with `{"name": "anna", "display_name": "Anna Martin", "quota_mb": 1024}` (add `"password"` to choose it; otherwise the response carries the generated password once).
- `PATCH` or `DELETE .../mailboxes/NAME` (`display_name`, `quota_mb`), `POST .../mailboxes/NAME/password`.
- `POST .../mailboxes/NAME/aliases` with `{"alias": "sales"}`, `DELETE /api/v1/mail/domains/DOMAIN/aliases/ALIAS`.
- `PUT .../catch-all` with `{"mailbox": "anna"}` (empty to turn it off), `PUT .../server-name`.

## Object storage

Object storage keeps files ("objects") in buckets: user uploads, images, documents, backups, exports, static websites. It speaks the S3 protocol, so any S3 tool or library works with it: the MinIO client and SDKs, rclone, s3cmd, Cyberduck, restic and most backup tools. Open **Storage** in the Services menu. The top of the page shows how many buckets you have and how much of your plan's storage you gave to them.

The page has three tabs: **Buckets**, **Access keys** and **Use from code**.

### Buckets

A bucket is a container for objects, private by default.

1. On the **Buckets** tab, under **Create a bucket**, enter a name such as `uploads` (3 to 40 lowercase letters, digits and dashes) and a **Quota** in MB.
2. Click **Create bucket** (developers).

Its S3 name is `<organization>-<name>`, for example `acme-uploads`: use that name in code and tools. The quota is the most the bucket may hold: writes that would go beyond it are refused. The quotas of all your buckets together stay within your plan's storage; the form shows how much is still free.

The bucket list shows each bucket's objects, use against its quota (refreshed every few minutes), and whether it is **private** or a **public website**. To delete buckets in bulk, select them, choose **Delete with their objects (admins)**, type `delete` and click **Apply**.

API: `GET/POST /api/v1/buckets` (`{"name": "uploads", "quota_mb": 1024}`), `DELETE /api/v1/buckets/NAME` (admins). CLI: `platformctl bucket list`, `platformctl bucket create uploads --quota 2048`, `platformctl bucket delete uploads`.

### Access keys

An access key is the user name and password an S3 tool uses. Each key reaches only the buckets you choose.

1. On the **Access keys** tab, under **Create an access key**, enter a **Name** (such as `laptop backups`).
2. For each bucket, choose **no access**, **read** or **read and write**.
3. Click **Create access key**. Copy the **secret access key** at once: it is shown only now, with the access key ID, endpoint and region.

The tab lists every key, the buckets it reaches and how, and which service it belongs to when it was made by **Connect**. Click **delete** to revoke a key: clients using it lose access within seconds. API: `POST /api/v1/storage-keys` with `{"name": "ci", "buckets": {"uploads": "write"}}` (`read` or `write`), `GET /api/v1/storage-keys`, `DELETE /api/v1/storage-keys/ID`. CLI: `platformctl key create ci uploads=write`, `platformctl key list`, `platformctl key delete ID`.

A key sees only its buckets: listing all buckets shows those only, other organizations' buckets do not exist for it, and a key cannot create or delete buckets.

### Connect a service to a bucket

To use a bucket from a service of your organization, connect it instead of copying keys.

1. Open the bucket, then its **Connect** tab.
2. Under **Connect to a service**, choose the service and click **Connect**.
3. Redeploy the service.

The platform creates a key for that service only (read and write on this bucket) and stores these secrets in it:

| Variable | Value |
| --- | --- |
| `S3_ENDPOINT` | `{{s3}}` (a full URL, with `https://`) |
| `S3_REGION` | `{{s3region}}` |
| `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY` | the service's own key |
| `S3_BUCKET` | the bucket's S3 name, such as `acme-uploads` |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`, `AWS_DEFAULT_REGION`, `AWS_ENDPOINT_URL`, `AWS_ENDPOINT_URL_S3` | the same values under the conventional variable names many S3 libraries read by themselves |

Use the `S3_*` names in your own code. The **Connect** tab also shows the bucket's connection details and **Keys with access** to it. API: `POST /api/v1/storage/buckets/NAME/attach` with `{"service": "project/service"}`.

### Connection settings

For any tool (the **Use from code** tab shows them too):

- **Endpoint:** `{{s3}}`. Some libraries want the host name and the HTTPS switch separately: take the host from the URL (without `https://`) and turn TLS on.
- **Region:** `{{s3region}}`.
- **Path-style addressing:** the bucket goes in the path, `{{s3}}/acme-uploads/file.txt`, not in the host name. Turn it on in your tool (`--path on`, `pathStyle: true`, `force_path_style`).
- **Signature V4**, the default of current S3 tools. Presigned URLs made with the old signature are refused.

### Use it from code

**Command line (rclone)**

Install rclone (https://rclone.org), then create a remote once and use it:

```
rclone config create store s3 provider Other endpoint "$S3_ENDPOINT" region "$S3_REGION" access_key_id "$S3_ACCESS_KEY_ID" secret_access_key "$S3_SECRET_ACCESS_KEY" force_path_style true
rclone lsf store:acme-uploads/reports/
rclone copy ./report.pdf store:acme-uploads/reports/
rclone sync ./public store:acme-site
rclone link --expire 1h store:acme-uploads/reports/report.pdf
```

`rclone link` prints a presigned download link; `rclone sync` makes the bucket match the folder (it deletes objects that are not in the folder).

**Node.js** (`npm install minio`)

```
import * as Minio from "minio";

const u = new URL(process.env.S3_ENDPOINT);
const s3 = new Minio.Client({
  endPoint: u.hostname,
  port: u.port ? Number(u.port) : (u.protocol === "https:" ? 443 : 80),
  useSSL: u.protocol === "https:",
  accessKey: process.env.S3_ACCESS_KEY_ID,
  secretKey: process.env.S3_SECRET_ACCESS_KEY,
  region: process.env.S3_REGION,
  pathStyle: true,
});
const bucket = process.env.S3_BUCKET;

await s3.putObject(bucket, "avatars/42.png", buffer, buffer.length, { "Content-Type": "image/png" });
const stream = await s3.getObject(bucket, "avatars/42.png");
const url = await s3.presignedGetObject(bucket, "avatars/42.png", 3600); // valid 1 hour
```

**Python** (`pip install minio`)

```
import os
from datetime import timedelta
from urllib.parse import urlparse
from minio import Minio

u = urlparse(os.environ["S3_ENDPOINT"])
s3 = Minio(u.netloc, access_key=os.environ["S3_ACCESS_KEY_ID"], secret_key=os.environ["S3_SECRET_ACCESS_KEY"],
           region=os.environ["S3_REGION"], secure=u.scheme == "https")
bucket = os.environ["S3_BUCKET"]

s3.fput_object(bucket, "reports/report.pdf", "report.pdf", content_type="application/pdf")  # multipart for large files
s3.fget_object(bucket, "reports/report.pdf", "/tmp/report.pdf")
for o in s3.list_objects(bucket, prefix="reports/", recursive=True):
    print(o.object_name, o.size)
url = s3.presigned_get_object(bucket, "reports/report.pdf", expires=timedelta(hours=1))
```

**Go** (`go get github.com/minio/minio-go/v7`)

```
u, _ := url.Parse(os.Getenv("S3_ENDPOINT"))
client, err := minio.New(u.Host, &minio.Options{
    Creds:        credentials.NewStaticV4(os.Getenv("S3_ACCESS_KEY_ID"), os.Getenv("S3_SECRET_ACCESS_KEY"), ""),
    Secure:       u.Scheme == "https",
    Region:       os.Getenv("S3_REGION"),
    BucketLookup: minio.BucketLookupPath,
})
_, err = client.PutObject(ctx, os.Getenv("S3_BUCKET"), "notes/today.txt",
    strings.NewReader("hello"), 5, minio.PutObjectOptions{ContentType: "text/plain"})
```

Other languages: any S3 library works with the endpoint, region, key, path-style addressing and Signature V4.

### What you can do

- **Upload, download, delete** objects of any size: up to 5 GB in one request, larger files with multipart upload (rclone and the SDKs' upload helpers switch to it by themselves). Batch delete removes up to 1000 objects in one call.
- **Folders** are key prefixes: `reports/2026/march.pdf` sits in the folder `reports/2026/`. List one level with a prefix and the `/` delimiter; `rclone sync` copies whole trees.
- **Content type and metadata:** set `Content-Type`, `Cache-Control`, `Content-Disposition` and your own metadata (user metadata in your S3 library) when you upload; they come back on download and `HEAD`.
- **Partial downloads** with the `Range` header (video seeking, resumable downloads), conditional requests, and **server-side copies** (`CopyObject`) without downloading.

### Presigned URLs

A presigned URL lets someone download (`GET`) or upload (`PUT`) one object for a limited time, without a key: a private invoice link in an email, a direct upload from a mobile app. Your backend creates it with its key (`presignedGetObject`, `presignedPutObject`, `presigned_get_object`, `rclone link`); the link stops working when it expires or when that key is deleted. Presigned `POST` (an HTML form that uploads a file) works too. With **Browser access** set, web pages can use these links directly (see below).

### Upload from the browser

A web page can send a file straight to the storage, without passing it through your service, which saves your service's bandwidth and memory.

1. Open the bucket's **Settings** tab. Under **Browser access (CORS)**, enter your site in **Allowed sites**, such as `https://shop.example.com` (one per line, no path; `*` allows every site), and click **Save browser access**. Leave it empty to turn it off. API: `PUT /api/v1/buckets/NAME/cors` with `{"origins": ["https://shop.example.com"]}`; `[]` turns it off.
2. Your service creates a presigned `PUT` URL for the object. It keeps the key; the page never sees it:

```
const url = await s3.presignedPutObject(process.env.S3_BUCKET, `uploads/${userId}/${name}`, 300); // 5 minutes
```

3. The page uploads with it:

```
await fetch(url, { method: "PUT", body: file });
```

The same setting lets pages read objects with `fetch` through presigned `GET` URLs (or from a public website bucket). Pages of other sites are refused. Browser access alone never makes objects public: a page still needs a presigned URL.

### Static website hosting

A bucket can serve a static website (HTML, CSS, JavaScript, images), for example a single-page app or documentation built by a generator.

1. Open the bucket's **Settings** tab, under **Quota and website**.
2. Tick **Public website**.
3. Set the **Index document** (`index.html` by default), served for a folder, and optionally an **Error document** (`404.html`).
4. Click **Save settings**.
5. Upload your site, for example `rclone sync ./dist store:acme-site`.

The site answers at `https://<organization>-<name>.web.{{apps}}/`, with HTTPS (shown on the **Connect** tab). Every object of the bucket becomes readable by anyone: keep private files in another bucket. Untick **Public website** to make the bucket private again.

### Browse objects in the console

The bucket's **Objects** tab is a simple file browser:

- open folders and go **up**;
- click a file to download it (through a link valid for 5 minutes);
- **delete** a file after confirming;
- **Upload a file** of up to 25 MB into the current folder.

It shows the first 200 entries of a folder; use an S3 tool to list everything, and for larger files or whole folders.

### React to new files

A function can run each time an object is uploaded to or deleted from a bucket, filtered by key prefix and suffix: create thumbnails, scan files, update a search index. Set it up on the function's **Triggers** tab (see [Triggers](/docs/user#triggers)).

### Bucket settings and deletion

- **Quota** (Settings tab): raise or lower it, within your plan's storage, then **Save settings**.
- **Delete bucket** (Settings tab, admins): type the bucket's name to confirm. It removes the bucket with all its objects; access keys keep working for your other buckets. This cannot be undone.

### Supported S3 operations

| Works | Not available |
| --- | --- |
| PutObject, GetObject, HeadObject, DeleteObject, DeleteObjects | Bucket policies and ACLs (use keys with per-bucket access) |
| ListObjectsV2 / ListObjects (prefix, delimiter, pagination) | Object tagging |
| CopyObject, ranges, conditional requests | Versioning and object lock |
| Multipart upload (create, upload part, complete, abort, list) | Lifecycle rules; CORS through S3 (use **Browser access** in the bucket's settings) |
| Presigned GET, PUT and POST | Creating or deleting buckets through S3 (use the console, the API or the CLI) |
| ListBuckets (the key's buckets), checksums | Storage classes, replication, archive tiers |

Server-side encryption headers are accepted and ignored: data is stored on the platform's own servers.

### Limits and plans

- **Quotas:** each bucket has its quota; uploads beyond it are refused with `QuotaExceeded`. Raise it in the bucket's settings, within your plan's storage.
- **Plans:** Trial includes 2 buckets and 1 GB, Starter 10 buckets and 10 GB, Pro 100 buckets and 100 GB. Stored data is billed per GB-month.
- **Console uploads** are limited to 25 MB per file; S3 tools have no such limit.

## Public status page

A status page tells your customers whether your application is up, without them having to email you. Each project can have one public page that anyone can open, without an account, at `/status/<organization>/<project>`.

1. Open the project, then its **Settings** tab (admins and owners).
2. Under **Public status page**, click **Make the status page public**.
3. The page's address appears: share it, or link to it from your site.

What it shows:

- each service's state: **operational**, **down** or **not running**;
- a bar per day for the last 30 days with the service's uptime;
- **Past incidents**: outages from the last 14 days, with when they started and were resolved (or **ongoing**).

The platform checks the services every few seconds. The page never shows logs, error messages, images, environment variables or secrets. Click **Turn off** at any time: the address stops working immediately. It is off by default.

## Private network between projects

Services often need to call each other: a website calling its API, a worker calling an internal service. Every web service answers on a private name, `<service>.<project>.internal`, over plain HTTP on port 80. From your code, call `http://api.shop.internal/orders` instead of the public address: the traffic stays on the server's private networks, it is faster, and the service does not even need to be public.

**Within a project**, services reach each other by these names at once. The project's **Settings** tab lists this project's names under **Private network**; a service's **Domains** tab shows its own.

**Between projects**, an admin connects them:

1. Open one project's **Settings** tab.
2. Under **Private network**, choose the other project and click **Connect**.
3. Services of both projects can now call each other; connections go both ways. The table lists each connected project and its private names.
4. **disconnect** removes the access (after you confirm).

Good to know:

- Connecting or disconnecting redeploys both projects' services, with no downtime, so their containers learn the names. A service created later is known to the other services after their next deployment.
- Requests from other projects, other organizations and the internet are refused, even when they send the right host name.
- Databases stay reachable from their own project only. Functions reach the services of their project by these names too. On an installation with several servers, private names work for projects on the main server.
- API: `GET /api/v1/projects/PROJECT/connections`, `POST /api/v1/projects/PROJECT/connections` with `{"project": "other"}` (admins), `DELETE /api/v1/projects/PROJECT/connections/OTHER`.

## Custom domains and HTTPS

Every public web service has a platform address under `{{apps}}`. Add your own domain, such as `shop.example.com`, so customers see your name; HTTPS certificates are issued and renewed automatically once you prove you own the domain.

### Add a domain

1. Open the service's **Domains** tab, enter the domain under **Custom domains** (for example `shop.example.com`) and click **Add domain**. You can also add it from the **Domains** page in the menu: enter the domain, choose the service, and click **Add domain**.
2. At your DNS provider, create a **TXT** record named `_platform-challenge.shop.example.com` with the value `platform-verify=<token>` shown on screen. It proves you own the domain.
3. Create a **CNAME** for `shop.example.com` pointing to the platform (or an A record to the platform's address). For the domain itself (`example.com`), most providers allow only an A record.
4. Click **Verify**. DNS changes can take a few minutes; click again if the record is not found yet.

Once verified, the domain serves the application within seconds, and an HTTPS certificate is issued on the first visit. Starter plans allow 3 custom domains, Pro plans 20. A domain can belong to only one organization. API: `POST /api/v1/projects/PROJECT/services/SERVICE/domains` with `{"hostname": "shop.example.com"}`.

If the domain's DNS is hosted on the platform, steps 2 to 4 happen by themselves: see [DNS hosting](/docs/user#dns-hosting). The domain's panel then says the platform handles its DNS, and warns you if the zone's delegation is incomplete.

### The Domains page

The **Domains** page (in the menu) lists every custom domain of the organization with its service and status (**verified** or **waiting for DNS**), out of your plan's limit. Type in **Find domains** to filter. To remove several, select them, choose **Remove**, type `delete` and click **Apply**. On a service's **Domains** tab, **remove** deletes one domain after you confirm.

### Platform address

The **Platform address** section of the service's **Domains** tab shows the service's address under `{{apps}}` (and its private name). Once a custom domain is verified, you may want customers to use only your domain:

1. Click **Turn off the platform address** and confirm.
2. The service now answers only on its own domains. Links to the old address stop working; extra public ports keep their addresses.
3. **Turn the platform address back on** undoes it.

If the service has no verified domain left, the platform address answers again on its own. API: `POST /api/v1/projects/PROJECT/services/SERVICE/default-address` with `{"on": false}` or `{"on": true}`.

A private service has no public address at all; untick **Private** in its settings to publish it (see [Public or private](/docs/user#public-or-private)).

### Paths served by other services

One domain can front several services, for example `shop.example.com` for the website and `shop.example.com/api` for the API, which avoids cross-site browser issues.

1. Under a verified custom domain on the **Domains** tab, enter a path (such as `/api`).
2. Choose the service that serves it.
3. Tick **remove the prefix** when the target expects `/orders` rather than `/api/orders`.
4. Click **Route path**.

Requests for `/api` and everything below go to that service, the rest to the domain's own service. The target can be any web service of the organization, private ones included. The longest matching path wins. Up to 20 paths per domain; **remove** deletes one.

## DNS hosting

DNS is the address book of the internet: it says which server answers for `example.com`, `www.example.com` and your mail. Host your domains' DNS on the platform to manage all of it in one place, and let the platform write the records for your services, email and mailboxes by itself. Open **DNS** in the Services menu.

### Add a zone

1. On the **DNS** page, click **Add a domain**, enter it (`example.com`) and click **Add domain** (developers).
2. At your domain's registrar (where you bought it), set the domain's name servers to the ones shown: two names for the same server.
3. The platform answers for the domain as soon as it sees the delegation, checked every 2 minutes (click **Check now** on the zone's page). Registrars can take up to a day to publish the change.

Until the delegation is seen, nothing is served, so nobody can take over a domain they do not control. Prepare your records meanwhile: they are served as soon as the zone is active. A zone not delegated within 14 days is removed. Your plan sets the number of zones.

The **DNS** page lists your zones with their status (**active** or **waiting for the name servers**) and number of records.

### Records

On the zone's page, under **Add a record**, fill in **Name**, **Type**, **Value**, **Priority** (MX and SRV) and **TTL**, then click **Add record**. Click **delete** next to a record to remove it.

| Type | Use | Value example |
| --- | --- | --- |
| A | a name's IPv4 address | `203.0.113.10` |
| AAAA | a name's IPv6 address | `2001:db8::10` |
| CNAME | an alias for another name | `shop.example.net` |
| TXT | text, for verification, SPF, DMARC | `v=spf1 ~all` |
| MX | the mail server for the name, with a priority | `mail.example.com` |
| CAA | which certificate authorities may issue certificates | `0 issue "letsencrypt.org"` |
| NS | delegate a sub-domain to other name servers | `ns1.example.net` |
| SRV | where a service runs: `weight port target`, priority in its own field | `0 443 sip.example.com` |

- **Names:** `@` for the domain itself, `www`, `api.v2`, or a wildcard such as `*.apps`.
- **TTL:** 60 seconds to a day (300 seconds by default). It is how long resolvers may keep an answer.
- A CNAME stands alone at its name, so it cannot be used for the domain itself (`@`) or next to other records.
- A zone holds at most 500 records.
- The SOA and NS records of the domain are provided by the platform.
- Changes apply within seconds; resolvers may keep earlier answers for their TTL.

API: `GET/POST /api/v1/dns/zones` (`{"domain": "example.com"}`), `GET/POST /api/v1/dns/zones/DOMAIN/records` (`{"name": "www", "type": "CNAME", "value": "…", "priority": 0, "ttl": 300}`), `DELETE /api/v1/dns/zones/DOMAIN/records/ID`.

### Point a name at a service

Under **Point a name at a service**, enter a name (`www`, or leave it empty for the domain itself), choose the service and click **Add record**. It adds a CNAME to the service's address (an A record to the platform's address for the domain itself). Then add the domain on the service's **Domains** tab so it is served with HTTPS: in a hosted zone it is verified at once.

### Import a zone from another provider

When you move a domain from another DNS provider, copy its records first so websites, mail (MX, DKIM, autodiscovery SRV) and verification records keep answering when the name servers change.

1. Add the zone here, but do not change the name servers yet.
2. Export the zone file from your current provider (the text "BIND" format most providers offer: for example OVH, DNS zone, *Change in text format*; Cloudflare, *Export*).
3. Paste it in **Import records** on the zone's page and click **Import**. The provider's SOA and name servers are left out; records already in the zone are kept.
4. Check the records, then at the registrar list **only** the platform's name servers. A mix with the old provider's makes some visitors get the old answers.

API: `POST /api/v1/dns/zones/ZONE/import` with the zone file as the body.

### Delegation check

After the zone is active, the platform keeps checking the delegation every hour. If the registrar lists other name servers too, or not all of the platform's, the zone's page shows **The delegation is incomplete** with the exact list the registrar must have, and the **DNS** page marks the zone **delegation incomplete**. Custom domains in the zone show the same warning. Resolvers may keep the old list for a day after you fix it.

### Automatic records

When a domain of your organization lies in a zone you host here, the platform writes its DNS records itself:

- a **custom domain** of a service (`shop.example.com` in the zone `example.com`): a CNAME to the service (an A record for the zone's own name) and the verification record. It is verified at once when the zone is active, or as soon as its delegation is confirmed: no record to copy, no **Verify** to click;
- an **email sending domain**: the ownership, DKIM, SPF and DMARC records, verified the same way. An SPF or DMARC record you already have is kept, not duplicated;
- a **mail domain** with mailboxes: the MX, DKIM, SPF, DMARC and SRV records once mail is turned on, and the A record of your own server name.

These records are marked **automatic** in the zone, with what they belong to; they change and disappear with the domain. A record you wrote yourself at the same name is never replaced. Adding a zone also does this for the domains you already had in it.

### Delete a zone

At the bottom of the zone's page, under **Delete the zone**, type the domain and click **Delete zone**. The platform stops answering for it. Change the name servers at your registrar first, or the domain stops resolving.

## Edge cache (CDN)

**What it is for:** the edge cache lets the platform's proxy answer repeat requests for the same page or file itself, without reaching your application. Use it for public content that many visitors load: images, scripts, style sheets, public pages and public API answers. Your application does less work and visitors get faster answers.

**How to use it:**

1. In your application, mark the responses that may be shared between visitors with a `Cache-Control` header, for example `Cache-Control: public, max-age=3600` (one hour), or `s-maxage=…` to set a separate lifetime for shared caches.
2. Open the service, then the **Settings** tab, and tick **Edge cache (CDN)**.
3. Save (tick **deploy** to apply it with a deployment, or deploy later).
4. Check that it works: load the same address twice and look at the `Cache-Status` response header. It says whether the answer was a hit (served from the cache) or a miss (served by your application).

With the API, send `"edge_cache": true` with the service settings (see [API and command-line tool](/docs/user#api-and-command-line-tool)).

**Details:**

- Only responses that explicitly allow shared caching are kept. Responses marked `private` or `no-store` are never kept, and responses without a `Cache-Control` header are treated as `private`, so turning the cache on never changes how such pages behave.
- Entries are kept per address (host name), so nothing is ever shared between two services.
- Responses larger than 10 MB are not kept.
- There is no "purge" button. To change cached content before it expires, use versioned file names (`app.3f2a.js`) or short `max-age` values.
- Never mark pages that show personal data (account pages, carts) as `public`.

## Private image registry

**What it is for:** your organization has its own private space for container images at `{{registry}}/<organization>/`. Push the images your CI builds there, then deploy them. Nobody outside your organization can read or write it, and every image pushed there is scanned for known vulnerabilities.

Open it from the product menu: **Container registry**. The page has three parts: **Registry** (the address and the login commands), **Tokens** and **Image scans**. It also shows your organization's scan policy (see [Security scanning](/docs/user#security-scanning)).

### Registry tokens

A registry token is the password your tools (`docker`, CI pipelines) use to log in to the registry.

1. On **Container registry**, go to **Tokens** (admins).
2. Enter a name (for example `ci`), and keep **can push** ticked if the token must upload images. Untick it for a pull-only token.
3. Click **Create token**. Copy the token right away: it is shown only once.
4. Store it in your CI's secret store.

Details:

- Tokens expire after one year. The **Last used** column shows whether a token is still in use; revoke tokens nobody uses.
- Click **revoke** next to a token to stop it working immediately (admins). Creating and revoking tokens is recorded in the audit log.
- Use a push token in CI only, and pull-only tokens elsewhere.
- Viewers and developers can see the page and the token list, but not create or revoke tokens.

### Push and pull images

1. Log in, with your organization's ID as the user name and the token as the password:

```
docker login {{registry}} -u <organization>
```

2. Tag your image under your organization's space and push it:

```
docker tag myapp:1.4.2 {{registry}}/<organization>/myapp:1.4.2
docker push {{registry}}/<organization>/myapp:1.4.2
```

3. Deploy it: set the service's **Image** to `{{registry}}/<organization>/myapp:1.4.2`, or run `platformctl deploy shop/web --image {{registry}}/<organization>/myapp:1.4.2 --wait`.

Details:

- The platform pulls your private images by itself: you never enter registry credentials in a service.
- An image from another organization's space is always refused, with the message "images from the platform registry must be under …".
- Images built by [Builds from git](/docs/user#builds-from-git) are pushed to the same space automatically.

### Image scans

**Image scans** lists the most recent scans of your organization's images: the image, where the scan came from (a push, a deployment or the nightly check), the number of findings per severity, and when it ran. Hover an error to see why a scan failed. See [Security scanning](/docs/user#security-scanning) for what the results mean.

### Automatic clean-up

To keep your space tidy, the platform deletes old tags once a day. A tag is deleted only when all of these are true:

- no service is currently set to it;
- none of the last 20 deployments of any service used it;
- it was not pushed or built in the last 30 days (your operator can change this number).

A tag that points to the same image as a tag that is kept is never deleted. Pin versions (`myapp:1.4.2`) so that rollbacks always find their image.

## Traffic protection

**What it is for:** traffic protection filters requests at the platform's proxy, before they reach your application. Use it to block common attacks, to slow down visitors who send too many requests, and to shut out addresses that misbehave.

**How to use it:**

1. Open the service and its **Security** tab. The top line sums up the current state (firewall on or off, the request limit, how many addresses are blocked).
2. Change the settings under **Traffic protection** (developers and above):
3. Tick **Web application firewall** to turn on the firewall.
4. Enter a number in **Requests per minute per visitor**, or leave it empty for no limit.
5. List addresses in **Blocked addresses**, one per line.
6. Click **Save protection**. Changes apply within a few seconds, on every address of the service (platform address and custom domains), without a redeploy.

**Details:**

- **Web application firewall:** uses the OWASP Core Rule Set to block common attacks such as SQL injection, cross-site scripting, path traversal and known scanner probes. Blocked requests get `403 Forbidden`. It is off by default because some applications (for example ones that accept HTML or code in forms) can trigger false positives: turn it on, then click through your application to check that everything still works.
- **Requests per minute per visitor:** from 10 to 100,000. Each visitor IP address may send at most this many requests per rolling minute; above it, they get `429 Too Many Requests` with a `Retry-After` header. Count every file a page loads (scripts, images, API calls) when you choose the number.
- **Blocked addresses:** up to 100 IPv4 or IPv6 addresses or ranges in CIDR form (for example `203.0.113.7` or `198.51.100.0/24`). They always get `403`.
- Health checks and alerts are not affected: the platform checks your application directly, not through the proxy.
- Every change is recorded in the audit log.

## Security scanning

**What it is for:** the platform checks your container images against a public database of known vulnerabilities (CVEs), so you learn when a package in your image has a security hole and which version fixes it.

**When images are scanned:**

- **On push** to your private registry.
- **On deployment**, before the application starts. A result from the last 24 hours for the same image is reused.
- **Every night**, for images that are running, because new vulnerabilities are published every day. When the nightly scan finds critical vulnerabilities in a running image, a `critical_vulns` alert is sent.

**Where to see the results:**

- The service's **Security** tab, under **Vulnerability scan**: the image of the latest deployment, the number of findings per severity, then up to 25 findings, most severe first, with the vulnerability ID, the package, the installed version and the version that fixes it ("no fix yet" when there is none).
- The service's **Overview** tab, under **At a glance**: the counts per severity.
- **Container registry** › **Image scans**: the latest scans of all your images.

**Scan policy:**

| Policy | Effect |
| --- | --- |
| warn (default) | Findings are shown; deployments are never blocked. |
| block-critical | An image with a critical vulnerability is refused, and the deployment shows which ones ("blocked by security policy"). If the scanner is unavailable, deployments fail rather than go out unscanned. |

The platform operator sets the policy for your organization; the **Container registry** page shows which one applies.

**How to fix findings:** most come from the base image. Rebuild your image on the latest patch version of its base image (for example `node:22-alpine` pulled again), push it and redeploy.

## Logs and metrics

**What it is for:** everything your application writes to standard output and standard error is collected, so you can follow what it does, search for errors and keep a history. CPU and memory are measured for every instance.

Write logs to standard output rather than to files: files are lost at each deployment unless they are in the persistent folder, and only standard output and error are collected.

### Logs of one service

1. Open the service and its **Logs** tab.
2. The last 6 hours are shown, up to 300 lines, refreshed every few seconds while the tab is open. Lines written to standard error are highlighted.
3. Type in **filter text** to show only matching lines.
4. Untick **follow** to stop the view from scrolling to the newest line while you read.

Command line: `platformctl logs shop/web --filter error`. API: `GET /api/v1/projects/shop/services/web/logs?q=error`.

### Metrics of one service

The service's **Overview** tab shows **Resource usage** over the last hour: CPU (in % of one core) and memory (MB against the service's limit). Metrics appear about a minute after the first deployment. For longer periods and your own metrics, see [Prometheus and Grafana](/docs/user#prometheus-and-grafana).

### Search the logs of every service

**Monitoring** › **Logs** searches the logs of every service of the organization at once.

1. Choose what to search: **all services**, one project, or one service.
2. Choose the period: last hour, 6 hours, 24 hours, 7 days or 30 days. Periods longer than your plan keeps are greyed out.
3. Optionally enter **text to find**, then click **Search**.
4. Lines appear newest first, up to 500, with the time (UTC), the service and the message.
5. Click **Download** to save the same search as a text file of up to 100,000 lines.

Every member with access to the whole organization can search and download logs (members limited to some projects cannot open Monitoring).

### Log retention

Your plan sets how long logs are kept: on the default plans, Trial keeps logs 1 day, Starter 7 days and Pro 30 days. The **Logs** page shows your plan's number. Older lines are no longer shown, downloaded or exported. To keep logs longer, export them.

### Log export

**What it is for:** an export copies every new log line, continuously, to one of your buckets or to your own HTTPS endpoint, so you can keep logs as long as you want or feed them to your own tools.

1. Go to **Monitoring** › **Logs**, section **Exports** (admins).
2. Under **Add an export**, enter a **Name** and choose **Logs of** every project or one project.
3. Choose the **Destination**:
4. **Bucket** (when you have object storage): pick the bucket and a **Prefix** (`logs/` by default; letters, digits, dots, dashes, underscores and slashes).
5. **HTTPS endpoint**: enter the **Endpoint URL** (public `https://` addresses only) and, optionally, a **Signing secret**.
6. Click **Add export**.

How it works:

- Each export starts with the last hour, then sends new lines every minute, in order and exactly once. A failed delivery is retried later without losing or repeating lines.
- To a **bucket**: one gzipped NDJSON file per hour under `<prefix>YYYY/MM/DD/HH/`.
- To an **HTTPS endpoint**: `POST` requests with gzipped NDJSON (`Content-Encoding: gzip`). When you set a secret, each request carries `X-Platform-Signature: sha256=<hex>`, the HMAC-SHA256 of the body with your secret (verify it as shown under [Alerts](/docs/user#alerts)).
- Every line is a JSON object: `{"time": "…", "project": "shop", "service": "web", "stream": "stdout", "message": "…"}`.
- The table shows each export's destination, its status (**ok**, **error** with the reason, or **waiting**), how far it has sent ("sent up to …") and the number of lines sent.
- Click **pause** to stop an export for a while and **resume** to continue where it stopped (lines older than your retention are no longer available), or **delete** to remove it.

### Privacy of logs and metrics

Your logs and metrics are stored separately from other clients' and are only readable through your organization.

## Prometheus and Grafana

**What it is for:** your metrics are available through a Prometheus-compatible API, limited to your organization. Use it to build dashboards in Grafana, to query metrics from scripts, and to collect your application's own metrics.

### Query your metrics

Point Grafana, `promtool` or any Prometheus client at `{{console}}/api/v1/prometheus`, with an API token as bearer (`Authorization: Bearer pla_…`; a viewer token is enough). The URL and an example are on **Monitoring** › **Grafana and Prometheus**.

```
curl -H "Authorization: Bearer $PLATFORM_TOKEN" \
  "{{console}}/api/v1/prometheus/api/v1/query?query=sum(rate(platform_container_cpu_seconds_total[5m]))"
```

- **Platform metrics** of every instance: `platform_container_cpu_seconds_total`, `platform_container_memory_bytes`, `platform_container_memory_limit_bytes`, `platform_container_network_rx_bytes_total` and `platform_container_network_tx_bytes_total`, labelled `service_id`, `project_id` and `instance`.
- **Supported endpoints:** `query`, `query_range`, `series`, `labels`, `label/<name>/values`, `metadata`, `query_exemplars`, `format_query` and `status/buildinfo`, with `GET` or `POST`.
- Metrics are kept 30 days.

### Your application's own metrics

1. Make your application serve Prometheus metrics, for example at `/metrics`.
2. In the service's **Settings** tab, set **Metrics path** (such as `/metrics`) and, if the metrics are on another port than the service's, **Metrics port**.
3. Save and deploy.

The platform then scrapes every instance every 30 seconds and adds the labels `service`, `project`, `service_id` and `instance`. Labels named `org_id`, `project_id`, `service_id`, `service`, `project` or `instance` set by your application are replaced. Each scrape may hold up to 20,000 samples and 4 MB.

### Managed Grafana

**What it is for:** a ready-made Grafana, already connected to your metrics, without any setup.

1. Go to **Monitoring** › **Grafana and Prometheus** (developers and above).
2. Choose a project in **Create a managed Grafana in** and click **Create Grafana**.
3. The next page shows the address and the password of the `admin` user. Copy the password now: it is shown once. It is also stored as the service's secret `GF_SECURITY_ADMIN_PASSWORD`.
4. The `grafana` service is built, then deployed on its own, usually within a few minutes. Click **Follow the build** to watch it, or watch the **in progress** indicator at the top of the page.
5. Sign in as `admin` and change the password in Grafana.

Details:

- The service comes with the Prometheus data source (using a read-only token of your organization) and a **Services** dashboard already set up. Its data is kept in a persistent folder, so it is backed up every night.
- It counts as one service of your plan (250 millicores, 256 MB) and its build counts as build minutes.
- A project can have only one service named `grafana`.

### Alarms on metrics

See [Alerts](/docs/user#metric-alarms) to be notified when a metric crosses a threshold.

## Analytics

**What it is for:** the **Analytics** page shows how your web services are used: how many requests and visitors they get, how fast they answer, which pages are popular and where visitors come from. Traffic needs no code at all; visitor behavior needs a small script. Neither stores IP addresses or cookies.

**How to use it:**

1. Open **Analytics** from the product menu.
2. Choose a service (`project/service`) and a period: last 1h, 24h, 7d or 30d. Click **Show**.
3. Switch between the **Traffic** and **Behavior** tabs.

### Traffic report

Measured at the platform's proxy for every request to the service's addresses, with nothing to add to your code. Requests appear within a few seconds.

- **Requests**, **Visitors**, **Server errors (5xx)**, latency **p50**, **p95** and **p99** (in milliseconds), and data **Sent**.
- **Requests over time**, with errors in red. Hover a bar for the exact numbers.
- **Top paths**, **Slowest paths (p95)**, **Status codes**, **Referrers**, **Browsers**, and **Systems and devices**.
- Bots (crawlers, scripts, uptime checkers) are recognized and left out of visitors, browsers and systems.
- Query strings are never recorded, as they can hold secrets.

### Behavior (page-view script)

A small cookieless script that counts page views, sessions and your own events.

1. On the **Behavior** tab, click **Turn on the page-view script** (developers and above).
2. Copy the snippet shown and put it in the `<head>` of your pages. It looks like `<script defer src="{{console}}/a/s.js" data-site="…"></script>`.
3. Deploy your pages. Page views appear within a few seconds.
4. To record your own events, call `platformTrack` in your pages:

```
platformTrack("signup", {plan: "pro"});
```

What you see: **Page views**, **Visitors**, **Sessions**, **Bounce rate**, **Custom events**, then **Top pages**, **Events**, **Where visitors come from** and **Campaigns (utm_source)**.

Details:

- Single-page applications are followed through browser history changes: no extra code is needed for client-side navigation.
- Event names: up to 60 characters (letters, digits, spaces and `_ . : / -`). Up to 10 properties per event, each value cut to 200 characters.
- `utm_source`, `utm_medium` and `utm_campaign` are read from the page address.
- Only pages served at the service's own addresses (its platform address and verified custom domains) can report to it; events sent from other sites are ignored. Bots are ignored too.
- Click **turn the script off** to stop collecting (developers and above). Remove the snippet from your pages as well.

### Privacy of analytics

No IP address and no cookie is stored. A visitor is counted with a hash of their address and browser, salted with a secret that changes every day, so visitors cannot be followed from one day to the next. Analytics data is kept as long as your logs (see [Logs and metrics](/docs/user#log-retention)).

### Analytics API

The same report is available with the API: `GET /api/v1/projects/shop/services/web/analytics?period=24h` (`1h`, `24h`, `7d` or `30d`; viewer token).

## Backups and restore

**What it is for:** the persistent folder of each service is backed up every night, so you can go back to an earlier copy of your data after a mistake, a bad update or a broken file. Backups are encrypted with a key unique to your organization. Services without a persistent folder hold no data to back up, and their **Backups** tab does not appear.

Managed databases have their own backups: see [Managed databases](/docs/user#managed-databases).

### Schedule and on-demand backups

1. Open the service and its **Backups** tab.
2. To change the time, choose an hour (UTC, 03:00 by default) next to **nightly at** and click **Save schedule** (developers and above).
3. To pause nightly backups, untick **nightly at** and click **Save schedule**; tick it again to resume. The **Overview** tab shows "paused" while they are off.
4. To take a snapshot right now, for example before a risky change, click **Back up now** (developers and above).

Details:

- **Retention:** 7 daily, 4 weekly and 6 monthly snapshots. Safety snapshots taken before a restore are always kept.
- Only one backup or restore runs at a time for a service.
- **Snapshots** lists every snapshot with its ID, when it was taken (UTC), its size and file count, and a badge: **nightly** or **before restore**.
- **Recent jobs** lists the latest backups and restores with their status and any error message. A failed job sends a `backup_failed` alert.

### Restore a snapshot

Admins and owners can restore.

1. In **Snapshots**, find the one to go back to.
2. Type the service ID in the box next to it and click **Restore**, then confirm.
3. The platform takes a **safety snapshot** of the current data, stops the application, replaces the folder with the snapshot, and starts the application again.
4. If the restore was a mistake, restore the snapshot marked **before restore**.

### Consistent backups

Backups are taken while the application runs. If your service runs its own database, dump it to the persistent folder on a schedule (for example with a [scheduled job](/docs/user#scheduled-jobs)) so the backup holds a consistent copy.

## Alerts

**What it is for:** the platform watches your applications all the time. When something goes wrong (an application stops answering, a deployment fails, a metric crosses a threshold), it opens an incident and notifies your team on the channels you choose: push notifications, email or your own webhook.

### Incidents

- **Monitoring** (the first page, **Incidents**) lists the incidents of the whole organization: what happened, the service, when it started and when it resolved.
- Each service's **Alerts** tab lists that service's incidents.
- The organization's home page shows open incidents under **Health**, and the service's **Overview** lists them under **At a glance**.

| Event | When it happens |
| --- | --- |
| service_down | 3 health checks in a row fail, or the container stopped or ran out of memory. Resolves on its own when the application answers again. |
| cpu_high, memory_high | Usage stayed above your threshold for 5 minutes. |
| deploy_failed | A deployment failed. |
| backup_failed | A backup or restore job failed. |
| critical_vulns | The nightly scan found critical vulnerabilities in a running image. |
| job_failed | A [scheduled job](/docs/user#scheduled-jobs) failed or timed out. |
| metric_alarm | A [metric alarm](/docs/user#metric-alarms) stayed beyond its threshold. |
| budget | Spending crossed a [budget](/docs/user#budgets) threshold, or the forecast passed the limit. |

A "Disk limit" notification is also sent at 90% and 100% of your plan's disk (see [Disk limits](/docs/user#disk-limits)). It goes to every channel, whatever events the channel selects.

### CPU and memory thresholds

1. Open the service and its **Alerts** tab (developers and above).
2. Under **Health and thresholds**, enter a **CPU alert** and/or a **Memory alert** in % of the service's limits (1 to 100). Leave a box empty to turn that alert off.
3. Click **Save thresholds**.

### Notification channels

Admins add channels on **Monitoring** › **Notification channels**, in the **Add a channel** form.

1. Enter a **Name** (for example `on-call`) and choose the **Type**:
2. **email** (when the platform sends email): enter the **Email address**.
3. **ntfy push**: push notifications to a phone or desktop with the ntfy app. Enter the ntfy server in **URL**, the **ntfy topic**, and an access token in **Secret** if the topic is protected.
4. **webhook (JSON)**: enter your endpoint in **URL** and, optionally, a signing **Secret**.
5. Untick the events this channel should not receive (all are ticked by default).
6. Click **Add channel**, then **send test** next to it to check that notifications arrive.

The channel list shows each channel's address, its events and its **last delivery** (sent or failed, with the reason). Click **remove** to delete a channel. Test notifications are limited to a few per minute.

Details:

- Channel URLs must be public HTTPS addresses; private and internal addresses are refused.
- ntfy notifications are sent with high priority when an incident opens, and normal priority when it resolves.
- Email notifications carry a link to the service in the console.
- A webhook receives a `POST` with a JSON body: `incident_id`, `kind` (the event), `status` (`firing`, `resolved` or `event`), `title`, `summary`, `organization`, `project`, `service`, `started_at`, `resolved_at` (once resolved) and `console_url`.

### Verify webhook signatures

If you set a webhook secret, each request carries `X-Platform-Signature: sha256=<hex>`, the HMAC-SHA256 of the raw body with your secret. Check it before trusting the request:

```
import hmac, hashlib
expected = "sha256=" + hmac.new(secret.encode(), raw_body, hashlib.sha256).hexdigest()
valid = hmac.compare_digest(expected, request.headers["X-Platform-Signature"])
```

The same signature is used by [log exports](/docs/user#log-export).

### Metric alarms

**What it is for:** a metric alarm watches any value you can compute from your metrics (CPU, memory, network, or your application's own metrics) and alerts you when it crosses a threshold.

1. Go to **Monitoring** › **Metric alarms** (developers and above).
2. Enter a **Name** (for example `High CPU on web`).
3. Enter a **Query**: a PromQL expression over your metrics that returns a number or an instant vector, such as `max(rate(platform_container_cpu_seconds_total[5m]))` or `max(platform_container_memory_bytes / platform_container_memory_limit_bytes)`.
4. Choose **Alert when the value is** above, at or above, below, or at or below, and enter the **Threshold**.
5. Set **For**: how many minutes in a row (0 to 120, 5 by default) the value must stay beyond the threshold.
6. Click **Create alarm**. The query is tested first; an invalid query is refused with the reason.

How it works:

- Alarms are checked every minute. When the value stays beyond the threshold for the chosen minutes, an incident opens and your channels are notified (event `metric_alarm`). It resolves on its own when the value comes back.
- The table shows each alarm's condition, its current value and when it was last checked, and its state: **ok**, **pending** (beyond the threshold, waiting for the minutes to pass), **firing**, **no data** or **paused**, with the last error if the query failed.
- Click **pause** to stop checking an alarm (this resolves its open incident) and **resume** to start again. Click **delete** to remove it.
- Alarm names are unique. Plans limit the number of alarms (5 on Trial, 20 on Starter, 100 on Pro by default).

API: `GET /api/v1/alarms`, `POST /api/v1/alarms` with `{"name": "High CPU", "query": "max(rate(platform_container_cpu_seconds_total[5m]))", "op": ">", "threshold": 0.8, "for_minutes": 5}` (`op` is one of `>`, `>=`, `<`, `<=`), `DELETE /api/v1/alarms/NAME` (or its ID).

## API and command-line tool

**What it is for:** everything you do in the console can be automated. Deploy from your CI pipeline, script your setup, or read your data from your own tools, with an API token, through the REST API or the `platformctl` command-line tool.

### API tokens

1. Go to **Access management** › **API tokens** (admins).
2. Enter a name (for example `github-actions`).
3. Pick a role: **viewer** (read only), **developer** (deploy and set secrets) or **admin**.
4. Pick the reach: **whole organization**, or **only project …** to limit the token to one project.
5. Click **Create token**. Copy it right away: it is shown only once. Tokens start with `pla_`.

Details:

- A token works only inside its organization and only with its role. Every change made with a token appears in the audit log, named after the token.
- Tokens expire after one year. The list shows each token's role, when it was last used and when it expires. Click **revoke** to stop a token immediately.
- **Project tokens** can be viewer or developer, never admin. They reach `/api/v1/projects/<that project>/…` and `/api/v1/me` only; any other call answers `403`.
- Other kinds of tokens are made elsewhere for one job only: a queue's own token (sends, receives and deletes that queue's messages, see [Queues](/docs/user#queues)) and an email sending token (sends email and reads the status of sent messages, see [Email](/docs/user#email)).

### Deploy from CI

1. Create a developer token (preferably limited to the project).
2. Set two variables in your CI: `PLATFORM_URL={{console}}` and `PLATFORM_TOKEN=pla_…` (as a secret).
3. Deploy:

```
platformctl deploy shop/web --image {{registry}}/<organization>/web:1.4.2 --wait
```

Or with `curl`:

```
curl -X POST -H "Authorization: Bearer $PLATFORM_TOKEN" \
  -d '{"image": "{{registry}}/<organization>/web:1.4.2"}' \
  "$PLATFORM_URL/api/v1/projects/shop/services/web/deploy"
```

### The platformctl tool

`platformctl` is a single program with no dependencies, easy to add to any CI image. Build it with `go build ./cmd/platformctl` from the platform's source, or ask your operator for a copy. It reads `PLATFORM_URL` and `PLATFORM_TOKEN` from the environment. Run `platformctl help` for the list below.

| Command | What it does |
| --- | --- |
| `platformctl whoami` | Shows the token's organization and role. |
| `platformctl projects` | Lists projects. |
| `platformctl services <project>` | Lists services and the status of their last deployment. |
| `platformctl status <project>/<service>` | Settings, address and recent deployments. |
| `platformctl deploy <project>/<service> [--image IMAGE] [--wait]` | Deploys the current settings, optionally with a new image; `--wait` follows it until live or failed and exits with an error on failure. |
| `platformctl scale <project>/<service> N [--wait]` | Runs N instances (1 to 10) with no downtime. |
| `platformctl logs <project>/<service> [--filter TEXT]` | Prints recent log lines (6 hours, up to 300). |
| `platformctl secret set <project>/<service> NAME` | Sets a secret; the value is read from standard input, never from the command line. |
| `platformctl var list <project>` | Lists the project's shared variables (secret values hidden). |
| `platformctl var set <project> NAME [--plain]` | Sets a shared project variable, secret unless `--plain`; the value is read from standard input. |
| `platformctl var unset <project> NAME` | Deletes a shared project variable. |
| `platformctl redeploy <project>` | Redeploys every running service of the project, for example after changing variables. |
| `platformctl job list <project>/<service>` | Lists the service's scheduled jobs, their last run and next run. |
| `platformctl job run <project>/<service> JOB [--wait]` | Runs a job now; `--wait` prints its output and exits with an error if it fails. |
| `platformctl job logs <project>/<service> JOB` | Prints the output of the job's last run. |
| `platformctl preview create <project>/<service> NAME [--image IMAGE] [--ttl HOURS] [--wait]` | Creates or updates a preview environment (for example `pr-42`). |
| `platformctl preview list <project>/<service>` | Lists the service's previews. |
| `platformctl preview delete <project>/<service> NAME` | Deletes a preview. |
| `platformctl build <project>/<service> [--no-deploy] [--wait]` | Builds the service's git branch, and deploys it unless `--no-deploy`. |
| `platformctl zip <project>/<service> <folder or file.zip> [--no-deploy] [--wait]` | Uploads source code (a folder is zipped without `.git` and `node_modules`), builds it and deploys it. |
| `platformctl fn list <project>` | Lists the project's functions. |
| `platformctl fn deploy <project>/<fn> --file index.mjs` (or `--zip code.zip`, `--runtime`, `--handler`) | Creates or updates a function. |
| `platformctl fn invoke <project>/<fn> [--event JSON] [--async]` | Runs it (`--event -` reads the event from standard input), prints its log on standard error and its result on standard output; exits with an error if the function failed. |
| `platformctl fn logs <project>/<fn>` | Recent invocations with status, duration and log. |
| `platformctl fn delete <project>/<fn>` | Deletes a function. |
| `platformctl queue list` | Lists queues with their available and in-flight messages. |
| `platformctl queue create NAME` | Creates a queue. |
| `platformctl queue purge NAME` | Deletes all the queue's messages. |
| `platformctl queue send NAME BODY` | Sends a message (`-` reads the body from standard input). |
| `platformctl queue receive NAME [--max N] [--wait S]` | Receives messages, printing each receipt and body. |
| `platformctl queue delete NAME RECEIPT` | Deletes a received message. |
| `platformctl bucket list` | Lists buckets with their usage, the S3 endpoint and region. |
| `platformctl bucket create NAME [--quota MB]` | Creates bucket `<organization>-NAME` (1024 MB quota by default). |
| `platformctl bucket delete NAME` | Deletes a bucket and all its objects (admin token). |
| `platformctl key list` | Lists S3 access keys and the buckets they reach. |
| `platformctl key create NAME BUCKET=read ...` | Creates an S3 access key for one or more buckets (`BUCKET=read` or `BUCKET=write`) and prints its secret once. |
| `platformctl key delete ID` | Deletes an S3 access key (by its ID or its access key ID). |
| `platformctl vault list` | Lists the secrets of the [secrets manager](/docs/user#secrets-manager), without values. |
| `platformctl vault create NAME VALUE` | Creates a secret (`-` reads the value from standard input). |
| `platformctl vault get NAME [--version N]` | Prints a secret's value; the read is recorded in the audit log. |
| `platformctl vault put NAME VALUE` | Stores a new version (`-` reads it from standard input); services using the secret redeploy. |
| `platformctl vault delete NAME` | Deletes a secret no service uses. |

Prefer `-` and standard input for secret values, so they do not end up in your shell history.

### How the REST API works

- Base address: `{{console}}/api/v1`. Send the token in every request: `Authorization: Bearer pla_…`.
- Bodies are JSON. Successful calls answer `200`, `201` (created), `202` (accepted, for email) or `204` (deleted, no body).
- Errors answer JSON `{"error": "…"}` with `400` (malformed body), `401` (missing, invalid, expired or revoked token), `403` (the token's role or project cannot do this, or the product is turned off for your organization), `404` (not found), `409` (already exists), `422` (refused, the message says why) or `429` (too many requests).
- Each address may send up to 600 API requests per minute. Repeated failed logins with bad tokens are blocked for a few minutes.
- Answers are never cached (`Cache-Control: no-store`).

In the tables below, `PROJECT`, `SERVICE`, `NAME`… stand for your own IDs, and the role is the minimum role the token needs.

### Endpoint reference: account, projects and services

| Method and path | Role | Body and notes |
| --- | --- | --- |
| `GET /api/v1/me` | viewer | The token's organization, name and role. |
| `GET /api/v1/projects` | viewer | Lists projects. |
| `POST /api/v1/projects` | admin | `{"slug": "shop", "name": "Shop"}` |
| `GET /api/v1/projects/PROJECT/services` | viewer | Services and their last deployment. |
| `POST /api/v1/projects/PROJECT/services` | developer | `{"slug": "web", "image": "nginx:1.27-alpine"}` |
| `GET /api/v1/projects/PROJECT/services/SERVICE` | viewer | Settings, address and recent deployments. |
| `PUT /api/v1/projects/PROJECT/services/SERVICE/settings` | developer | The full settings form (see below). |
| `POST /api/v1/projects/PROJECT/services/SERVICE/deploy` | developer | Optional `{"image": "…"}` |
| `POST /api/v1/projects/PROJECT/services/SERVICE/scale` | developer | `{"instances": 3}` |
| `GET /api/v1/projects/PROJECT/services/SERVICE/deployments/REVISION` | viewer | One deployment and its status. |
| `GET /api/v1/projects/PROJECT/services/SERVICE/logs?q=text` | viewer | Last 6 hours, up to 300 lines. |
| `PUT /api/v1/projects/PROJECT/services/SERVICE/secrets/NAME` | developer | `{"value": "…"}` |
| `GET /api/v1/projects/PROJECT/services/SERVICE/ports` | viewer | Extra public HTTP ports. |
| `POST /api/v1/projects/PROJECT/services/SERVICE/ports` | developer | `{"label": "admin", "port": 8081}` |
| `DELETE /api/v1/projects/PROJECT/services/SERVICE/ports/LABEL` | developer | |
| `POST /api/v1/projects/PROJECT/services/SERVICE/domains` | developer | `{"hostname": "shop.example.com"}` |
| `POST /api/v1/projects/PROJECT/services/SERVICE/default-address` | developer | `{"on": false}` turns the platform address off (a custom domain must be verified), `{"on": true}` turns it back on. |
| `GET /api/v1/projects/PROJECT/services/SERVICE/schedules` | viewer | Scheduled scaling rules. |
| `POST /api/v1/projects/PROJECT/services/SERVICE/schedules` | developer | `{"cron": "0 8 * * 1-5", "instances": 3, "label": "office hours"}` |
| `DELETE /api/v1/projects/PROJECT/services/SERVICE/schedules/ID` | developer | |
| `GET /api/v1/projects/PROJECT/services/SERVICE/analytics?period=24h` | viewer | Traffic and behavior report (`1h`, `24h`, `7d`, `30d`). |

The settings endpoint takes the same fields as the **Settings** tab: `image`, `port`, `health_path`, `cpu_millis`, `memory_mb`, `instances`, `kind` (`web` or `worker`), `env` (one `NAME=value` per line, as a single string with `\n` between lines), `volume_path`, `private`, `edge_cache`, `metrics_path`, `metrics_port`, `ports` (public TCP/UDP ports, one per line), `autoscale`, `autoscale_min`, `autoscale_max`, `autoscale_cpu`, and `deploy` (`true` to deploy right after saving). It replaces the whole form: send every field you want to keep, as fields you leave out are cleared. Read the current values first with `GET …/services/SERVICE`.

### Endpoint reference: deployments, jobs, previews and builds

| Method and path | Role | Body and notes |
| --- | --- | --- |
| `POST /api/v1/projects/PROJECT/redeploy` | developer | Redeploys every running service of the project. |
| `GET /api/v1/projects/PROJECT/services/SERVICE/jobs` | viewer | Scheduled jobs. |
| `PUT /api/v1/projects/PROJECT/services/SERVICE/jobs/JOB` | developer | `{"schedule": "0 3 * * *", "command": "…", "timeout_seconds": 600, "enabled": true}` |
| `DELETE /api/v1/projects/PROJECT/services/SERVICE/jobs/JOB` | developer | |
| `POST /api/v1/projects/PROJECT/services/SERVICE/jobs/JOB/run` | developer | Runs the job now. |
| `GET /api/v1/projects/PROJECT/services/SERVICE/jobs/JOB/runs` | viewer | Recent runs. |
| `GET /api/v1/projects/PROJECT/services/SERVICE/jobs/JOB/runs/ID` | viewer | One run, with its log. |
| `GET /api/v1/projects/PROJECT/services/SERVICE/previews` | viewer | Preview environments. |
| `POST /api/v1/projects/PROJECT/services/SERVICE/previews` | developer | `{"name": "pr-42", "image": "…", "ttl_hours": 72}`; answers `201` when created, `200` when updated. |
| `DELETE /api/v1/projects/PROJECT/services/SERVICE/previews/NAME` | developer | |
| `POST /api/v1/projects/PROJECT/services/SERVICE/builds` | developer | Builds the git branch; optional `{"deploy": false}` to build without deploying. |
| `GET /api/v1/projects/PROJECT/services/SERVICE/builds/NUMBER` | viewer | One build, its status and log. |
| `POST /api/v1/projects/PROJECT/services/SERVICE/zip` | developer | The zip archive as the body (`Content-Type: application/zip`, up to 100 MB); `?deploy=false` only builds; `?filename=` names it in the log. |

### Endpoint reference: project variables and connections

| Method and path | Role | Body and notes |
| --- | --- | --- |
| `GET /api/v1/projects/PROJECT/variables` | viewer | Secret values are omitted. |
| `PUT /api/v1/projects/PROJECT/variables/NAME` | developer | `{"value": "…", "secret": true}` |
| `DELETE /api/v1/projects/PROJECT/variables/NAME` | developer | |
| `GET /api/v1/projects/PROJECT/connections` | viewer | Projects connected on the [private network](/docs/user#private-network-between-projects). |
| `POST /api/v1/projects/PROJECT/connections` | admin | `{"project": "billing"}` |
| `DELETE /api/v1/projects/PROJECT/connections/OTHER` | admin | |

### Endpoint reference: databases

| Method and path | Role | Body and notes |
| --- | --- | --- |
| `POST /api/v1/projects/PROJECT/databases` | developer | `{"slug": "main", "engine": "postgres:17"}` |
| `POST /api/v1/projects/PROJECT/databases/DB/attach` | developer | `{"service": "web", "name": "DATABASE_URL"}` |
| `POST /api/v1/projects/PROJECT/databases/DB/public` | admin | `{"allow": "203.0.113.7"}` to open public access to an address, or `{"close": true}` |

### Endpoint reference: functions

| Method and path | Role | Body and notes |
| --- | --- | --- |
| `GET /api/v1/projects/PROJECT/functions` | viewer | |
| `PUT /api/v1/projects/PROJECT/functions/FN` | developer | Creates or updates: `{"runtime": "nodejs22", "code": "…"}` or `{"zip_base64": "…", "filename": "code.zip"}`, plus any of `handler`, `memory_mb`, `timeout_seconds`, `concurrency`, `env` (an object), `url_enabled`, `url_auth`, `schedule`, `schedule_enabled`, `web_command`, `web_port`. Fields you leave out keep their value. |
| `POST /api/v1/projects/PROJECT/functions/FN/invoke` | developer | The event as the body. `X-Invocation-Type: Event` runs it asynchronously; `X-Log-Type: Tail` returns the log in `X-Platform-Log-Result`. |
| `GET /api/v1/projects/PROJECT/functions/FN/invocations` | viewer | Recent invocations with status, duration and log. |
| `DELETE /api/v1/projects/PROJECT/functions/FN` | developer | |
| `POST /api/v1/projects/PROJECT/functions/FN/triggers` | developer | `{"bucket": "photos", "events": "created", "prefix": "in/", "suffix": ".jpg"}` |
| `POST /api/v1/projects/PROJECT/functions/FN/webhook` | developer | `{}`; answers `{"url": "…"}`, shown once. |

### Endpoint reference: queues

| Method and path | Role | Body and notes |
| --- | --- | --- |
| `GET /api/v1/queues` | viewer | Queues with their message counts. |
| `POST /api/v1/queues` | developer | `{"name": "orders"}`, optionally `visibility_timeout_seconds`, `retention_seconds`, `max_receives`, `dead_letter_queue` |
| `POST /api/v1/queues/QUEUE/messages` | developer or the queue's token | `{"body": "…", "attributes": {"k": "v"}, "delay_seconds": 0}`, or several at once with `{"messages": [ … ]}` |
| `POST /api/v1/queues/QUEUE/receive?max=10&wait=20&visibility=60` | developer or the queue's token | `max` 1 to 10, `wait` 0 to 20 seconds, `visibility` 0 to 43200 seconds. |
| `DELETE /api/v1/queues/QUEUE/messages/RECEIPT` | developer or the queue's token | Deletes a received message. |
| `POST /api/v1/queues/QUEUE/purge` | developer | Deletes every message. |
| `DELETE /api/v1/queues/QUEUE` | admin | |
| `POST /api/v1/queues/QUEUE/attach` | developer | `{"service": "shop/web"}` |

### Endpoint reference: object storage

| Method and path | Role | Body and notes |
| --- | --- | --- |
| `GET /api/v1/buckets` | viewer | Buckets, usage, endpoint and region. |
| `POST /api/v1/buckets` | developer | `{"name": "uploads", "quota_mb": 1024}` |
| `PUT /api/v1/buckets/BUCKET/cors` | developer | `{"origins": ["https://shop.example.com"]}` |
| `DELETE /api/v1/buckets/BUCKET` | admin | Deletes the bucket and all its objects. |
| `GET /api/v1/storage-keys` | viewer | |
| `POST /api/v1/storage-keys` | developer | `{"name": "ci", "buckets": {"uploads": "write"}}`; the answer holds the secret, once. |
| `DELETE /api/v1/storage-keys/ID` | developer | |
| `POST /api/v1/storage/buckets/BUCKET/attach` | developer | `{"service": "shop/web"}` |

### Endpoint reference: secrets manager

| Method and path | Role | Body and notes |
| --- | --- | --- |
| `GET /api/v1/secrets` | viewer | Secrets, without values. |
| `POST /api/v1/secrets` | developer | `{"name": "stripe-key", "description": "…", "value": "…"}` |
| `GET /api/v1/secrets/NAME?version=N` | developer | The value (latest version without `version`); the read is audited. |
| `PUT /api/v1/secrets/NAME` | developer | `{"value": "…"}` stores a new version (optional `description`); answers how many services were redeployed (`services_redeployed`). |
| `DELETE /api/v1/secrets/NAME` | developer | Only when no service uses it. |

### Endpoint reference: DNS hosting

| Method and path | Role | Body and notes |
| --- | --- | --- |
| `GET /api/v1/dns/zones` | viewer | |
| `POST /api/v1/dns/zones` | developer | `{"domain": "example.com"}`; answers the name servers to set at your registrar. |
| `GET /api/v1/dns/zones/ZONE/records` | viewer | |
| `POST /api/v1/dns/zones/ZONE/records` | developer | `{"name": "www", "type": "CNAME", "value": "target.example.com"}`, optionally `priority` and `ttl` |
| `POST /api/v1/dns/zones/ZONE/import` | developer | A zone file as the body (or `{"zone_file": "…"}`); answers `{"added": N, "skipped": [ … ]}` |
| `DELETE /api/v1/dns/zones/ZONE/records/ID` | developer | |

### Endpoint reference: email

| Method and path | Role | Body and notes |
| --- | --- | --- |
| `GET /api/v1/email/domains` | viewer | Sending domains and their DNS status. |
| `POST /api/v1/email/domains` | developer | `{"domain": "example.com"}` |
| `DELETE /api/v1/email/domains/DOMAIN` | developer | |
| `POST /api/v1/email/send` | developer or an email token | `{"from": "shop@example.com", "to": ["ann@example.com"], "subject": "…", "text": "…"}`, optionally `html`, `reply_to`, `attachments` (`[{"filename", "content_type", "content"}]`, content in base64); answers `202 {"id": "…", "status": "queued"}` |
| `GET /api/v1/email/messages/ID` | viewer or an email token | Delivery status of a sent message. |
| `GET /api/v1/email/suppressions` | viewer | Addresses that are no longer sent to. |
| `POST /api/v1/email/suppressions` | developer | `{"address": "ann@example.com"}` |
| `DELETE /api/v1/email/suppressions/ADDRESS` | developer | |
| `GET /api/v1/email/webhook` | viewer | |
| `PUT /api/v1/email/webhook` | developer | `{"url": "https://hooks.example.com/email", "events": ["bounced"]}` |
| `DELETE /api/v1/email/webhook` | developer | |
| `POST /api/v1/email/attach` | developer | `{"service": "shop/web"}` |

### Endpoint reference: mailboxes

| Method and path | Role | Body and notes |
| --- | --- | --- |
| `GET /api/v1/mail/domains` | viewer | |
| `POST /api/v1/mail/domains` | developer | `{"domain": "example.com"}` |
| `GET /api/v1/mail/domains/DOMAIN` | viewer | The domain, its DNS records and status. |
| `POST /api/v1/mail/domains/DOMAIN/activate` | developer | Checks the DNS records and turns the domain on. |
| `DELETE /api/v1/mail/domains/DOMAIN` | developer | |
| `PUT /api/v1/mail/domains/DOMAIN/catch-all` | developer | `{"mailbox": "info"}`; an empty value turns it off. |
| `PUT /api/v1/mail/domains/DOMAIN/server-name` | developer | `{"name": "mail"}`; empty uses the platform's name. |
| `GET /api/v1/mail/domains/DOMAIN/mailboxes` | viewer | |
| `POST /api/v1/mail/domains/DOMAIN/mailboxes` | developer | `{"name": "ann", "display_name": "Ann", "quota_mb": 1024}`; without `password`, one is generated and returned once. |
| `PATCH /api/v1/mail/domains/DOMAIN/mailboxes/MAILBOX` | developer | `{"display_name": "…", "quota_mb": 2048}` |
| `DELETE /api/v1/mail/domains/DOMAIN/mailboxes/MAILBOX` | developer | |
| `POST /api/v1/mail/domains/DOMAIN/mailboxes/MAILBOX/password` | developer | `{"password": "…"}`; empty generates one and returns it. |
| `POST /api/v1/mail/domains/DOMAIN/mailboxes/MAILBOX/aliases` | developer | `{"alias": "sales"}` |
| `DELETE /api/v1/mail/domains/DOMAIN/aliases/ALIAS` | developer | |

### Endpoint reference: monitoring and costs

| Method and path | Role | Body and notes |
| --- | --- | --- |
| `GET or POST /api/v1/prometheus/api/v1/…` | viewer | The Prometheus-compatible API (see [Prometheus and Grafana](/docs/user#prometheus-and-grafana)). |
| `GET /api/v1/alarms` | viewer | Metric alarms with their state. |
| `POST /api/v1/alarms` | developer | `{"name": "…", "query": "…", "op": ">", "threshold": 0.8, "for_minutes": 5}` |
| `DELETE /api/v1/alarms/NAME` | developer | By name or ID. |
| `GET /api/v1/costs?month=YYYY-MM` | admin | Cost by project; this month without `month`. |
| `GET /api/v1/budgets` | admin | |
| `POST /api/v1/budgets` | admin | `{"project": "shop", "limit_cents": 5000, "thresholds": [50, 80, 100], "forecast_alert": true}`; leave `project` empty for the whole organization. |
| `DELETE /api/v1/budgets/ID` | admin | |

### Console actions on the API

Some endpoints above run the console's own form behind the scenes: creating projects, services and databases, saving settings, adding domains, attaching resources, the platform address switch, function triggers and webhooks. They apply exactly the same checks, plan limits and audit as the console. They answer `200 {"message": "…"}` on success, or `422 {"error": "…"}` with the message the console would show when it refuses the form. In their bodies, `true` means a ticked box and `false` an unticked one (use `0` for the `on` switch of the platform address).

## Disk limits

**What it is for:** each plan includes an amount of persistent storage, so one organization cannot fill the servers' disks. It counts service persistent folders and managed databases (with their backup dumps).

- The organization's home page shows **Disk** under **Usage of your plan**: used and allowed, in MB. It is measured every 15 minutes.
- At 90% and 100% of the limit, every alert channel gets a "Disk limit" notification.
- Above 110%, new deployments and new databases are refused ("Disk limit exceeded…") until you delete data or move to a larger plan. Running applications keep running, and rolling back is always allowed.
- On the default plans, Trial includes 2 GB, Starter 10 GB and Pro 50 GB.

To free space, delete old files from persistent folders, drop unused tables or databases, and delete services you no longer need. Object storage buckets have their own quotas (see [Object storage](/docs/user#object-storage)).

## Billing and usage

**What it is for:** the **Billing** page shows what your organization uses and what it costs: this month so far, the cost of each project, budgets that warn you before you overspend, your invoices and the details printed on them. Admins and owners open it from the product menu (**Billing** › **Usage and invoices**) or from **Usage of your plan** on the home page.

### What is billed

- Your plan's monthly fee. The fee of your first month is counted from the day the organization was created.
- The CPU and memory **allocated** to each running service and database, per hour, at the size you chose (not the share the application actually used).
- Network traffic going out of your services, per GB.
- Builds from git, per build minute.
- Object storage, per GB-month of data stored in your buckets (measured every hour).
- Functions, per million requests and per GB-second (memory × run time of each invocation).
- Queues, per million requests.

Stopped or deleted resources cost nothing. The prices of your plan are listed under the current month's table.

### This month so far

The first section shows the estimated amount up to now (with tax if your operator charges it) and one line per billed item, with quantity, unit price and amount. It updates every minute.

**Usage by resource** lists each service, database and bucket with its CPU (vCPU-hours), memory (GB-hours), network out (GB) and storage (GB-month).

### Cost by project

**Cost by project** prices this month's usage per project, before tax, with:

- **so far**: what the project has cost this month;
- **Forecast for the month**: the amount at the end of the month if the rest of the month continues at the same pace;
- **Last month**: what it cost last month.

Object storage, queues, email and the plan fee belong to the whole organization and appear as **Organization-wide**. API (admin tokens): `GET /api/v1/costs`, or `GET /api/v1/costs?month=2026-10` for an earlier month.

### Budgets

**What it is for:** a budget warns you when spending gets close to a monthly limit you set, for the whole organization or for one project.

1. In **Budgets**, choose the scope: **Whole organization** or **Project …**.
2. Enter the monthly limit in your currency (for example `50`).
3. Enter the alert thresholds in % of the limit, separated by commas (`50, 80, 100` by default; up to 5 values, from 1 to 500).
4. Keep **warn on forecast** ticked to be warned as soon as the forecast for the month passes the limit.
5. Click **Save budget**.

How it works:

- Every hour the platform compares the month's spending with each budget. When a threshold is first crossed, or the forecast passes the limit, your [alert channels](/docs/user#notification-channels) are notified (event `budget`).
- The table shows each budget's limit, what was spent (and the %), the forecast, and its thresholds. Amounts over the limit are shown in red.
- A budget only warns: nothing is ever stopped.
- There is one budget per scope. Saving again for the same scope replaces its limit and thresholds and starts its alerts over for the month. Click **Delete** to remove a budget.
- API (admin tokens): `GET /api/v1/budgets`, `POST /api/v1/budgets` with `{"project": "shop", "limit_cents": 5000, "thresholds": [50, 80, 100], "forecast_alert": true}`, `DELETE /api/v1/budgets/ID`.

### Invoices

- Invoices cover one calendar month (UTC) and appear on the 1st of the next month, with a status: **to pay**, **paid** or **cancelled**.
- Click an invoice number to open it, then **Print or save as PDF** to print it or save it from your browser.
- **Paying:** follow the payment instructions you received from the platform operator. An invoice shows **paid** once the operator has recorded your payment.
- If the page says invoicing is not turned on for this installation, usage is still measured but no invoices are issued.

### Billing details

1. In **Billing details**, enter the **Company or person**, the **Address** and the **Tax / VAT ID** to print on your invoices. Leave the name empty to use the organization's name.
2. Click **Save billing details**.

Changes apply to the next invoices, not to ones already issued.

## Single sign-on

**What it is for:** single sign-on (SSO) lets your team sign in with your company's identity provider instead of a separate password: Google Workspace, Microsoft Entra ID, Okta, Keycloak, Authentik or any OpenID Connect provider. People who leave the company lose access as soon as you disable them at the provider, and roles can follow the provider's groups.

Admins set it up in **Access management** › **Single sign-on**.

### Set it up

1. **At your provider**, create an OpenID Connect application of the "web" type. Register the redirect URI shown on the tab (`{{console}}/sso/callback`) and the scopes `openid email profile`. Note the issuer URL, the client ID and the client secret.
2. **On the platform**, enter the **Issuer URL** (for example `https://accounts.google.com` or `https://login.microsoftonline.com/<tenant>/v2.0`), the **Client ID** and the **Client secret**.
3. Enter your **Email domains**, separated by commas (for example `example.com`). People with an email of these domains sign in to this organization.
4. Choose the **Role of new members**: viewer, developer or admin.
5. Optionally, map provider groups to roles: the **Groups claim** (the claim of the ID token that lists groups, `groups` by default), then **Admin groups**, **Developer groups** and **Viewer groups**.
6. Tick **Turn on single sign-on** and click **Save single sign-on**. The platform checks that it can reach your provider when you save.

To change the client secret later, type the new one; leave the box empty to keep the saved one.

### How members sign in

1. On the login page, click **Sign in with SSO** and enter their work email.
2. They sign in at your provider and come back to the console.
3. Their account is created at their first sign-in, with the role of new members (or the role of their groups).

### Single sign-on tips

- **Require it:** tick **Require it: refuse password sign-in for emails of these domains** so that everyone goes through your provider and its own two-step checks. Owners keep their password, so a broken provider cannot lock you out.
- **Groups:** when you fill in group lists, a member's role follows the provider at each sign-in (owners keep theirs).
- A domain belongs to one organization only. An email outside your domains is refused, and so is an email the provider has not verified.
- Sign-in links are single-use and expire after 10 minutes.
- Every SSO sign-in and every role change it makes is recorded in the audit log. The tab's **status** line shows whether SSO is on and required.

## Team members and roles

**What it is for:** invite your colleagues and give each one the access they need, from read-only to full control. Members are managed in **Access management** › **Members**.

### What each role can do

Each member has one role per organization; each role includes everything the roles above it can do.

| Role | Can do |
| --- | --- |
| viewer | See projects, services, deployments, logs, metrics, analytics, scan reports, backups and incidents; search and download logs. |
| developer | Create services, functions, databases, buckets and queues; delete services and functions; change settings, secrets, domains and traffic protection; deploy and roll back; start a backup; set alert thresholds; create metric alarms and Grafana. |
| admin | Create and delete projects; delete databases, buckets and queues; invite and remove members; limit members to projects; create API and registry tokens; manage alert channels, log exports and single sign-on; restore backups; see billing and budgets; read the audit log. |
| owner | Everything, including inviting or removing owners. An organization always keeps at least one owner. |

### Invite someone

1. On **Members**, under **Invite someone** (admins), enter the email address and choose the role.
2. Keep **send it by email** ticked (when the platform sends email) and click **Invite**.
3. The person receives an email with a link to create their account and join. Someone who already has an account signs in with that email to accept.
4. The link is also shown to you once, in case the email does not arrive: send it privately. It works once and expires in 7 days.

Details:

- Pending invitations are listed with their role and expiry date.
- Only owners can invite owners.
- If the platform does not send email, you always get a link to send yourself.

### Remove a member or change a role

- Click **remove** next to a member and confirm (admins). You cannot remove yourself, and the last owner cannot be removed; only owners can remove owners.
- To change someone's role, remove them and invite them again with the new role. With single sign-on group lists, roles follow the provider instead.

### Access to some projects only

**What it is for:** give a contractor or a team access to one or a few projects without showing them the rest of the organization.

1. On **Members**, click **change** in the **Access** column next to a developer or viewer (admins).
2. Choose a role for each project: **no access**, **viewer** or **developer** (never more than their own role).
3. Click **Save access**.

They then:

- see and work only in those projects, with the role chosen for each (a viewer cannot deploy);
- get "not found" for every other project and for organization-wide pages (services list, databases list, billing, access management, storage, queues, secrets, monitoring…);
- see only their projects on the organization's home page and in the in-progress indicator.

The **Access** column shows "whole organization" or the projects and roles. Leaving every project on **no access** gives them back the whole organization with their role. Owners and admins always manage the whole organization. Changes are recorded in the audit log.

API tokens can be limited the same way: see [API tokens](/docs/user#api-tokens).

### Audit log

**What it is for:** know who did what, and when.

Admins open **Access management** › **Audit log**. It lists the last 300 actions of the organization, newest first: when (UTC), who (a person, or a token's name), the action, the target, details and the IP address. Entries are hash-chained, so they cannot be altered without detection.

## Account and security

**What it is for:** your personal settings, the same in every organization you belong to. Open the menu at the top right of the console.

- **Account and security** opens your account page: your name and email, your [passkeys](/docs/user#passkeys) and your recovery codes.
- The **Light**, **Dark** and **Auto** buttons change the console's appearance. **Auto** follows your device's setting. The choice is remembered in this browser for a year.
- **Log out** ends your session in this browser.

### Manage passkeys

1. On the account page, under **Passkeys**, enter a name (for example `Work laptop`) and click **Add a passkey**.
2. Confirm on your device (fingerprint, face, PIN or security key).
3. Next time, choose **Sign in with a passkey** on the login page.

Each passkey shows when it was added and last used, and whether it is synced across your devices. Click **Remove** to delete one. Only a public key is stored on the platform; the private key never leaves your device or password manager. See [Passkeys](/docs/user#passkeys) for more.

### Two-step login and recovery codes

Every account must use two-step login with an authenticator app (time-based, 6-digit codes). It is set up at your first sign-in (see [Getting started](/docs/user#getting-started)).

- **Recovery codes** each replace the 6-digit code once, if you lose your authenticator app. The account page shows how many unused codes you have left.
- Click **Create recovery codes** (or **Create new codes**) to get a fresh set. The old codes stop working. The new codes are shown only once: save them in a password manager or click **Print**.
- After you sign in with a recovery code, set up two-step login again from your new device soon.

## Limits and good practices

### Plan limits

Your plan sets how much you can run. The organization's home page shows your usage of the main limits under **Usage of your plan**. These are the default plans; your platform operator may offer different ones.

| Limit | Trial | Starter | Pro |
| --- | --- | --- | --- |
| Services | 2 | 3 | 10 |
| CPU in total | 500 millicores | 1000 millicores | 4000 millicores |
| Memory in total | 512 MB | 1024 MB | 4096 MB |
| Custom domains | 0 | 3 | 20 |
| Databases | 1 | 1 | 5 |
| Disk | 2 GB | 10 GB | 50 GB |
| Instances per service | 1 | 5 | 5 |
| Log retention | 1 day | 7 days | 30 days |
| Metric alarms | 5 | 20 | 100 |

Limits of other products (buckets, functions, queues, email, DNS zones, mailboxes, public ports, secrets) are explained in their own sections. When an action would go over a limit, it is refused with a message that names the limit.

### Network rules

- **No direct email:** outgoing ports 25, 465 and 587 are blocked; use the platform's [Email](/docs/user#email) product or an email provider's HTTPS API.
- **No access to internal networks:** applications reach the internet, but not the server, other clients or private ranges. Your own services reach each other on the private network.

### Good practices

- **Pin image versions** (`myapp:1.4.2`, not `latest`), so rollbacks are exact and old images are not cleaned up while you may need them.
- **Keep the health check fast** and independent of slow services.
- **Keep data in the persistent folder** or a managed database, never in the container itself.
- **Use secrets** for passwords and keys, never plain environment variables or your image.
- **Add an alert channel**, so incidents reach you even when the console is closed.
- **Use one token per tool**, with the smallest role and project that works, and revoke tokens you no longer use.
- **Set a budget**, so spending never surprises you.

## Troubleshooting

| Message | What to do |
| --- | --- |
| nothing answering on port N | Check the **Port** setting, and that the application listens on `0.0.0.0`, not only `127.0.0.1`. |
| health check returned HTTP 5xx | Point the health path to a lightweight page and read the logs. |
| container stopped (exit code N) | Read the log lines under the status: usually a missing environment variable or secret. |
| killed: out of memory | Raise **Memory**, or reduce the application's memory use. |
| could not pull image | Check the spelling and tag; push private images to your registry first. |
| images from the platform registry must be under … | Use `{{registry}}/<your organization>/…`. |
| blocked by security policy | Rebuild on an updated base image, push and redeploy (see [Security scanning](/docs/user#security-scanning)). |
| CPU limit reached / Memory limit reached | Lower other services' resources, delete unused services, or ask for a bigger plan. |
| Disk limit exceeded | Delete data from persistent folders or databases, or ask for a bigger plan (see [Disk limits](/docs/user#disk-limits)). |
| The TXT record … was not found yet | Check the record name and value, wait a few minutes, verify again. |
| invalid or missing CSRF token | Reload the page and try again. |
| Too many attempts | Wait 15 minutes. |
| API: invalid, expired or revoked API token | Check `PLATFORM_TOKEN`; create a new token if it expired (tokens last one year). |
| API: this token's role (…) cannot do this | Use a token with the role named in the message. |
| API: this token only works for project … | The token is limited to one project; use a token for the whole organization. |
| API: … is turned off for this organization | The platform operator turned that product off for you; ask them. |
| API: too many requests | Slow down: an address may send up to 600 API requests per minute. |
| The provider could not be reached (single sign-on) | Check the issuer URL, and that the provider is reachable from the internet. |
| Your email is not in this organization's domains | Sign in with your work email, or ask an admin to add your domain in **Single sign-on**. |

Still stuck? Send the platform operator the service address and the time of the problem.
