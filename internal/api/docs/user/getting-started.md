# Getting started

Create your account, secure it with two-step login, and learn how the console is organized. This page also covers the free trial, projects and the tools that help you move around.

## Overview

Before you can use the console you need an account. Accounts are created by invitation or, when the platform allows it, by signing up. Every account must use two-step login with an authenticator app (or a passkey), so a stolen password alone never opens your organization.

Everything you create lives in a simple hierarchy: your **organization** holds **projects**, and projects hold **services**, **databases** and **functions**.

- **Organization**: your company's space. It holds the members and their roles, the plan and its limits, the audit log, your private registry space, notification channels, buckets, queues, email domains and the secrets manager.
- **Project**: a group of related resources that make up one application, for example `shop`. Resources in a project share a private network and the project's shared variables.
- **Service**: one running container (a website, an API or a background worker) with its own address: `<service>-<project>-<organization>.{{apps}}`.
- **For each service**: deployments (numbered revisions), settings and secrets, domains, logs, metrics, health and alerts, a vulnerability report and, when it has a persistent folder, backups.

Everything inside your organization is private to its members: other clients cannot see or reach it, through the console, the network or the registry.

New organizations created with **Create an account** start on a free **Trial** plan for 14 days, so you can try the platform with real applications before choosing a paid plan.

## Use cases

