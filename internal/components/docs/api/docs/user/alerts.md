# Alerts

The platform watches your applications all the time. When something goes wrong, it opens an incident and notifies your team by push notification, email or your own webhook.

## Overview

An alert is raised when an application stops answering, a deployment fails, a metric crosses a threshold and in a few other cases. Each one opens an **incident**, which closes by itself when the problem is gone (for events that can resolve).

You decide who is told:

- **Notification channels** (email, ntfy push, or a JSON webhook) are added by admins. Each channel chooses which events it receives.
- **CPU and memory thresholds** are set per service.
- **Metric alarms** watch any value you can compute from your metrics, using a query.

Incidents are listed in four places: **Monitoring** (the first page, **Incidents**) for the whole organization, each service's **Alerts** tab, the organization's home page under **Health**, and the service's **Overview** under **At a glance**.

| Event | When it happens |
| --- | --- |
| `service_down` | 3 health checks in a row fail, or the container stopped or ran out of memory. Resolves on its own when the application answers again. |
| `cpu_high`, `memory_high` | Usage stayed above your threshold for 5 minutes. |
| `deploy_failed` | A deployment failed. |
| `backup_failed` | A backup or restore job failed. |
| `critical_vulns` | The nightly scan found critical vulnerabilities in a running image. |
| `job_failed` | A [scheduled job](/docs/scheduled-jobs) failed or timed out. |
| `metric_alarm` | A [metric alarm](#create-a-metric-alarm) stayed beyond its threshold. |
| `budget` | Spending crossed a [budget](/docs/billing) threshold, or the forecast passed the limit. |

A "Disk limit" notification is also sent at 90% and 100% of your plan's disk (see [Billing](/docs/billing)). It goes to every channel, whatever events the channel selects.

## Use cases

- **Be woken up when the site is down**: add an ntfy channel and subscribe on your phone. You get a high-priority push when an incident opens.
- **Tell the team by email**: add an email channel for `ops@example.com`.
- **Open a ticket automatically**: add a webhook channel pointing at your tool and read the JSON body.
- **Catch a runaway process**: set a **CPU alert** or **Memory alert** on the service.
- **Watch your own number**: create a metric alarm on a queue length or an error rate from your application's metrics.
- **Know when a deployment failed**: keep `deploy_failed` ticked on a channel used by your developers.
- **Split noise from urgency**: untick events on a channel so it only receives what matters to it.

## How to use it

### Read the incidents

1. Open **Monitoring** (the first page, **Incidents**) to see the incidents of the whole organization: what happened, the service, when it started and when it resolved.
2. Open a service and its **Alerts** tab to see only that service's incidents.

### Set CPU and memory thresholds

1. Open the service and its **Alerts** tab (developers and above).
2. Under **Health and thresholds**, enter a **CPU alert** and/or a **Memory alert** in % of the service's limits (1 to 100). Leave a box empty to turn that alert off.
3. Click **Save thresholds**.

### Add a notification channel

Admins add channels on **Monitoring** › **Notification channels**, in the **Add a channel** form.

1. Enter a **Name** (for example `on-call`) and choose the **Type**.
2. For **email** (when the platform sends email), enter the **Email address**.
3. For **ntfy push** (push notifications to a phone or desktop with the ntfy app), enter the ntfy server in **URL**, the **ntfy topic**, and an access token in **Secret** if the topic is protected.
4. For **webhook (JSON)**, enter your endpoint in **URL** and, optionally, a signing **Secret**.
5. Untick the events this channel should not receive (all are ticked by default).
6. Click **Add channel**.

### Test or remove a channel

1. Click **send test** next to a channel to check that notifications arrive. Test notifications are limited to 10 per 10 minutes for the organization.
2. Read the channel list: it shows each channel's address, its events and its **last delivery** (sent or failed, with the reason).
3. Click **remove** to delete a channel.

### Create a metric alarm

A metric alarm watches any value you can compute from your metrics (CPU, memory, network, or your application's own metrics) and alerts you when it crosses a threshold.

1. Go to **Monitoring** › **Metric alarms** (developers and above).
2. Enter a **Name** (for example `High CPU on web`).
3. Enter a **Query**: a PromQL expression over your metrics that returns a number or an instant vector, such as `max(rate(platform_container_cpu_seconds_total[5m]))` or `max(platform_container_memory_bytes / platform_container_memory_limit_bytes)`.
4. Choose **Alert when the value is** above, at or above, below, or at or below, and enter the **Threshold**.
5. Set **For**: how many minutes in a row (0 to 120, 5 by default) the value must stay beyond the threshold.
6. Click **Create alarm**. The query is tested first; an invalid query is refused with the reason.

```
curl -X POST -H "Authorization: Bearer $PLATFORM_TOKEN" \
  -d '{"name": "High CPU", "query": "max(rate(platform_container_cpu_seconds_total[5m]))", "op": ">", "threshold": 0.8, "for_minutes": 5}' \
  "{{console}}/api/v1/alarms"
```

### Pause, resume or delete an alarm

1. In the alarm table, click **pause** to stop checking an alarm. This resolves its open incident.
2. Click **resume** to start again.
3. Click **delete** to remove it.

```
curl -X DELETE -H "Authorization: Bearer $PLATFORM_TOKEN" \
  "{{console}}/api/v1/alarms/NAME"
```

### Verify webhook signatures

If you set a webhook secret, each request carries `X-Platform-Signature: sha256=<hex>`, the HMAC-SHA256 of the raw body with your secret. Check it before trusting the request:

```
import hmac, hashlib
expected = "sha256=" + hmac.new(secret.encode(), raw_body, hashlib.sha256).hexdigest()
valid = hmac.compare_digest(expected, request.headers["X-Platform-Signature"])
```

The same signature is used by [log exports](/docs/logs-and-metrics#export-logs-to-a-bucket-or-an-endpoint).

## Technical reference

### Limits and plans

| Item | Limit or default | Notes |
| --- | --- | --- |
| Metric alarms | 5 on Trial, 20 on Starter, 100 on Pro | Default plans. Alarm names are unique. |
| CPU and memory thresholds | 1 to 100 (% of the service's limits) | Empty turns the alert off. |
| Duration before `cpu_high` or `memory_high` | 5 minutes | |
| Alarm **For** | 0 to 120 minutes, 5 by default | |
| Alarm check | Every minute | |
| Test notifications | 10 per 10 minutes, for the organization | |

### Notification channels

| Type | Fields | Behavior |
| --- | --- | --- |
| email | **Email address** | Carries a link to the service in the console. |
| ntfy push | **URL** (ntfy server), **ntfy topic**, **Secret** (access token, optional) | High priority when an incident opens, normal priority when it resolves. |
| webhook (JSON) | **URL**, **Secret** (signing, optional) | `POST` with a JSON body, signed when a secret is set. |

Channel URLs must be public HTTPS addresses; private and internal addresses are refused.

### Webhook body

A webhook receives a `POST` with a JSON body:

| Field | Description |
| --- | --- |
| `incident_id` | The incident's ID. |
| `kind` | The event, for example `service_down`. |
| `status` | `firing`, `resolved` or `event`. |
| `title`, `summary` | Short description. |
| `organization`, `project`, `service` | Where it happened. |
| `started_at` | When the incident started. |
| `resolved_at` | Once resolved. |
| `console_url` | Link to the service in the console (to the organization when there is no service). |

### Metric alarm states

| State | Meaning |
| --- | --- |
| **ok** | The value is within the threshold. |
| **pending** | Beyond the threshold, waiting for the minutes to pass. |
| **firing** | An incident is open. |
| **no data** | The query returned nothing. |
| **paused** | Not checked. |

The table also shows each alarm's condition, its current value, when it was last checked, and the last error if the query failed.

### API

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/v1/alarms` | Metric alarms with their state (viewer). |
| `POST` | `/api/v1/alarms` | Creates an alarm, answers `201` (developer). A duplicate name answers `409`. |
| `DELETE` | `/api/v1/alarms/NAME` | Deletes an alarm, by name or ID (developer). |

Body of `POST /api/v1/alarms`:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | string | yes | Unique alarm name, 1 to 80 characters. |
| `query` | string | yes | PromQL expression, 1 to 1000 characters; tested when you create the alarm. |
| `op` | string | yes | One of `>`, `>=`, `<`, `<=`. |
| `threshold` | number | no | Value to compare with (0 when omitted). |
| `for_minutes` | integer | no | Minutes in a row (0 to 120, 5 by default). |

### Behind the scenes

- Alarms are checked every minute. When the value stays beyond the threshold for the chosen minutes, an incident opens and your channels are notified (event `metric_alarm`). It resolves on its own when the value comes back.
- `service_down` resolves on its own when the application answers again.

## Troubleshooting

### I get no notification

Click **send test** next to the channel and read its **last delivery**. Check that the event is ticked on the channel, and that the URL is a public HTTPS address.

### An alarm is refused

The query is tested when you create the alarm. The message gives the reason, for example a syntax error.
