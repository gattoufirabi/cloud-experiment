# API and command-line tool

Everything you do in the console can be automated with an API token, through the REST API or the `platformctl` command-line tool. Deploy from your CI pipeline, script your setup, or read your data from your own tools.

## Overview

There are two ways to automate the platform, and both use the same API token:

- **The REST API** at `{{console}}/api/v1`: JSON over HTTPS, one bearer token per request.
- **`platformctl`**: a single program with no dependencies that wraps the API. It is easy to add to any CI image.

A token belongs to one organization and has a role: **viewer** (read only), **developer** (deploy and set secrets) or **admin**. It can reach the whole organization or be limited to one project. Every change made with a token appears in the audit log, under the member who created the token, with the token's name in the details.

The API covers the main areas of the console: projects and services, deployments, jobs, previews, builds, variables, private network connections, databases, functions, queues, object storage, secrets, DNS, email, mailboxes, monitoring and costs. The [Technical reference](#technical-reference) lists every route and every command, grouped by area, with a link to the topic that explains the area.

## Use cases

- **Deploy from CI**: run `platformctl deploy shop/web --image ... --wait` after your pipeline pushes an image. See [Deploy from CI](#deploy-from-ci).
- **Preview every pull request**: create a preview named `pr-42` when a pull request opens and delete it when it closes (see [Preview environments](/docs/previews)).
- **Upload source code from a script**: use `platformctl zip` to build and deploy a folder without git (see [Builds](/docs/builds)).
- **Rotate a secret**: set a variable or a [secrets manager](/docs/variables) secret from a script, reading the value from standard input.
- **Read your data in your own tools**: list projects, services, costs or analytics as JSON.
- **Create a restricted token**: give a contractor a project token that can only deploy one project.
- **Monitor with Grafana**: use a viewer token against the Prometheus-compatible API (see [Logs and metrics](/docs/logs-and-metrics)).
- **Send transactional email**: send from your own code with an email sending token (see [Email](/docs/email)).

## How to use it

### Create an API token

1. Go to **Access management** › **API tokens** (admins).
2. Enter a name (for example `github-actions`).
3. Pick a role: **viewer** (read only), **developer** (deploy and set secrets) or **admin**.
4. Pick the reach: **whole organization**, or **only project …** to limit the token to one project.
5. Click **Create token**. Copy it right away: it is shown only once. Tokens start with `pla_`.

The list shows each token's role, when it was last used and when it expires.

### Revoke a token

1. Go to **Access management** › **API tokens** (admins).
2. Click **revoke** next to the token. It stops working immediately.

### Install platformctl

`platformctl` is a single program with no dependencies.

1. Build it with `go build ./cmd/platformctl` from the platform's source, or ask your platform operator for a copy (see the [admin guide](/docs/admin)).
2. Set two environment variables: `PLATFORM_URL={{console}}` and `PLATFORM_TOKEN=pla_…`.
3. Check that it works:

```
platformctl whoami
```

Run `platformctl help` for the list of commands.

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

### Call the API from your own code

1. Send the token in every request: `Authorization: Bearer pla_…`.
2. Send JSON bodies.
3. Read the status code and, on errors, the `{"error": "…"}` message.

```
curl -H "Authorization: Bearer $PLATFORM_TOKEN" "{{console}}/api/v1/projects"
```

### Keep secrets out of your shell history

1. Give secret values to `platformctl` on standard input, or pass `-` where the command allows it.
2. For example: `echo -n "s3cr3t" | platformctl secret set shop/web API_KEY`.

## Technical reference

### API tokens

| Kind | Reach | Notes |
| --- | --- | --- |
| Organization token | Whole organization | Role viewer, developer or admin. |
| Project token | One project | Viewer or developer, never admin. Reaches `/api/v1/projects/<that project>/…` and `/api/v1/me` only; any other call answers `403`. |
| Queue token | One queue | Sends, receives and deletes that queue's messages. See [Queues](/docs/queues). |
| Email sending token | Email sending | Sends email and reads the status of sent messages. See [Email](/docs/email). |

- A token works only inside its organization and only with its role.
- Tokens start with `pla_` and expire after one year.
- Queue and email tokens are made elsewhere for one job only.

### How the REST API works

| Item | Value |
| --- | --- |
| Base address | `{{console}}/api/v1` |
| Authentication | `Authorization: Bearer pla_…` on every request |
| Bodies | JSON |
| Success | `200`, `201` (created), `202` (accepted, for email), `204` (deleted, no body) |
| Rate limit | Up to 600 API requests per minute per address |
| Failed logins | Repeated bad tokens are blocked for a few minutes |
| Caching | Never cached (`Cache-Control: no-store`) |

Errors answer JSON `{"error": "…"}`:

| Status | Meaning |
| --- | --- |
| `400` | Malformed body. |
| `401` | Missing, invalid, expired or revoked token. |
| `403` | The token's role or project cannot do this, or the product is turned off for your organization. |
| `404` | Not found. |
| `409` | Already exists. |
| `422` | Refused; the message says why. |
| `429` | Too many requests. |

In the tables below, `PROJECT`, `SERVICE`, `NAME`… stand for your own IDs, and the role is the minimum role the token needs.

### API: account, projects and services

These routes are explained in [Applications](/docs/applications), with domains in [Domains](/docs/domains) and analytics in [Analytics](/docs/analytics).

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
| `GET /api/v1/projects/PROJECT/services/SERVICE/logs?q=text` | viewer | Last 6 hours, up to 300 lines. See [Logs and metrics](/docs/logs-and-metrics). |
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

The settings endpoint takes the same fields as the **Settings** tab:

| Field | Description |
| --- | --- |
| `image` | The container image. |
| `port` | The port the application listens on. |
| `health_path` | Health check path. |
| `cpu_millis`, `memory_mb` | CPU and memory limits. |
| `instances` | Number of instances. |
| `kind` | `web` or `worker`. |
| `env` | One `NAME=value` per line, as a single string with `\n` between lines. |
| `volume_path` | Persistent folder path. |
| `private`, `edge_cache` | Private service and edge cache switches. |
| `metrics_path`, `metrics_port` | Where your application serves Prometheus metrics. |
| `ports` | Public TCP/UDP ports, one per line. |
| `autoscale`, `autoscale_min`, `autoscale_max`, `autoscale_cpu` | Autoscaling. |
| `deploy` | `true` to deploy right after saving. |

It replaces the whole form: send every field you want to keep, as fields you leave out are cleared. Read the current values first with `GET …/services/SERVICE`.

### API: deployments, jobs, previews and builds

These routes are explained in [Scheduled jobs](/docs/scheduled-jobs), [Preview environments](/docs/previews) and [Builds](/docs/builds).

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

### API: project variables and connections

Variables are explained in [Variables and secrets](/docs/variables) and connections in [Private network](/docs/private-network).

| Method and path | Role | Body and notes |
| --- | --- | --- |
| `GET /api/v1/projects/PROJECT/variables` | viewer | Secret values are omitted. |
| `PUT /api/v1/projects/PROJECT/variables/NAME` | developer | `{"value": "…", "secret": true}` |
| `DELETE /api/v1/projects/PROJECT/variables/NAME` | developer | |
| `GET /api/v1/projects/PROJECT/connections` | viewer | Projects connected on the private network. |
| `POST /api/v1/projects/PROJECT/connections` | admin | `{"project": "billing"}` |
| `DELETE /api/v1/projects/PROJECT/connections/OTHER` | admin | |

### API: databases

See [Databases](/docs/databases).

| Method and path | Role | Body and notes |
| --- | --- | --- |
| `POST /api/v1/projects/PROJECT/databases` | developer | `{"slug": "main", "engine": "postgres:17"}` |
| `POST /api/v1/projects/PROJECT/databases/DB/attach` | developer | `{"service": "web", "name": "DATABASE_URL"}` |
| `POST /api/v1/projects/PROJECT/databases/DB/public` | admin | `{"allow": "203.0.113.7"}` to open public access to an address, or `{"close": true}` |

### API: functions

See [Functions](/docs/functions).

| Method and path | Role | Body and notes |
| --- | --- | --- |
| `GET /api/v1/projects/PROJECT/functions` | viewer | |
| `PUT /api/v1/projects/PROJECT/functions/FN` | developer | Creates or updates: `{"runtime": "nodejs22", "code": "…"}` or `{"zip_base64": "…", "filename": "code.zip"}`, plus any of `handler`, `memory_mb`, `timeout_seconds`, `concurrency`, `env` (an object), `url_enabled`, `url_auth`, `schedule`, `schedule_enabled`, `web_command`, `web_port`. Fields you leave out keep their value. |
| `POST /api/v1/projects/PROJECT/functions/FN/invoke` | developer | The event as the body. `X-Invocation-Type: Event` runs it asynchronously; `X-Log-Type: Tail` returns the log in `X-Platform-Log-Result`. |
| `GET /api/v1/projects/PROJECT/functions/FN/invocations` | viewer | Recent invocations with status, duration and log. |
| `DELETE /api/v1/projects/PROJECT/functions/FN` | developer | |
| `POST /api/v1/projects/PROJECT/functions/FN/triggers` | developer | `{"bucket": "photos", "events": "created", "prefix": "in/", "suffix": ".jpg"}` |
| `POST /api/v1/projects/PROJECT/functions/FN/webhook` | developer | `{}`; answers `{"url": "…"}`, shown once. |

### API: queues

See [Queues](/docs/queues).

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

### API: object storage

See [Object storage](/docs/object-storage).

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

### API: secrets manager

The secrets manager is explained in [Variables and secrets](/docs/variables).

| Method and path | Role | Body and notes |
| --- | --- | --- |
| `GET /api/v1/secrets` | viewer | Secrets, without values. |
| `POST /api/v1/secrets` | developer | `{"name": "stripe-key", "description": "…", "value": "…"}` |
| `GET /api/v1/secrets/NAME?version=N` | developer | The value (latest version without `version`); the read is audited. |
| `PUT /api/v1/secrets/NAME` | developer | `{"value": "…"}` stores a new version (optional `description`); answers how many services were redeployed (`services_redeployed`). |
| `DELETE /api/v1/secrets/NAME` | developer | Only when no service uses it. |

### API: DNS hosting

See [DNS hosting](/docs/dns).

| Method and path | Role | Body and notes |
| --- | --- | --- |
| `GET /api/v1/dns/zones` | viewer | |
| `POST /api/v1/dns/zones` | developer | `{"domain": "example.com"}`; answers the name servers to set at your registrar. |
| `GET /api/v1/dns/zones/ZONE/records` | viewer | |
| `POST /api/v1/dns/zones/ZONE/records` | developer | `{"name": "www", "type": "CNAME", "value": "target.example.com"}`, optionally `priority` and `ttl` |
| `POST /api/v1/dns/zones/ZONE/import` | developer | A zone file as the body (or `{"zone_file": "…"}`); answers `{"added": N, "skipped": [ … ]}` |
| `DELETE /api/v1/dns/zones/ZONE/records/ID` | developer | |

### API: email

See [Email](/docs/email).

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

### API: mailboxes

See [Mailboxes](/docs/mailboxes).

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

### API: monitoring and costs

Monitoring is explained in [Logs and metrics](/docs/logs-and-metrics) and [Alerts](/docs/alerts). Costs and budgets are explained in [Billing](/docs/billing).

| Method and path | Role | Body and notes |
| --- | --- | --- |
| `GET or POST /api/v1/prometheus/api/v1/…` | viewer | The Prometheus-compatible API. |
| `GET /api/v1/alarms` | viewer | Metric alarms with their state. |
| `POST /api/v1/alarms` | developer | `{"name": "…", "query": "…", "op": ">", "threshold": 0.8, "for_minutes": 5}` |
| `DELETE /api/v1/alarms/NAME` | developer | By name or ID. |
| `GET /api/v1/costs?month=YYYY-MM` | admin | Cost by project; this month without `month`. |
| `GET /api/v1/budgets` | admin | |
| `POST /api/v1/budgets` | admin | `{"project": "shop", "limit_cents": 5000, "thresholds": [50, 80, 100], "forecast_alert": true}`; leave `project` empty for the whole organization. |
| `DELETE /api/v1/budgets/ID` | admin | |

### Console actions on the API

Some endpoints above run the console's own form behind the scenes: creating projects, services and databases, saving settings, adding domains, attaching resources (databases, buckets, queues, email), the platform address switch, function triggers and webhooks. They apply exactly the same checks, plan limits and audit as the console. They answer `200 {"message": "…"}` on success, or `422 {"error": "…"}` with the message the console would show when it refuses the form. In their bodies, `true` means a ticked box and `false` an unticked one (use `0` for the `on` switch of the platform address).

### Command-line tool

`platformctl` reads `PLATFORM_URL` and `PLATFORM_TOKEN` from the environment. Run `platformctl help` for the list.

| Variable | Value |
| --- | --- |
| `PLATFORM_URL` | `{{console}}` |
| `PLATFORM_TOKEN` | An API token starting with `pla_`. |

#### Commands: account, services and deployments

See [Applications](/docs/applications).

| Command | What it does |
| --- | --- |
| `platformctl whoami` | Shows the token's organization and role. |
| `platformctl projects` | Lists projects. |
| `platformctl services <project>` | Lists services and the status of their last deployment. |
| `platformctl status <project>/<service>` | Settings, address and recent deployments. |
| `platformctl deploy <project>/<service> [--image IMAGE] [--wait]` | Deploys the current settings, optionally with a new image; `--wait` follows it until live or failed and exits with an error on failure. |
| `platformctl scale <project>/<service> N [--wait]` | Runs N instances (1 to 10) with no downtime. |
| `platformctl redeploy <project>` | Redeploys every running service of the project, for example after changing variables. |
| `platformctl logs <project>/<service> [--filter TEXT]` | Prints recent log lines (6 hours, up to 300). See [Logs and metrics](/docs/logs-and-metrics). |

#### Commands: variables and secrets

See [Variables and secrets](/docs/variables).

| Command | What it does |
| --- | --- |
| `platformctl secret set <project>/<service> NAME` | Sets a secret; the value is read from standard input, never from the command line. |
| `platformctl var list <project>` | Lists the project's shared variables (secret values hidden). |
| `platformctl var set <project> NAME [--plain]` | Sets a shared project variable, secret unless `--plain`; the value is read from standard input. |
| `platformctl var unset <project> NAME` | Deletes a shared project variable. |
| `platformctl vault list` | Lists the secrets of the secrets manager, without values. |
| `platformctl vault create NAME VALUE` | Creates a secret (`-` reads the value from standard input). |
| `platformctl vault get NAME [--version N]` | Prints a secret's value; the read is recorded in the audit log. |
| `platformctl vault put NAME VALUE` | Stores a new version (`-` reads it from standard input); services using the secret redeploy. |
| `platformctl vault delete NAME` | Deletes a secret no service uses. |

#### Commands: jobs, previews and builds

See [Scheduled jobs](/docs/scheduled-jobs), [Preview environments](/docs/previews) and [Builds](/docs/builds).

| Command | What it does |
| --- | --- |
| `platformctl job list <project>/<service>` | Lists the service's scheduled jobs, their last run and next run. |
| `platformctl job run <project>/<service> JOB [--wait]` | Runs a job now; `--wait` prints its output and exits with an error if it fails. |
| `platformctl job logs <project>/<service> JOB` | Prints the output of the job's last run. |
| `platformctl preview create <project>/<service> NAME [--image IMAGE] [--ttl HOURS] [--wait]` | Creates or updates a preview environment (for example `pr-42`). |
| `platformctl preview list <project>/<service>` | Lists the service's previews. |
| `platformctl preview delete <project>/<service> NAME` | Deletes a preview. |
| `platformctl build <project>/<service> [--no-deploy] [--wait]` | Builds the service's git branch, and deploys it unless `--no-deploy`. |
| `platformctl zip <project>/<service> <folder or file.zip> [--no-deploy] [--wait]` | Uploads source code (a folder is zipped without `.git` and `node_modules`), builds it and deploys it. |

#### Commands: functions

See [Functions](/docs/functions).

| Command | What it does |
| --- | --- |
| `platformctl fn list <project>` | Lists the project's functions. |
| `platformctl fn deploy <project>/<fn> --file index.mjs` (or `--zip code.zip`, `--runtime`, `--handler`) | Creates or updates a function. |
| `platformctl fn invoke <project>/<fn> [--event JSON] [--async]` | Runs it (`--event -` reads the event from standard input), prints its log on standard error and its result on standard output; exits with an error if the function failed. |
| `platformctl fn logs <project>/<fn>` | Recent invocations with status, duration and log. |
| `platformctl fn delete <project>/<fn>` | Deletes a function. |

#### Commands: queues

See [Queues](/docs/queues).

| Command | What it does |
| --- | --- |
| `platformctl queue list` | Lists queues with their available and in-flight messages. |
| `platformctl queue create NAME` | Creates a queue. |
| `platformctl queue purge NAME` | Deletes all the queue's messages. |
| `platformctl queue send NAME BODY` | Sends a message (`-` reads the body from standard input). |
| `platformctl queue receive NAME [--max N] [--wait S]` | Receives messages, printing each receipt and body. |
| `platformctl queue delete NAME RECEIPT` | Deletes a received message. |

#### Commands: object storage

See [Object storage](/docs/object-storage).

| Command | What it does |
| --- | --- |
| `platformctl bucket list` | Lists buckets with their usage, the S3 endpoint and region. |
| `platformctl bucket create NAME [--quota MB]` | Creates bucket `<organization>-NAME` (1024 MB quota by default). |
| `platformctl bucket delete NAME` | Deletes a bucket and all its objects (admin token). |
| `platformctl key list` | Lists S3 access keys and the buckets they reach. |
| `platformctl key create NAME BUCKET=read ...` | Creates an S3 access key for one or more buckets (`BUCKET=read` or `BUCKET=write`) and prints its secret once. |
| `platformctl key delete ID` | Deletes an S3 access key (by its ID or its access key ID). |

### Behind the scenes

- Project tokens are checked on every request: any path outside `/api/v1/projects/<that project>/…` and `/api/v1/me` answers `403`.
- Queue tokens only reach that queue's message routes; email sending tokens only reach `POST /api/v1/email/send` and `GET /api/v1/email/messages/ID`.
- Every change made with a token is recorded in the audit log under the member who created it, with the token's name in the details.

## Troubleshooting

### 401 on every call

The token is missing, malformed, expired (tokens last one year) or revoked. Create a new one and check that you send `Authorization: Bearer pla_…`.

### 403 with a project token

Project tokens only reach `/api/v1/projects/<that project>/…` and `/api/v1/me`. Use an organization token for other routes. A `403` can also mean the product is turned off for your organization.

### 429 too many requests

Each address may send up to 600 API requests per minute. Slow down or batch your calls.

### Settings fields disappeared after a PUT

The settings endpoint replaces the whole form. Read the current values with `GET …/services/SERVICE`, change what you need and send everything back.
