# Traffic protection

Filter requests at the platform's proxy, before they reach your application. Block common attacks, slow down visitors who send too many requests, shut out addresses that misbehave, and keep an overloaded application responsive.

## Overview

Traffic protection is a set of four settings on each service: a web application firewall, a request limit per visitor, an overload limit (requests in progress per instance), and a list of blocked addresses. The proxy applies them to every address of the service (the platform address and custom domains), so blocked or slowed requests never use your application's resources.

Changes apply within a few seconds, without a redeploy. Health checks and alerts are not affected: the platform checks your application directly, not through the proxy.

Developers and above can change the settings. Every change is recorded in the audit log.

## Use cases

- **Block common attacks**: turn on the **Web application firewall** to stop SQL injection, cross-site scripting, path traversal and scanner probes.
- **Slow down scrapers and brute force**: set **Requests per minute per visitor** so one address cannot flood a login page.
- **Shut out a bad address**: add an IP or a range to **Blocked addresses**.
- **Protect an API**: combine a request limit with the firewall on a public API service.
- **Survive a sudden rush**: set **Requests in progress per instance** so that, when every instance is busy, extra visitors get a quick `503` instead of waiting in a queue that grows until nobody is served. The application stays fast for the requests it accepts and recovers as soon as traffic drops. With autoscaling, the first extra instance usually starts within a minute or two of the rush; the limit keeps visitors from waiting meanwhile.
- **Roll out the firewall carefully**: turn it on, then click through your application to check for false positives.

## How to use it

### Change the protection settings

1. Open the service and its **Security** tab. The top line sums up the current state: firewall on or off, the request limit, the overload limit, how many addresses are blocked.
2. Under **Traffic protection**, tick **Web application firewall** to turn the firewall on.
3. Enter a number in **Requests per minute per visitor**, or leave it empty for no limit.
4. Enter a number in **Requests in progress per instance**, or leave it empty for no limit.
5. List addresses in **Blocked addresses**, one per line.
6. Click **Save protection**.

Changes apply within a few seconds, on every address of the service, without a redeploy.

### Check the firewall against your application

The firewall is off by default because some applications (for example ones that accept HTML or code in forms) can trigger false positives.

1. Turn on **Web application firewall** and save.
2. Click through your application and submit your forms.
3. If a legitimate action gets `403 Forbidden`, turn the firewall off again and adapt your application, or keep it off.

## Technical reference

### Settings

| Setting | Values | Default | Effect |
| --- | --- | --- | --- |
| **Web application firewall** | on or off | off | Applies the OWASP Core Rule Set. Blocked requests get `403 Forbidden`. |
| **Requests per minute per visitor** | 10 to 100,000, or empty | empty (no limit) | Each visitor IP address may send at most this many requests per rolling minute. Above it: `429 Too Many Requests` with a `Retry-After` header. |
| **Requests in progress per instance** | 1 to 100,000, or empty | empty (no limit) | When every instance already has this many requests in progress, new requests get `503 Service Unavailable` at once instead of waiting. Do not use it for WebSockets, server-sent events or long polling: an open connection counts as a request in progress. |
| **Blocked addresses** | up to 100 IPv4 or IPv6 addresses or CIDR ranges | none | These addresses always get `403`. |

### Blocked address format

| Rule | Detail |
| --- | --- |
| Separators | One per line (commas and spaces also work). |
| Examples | `203.0.113.7`, `198.51.100.0/24` |
| Duplicates | Removed. |
| Invalid entry | Refused with "Not an address or range". |
| Maximum | 100 addresses or ranges. |

### What the firewall blocks

The firewall uses the OWASP Core Rule Set to block common attacks such as SQL injection, cross-site scripting, path traversal and known scanner probes.

### Responses

| Status | When |
| --- | --- |
| `403 Forbidden` | The firewall blocked the request, or the visitor's address is blocked. |
| `429 Too Many Requests` | The visitor went over the request limit. A `Retry-After` header says when to retry. |
| `503 Service Unavailable` | Every instance already had as many requests in progress as the overload limit allows. |

### Behind the scenes

- The settings are stored on the service, and the proxy configuration is refreshed within a few seconds.
- Count every file a page loads (scripts, images, API calls) when you choose the request limit.
- Choosing the overload limit: about the number of requests one instance handles in one or two seconds (for example 20 to 50 for an app answering in 20 to 50 ms). Too low and normal bursts get `503`; too high and visitors wait long before the limit applies.
- Without an overload limit, an application that receives more than it can serve queues every request; visitors give up, but the application still works through the queue, so it can stay slow for many minutes after the rush is over.
- Every change is recorded in the audit log.

## Troubleshooting

### A legitimate request gets 403

The firewall may have flagged it (a form that accepts HTML or code, for example), or the visitor's address is in **Blocked addresses**. Check the list, and try turning the firewall off.

### Visitors get 429

The request limit is too low for your pages. A page loads many files, so raise **Requests per minute per visitor**, or leave it empty.

### The settings will not save

The request limit must be between 10 and 100000, or empty. Blocked addresses must be valid IPs or CIDR ranges, at most 100.
