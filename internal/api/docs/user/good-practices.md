# Limits and troubleshooting

Know what your plan allows, which network rules apply, how to run your services safely, and what to do when a message appears.

## Overview

Your plan sets how much you can run. The organization's home page shows your usage of the main limits under **Usage of your plan**. When an action would go over a limit, it is refused with a message that names the limit.

Limits of other products (buckets, functions, queues, email, DNS zones, mailboxes, public ports, secrets) are explained on their own pages. Disk limits are explained in [Billing and usage](/docs/billing#disk-limit-behavior).

Two network rules apply to all applications: no direct email, and no access to internal networks.

## Use cases

- **Check before you scale**: read **Usage of your plan** on the home page before adding a service or raising CPU.
- **Understand a refusal**: find the message in the troubleshooting list to see what to change.
- **Make rollbacks exact**: pin image versions.
- **Send email from an app**: use the platform's email product, since ports 25, 465 and 587 are blocked.
- **Harden access**: use one token per tool, with the smallest role and project that works.
- **Stay informed**: add an alert channel and set a budget.

## How to use it

### Check your plan limits

1. Open the organization's home page.
2. Under **Usage of your plan**, read your usage of the main limits.
3. If a limit is close, delete unused services, lower resources, or ask your platform operator for a bigger plan (see [the admin guide](/docs/admin)).

### Send email from an application

Outgoing ports 25, 465 and 587 are blocked, so connect differently.

1. Use the platform's [Email](/docs/email) product, or an email provider's HTTPS API.
2. Keep the credentials in secrets, not in plain environment variables.

### Apply the good practices

1. Pin image versions (`myapp:1.4.2`, not `latest`).
2. Point the health check to a fast page.
3. Store data in the persistent folder or a managed database.
4. Store passwords and keys as secrets.
5. Add an alert channel in [Alerts](/docs/alerts#notification-channels).
6. Create one API token per tool and revoke the ones you no longer use.
7. Set a budget in [Billing and usage](/docs/billing#set-a-budget).

## Technical reference

### Plan limits

These are the default plans. Your platform operator may offer different ones.

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

### Network rules

| Rule | Detail |
| --- | --- |
| No direct email | Outgoing ports 25, 465 and 587 are blocked. Use the platform's [Email](/docs/email) product or an email provider's HTTPS API. |
| No access to internal networks | Applications reach the internet, but not the server, other clients or private ranges. Your own services reach each other on the [private network](/docs/private-network). |

### Request and sign-in limits

| Limit | Value |
| --- | --- |
| API requests | Up to 600 per minute per address |
| API token lifetime | One year |
| Failed passwords | Account locked 15 minutes after 5 attempts |

### Good practices

| Practice | Why |
| --- | --- |
| Pin image versions | Rollbacks are exact and old images are not cleaned up while you may need them. |
| Keep the health check fast | It stays independent of slow services. |
| Keep data in the persistent folder or a managed database | Data inside the container is lost on redeploy. |
| Use secrets | Passwords and keys stay out of plain environment variables and your image. |
| Add an alert channel | Incidents reach you even when the console is closed. |
| One token per tool | Smallest role and project that works; revoke unused tokens. |
| Set a budget | Spending never surprises you. |

## Troubleshooting

### nothing answering on port N

Check the **Port** setting, and that the application listens on `0.0.0.0`, not only `127.0.0.1`.

### health check returned HTTP 5xx

Point the health path to a lightweight page and read the logs.

### container stopped (exit code N)

Read the log lines under the status. It is usually a missing environment variable or secret.

### killed: out of memory

Raise **Memory**, or reduce the application's memory use.

### could not pull image

Check the spelling and tag. Push private images to your registry first.

### images from the platform registry must be under …

Use `{{registry}}/<your organization>/…`.

### blocked by security policy

Rebuild on an updated base image, push and redeploy (see [Security scanning](/docs/security-scanning)).

### CPU limit reached or Memory limit reached

Lower other services' resources, delete unused services, or ask for a bigger plan.

### Disk limit exceeded

Delete data from persistent folders or databases, or ask for a bigger plan (see [Billing and usage](/docs/billing#disk-limit-behavior)).

### The TXT record … was not found yet

Check the record name and value, wait a few minutes, then verify again.

### invalid or missing CSRF token

Reload the page and try again.

### Too many attempts

Wait a few minutes and try again. After 5 wrong passwords an account is locked for 15 minutes.

### API: invalid, expired or revoked API token

Check `PLATFORM_TOKEN`. Create a new token if it expired (tokens last one year).

### API: this token's role (…) cannot do this

Use a token with the role named in the message.

### API: this token only works for project …

The token is limited to one project. Use a token for the whole organization.

### API: … is turned off for this organization

Your platform operator turned that product off for you. Ask them.

### API: too many requests

Slow down: an address may send up to 600 API requests per minute.

### The provider could not be reached (single sign-on)

Check the issuer URL, and that the provider is reachable from the internet (see [Single sign-on](/docs/single-sign-on)).

### Your email is not in this organization's domains

Sign in with your work email, or ask an admin to add your domain in **Single sign-on**.

### Still stuck

Send your platform operator the service address and the time of the problem.
