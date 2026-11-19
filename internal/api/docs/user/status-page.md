# Status page

A status page tells your customers whether your application is up, without them having to email you. Each project can have one public page that anyone can open without an account.

## Overview

The status page of a project lives at `/status/<organization>/<project>` on the console address. It is off by default. When you turn it on, the page shows the state of the project's services and their recent history.

What it shows:

- each service's state: **operational**, **down** or **not running**;
- a bar per day for the last 30 days with the service's uptime;
- **Past incidents**: outages from the last 14 days, with when they started and were resolved (or **ongoing**).

The platform checks the services every few seconds. The page never shows logs, error messages, images, environment variables or secrets. Turning the page off makes its address stop working immediately.

## Use cases

- **Answer "is it down?" before it is asked**: share the address with your customers.
- **Link from your site**: put the address in your site's footer or help page.
- **Show your uptime history**: the 30-day bars give a quick picture of reliability.
- **Be transparent about outages**: **Past incidents** lists the last 14 days.
- **Take the page down quickly**: click **Turn off** and the address stops working at once.

## How to use it

### Turn the status page on

1. Open the project, then its **Settings** tab (admins and owners).
2. Under **Public status page**, click **Make the status page public**.
3. The page's address appears: share it, or link to it from your site.

### Turn the status page off

1. Open the project's **Settings** tab and find **Public status page**.
2. Click **Turn off**. The address stops working immediately.

## Technical reference

### Status page content

| Item | Value | Notes |
| --- | --- | --- |
| Service state | **operational**, **down**, **not running** | Checked every few seconds. |
| Uptime history | One bar per day, last 30 days | |
| Past incidents | Last 14 days | Start and resolution time, or **ongoing**. |
| Default | Off | Per project. |
| Who can change it | Admins and owners | In the project's **Settings** tab. |

### Address

| Item | Value |
| --- | --- |
| Public address | `{{console}}/status/<organization>/<project>` |
| Access | Anyone, without an account, only while the page is on |

### Behind the scenes

- The page is built from service state and outage history only. It never exposes logs, error messages, images, environment variables or secrets.

## Troubleshooting

### The address does not work

The page is off by default and stops working as soon as it is turned off. Open the project's **Settings** tab and check **Public status page**.
