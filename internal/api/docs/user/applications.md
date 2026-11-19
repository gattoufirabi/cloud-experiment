# Applications

Run your application from a container image as a service, usually live within a minute, with HTTPS, health checks, no-downtime updates and one-click rollbacks. A service can be a website, an API, a background worker or a TCP/UDP server.

## Overview

A **service** runs one application from a container image. You can push an image you built yourself or in your CI to any registry, or let the platform build it from git or a zip file (see [Builds from git](/docs/builds)). Each service belongs to a [project](/docs/getting-started#create-a-project) and gets its own address: `https://<service>-<project>-<organization>.{{apps}}`.

There are two types of service:

- **Web service**: answers HTTP at a public HTTPS address. The platform checks its health on the port it listens on.
- **Worker**: runs in the background (a queue consumer, a bot). It has no address, no port and no domains, and is healthy while it keeps running.

How it works:

- Every deployment is a numbered **revision**. The new version starts next to the old one, and traffic switches only after it passes its health check, so visitors see no downtime. A failed deployment never replaces a working one.
- A service has settings (image, type, port, health check, CPU, memory, instances), environment variables and secrets. Settings take effect at the next deployment.
- Every application runs in a sandbox with its own private network, a limit of 256 processes and no special system privileges.
- Resources count against your plan: CPU, memory, instances, public TCP/UDP ports and disk. See [Limits and plans](#limits-and-plans).

Other tabs of the service page are explained in their own topics: **Logs** ([Logs and metrics](/docs/logs-and-metrics)), **Domains** ([Domains](/docs/domains)), **Jobs** ([Scheduled jobs](/docs/scheduled-jobs)), **Security** ([Traffic protection](/docs/traffic-protection), [Security scanning](/docs/security-scanning)), **Backups** ([Backups](/docs/backups)), **Alerts** ([Alerts](/docs/alerts)), **Builds** ([Builds from git](/docs/builds)) and **Previews** ([Preview environments](/docs/previews)).

## Use cases

- **Website or API**: deploy a container as a **Web service** and open its HTTPS address. See [Deploy your first application](#deploy-your-first-application).
- **Background worker**: choose the **Worker** type for a queue consumer or a bot that needs no address. See [Change the settings of a service](#change-the-settings-of-a-service).
- **Internal API**: tick **Private: no public address** so only your other services can call it at `http://<service>.<project>.internal`. See [Make a service private](#make-a-service-private).
- **Only your own domain**: once a custom domain works, turn off the platform address. See [Turn the platform address on or off](#turn-the-platform-address-on-or-off).
- **Admin interface on a second port**: publish another port of the same container on its own HTTPS address. See [Publish more public ports](#publish-more-public-ports).
- **Game server, MQTT broker, voice server**: expose TCP and UDP ports directly to the internet. See [Expose TCP and UDP ports](#expose-tcp-and-udp-ports).
- **Share the load and survive a failure**: run several instances, or let autoscaling add them when the CPU is high. See [Run several instances](#run-several-instances) and [Autoscaling](#set-up-autoscaling).
- **Predictable peaks**: scale up on weekdays at 08:00 and down at 20:00 with scheduled scaling. See [Schedule scaling](#schedule-scaling).
- **Ship and undo safely**: update with a new image tag and roll back to an earlier revision if something is wrong. See [Update, redeploy and roll back](#update-redeploy-and-roll-back).

## How to use it

### Deploy your first application

A service runs your application from a container image and is usually live within a minute.

1. **Create a project** (admins): **Projects** → **Create project**, for example `shop`.
2. **Create the service** (developers): open **Compute** and click **Create service**. The dialog has five steps.
3. **Name:** choose the **Project** and a **Service name** (2 to 20 lowercase letters, digits and dashes), such as `web`.
4. **Source:** the **Container image**, such as `nginx:1.27-alpine` or `ghcr.io/acme/api:1.4`. Leave it empty to build from a git repository later, on the service's **Builds** tab.
5. **Type and network:** **Web service** (public HTTPS address, health checks on the port) or **Worker** (background process, no address); the **Port** your app listens on (nginx: `80`) and the **Health check path**.
6. **Resources:** **CPU** in millicores, **Memory** in MB and **Instances**.
7. **Environment:** variables, one `NAME=value` per line (add secrets on the service page afterwards). Keep **Deploy now** ticked (it needs an image) and click **Create service**.
8. **Follow the deployment** on the service's **Overview** tab: each step appears (pulling, scanning, starting, health check), and the status updates on its own.
9. **Open the address** shown at the top right of the service page.

Shortcut: on a project's **Services** tab, **Add a service** creates one with just an ID and an image; then check its **Settings** tab and deploy.

```
POST /api/v1/projects/PROJECT/services
{"slug": "web", "image": "nginx:1.27-alpine"}

POST /api/v1/projects/PROJECT/services/SERVICE/deploy
```

```
platformctl deploy shop/web --image nginx:1.27-alpine --wait
```

If the deployment fails, the reason and the last log lines appear under its status. A failed deployment never replaces a working one. See [Troubleshooting](#troubleshooting).

### Change the settings of a service

Open the service and its **Settings** tab to say what to run, how to reach it, how big it is and what it receives.

1. Change the values in the **Container** form. See [Settings](#settings) for each field.
2. Click **Save and deploy** to apply them now, or **Save only** to deploy later (with **Deploy current settings** on the **Overview** tab). Settings take effect at the next deployment.

```
PUT /api/v1/projects/PROJECT/services/SERVICE/settings
{"image": "nginx:1.27-alpine", "kind": "web", "port": 80, "cpu_millis": 250, "memory_mb": 256, "instances": 1, "env": "A=1\nB=2", "deploy": true}
```

```
platformctl status shop/web
```

The command shows the current settings and recent deployments.

### Add secrets to a service

Secrets are values your app needs but nobody should read back, such as a database password or an API key. They are encrypted at rest, passed as environment variables and never shown again. For secrets shared by several services, use the [secrets manager](/docs/variables#create-and-use-a-managed-secret).

1. On the service's **Settings** tab, find **Secrets**.
2. Enter a **NAME** in capitals (`DATABASE_URL`) and a value (up to 8192 characters), then click **Save secret**.
3. **delete** removes a secret.
4. Deploy to apply changes.

```
PUT /api/v1/projects/PROJECT/services/SERVICE/secrets/NAME
{"value": "..."}
```

```
platformctl secret set shop/web NAME
```

The value is read from standard input, never from the command line. The **From the project** list below shows the project's shared variables this service also receives (see [Variables and secrets](/docs/variables)).

### Delete a service

1. At the bottom of the **Settings** tab, type the service ID.
2. Click **Delete service**.

The container stops, and the service is deleted with its settings, secrets and domains. Data in the persistent folder is kept by your platform operator until it is cleaned up.

### Make a service private

Make a service private when only your other services should call it, for example an internal API or an admin back end: it is then unreachable from the internet.

1. On the service's **Settings** tab, tick **Private: no public address**.
2. Click **Save and deploy**.

- Its platform address and custom domains stop answering. Your other services in the same project, and in connected projects, still reach it at `http://<service>.<project>.internal` (plain HTTP, port 80; see [Private network](/docs/private-network)).
- The **Domains** tab then shows a **private** badge. Untick the box to publish the service again.
- A path of one of your public domains can still send traffic to it (see [Domains](/docs/domains)).
- Previews of a private service are private too.

### Turn the platform address on or off

Once your own domain works, you can turn off `https://<service>-<project>-<organization>.{{apps}}` so visitors and search engines only use your domain.

1. Add and verify a custom domain on the service's **Domains** tab (see [Domains](/docs/domains)).
2. Under **Platform address**, click **Turn off the platform address** and confirm.
3. To undo it, click **Turn the platform address back on**.

```
POST /api/v1/projects/PROJECT/services/SERVICE/default-address
{"on": false}
```

- It applies within seconds, without a redeploy, and is recorded in the audit log.
- The button only appears once a custom domain is verified. If the service later has no verified domain left, the platform address answers again on its own, so the service is never left without an address.
- Private access at `http://<service>.<project>.internal` is not affected.

### Publish more public ports

Some applications serve a second web interface on another port, for example an app on port 80 and an admin interface on port 9000. You can publish that port on its own HTTPS address.

1. Open the service's **Domains** tab, section **More public ports**.
2. Enter a **label** (a lowercase letter, then up to 19 lowercase letters or digits, such as `admin`) and the **port** (1 to 65535).
3. Click **Publish port**. The address is `https://<label>.<service address>`, for example `https://admin.web-shop-acme.{{apps}}`, with HTTPS.
4. **unpublish** removes it.

```
POST /api/v1/projects/PROJECT/services/SERVICE/ports
{"label": "admin", "port": 9000}
```

### Expose TCP and UDP ports

Services that do not speak HTTP (game servers, MQTT brokers, voice servers, custom protocols) can expose TCP and UDP ports directly to the internet.

1. On the service's **Settings** tab, list the ports your container listens on in **Public TCP/UDP ports**, one per line: `25565/tcp`, `27015/udp`. Choose the type **Worker** if the service has no website; a web service can have public ports too.
2. Click **Save and deploy**. Each port gets a **public port** of the platform (for example `20003`), kept as long as the service lists it.
3. The **Overview** tab shows **Public TCP/UDP endpoints**: where to connect (`<service>-<project>-<organization>.{{apps}}:20003`), the protocol and the container port.

Good to know:

- A service with public ports runs a **single instance**, and a deployment replaces the running container: connections drop for a few seconds (the previous version is started again if the new one fails to start).
- Traffic reaches the container directly, without the proxy: the request limit and web application firewall do not apply, so your server must handle its own authentication and limits.
- Previews never get public ports.

### Run several instances

Run more than one copy of a service to share the load and to keep it up if one copy fails.

1. On the service's **Settings** tab, set **Instances** (1 to 10, up to your plan's limit) and click **Save and deploy**.
2. Or from the command line or API, as below.

```
platformctl scale shop/web 3 --wait
```

```
POST /api/v1/projects/PROJECT/services/SERVICE/scale
{"instances": 3}
```

- Every update starts all new instances, waits until each one passes its health check, then switches traffic at once: there is no downtime.
- The proxy sends requests to the instances in turn and stops sending to one that fails for 20 seconds. If an instance disappears, the platform replaces the whole set with a fresh revision.
- CPU and memory count for each instance: 3 instances of 250 millicores use 750 millicores of your plan and are billed that way.
- Instances do not share memory or local files: keep sessions and uploads in a database, Redis or [object storage](/docs/object-storage).
- A service with a persistent folder or public TCP/UDP ports runs a single instance; previews always run one. Trial organizations have one instance per service.

### Set up autoscaling

Autoscaling adds instances when your service is busy and removes them when it is quiet, so you pay for capacity only when you need it.

1. On the service's **Settings** tab, tick **Autoscaling**.
2. Choose the **Minimum instances**, **Maximum instances** (1 to 10) and a **CPU target** (10 to 95% of the CPU you give each instance; 70% by default).
3. Click **Save and deploy**.

- Every 15 seconds the platform compares the average CPU of the service's live instances (over the last minute) with the target, and computes how many instances are needed to get back to it.
- It **scales out** as soon as the CPU is above the target and the current instances have run for about 35 seconds (long enough to be measured). An instance cannot use more than its CPU limit, so when the instances are at 90% of it or more, the real demand is unknown: the number of instances then at least **doubles** (within the maximum) instead of growing one at a time.
- It **scales in** only after 5 minutes without changes and when the CPU is well below the target, so short spikes do not make it flap.
- Each change is a deployment of the live settings with the new number of instances: the new instances start next to the old ones, traffic moves when they are all healthy (usually 3 to 5 seconds), and the old ones stop once the requests they were serving have finished.
- What to expect: in a test where traffic jumped at once from 10 to 60 requests per second (about 3 times what one instance could serve), the first extra instance started after about 80 seconds, the service reached 4 then 5 instances within the next 2 minutes, and from then on every request was answered normally. Requests that arrived during those first minutes waited or failed.
- If a sudden rush must never queue, also set an overload limit (see [Traffic protection](/docs/traffic-protection)), keep a higher minimum, or schedule scaling before a known peak.
- Each change is a normal deployment with no downtime, recorded in the audit log with the measured CPU.
- Your plan's instance limit, CPU and memory are checked against the maximum, so the maximum must fit in your plan.
- Services with a persistent folder or public TCP/UDP ports run a single instance and cannot autoscale. Previews do not autoscale.

### Schedule scaling

For load you can predict (office hours, a nightly batch, a weekly sale), set the number of instances at given times.

1. On the service's **Settings** tab, find **Scheduled scaling**.
2. Enter a time as a cron expression in UTC (`minute hour day month weekday`), the number of instances, and an optional label.
3. Click **Add schedule**. For example `0 8 * * 1-5` with 4 instances and `0 20 * * 1-5` with 1 instance gives 4 instances during office hours on weekdays.
4. The table shows each schedule with its next time, when it was last applied and any error. **delete** removes one.

```
POST /api/v1/projects/PROJECT/services/SERVICE/schedules
{"cron": "0 8 * * 1-5", "instances": 4, "label": "office hours"}
```

- The platform applies a schedule within a minute of its time, as a normal deployment with no downtime, and records it in the audit log.
- On an autoscaled service, a schedule moves the autoscaling **minimum** instead (raising the maximum if needed); the autoscaler still adds instances above it when the CPU needs them.
- A schedule runs at most every 15 minutes; a service has up to 10. The same rules as above apply: no persistent folder, no public ports.

### Update, redeploy and roll back

Every deployment is a numbered revision, so you can always see what ran when and go back to a version that worked. The service's **Overview** tab lists the revisions (**Deployments**) with their status, image, when and by whom; it updates on its own while a deployment runs.

1. **Update:** change the image tag on the **Settings** tab and click **Save and deploy**.
2. **Redeploy as is:** click **Deploy current settings** (top of the **Deployments** table) to start a new revision with the saved settings, for example to pick up a changed secret or a new image pushed under the same tag.
3. **Roll back:** click **Redeploy** next to an earlier superseded or failed revision and confirm. Its exact settings come back as a new revision; secrets and project variables use their current values.
4. **Follow a deployment:** watch the **Overview** tab, or use the API or `--wait` below.

From CI:

```
platformctl deploy shop/web --image {{registry}}/acme/web:1.4.2 --wait
```

```
POST /api/v1/projects/PROJECT/services/SERVICE/deploy
{"image": "..."}

GET /api/v1/projects/PROJECT/services/SERVICE/deployments/REVISION
```

`--wait` on the CLI follows the deployment for you and exits with an error if it fails.

The **Overview** tab also shows **Resource usage** (CPU and memory over the last hour, about a minute after the first deployment) and **At a glance**: the address, verified domains, vulnerability counts, backup schedule and open incidents.

### Redeploy several services at once

1. In **Compute** → **Services**, tick the services (see [bulk actions](/docs/getting-started#use-bulk-actions)).
2. Choose **Redeploy with current settings** in **Actions** and click **Apply**.

To redeploy every running service of a project, use `platformctl redeploy shop` or `POST /api/v1/projects/PROJECT/redeploy` (see [Variables and secrets](/docs/variables#set-a-project-variable)).

## Technical reference

### Settings

| Setting | Values | Default | Effect |
| --- | --- | --- | --- |
| Container image | Public (`nginx:1.27-alpine`) or private (`{{registry}}/<organization>/<name>:<tag>`) | none | What runs. Pin a version tag rather than `latest`, so rollbacks are exact. |
| Type | **Web service**, **Worker** | Web service | Web service: answers HTTP at a public address. Worker: runs in the background with no address, no port and no domains; healthy while it keeps running. Remove a service's custom domains before making it a worker. |
| Port the app listens on | 1 to 65535 | 8080 | Also passed to the application as the `PORT` environment variable (a `PORT` you set yourself is overridden). Web services only. |
| Health check path | starts with `/`, up to 200 characters (letters, digits and `. _ ~ / -`) | `/` | Any answer below HTTP 500 counts as healthy. The application has 90 seconds to answer after it starts. Web services only. |
| CPU | 50 to 8000 millicores (1000 = one core) | 250 | Counts against your plan. |
| Memory | 32 to 16384 MB | 256 | The application is stopped if it uses more, so leave headroom. |
| Instances | 1 to 10 | 1 | Identical copies, up to your plan's limit (5 on the default paid plans, 1 on Trial). |
| Autoscaling | on or off; minimum, maximum (1 to 10), CPU target (10 to 95%) | off, target 70% | See [Set up autoscaling](#set-up-autoscaling). |
| Private: no public address | on or off | off | See [Make a service private](#make-a-service-private). |
| Edge cache (CDN) | on or off | off | The proxy keeps responses your app marks as cacheable. See [Edge cache](/docs/cdn). |
| Metrics path and port | path such as `/metrics`; port | none; port defaults to the service's port | Your app's Prometheus endpoint, scraped every 30 seconds. See [Logs and metrics](/docs/logs-and-metrics). |
| Public TCP/UDP ports | one `port/tcp` or `port/udp` per line, up to 5 per service | none | For non-HTTP traffic. See [Expose TCP and UDP ports](#expose-tcp-and-udp-ports). |
| Persistent folder | a folder such as `/data` | none | Kept across deployments and backed up. System folders are refused. Everything else in the container is reset at each deployment. A service with a persistent folder runs a single instance. |
| Environment variables | one `KEY=value` per line, up to 100; names of up to 64 letters, digits and underscores; values up to 4096 characters | none | Names starting with `PLATFORM_` are reserved. Not for passwords: use secrets. A value can be a reference to the secrets manager, `${secret:NAME}`. |

### Limits and plans

| Item | Limit | Notes |
| --- | --- | --- |
| Service name | 2 to 20 lowercase letters, digits and dashes | |
| Instances per service | 1 to 10, up to the plan | 5 on the default paid plans, 1 on Trial. |
| Public TCP/UDP ports (all services and public database endpoints together) | Trial none, Starter 2, Pro 10 | Defaults. Previews never get public ports. |
| More public ports per service | 5 | Public web services only (not workers, private services or previews). |
| Scheduled scaling rules per service | 10 | A schedule runs at most every 15 minutes. |
| Public TCP/UDP ports per service | 5 | Also counted against the plan's public ports. |
| Environment variables per service | 100 | Name up to 64 characters, value up to 4096 characters. |
| Service secret | name of up to 64 capitals, digits and underscores; value 1 to 8192 characters | |
| Processes per application | 256 | No special system privileges. |

### Deployment statuses

| Status | Meaning |
| --- | --- |
| pending | Waiting to start. |
| deploying | Pulling, scanning, starting or waiting for the health check. |
| live | Serving traffic. Only one revision per service is live. |
| superseded | A newer revision replaced it. It can be redeployed. |
| failed | It did not start or did not pass its health check. The previous live revision keeps serving. |

### API

| Method | Path | Role | Purpose |
| --- | --- | --- | --- |
| GET | `/api/v1/projects/PROJECT/services` | viewer | List the services of a project (`id`, `image`, `status`). |
| POST | `/api/v1/projects/PROJECT/services` | developer | Create a service. |
| GET | `/api/v1/projects/PROJECT/services/SERVICE` | viewer | Show a service: settings, `url`, `default_address` and the 10 latest `deployments`. |
| PUT | `/api/v1/projects/PROJECT/services/SERVICE/settings` | developer | Save settings. |
| POST | `/api/v1/projects/PROJECT/services/SERVICE/deploy` | developer | Deploy the current settings, or a new image. |
| GET | `/api/v1/projects/PROJECT/services/SERVICE/deployments/REVISION` | viewer | Status of a revision. |
| POST | `/api/v1/projects/PROJECT/services/SERVICE/scale` | developer | Set the number of instances. |
| PUT | `/api/v1/projects/PROJECT/services/SERVICE/secrets/NAME` | developer | Set a service secret. |
| POST | `/api/v1/projects/PROJECT/services/SERVICE/default-address` | developer | Turn the platform address on or off. |
| GET | `/api/v1/projects/PROJECT/services/SERVICE/ports` | viewer | List the more public ports (`label`, `port`, `url`). |
| POST | `/api/v1/projects/PROJECT/services/SERVICE/ports` | developer | Publish a port. |
| DELETE | `/api/v1/projects/PROJECT/services/SERVICE/ports/LABEL` | developer | Unpublish a port. |
| GET | `/api/v1/projects/PROJECT/services/SERVICE/schedules` | viewer | List scheduled scaling rules (`id`, `cron`, `instances`, `label`, `last_run`, `last_error`). |
| POST | `/api/v1/projects/PROJECT/services/SERVICE/schedules` | developer | Add a scheduled scaling rule. |
| DELETE | `/api/v1/projects/PROJECT/services/SERVICE/schedules/ID` | developer | Delete a scheduled scaling rule. |
| POST | `/api/v1/projects/PROJECT/redeploy` | developer | Redeploy every running service of the project. |

Create a service (`POST …/services`):

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `slug` | string | yes | Service ID: 2 to 20 lowercase letters, digits and dashes; an ID already used in the project returns 422. |
| `image` | string | no | Container image. The service is created with the default settings (port 8080, health path `/`, 250 millicores, 256 MB, 1 instance). |

The other fields of the console dialog (type, port, resources, variables) are not read by this call: set them with the settings call below.

Save settings (`PUT …/settings`). The call replaces all the settings at once: a field you leave out is reset (variables emptied, autoscaling off, **Private** off, no public ports), so always send every setting you want to keep.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `image` | string | yes | Container image. |
| `kind` | string | no | Service type: `worker` for a worker, otherwise a web service. |
| `port` | number | yes for a web service | Port the app listens on (1 to 65535). |
| `health_path` | string | yes for a web service | Health check path, starting with `/`. |
| `cpu_millis` | number | yes | CPU in millicores (50 to 8000). |
| `memory_mb` | number | yes | Memory in MB (32 to 16384). |
| `instances` | number | no | Number of instances (1 to 10); 1 when omitted. |
| `autoscale`, `autoscale_min`, `autoscale_max`, `autoscale_cpu` | bool, number | no | Autoscaling switch, bounds and CPU target. |
| `private` | bool | no | No public address. |
| `edge_cache` | bool | no | Edge cache. |
| `metrics_path`, `metrics_port` | string, number | no | Prometheus endpoint. |
| `ports` | string | no | Public TCP/UDP ports. |
| `volume_path` | string | no | Persistent folder. |
| `env` | string | no | Environment variables as `"A=1\nB=2"`. |
| `deploy` | bool | no | Deploy at once; otherwise the settings apply at the next deployment. |

Other bodies:

| Call | Body |
| --- | --- |
| `POST …/deploy` | `{"image": "…"}`, optional: without a body the current settings are deployed; with an image it is saved as the service's image. |
| `POST …/scale` | `{"instances": 3}`, required (1 to 10, within the plan). |
| `PUT …/secrets/NAME` | `{"value": "…"}`, required (1 to 8192 characters). `NAME` uses capitals, digits and underscores, up to 64, and does not start with `PLATFORM_`. |
| `POST …/default-address` | `{"on": false}`. Without `on` the address is turned off. Only for web services, and turning it off needs a verified custom domain. |
| `POST …/ports` | `{"label": "admin", "port": 9000}`, both required. Label: a lowercase letter then up to 19 lowercase letters or digits, not starting with `pr`. The port must differ from the main port. At most 5 per service. |
| `POST …/schedules` | `{"cron": "0 8 * * 1-5", "instances": 4, "label": "office hours"}`: `cron` and `instances` (1 to 10) required, `label` optional (up to 60 characters). |

A deployment or scale call answers `202` with `revision`, `status` (`pending`) and `status_url`, the path `…/deployments/REVISION` (also in the `Location` header). `GET …/deployments/REVISION` returns `revision`, `status`, `message`, `image`, `by` and `created_at`. A project redeploy answers `202` with `{"redeploying": N}`. Adding a port or a schedule answers `201`, deleting one `204`. The console actions (create, settings, default-address) answer `{"message": "…"}`, or `422` with `{"error": "…"}` when refused.

### Command-line tool

| Command | Purpose |
| --- | --- |
| `platformctl services PROJECT` | List the services of a project. |
| `platformctl status PROJECT/SERVICE` | Settings and recent deployments. |
| `platformctl deploy PROJECT/SERVICE [--image IMAGE] [--wait]` | Deploy the current settings, optionally with a new image; `--wait` waits until live or failed. |
| `platformctl scale PROJECT/SERVICE N [--wait]` | Run N instances (1 to 10) behind the proxy, with no downtime. |
| `platformctl secret set PROJECT/SERVICE NAME` | Set a secret; the value is read from standard input. |
| `platformctl redeploy PROJECT` | Redeploy every running service of the project. |
| `platformctl logs PROJECT/SERVICE [--filter TEXT]` | Last log lines (6 hours, up to 300). |

### Environment variables

| Variable | Value |
| --- | --- |
| `PORT` | The service's port, set by the platform on web services (not on workers). |
| `PLATFORM_*` | Reserved names; you cannot set them as variables or secrets. |

### Behind the scenes

- Each update starts the new revision next to the old one, scans the image, waits for the health check, then switches traffic.
- The proxy skips an instance that fails for 20 seconds, also when it answers 502, 503 or 504.
- Autoscaling runs every 15 seconds on the average CPU of the live instances over the last minute; it waits about 35 seconds after a deployment goes live before measuring it, doubles the instances when they are at their CPU limit, and waits 5 minutes without changes before scaling in. Only the live deployment's containers are measured, so the old containers of a switch do not count.
- During every deployment, the old instances stop only when the requests in progress on them have finished (at most 15 seconds).
- Scheduled scaling is applied within a minute of its time.
- On an installation with several servers, a service with several instances stays up when one instance stops answering: the proxy retries the request on another instance. There is nothing to set up.
- When a whole server stops reporting for a few minutes (3 by default), the platform stops placing work on it and redeploys its projects without data on the other servers. Projects with a database or a persistent folder wait for the operator, who brings the server back or restores them from backups (see [admin guide](/docs/admin)). Running several instances is the best way to ride out a server failure.

## Troubleshooting

### nothing answering on port N

Check the **Port** setting, and that the application listens on `0.0.0.0`, not only `127.0.0.1`.

### health check returned HTTP 5xx

Point the health path to a lightweight page and read the logs.

### container stopped (exit code N)

Read the log lines under the status: usually a missing environment variable or secret.

### killed: out of memory

Raise **Memory**, or reduce the application's memory use.

### could not pull image

Check the spelling and tag; push private images to your [registry](/docs/registry) first.

### images from the platform registry must be under …

Use `{{registry}}/<your organization>/…`.

### blocked by security policy

Rebuild on an updated base image, push and redeploy (see [Security scanning](/docs/security-scanning)).

### CPU limit reached or Memory limit reached

Lower other services' resources, delete unused services, or ask for a bigger plan.

### A deployment failed but the site still works

That is by design: a failed deployment never replaces a working one. Read the reason and the last log lines under its status.

### Persistent disk full

If you see "Disk limit exceeded", delete data from persistent folders or databases, or ask for a bigger plan (see [Billing](/docs/billing)).
