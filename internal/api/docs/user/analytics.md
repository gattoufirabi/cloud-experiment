# Analytics

The **Analytics** page shows how your web services are used: how many requests and visitors they get, how fast they answer, which pages are popular and where visitors come from. It stores no IP addresses and no cookies.

## Overview

Analytics has two reports for each web service:

- **Traffic**: measured at the platform's proxy for every request to the service's addresses. It needs no code at all, and requests appear within a few seconds.
- **Behavior**: page views, sessions and your own events, counted by a small cookieless script that you add to your pages.

Bots (crawlers, scripts, uptime checkers) are recognized and left out of visitors, browsers and systems. Query strings are never recorded, as they can hold secrets.

A visitor is counted with a hash of their address and browser, salted with a secret that changes every day, so visitors cannot be followed from one day to the next. Analytics data is kept as long as your logs (see [Logs and metrics](/docs/logs-and-metrics#limits-and-retention)).

## Use cases

- **Check the health of a site**: look at **Server errors (5xx)** and the p95 and p99 latency for the last 24 hours.
- **Find slow pages**: read **Slowest paths (p95)**.
- **See what is popular**: **Top paths** and **Top pages** show where visitors go.
- **Measure a campaign**: tag your links with `utm_source`, `utm_medium` and `utm_campaign` and read **Campaigns (utm_source)**.
- **Track a sign-up**: call `platformTrack("signup", {plan: "pro"})` and see it in **Events**.
- **Understand your audience**: **Browsers** and **Systems and devices** show how visitors reach you.
- **Automate a report**: read the same data from the API.

## How to use it

### Open the report

1. Open **Analytics** from the product menu.
2. Choose a service (`project/service`) and a period: last 1h, 24h, 7d or 30d. Click **Show**.
3. Switch between the **Traffic** and **Behavior** tabs.

### Read the traffic report

The **Traffic** tab needs nothing to be installed. It shows:

- **Requests**, **Visitors**, **Server errors (5xx)**, latency **p50**, **p95** and **p99** (in milliseconds), and data **Sent**.
- **Requests over time**, with errors in red. Hover a bar for the exact numbers.
- **Top paths**, **Slowest paths (p95)**, **Status codes**, **Referrers**, **Browsers**, and **Systems and devices**.

### Turn on the page-view script

1. On the **Behavior** tab, click **Turn on the page-view script** (developers and above).
2. Copy the snippet shown and put it in the `<head>` of your pages. It looks like `<script defer src="{{console}}/a/s.js" data-site="…"></script>`.
3. Deploy your pages. Page views appear within a few seconds.

The **Behavior** tab then shows **Page views**, **Visitors**, **Sessions**, **Bounce rate**, **Custom events**, then **Top pages**, **Events**, **Where visitors come from** and **Campaigns (utm_source)**.

### Record your own events

Call `platformTrack` in your pages, with an event name and optional properties:

```
platformTrack("signup", {plan: "pro"});
```

### Turn the script off

1. On the **Behavior** tab, click **turn the script off** (developers and above) to stop collecting.
2. Remove the snippet from your pages as well.

### Read the report with the API

The same report is available with the API (viewer token).

```
curl -H "Authorization: Bearer $PLATFORM_TOKEN" \
  "{{console}}/api/v1/projects/shop/services/web/analytics?period=24h"
```

## Technical reference

### API

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/v1/projects/PROJECT/services/SERVICE/analytics?period=24h` | Traffic and behavior report (viewer). `period` is `1h`, `24h`, `7d` or `30d`. |

### Limits of events

| Item | Limit | Notes |
| --- | --- | --- |
| Event name | Up to 60 characters | Letters, digits, spaces and `_ . : / -`. |
| Properties per event | Up to 10 | Each value is cut to 200 characters. |
| Property name | Up to 30 characters | Letters, digits and `_`; other names are ignored. |
| Campaign values | Up to 100 characters | `utm_source`, `utm_medium` and `utm_campaign` values longer than that are ignored. |
| Periods | 1h, 24h, 7d, 30d | |

### What is measured

| Report | Source | Notes |
| --- | --- | --- |
| Traffic | The platform's proxy | Every request to the service's addresses. No code needed. |
| Behavior | The page-view script | Page views, sessions, bounce rate, custom events. |

### Behind the scenes

- Single-page applications are followed through browser history changes: no extra code is needed for client-side navigation.
- `utm_source`, `utm_medium` and `utm_campaign` are read from the page address.
- Only pages served at the service's own addresses (its platform address and verified custom domains) can report to it. Events sent from other sites are ignored. Bots are ignored too.
- No IP address and no cookie is stored.

## Troubleshooting

### Page views do not appear

Check that the snippet is in the `<head>` of pages served from the service's own addresses (its platform address or a verified custom domain), and that you deployed after adding it. Visits from bots are ignored.