- **Join your team**: accept an invitation, choose a password and set up two-step login. See [Accept an invitation](#accept-an-invitation).
- **Start on your own**: create an account and get your own organization on a free trial. See [Create an account without an invitation](#create-an-account-without-an-invitation).
- **Sign in without typing codes**: add a passkey and use your fingerprint, face or device PIN. See [Add a passkey](#add-a-passkey).
- **Group an application's parts**: create one project per application, for example `shop`, so services, databases and variables stay together. See [Create a project](#create-a-project).
- **Understand an application at a glance**: open the project **Map** tab to see what comes in, what runs and what it uses. See [Read the project map](#read-the-project-map).
- **Act on many resources at once**: redeploy every service after a base image update, or back up all databases before a migration. See [Use bulk actions](#use-bulk-actions).
- **Find anything quickly**: use search, the project filter, favorites and the project tree. See [Get around the console](#get-around-the-console).
- **Track long operations**: use the activity indicator to follow builds, deployments and backups. See [Follow what is in progress](#follow-what-is-in-progress).

## How to use it

### Accept an invitation

Use this when someone invited you to an organization.

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
4. Set up two-step login as described in [Accept an invitation](#accept-an-invitation). You get your own organization on a free trial (see [Free trial limits](#free-trial-limits)).

### Log in, recover access and switch organization

1. Log in with your email and password, then the current 6-digit code. If your organization uses its own identity provider, click **Sign in with SSO** instead (see [Single sign-on](/docs/single-sign-on)).
2. **Lost phone:** log in with your password and a recovery code (type it in the **or a recovery code** field), then set up two-step login again. No codes left? Ask your platform operator ([admin guide](/docs/admin)).
3. **Forgot your password:** choose **Forgot your password?** on the login page to get a one-hour link by email (or ask the operator for one). All your sessions end when you set the new password, and two-step login is still required.
4. **Several organizations:** after logging in you see the list of organizations you belong to, with your role in each. Open one to enter its console. Later, use the **Organization menu** (top right) to switch.
5. To get a fresh set of recovery codes, open **Account and security** (account menu, top right) and click **Create new codes**. The old codes stop working.

### Add a passkey

A passkey lets you sign in with your fingerprint, face or device PIN instead of your password and 6-digit code. It works with phones, laptops, security keys and password managers that support passkeys.

1. Open the account menu (top right), choose **Account and security**, and find **Passkeys**.
2. Type a **Name** you will recognize, such as "Work laptop", and click **Add a passkey**. Confirm on your device.
3. Next time, on the login page, choose **Sign in with a passkey** and confirm on your device.
4. To remove one (a lost or replaced device), click **Remove** next to it.

The list shows each passkey with when it was added and last used. A **synced** tag means the passkey lives in a password manager or cloud keychain and is available on your other devices too. More about accounts: [Account and security](/docs/account).

### Create a project

A project keeps one application's parts together, so you can see them at a glance and share settings between them. Only admins can create projects.

1. Open **Projects** from the Services menu (or the **Projects** widget on the console home) and click **Create project**.
2. Enter a **Project ID**: 2 to 20 lowercase letters, digits or dashes, for example `shop`. It is used in addresses and cannot be changed. Optionally add a **Display name** such as "Online shop".
3. Click **Create project**.

```
POST /api/v1/projects
{"slug": "shop", "name": "Online shop"}
```

```
platformctl projects
```

The command lists your projects.

### Use the project page

The project page has these tabs:

| Tab | What it shows |
| --- | --- |
| **Overview** | Counters (live services, databases, shared variables, domains and jobs) and **All resources**: every service, database, domain and scheduled job of the project with its status, with a search box. |
| **Map** | The project map (see [Read the project map](#read-the-project-map)). |
| **Services** | The project's services with their image and last deployment, and an **Add a service** form. |
| **Databases** | The project's databases with engine, resources and status, and a **Create a database** form. |
| **Variables** | Shared variables (see [Variables and secrets](/docs/variables)). |
| **Settings** (admins) | **Private network** connections to other projects (see [Private network](/docs/private-network)), the **Public status page** switch (see [Status page](/docs/status-page)), and **Delete project**. |

### Read the project map

The **Map** tab shows how the project fits together, so a newcomer understands an application in seconds and you can spot a missing link.

1. Open the project and its **Map** tab.
2. Read it from left to right: **Comes in from** (the internet, custom domains, schedules, function webhooks and connected projects), **Runs** (services, workers and functions) and **Uses** (databases, queues, buckets and the email sending service).
3. Check the status color of each box: running, deploying or starting, failed or stopped. A dashed line means a service calls another through a private name or public address found in its plain environment variables.
4. Click a box to open that resource. The map refreshes on its own, every 3 seconds while something is changing.

### Delete a project

Admins only. Take the backups you need first: this cannot be undone.

1. Open the project's **Settings** tab.
2. Type the project ID and click **Delete project**.

Everything in it is deleted (services, databases, functions, variables, jobs, previews) and its containers are stopped.

### Get around the console

The console works like a cloud provider's console: each product has its own area, and a few tools help you move around fast.

1. **Services menu** (left side): every product, grouped as Compute, Data, Networking, Delivery, Operations and Management. Products the platform does not offer are hidden.
2. **Favorites:** open **All services** (bottom of the left menu) to see the whole catalog. Click **Pin** (the star) on a product to add it to the favorites bar at the top of the left menu and to the **Favorites** widget of the console home; click **Pinned** to remove it. Pins are personal to you. By default Compute, Functions, Databases, Object storage and Monitoring are pinned.
3. **Search** (top bar, or press `/` anywhere outside a text field): type part of a name and press Enter. It finds projects, services (by name or image), databases (by name or engine), functions, queues, buckets and domains, and products by what they do ("backup", "email"). Up to 50 results.
4. **Project filter** (the **Project** box in the top bar): narrows the lists of every area (services, databases, domains, scheduled jobs, previews) to one project. Choose **All projects** to see everything again. The choice is remembered in your browser for 6 months.
5. **Project tree:** the left pane lists every project with its services, databases (`db`) and functions (`fn`), and the status of each service's latest deployment: ● live, × failed, ◌ deploying, ○ not deployed. Click a name to open it.
6. **Area menu:** inside a product, its own pages appear under its name, for example Compute → Services, Scheduled jobs, Previews.
7. **Breadcrumbs** above each page show where you are (Console › Compute › Online shop › storefront); click a part to go back up.
8. **Lists:** every list spans all your projects (or the filtered one), shows a count, and has a search box that hides non-matching rows as you type.
9. **Tabs** on a resource page (Overview, Settings and so on) can also be changed with the arrow keys.
10. **Organization menu** (top right): switch to another organization you belong to.
11. **Account menu** (top right): **Account and security** (passkeys, recovery codes), **Appearance** (**Light**, **Dark**, or **Auto** to follow your device), the user guide and **Log out**.
12. **Small screens:** the menu button at the top left shows or hides the left menu.

### Use the console home

The console home (the logo, or **Console home** in the left menu) gives you the state of the organization at a glance:

- **Favorites:** your pinned products.
- **Projects:** each project with its number of services; **View all** opens the Projects list.
- **Health:** open incidents, or "healthy" when every live application answers its checks.
- **Usage of your plan:** services, CPU, memory, databases and disk used against your plan's limits.
- **Get started:** shortcuts to deploy a container, create a database, create a bucket and deploy from CI with an API token.

During the trial, the home also shows a notice with the days left.

### Follow what is in progress

When something is in progress, the top bar shows a spinner with **N in progress**, so nothing seems to hang without a word.

1. Click it to see each item with its state: builds (queued, with their place in the queue, or building, with the current step), deployments (waiting or deploying), databases starting, and backups or restores.
2. When an item finishes, a notice appears for a few seconds ("build finished. See the result") with a link to it. If you are on that item's page, the page reloads to show the result.

It checks every 3 seconds while something is in progress, and every 15 seconds otherwise. Members limited to some projects see only their projects' items.

### Use create dialogs

The main create buttons at the top right of a page (**Create service**, **Create database**, **Create project**, **Create secret** and others) open their form in a dialog over the page. If the form is refused (a name already taken, a plan limit), the page comes back with the error and the dialog open again, so you can fix it. Without JavaScript, the button scrolls to the same form at the bottom of the page.

### Use bulk actions

Bulk actions let you act on many resources at once.

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

## Technical reference

### Sign-in rules

| Item | Rule |
| --- | --- |
| Password | At least 12 characters. |
| Two-step login | Required. Time-based codes, 6 digits, 30 seconds. |
| Session length | Up to 12 hours, ends after 2 hours without activity. |
| Lockout | After 5 wrong passwords the account is locked for 15 minutes. Too many tries in a short time also show "Too many attempts" (see [Account security](/docs/account#limits-and-defaults)). |
| Recovery codes | 10 one-time codes, format `abcde-fghij`. Each replaces the 6-digit code once. **Create new codes** makes a new set and the old ones stop working. |
| Invitation link | Works once, expires after 7 days. |
| Sign-up confirmation link | Works once, within 24 hours. |
| Password reset link | Valid for one hour. All sessions end when the new password is set. |
| Organization ID | 2 to 20 lowercase letters, digits and dashes. |
| Project ID | 2 to 20 lowercase letters, digits and dashes. Cannot be changed. |

### Passkeys

- Only a public key is stored on the platform; the private key never leaves your device or password manager.
- Your password and authenticator app keep working: use them if you lose the device, then remove the lost passkey.
- A passkey only works on this platform's address, so a look-alike phishing site cannot use it.
- If your browser cannot create passkeys, the page says "This browser does not support passkeys".

### Free trial limits

These are the default values; your operator may set others.

| Resource | Trial |
| --- | --- |
| Duration | 14 days |
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

### Trial end and suspension

- You get an email 3 days before the trial ends (when the platform sends email).
- When the trial ends, your applications stop and changes are blocked; your data is kept. Contact your platform operator to choose a plan and everything starts again ([admin guide](/docs/admin)). See also [Billing](/docs/billing).
- Applications that use all their CPU for a long time (typical of crypto-mining) get a trial organization suspended automatically. The console home shows a **suspended** badge and says why; contact the operator if it was legitimate work.

### API

| Method | Path | Purpose |
| --- | --- | --- |
| POST | `/api/v1/projects` | Create a project (admin). Body: `slug` (required, the project ID), `name` (optional display name, up to 100 characters, defaults to the ID). Returns `{"message": ...}`; an invalid or taken ID returns 422. |
| GET | `/api/v1/projects` | List projects (viewer). Each item has `id`, `name` and `services` (the number of services). |
| GET | `/api/v1/me` | Show the token's organization and role (used by `platformctl whoami`). |

### Command-line tool

| Command | Purpose |
| --- | --- |
| `platformctl whoami` | Show the token's organization and role. |
| `platformctl projects` | List projects. |

### Addresses

| Item | Address |
| --- | --- |
| Service (public) | `https://<service>-<project>-<organization>.{{apps}}` |
| Console | `{{console}}` |

## Troubleshooting

### "Too many attempts"

Too many login tries in a short time, or 5 wrong passwords (the account is then locked for 15 minutes). Wait a few minutes, then log in again, or use **Forgot your password?**.

### The invitation link does not work

It works once and expires after 7 days. Ask your organization's admin or the platform operator for a new one.

### No confirmation email after signing up

Check your spam folder, then sign up again. The link works once, within 24 hours.

### My applications stopped and changes are blocked

The trial may have ended. Your data is kept. Contact your platform operator to choose a plan.
