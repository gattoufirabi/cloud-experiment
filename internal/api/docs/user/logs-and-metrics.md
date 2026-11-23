# Logs and metrics

Everything your application writes to standard output and standard error is collected, and CPU and memory are measured for every instance. You can read, search, download and export your logs, and query your metrics from Grafana or any Prometheus client.

## Overview

The platform collects two kinds of data for every service of your organization:

- **Logs**: each line your application writes to standard output or standard error. Only these two streams are collected. Files are lost at each deployment unless they are in the persistent folder, so write logs to standard output rather than to files.
- **Metrics**: CPU, memory and network use of every instance, plus any metrics your own application serves in Prometheus format.

You can look at them in three places: a service's **Logs** tab (one service), **Monitoring** › **Logs** (every service at once, with download and export), and a Prometheus-compatible API (for Grafana, scripts and alarms). A managed Grafana can be created for you in one click.

How long logs are kept depends on your plan: on the default plans, Trial keeps logs 1 day, Starter 7 days and Pro 30 days. Metrics are kept 30 days. Your logs and metrics are stored separately from other clients' and are only readable through your organization.

To be notified when a metric crosses a threshold, see [Alerts](/docs/alerts#create-a-metric-alarm).

## Use cases

- **Follow a deployment**: open the service's **Logs** tab and watch new lines arrive while the new version starts.
- **Find an error**: type the error text in **filter text**, or search every service at once in **Monitoring** › **Logs**.
- **Investigate an incident later**: choose a period of up to 30 days (within your plan's retention) and download the result as a text file.
- **Keep logs for years**: add a log export to one of your [object storage](/docs/object-storage) buckets.
- **Feed your own tools**: export logs continuously to your HTTPS endpoint, signed with a secret.
- **Build dashboards**: create a managed Grafana, or point your own Grafana at the Prometheus-compatible API.
- **Measure your own code**: serve metrics at `/metrics` and set **Metrics path** on the service.
- **Script a check**: read logs from a CI job with `platformctl logs`.

## How to use it

### Read the logs of one service

Use the **Logs** tab to see what one service prints, as it happens.

1. Open the service and its **Logs** tab.
2. The last 6 hours are shown, up to 300 lines, refreshed every few seconds while the tab is open. Lines written to standard error are highlighted.
3. Type in **filter text** to show only matching lines.
4. Untick **follow** to stop the view from scrolling to the newest line while you read.

```
platformctl logs shop/web --filter error
```

```
curl -H "Authorization: Bearer $PLATFORM_TOKEN" \
  "{{console}}/api/v1/projects/shop/services/web/logs?q=error"
```

### See the metrics of one service

The service's **Overview** tab shows **Resource usage** over the last hour: CPU (in % of one core) and memory (MB against the service's limit). Metrics appear about a minute after the first deployment. For longer periods and your own metrics, use [Grafana or the Prometheus API](#query-your-metrics).

### Search the logs of every service

**Monitoring** › **Logs** searches the logs of every service of the organization at once.

1. Choose what to search: **all services**, one project, or one service.
2. Choose the period: last hour, 6 hours, 24 hours, 7 days or 30 days. Periods longer than your plan keeps are greyed out.
3. Optionally enter **text to find**, then click **Search**.
4. Lines appear newest first, up to 500, with the time (UTC), the service and the message.
5. Click **Download** to save the same search as a text file of up to 100,000 lines.

Every member with access to the whole organization can search and download logs. Members limited to some projects cannot open Monitoring. The **Logs** page shows how many days your plan keeps.

### Export logs to a bucket or an endpoint

An export copies every new log line, continuously, to one of your buckets or to your own HTTPS endpoint, so you can keep logs as long as you want or feed them to your own tools.

1. Go to **Monitoring** › **Logs**, section **Exports** (admins).
2. Under **Add an export**, enter a **Name** and choose **Logs of** every project or one project.
3. Choose the **Destination**: **Bucket** (when you have object storage) or **HTTPS endpoint**.
4. For a bucket, pick the bucket and a **Prefix** (`logs/` by default; letters, digits, dots, dashes, underscores and slashes).
5. For an HTTPS endpoint, enter the **Endpoint URL** (public `https://` addresses only) and, optionally, a **Signing secret**.
6. Click **Add export**.

The table shows each export's destination, its status (**ok**, **error** with the reason, or **waiting**), how far it has sent ("sent up to …") and the number of lines sent.

### Pause, resume or delete an export

1. In the **Exports** table, click **pause** to stop an export for a while.
2. Click **resume** to continue where it stopped. Lines older than your retention are no longer available.
3. Click **delete** to remove the export.

### Query your metrics

Point Grafana, `promtool` or any Prometheus client at the Prometheus-compatible API, limited to your organization.

1. Create an API token (a viewer token is enough). See [API and command-line tool](/docs/api-and-cli#api-tokens).
2. Open **Monitoring** › **Grafana and Prometheus** to see the URL and an example.
3. Use the token as a bearer token in your client (`Authorization: Bearer pla_…`).

```
curl -H "Authorization: Bearer $PLATFORM_TOKEN" \
  "{{console}}/api/v1/prometheus/api/v1/query?query=sum(rate(platform_container_cpu_seconds_total[5m]))"
```

### Send your application's own metrics

1. Make your application serve Prometheus metrics, for example at `/metrics`.
2. In the service's **Settings** tab, set **Metrics path** (such as `/metrics`) and, if the metrics are on another port than the service's, **Metrics port**.
3. Save and deploy.

The platform then scrapes every instance every 30 seconds.

### Create a managed Grafana

A managed Grafana is a ready-made Grafana, already connected to your metrics, without any setup.

1. Go to **Monitoring** › **Grafana and Prometheus** (developers and above).
2. Choose a project in **Create a managed Grafana in** and click **Create Grafana**.
3. The next page shows the address and the password of the `admin` user. Copy the password now: it is shown once.
4. Click **Follow the build** to watch the build, or watch the **in progress** indicator at the top of the page.
5. Sign in as `admin` and change the password in Grafana.

## Technical reference

### Limits and retention

| Item | Limit or default | Notes |
| --- | --- | --- |
| Log retention | Trial 1 day, Starter 7 days, Pro 30 days | Default plans. Older lines are no longer shown, downloaded or exported. |
| Metrics retention | 30 days | |
| Service logs tab | Last 6 hours, up to 300 lines | Same for the API and `platformctl logs`. |
| Organization log search | Up to 500 lines, newest first | Periods: 1 hour, 6 hours, 24 hours, 7 days, 30 days. |
| Log download | Up to 100,000 lines | Text file of the same search. |
| Application scrape | Every 30 seconds, up to 20,000 samples and 4 MB per scrape | |
| Managed Grafana size | 250 millicores, 256 MB | Counts as one service of your plan. |

### Platform metrics

Labelled `service_id`, `project_id` and `instance`.

| Metric | Meaning |
| --- | --- |
| `platform_container_cpu_seconds_total` | CPU time used. |
| `platform_container_memory_bytes` | Memory used. |
| `platform_container_memory_limit_bytes` | Memory limit of the service. |
| `platform_container_network_rx_bytes_total` | Bytes received. |
| `platform_container_network_tx_bytes_total` | Bytes sent. |

Metrics scraped from your application get the labels `service`, `project`, `service_id` and `instance`. Labels named `org_id`, `project_id`, `service_id`, `service`, `project` or `instance` set by your application are replaced.

### API

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/v1/projects/PROJECT/services/SERVICE/logs?q=text` | Last 6 hours of a service's logs, up to 300 lines (viewer). |
| `GET`, `POST` | `/api/v1/prometheus/api/v1/…` | Prometheus-compatible API (a viewer token is enough). |

The Prometheus-compatible API supports these endpoints, with `GET` or `POST`: `query`, `query_range`, `series`, `labels`, `label/<name>/values`, `metadata`, `query_exemplars`, `format_query` and `status/buildinfo`.

### Command-line tool

| Command | Purpose |
| --- | --- |
| `platformctl logs <project>/<service> [--filter TEXT]` | Prints recent log lines (6 hours, up to 300). |

### Service settings for metrics

| Setting | Values | Default | Effect |
| --- | --- | --- | --- |
| **Metrics path** | A path such as `/metrics` | empty | The platform scrapes this path on every instance. |
| **Metrics port** | A port number | The service's port | Use it when metrics are on another port. |

In the settings API these are `metrics_path` and `metrics_port`.

### Export file format

Every line is a JSON object:

```
{"time": "…", "project": "shop", "service": "web", "stream": "stdout", "message": "…"}
```

| Destination | What is sent |
| --- | --- |
| Bucket | One gzipped NDJSON file per hour under `<prefix>YYYY/MM/DD/HH/`. |
| HTTPS endpoint | `POST` requests with gzipped NDJSON (`Content-Encoding: gzip`). |

When you set a signing secret, each request to an endpoint carries `X-Platform-Signature: sha256=<hex>`, the HMAC-SHA256 of the body with your secret. Verify it as shown in [Alerts](/docs/alerts#verify-webhook-signatures).

### Behind the scenes

- Each export starts with the last hour, then sends new lines every minute, in order and exactly once. A failed delivery is retried later without losing or repeating lines.
- Pausing an export and resuming it continues where it stopped, as long as the lines are still within your retention.
- The managed Grafana is a service named `grafana`, built then deployed on its own, usually within a few minutes. A project can have only one service named `grafana`.
- It comes with the Prometheus data source (using a read-only token of your organization) and a **Services** dashboard. Its data is kept in a persistent folder, so it is backed up every night.
- The admin password is stored as the service's secret `GF_SECURITY_ADMIN_PASSWORD`, and that secret is the reference: at every start, Grafana's admin password is set from it. To change the password, change the secret on the service's **Settings** page, then redeploy. A password changed inside Grafana itself is replaced by the secret at the next start.
- **Create Grafana** in a project that already has a working Grafana rebuilds it with the latest image, keeping its data, secrets and settings. A Grafana created before November 2026 needs this once for the password secret to apply.
- Its build counts as build minutes.

## Troubleshooting

### A new Grafana password is not accepted

The password follows the service's `GF_SECURITY_ADMIN_PASSWORD` secret at each start, so change the secret, then redeploy the `grafana` service. If your Grafana was created before November 2026, click **Create Grafana** in its project once first: that rebuilds it with the image that applies the secret, keeping your dashboards and data.

### No metrics after a deployment

Metrics appear about a minute after the first deployment. Wait a little and reload the **Overview** tab.

### My log lines are missing

Only standard output and standard error are collected. Make your application print to them instead of writing files. Lines older than your plan's retention are no longer shown.

### My export shows an error

The status column gives the reason. For an endpoint, check that the URL is a public `https://` address and answers successfully. A failed delivery is retried later without losing lines.
