# Traffic protection

Filter requests at the platform's proxy, before they reach your application. Block common attacks, slow down visitors who send too many requests, and shut out addresses that misbehave.

## Overview

Traffic protection is a set of three settings on each service: a web application firewall, a request limit per visitor, and a list of blocked addresses. The proxy applies them to every address of the service (the platform address and custom domains), so blocked or slowed requests never use your application's resources.

Changes apply within a few seconds, without a redeploy. Health checks and alerts are not affected: the platform checks your application directly, not through the proxy.

Developers and above can change the settings. Every change is recorded in the audit log.

## Use cases

- **Block common attacks**: turn on the **Web application firewall** to stop SQL injection, cross-site scripting, path traversal and scanner probes.
- **Slow down scrapers and brute force**: set **Requests per minute per visitor** so one address cannot flood a login page.
- **Shut out a bad address**: add an IP or a range to **Blocked addresses**.
- **Protect an API**: combine a request limit with the firewall on a public API service.
- **Roll out the firewall carefully**: turn it on, then click through your application to check for false positives.

## How to use it

### Change the protection settings

1. Open the service and its **Security** tab. The top line sums up the current state: firewall on or off, the request limit, how many addresses are blocked.
2. Under **Traffic protection**, tick **Web application firewall** to turn the firewall on.
3. Enter a number in **Requests per minute per visitor**, or leave it empty for no limit.
4. List addresses in **Blocked addresses**, one per line.
5. Click **Save protection**.

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

### Behind the scenes

- The settings are stored on the service, and the proxy configuration is refreshed within a few seconds.
- Count every file a page loads (scripts, images, API calls) when you choose the request limit.
- Every change is recorded in the audit log.

## Troubleshooting

### A legitimate request gets 403

The firewall may have flagged it (a form that accepts HTML or code, for example), or the visitor's address is in **Blocked addresses**. Check the list, and try turning the firewall off.

### Visitors get 429

The request limit is too low for your pages. A page loads many files, so raise **Requests per minute per visitor**, or leave it empty.

### The settings will not save

The request limit must be between 10 and 100000, or empty. Blocked addresses must be valid IPs or CIDR ranges, at most 100.
