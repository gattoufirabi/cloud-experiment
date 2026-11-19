# Preview environments

A preview is a temporary copy of a service at its own address, so reviewers can try a change before it is merged. It is deleted automatically when it expires.

## Overview

A preview has the same settings, secrets and traffic protection as its service, but another image, at its own address: `https://<name>.<service>-<project>-<organization>.{{apps}}`. The typical use is one preview per pull request, so reviewers can click through a change without touching the live service.

- You create a preview from the service's **Previews** tab, from CI, or automatically for every pull request when [builds from git](/docs/builds) are set up.
- Previews use your plan's CPU and memory and are billed like services, but they do not count as services.
- A preview runs one instance and has no persistent folder, backups, public TCP/UDP ports or jobs, and does not appear on public status pages.

## Use cases

- **Review a pull request**: one preview per pull request, deployed by CI or automatically. See [A preview for each pull request](#a-preview-for-each-pull-request).
- **Test an image before release**: deploy a candidate tag next to the live service. See [Create or update a preview](#create-or-update-a-preview).
- **Share a work in progress**: send the preview address to a colleague or client; set a **Lifetime** so it cleans itself up.
- **Deploy from CI on every push**: run `platformctl preview create` on each push; it redeploys the preview and renews its lifetime. See [Manage previews from CI](#manage-previews-from-ci).
- **Clean up when a pull request closes**: `platformctl preview delete`, or let the automatic previews do it.
- **See all previews at once**: **Compute** → **Previews** lists them with status and expiry.

## How to use it

### Create or update a preview

1. Open the service and its **Previews** tab.
2. Under **Create or update a preview**, enter a **Name** such as `pr-42` (lowercase letters, digits and dashes, up to 20), an **Image** (empty = the service's image) and a **Lifetime** in hours (1 to 720, 72 by default).
3. Click **Deploy preview**. Using an existing name redeploys that preview with the new image and renews its lifetime.

### Open, inspect and delete a preview

1. Click the preview's name to open it.
2. Click **logs and details** for its own page (deployments, logs, settings).
3. Click **Delete** to remove it.

### List every preview

**Compute** → **Previews** lists every preview of the organization (or of the filtered project, see [Getting started](/docs/getting-started#get-around-the-console)) with its status and expiry.

### Manage previews from CI

```
platformctl preview create shop/web pr-42 --image {{registry}}/acme/web:pr-42 --wait
platformctl preview list shop/web
platformctl preview delete shop/web pr-42
```

`--ttl HOURS` sets the lifetime and `--wait` waits until the preview is live or failed. Running `create` again redeploys the preview and renews its lifetime, so it can run on every push. Delete it when the pull request is closed.

```
POST /api/v1/projects/PROJECT/services/SERVICE/previews
{"name": "pr-42", "image": "…", "ttl_hours": 72}
```

### A preview for each pull request

With builds from git set up, a service can get a preview for every pull request automatically, with nothing to script in CI.

1. On the service's **Builds** tab, under **Source**, tick **Preview every pull request** and click **Save source**.
2. In your repository's webhook settings (GitHub, Gitea or Forgejo), use the service's webhook address and add **pull request** events to push events.
3. Open a pull request: its branch is built and deployed as preview `pr-<number>`, at `pr-<number>.<service>-<project>-<organization>.{{apps}}`. Every push to the pull request rebuilds it; closing or merging the pull request deletes the preview.

Good to know:

- Pull requests from forks are ignored: a preview receives the service's secrets and project variables, so only branches of the repository itself are previewed.
- A pull request preview lives 14 days after its last push if it is never closed.
- The previews count against the limit of previews per service and your plan's CPU and memory. Builds of pull requests are listed on the **Builds** tab as "pull request #N".

## Technical reference

### Limits and plans

| Item | Limit/Default | Notes |
| --- | --- | --- |
| Previews per service | 5 | |
| Name | lowercase letters, digits and dashes, up to 20 | |
| Lifetime | 1 to 720 hours, 72 by default | Renewed when the preview is redeployed. |
| Pull request preview lifetime | 14 days after its last push | If the pull request is never closed. |
| Instances | 1 | Previews never autoscale. |
| CPU and memory | Plan's | Billed like services. |

### What a preview has or lacks

| Feature | In a preview |
| --- | --- |
| Settings, traffic protection | Same as the service. |
| Secrets | Copied when the preview is created; later changes to the service's secrets do not reach existing previews. |
| Project variables | Passed as for the service. |
| Persistent folder, backups | Never. |
| Public TCP/UDP ports | Never. |
| Scheduled jobs | None. |
| Public status page | Not shown. |
| Private services | Previews of a private service are private too. |

### API

| Method | Path | Role | Purpose |
| --- | --- | --- | --- |
| GET | `/api/v1/projects/PROJECT/services/SERVICE/previews` | viewer | List previews (`name`, `url`, `image`, `status`, `expires_at`). |
| POST | `/api/v1/projects/PROJECT/services/SERVICE/previews` | developer | Create or update a preview. |
| DELETE | `/api/v1/projects/PROJECT/services/SERVICE/previews/NAME` | developer | Delete a preview (`204`). |

Create or update (`POST …/previews`):

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | string | yes | Preview name, such as `pr-42`: 1 to 20 lowercase letters, digits and dashes, starting with a letter or digit. |
| `image` | string | no | Image; empty means the service's image. |
| `ttl_hours` | number | no | Lifetime in hours (1 to 720, 72 by default). |

The call answers `201` when the preview is new and `200` when an existing one was redeployed, with `name`, `url`, `image`, `status` (`pending`), `expires_at` and `revision`. Refusals: `422` for an invalid name or lifetime, or when neither the request nor the service has an image; `409` when the service already has 5 previews or the plan's CPU or memory is used up.

### Command-line tool

| Command | Purpose |
| --- | --- |
| `platformctl preview create PROJECT/SERVICE NAME [--image IMAGE] [--ttl HOURS] [--wait]` | Create or update a preview. |
| `platformctl preview list PROJECT/SERVICE` | List previews. |
| `platformctl preview delete PROJECT/SERVICE NAME` | Delete a preview. |

### Addresses

| Item | Address |
| --- | --- |
| Preview | `https://<name>.<service>-<project>-<organization>.{{apps}}` |
| Pull request preview | `https://pr-<number>.<service>-<project>-<organization>.{{apps}}` |

## Troubleshooting

### The preview was not created

A service has at most 5 previews, and previews use your plan's CPU and memory. Delete an old preview or lower the service's resources.

### A pull request has no preview

Pull requests from forks are ignored. Also check that **Preview every pull request** is ticked on the **Builds** tab and that your webhook sends **pull request** events.

### A changed secret does not reach the preview

Secrets are copied when the preview is created. Delete the preview and create it again after changing the service's secrets.
