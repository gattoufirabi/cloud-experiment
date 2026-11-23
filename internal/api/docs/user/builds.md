# Builds from git

Let the platform build your repository or a zip archive into an image and deploy it, with no CI pipeline of your own. It uses your Dockerfile when there is one, and detects the language otherwise.

## Overview

Instead of building and pushing images yourself, point a service at a git repository. The platform builds it with its **Dockerfile** if it has one, otherwise with **Railpack**, which detects the language (Node, Python, Go, PHP, Ruby, Java, Rust, Elixir, static sites) and builds it without any configuration.

- The result is an image stored in your [registry](/docs/registry) space and scanned like any pushed image.
- With **deploy when built** ticked, the service is redeployed with the new image and its current settings (see [Applications](/docs/applications)).
- You can start builds by hand, from CI, or on every push with a webhook. You can also deploy a zip archive with no git repository at all.
- Builds run in an isolated sandbox: no access to the platform's internal network, other customers or the server; the internet is reachable for base images and packages.

The **Builds** tab appears on a service when the platform offers builds. The Trial plan does not include them: push an image instead. Build minutes are billed (see [Billing](/docs/billing)).

## Use cases

- **No CI yet**: enter your repository address and click **Build now**. See [Build from a git repository](#build-from-a-git-repository).
- **Deploy on every push**: add the webhook address to GitHub, Gitea, Forgejo or GitLab. See [Build on every push](#build-on-every-push).
- **Private repository**: add a read-only access token. See [Build from a git repository](#build-from-a-git-repository).
- **Deploy from CI with one command**: `platformctl build shop/web --wait`. See [Build from CI](#build-from-ci).
- **Quick test from your laptop**: upload a zip or run `platformctl zip`. See [Deploy a zip archive](#deploy-a-zip-archive).
- **Review every pull request**: build each pull request into a preview. See [Preview environments](/docs/previews#a-preview-for-each-pull-request).
- **Build only, deploy later**: use `--no-deploy` or untick **deploy when built**.

## How to use it

### Build from a git repository

1. On the service's **Builds** tab, under **Source**, enter the **Repository** https address (`https://github.com/acme/web.git`), the **Branch** and, if the app is not at the root, its **Folder** (`services/web`).
2. For a private repository, add an **Access token**: a read-only token (GitHub fine-grained token, GitLab or Gitea deploy token). It is stored encrypted and never shown again; leave the field empty to keep it, or tick **remove the saved token**. Click **Save source**.
3. Click **Build now**. With **deploy when built** (ticked by default) the service is redeployed with the new image and its current settings.

Emptying the **Repository** field turns builds from git off.

### Build from CI

```
platformctl build shop/web --wait
```

`--wait` waits until the build finishes and prints its log; `--no-deploy` only builds and pushes the image.

```
POST /api/v1/projects/PROJECT/services/SERVICE/builds
{"deploy": true}
```

The body is optional: without it the image is deployed once built, and `{"deploy": false}` only builds it.

### Build on every push

1. On the **Builds** tab, click **Create webhook address** under **Build on every push**.
2. Copy the address shown. It is shown once.
3. Add it as a webhook in GitHub, Gitea, Forgejo or GitLab (push events, content type JSON). Pushes to other branches are ignored.
4. **New webhook address** replaces it; the old one stops working.

### Read the builds list and logs

The **Builds** table lists each build with its status, commit, who or what started it (a person, the webhook, a pull request, a zip) and how long it took. Click a build number for its page with the full log; it refreshes while the build runs.

```
GET /api/v1/projects/PROJECT/services/SERVICE/builds/N
```

Builds of pull requests appear as "pull request #N". While a build waits, the activity indicator shows its place in the queue (see [Getting started](/docs/getting-started#follow-what-is-in-progress)).

### Deploy a zip archive

No git repository needed: upload your application's source as a zip archive and the platform builds it the same way (its Dockerfile, or Railpack without one), then deploys it. Handy for a quick test or a project kept on your laptop.

1. **Console:** on the service's **Builds** tab, under **Deploy a zip**, choose the archive (up to 100 MB), keep **deploy when built** ticked if you want it deployed, and click **Upload and build**.
2. **Command line:** run the command below. It zips the folder (leaving out `.git` and `node_modules`) and uploads it; a `.zip` file is uploaded as is.
3. **API:** send the archive as the body.

```
platformctl zip shop/web ./my-app --wait
```

```
POST /api/v1/projects/PROJECT/services/SERVICE/zip
Content-Type: application/zip
```

Add `?deploy=false` to only build, and `?filename=app.zip` to name it in the log. The answer gives the build's `status_url`. On the command line, `--no-deploy` only builds and pushes the image.

### Build on your own servers

Builds can run on your own machines instead of the platform's, like self-hosted CI agents: a VM at AWS or Azure, or hardware of your own, attached as described in [Your own servers](/docs/servers#run-builds-on-your-servers).

1. On **Your servers**, tick **runs builds** for one or more servers. Give them labels if they differ, for example `gpu` or `arm64`.
2. On the service's **Builds** tab, under **Source**, set **Build on** to **your build servers**. To pick particular servers, enter the labels they must have under **Server labels**.

Builds from git, from the webhook, from pull requests and from zip archives then wait for one of those servers, which takes them one at a time. The builds list shows which server ran each build. The image goes to your registry space and is deployed as usual, whether the service runs on the platform or on a server.

## Technical reference

### Limits and plans

| Item | Limit | Notes |
| --- | --- | --- |
| Trial plan | not included | Push an image instead. |
| CPU per build | 2 | Default; your operator may set other limits. |
| Memory per build | 2 GB | Default. |
| Time per build | 15 minutes | Default. |
| Image size | 8 GB | Default; uncompressed. |
| Builds at a time per service | 1 | Starting a build while one is waiting reuses the waiting one. |
| Builds waiting or running per organization | 5 | |
| Zip archive upload | 100 MB | May unpack to 500 MB and 20,000 files. |
| Builds on your own servers | no CPU or memory limit, not billed | One build at a time per server; same time and image size limits. A build no server takes within an hour fails. |

### API

| Method | Path | Role | Purpose |
| --- | --- | --- | --- |
| POST | `/api/v1/projects/PROJECT/services/SERVICE/builds` | developer | Start a build of the configured repository. Optional body `{"deploy": false}` to only build. |
| GET | `/api/v1/projects/PROJECT/services/SERVICE/builds/N` | viewer | Status and log of build N. |
| POST | `/api/v1/projects/PROJECT/services/SERVICE/zip` | developer | Build (and deploy) a zip archive sent as the body. |

Starting a build or uploading a zip answers `202` with `number`, `status` (`queued`) and `status_url` (also in the `Location` header). `GET …/builds/N` returns `number`, `status`, `commit`, `image`, `error`, `log`, `created_at`, `finished_at`, `runs_on` (`platform` or `servers`) and `server` (the build server that took it). Errors: `403` when the plan does not include builds, `409` when builds are not enabled on the platform or no repository is set, `429` when 5 builds of the organization are already waiting or running, `413` when a zip is over 100 MB.

Zip query parameters:

| Parameter | Effect |
| --- | --- |
| `deploy=false` | Only build and push the image. |
| `filename=app.zip` | Name shown in the build log. |

### Command-line tool

| Command | Purpose |
| --- | --- |
| `platformctl build PROJECT/SERVICE [--no-deploy] [--wait]` | Build the service's git branch and deploy it unless `--no-deploy`. |
| `platformctl zip PROJECT/SERVICE FOLDER\|FILE.zip [--no-deploy] [--wait]` | Upload the source (a folder is zipped without `.git` and `node_modules`), build it and deploy. |

### Environment variables

| Variable | Value |
| --- | --- |
| `PORT` | Set by the platform to the service's port. Without a Dockerfile, make your app listen on it. |

### Image names

| Source | Image tag |
| --- | --- |
| Git | `…/<organization>/<service>:git-<first 12 characters of the commit>` |
| Zip | `…/<organization>/<service>:zip-<first 12 characters of the archive's SHA-256>` |

### Behind the scenes

- An archive whose files all sit in one folder (a repository's **Download ZIP**) is built from that folder. Links and paths leaving the archive are refused.
- The archive itself is deleted as soon as its build finishes; the build log keeps its name and SHA-256.
- The access token is stored encrypted and is never shown again.
- The webhook address is shown once; a new one replaces it.

## Troubleshooting

### The build fails and mentions Railpack

Railpack could not detect the project. Its explanation is in the build log: add a Dockerfile.

### The app does not answer after a build without a Dockerfile

Make your app listen on the `PORT` environment variable (the platform sets it to the service's port), on `0.0.0.0`.

### Pushes do not trigger a build

Check that the webhook sends push events with content type JSON, and that you pushed to the configured branch: other branches are ignored. If the address was replaced with **New webhook address**, update it in your git host.

### My build stays queued

Only one build runs at a time per service, and at most 5 are waiting or running per organization. The activity indicator shows its place in the queue.

If the service builds on your servers, the builds list shows "waiting for a build server": check on **Your servers** that a server is online, has **runs builds** ticked, and has every label the service asks for.

### There is no Builds tab

The platform does not offer builds, or your organization is on the Trial plan. Push an image instead.
