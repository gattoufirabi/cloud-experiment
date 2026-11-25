# Variables and secrets

Give your applications their settings and passwords without putting them in your images. Set a value once for a whole project, or keep a versioned secret that several services share.

## Overview

There are three ways to give a service values, from narrowest to widest:

- **Service environment variables and secrets**: set on one service (see [Applications](/docs/applications#add-secrets-to-a-service)).
- **Project variables**: set once on a project; every service of the project, and its previews, receives them as environment variables.
- **Secrets manager**: secrets shared by several services or projects, encrypted and versioned. A variable references one with `${secret:NAME}`; when you rotate it, every live service using it is updated.

Values are encrypted at rest. Secret values are never shown again, not even to admins or your platform operator.

When the same name is set in several places, a service's own environment variable or secret wins over the project variable. `PORT` and names starting with `PLATFORM_` are reserved.

## Use cases

- **A key every service needs**: set `API_BASE_URL` once as a project variable. See [Set a project variable](#set-a-project-variable).
- **Feature flags**: add a plain (non-secret) variable you can read back later.
- **Payment provider key shared by several services**: store it in the secrets manager and reference `${secret:stripe-key}`. See [Create and use a managed secret](#create-and-use-a-managed-secret).
- **Rotate a leaked key**: save a new version; every live service using it is redeployed with no downtime. See [Rotate a secret](#rotate-a-secret).
- **Check an older value before a rollback**: **reveal** a previous version (audited). See [Read a value](#read-a-value).
- **Script it from CI**: `platformctl var set` and `platformctl vault put`.
- **Override for one service**: set the same name on the service to beat the project value.

## How to use it

### Set a project variable

1. Open the project and its **Variables** tab.
2. Under **Add or change a variable**, enter a name in capitals (`API_BASE_URL`, up to 64 characters) and a value (up to 8192 characters). Keep **Secret** ticked to hide the value after saving; untick it for harmless values you want to read back later.
3. Tick **Redeploy the running services now to apply it** to apply it at once, then click **Save variable**. Or click **Redeploy all services** (section **Apply changes**) later. Each service switches over with no downtime.
4. To change a variable, save it again with the same name. **delete** removes one (redeploy to apply).

A service's **Settings** tab lists the project variables it receives, under **From the project**.

```
PUT /api/v1/projects/PROJECT/variables/NAME
{"value": "…", "secret": true}
```

```
platformctl var list shop
platformctl var set shop NAME [--plain]
platformctl var unset shop NAME
platformctl redeploy shop
```

The value of `var set` is read from standard input; the variable is secret unless you pass `--plain`.

### Create and use a managed secret

1. Open **Secrets manager** from the Services menu and click **Create secret**.
2. Enter a **Name** such as `stripe-key` (letters, digits, dots, dashes and underscores, up to 64 characters), an optional **Description**, and the **Value** (up to 64 KB). Click **Create secret**.
3. In a service's or function's environment variables, set a variable to the reference `${secret:stripe-key}`, for example `STRIPE_KEY=${secret:stripe-key}`. The whole value must be the reference.
4. Deploy. The service receives the secret's current value as `STRIPE_KEY`.

```
POST /api/v1/secrets
{"name": "stripe-key", "description": "…", "value": "…"}
```

```
platformctl vault create stripe-key -
```

### Rotate a secret

1. Open the secret.
2. Enter the **New value** under **New version** and click **Save**. Leave the value empty to only change the description.

It becomes the current version, every live service using it is redeployed with no downtime, and functions pick it up at their next start. The last 10 versions are kept.

```
platformctl vault put stripe-key -
```

### Read a value

1. On the secret's page, click **reveal** next to a version (developers and admins).
2. The value shows until you leave the page. Each read is recorded in the audit log.

This helps to check what an older version was before rolling back.

```
GET /api/v1/secrets/NAME?version=N
```

```
platformctl vault get NAME [--version N]
```

### Delete a secret

1. Open the secret, type its name and click **Delete secret**.
2. If it is in use, the page lists the services and functions that use it under **Used by**: remove those references first.

```
platformctl vault delete NAME
```

## Technical reference

### Limits and plans

| Item | Limit | Notes |
| --- | --- | --- |
| Project variable name | up to 64 capitals, digits and underscores, not starting with a digit | `PORT` and `PLATFORM_*` are reserved. |
| Project variable value | 8192 characters | |
| Managed secret name | letters, digits, dots, dashes, underscores; up to 64 characters, starting with a letter or digit | |
| Managed secret description | 200 characters | |
| Managed secret value | 64 KB | |
| Managed secret versions kept | 10 | |
| Managed secrets per organization | Trial 10, Starter 50, Pro 500 | Defaults. The list header shows how many you use. |
| Service secret value | 8192 characters | |

### How values combine

| Rule | Effect |
| --- | --- |
| Project variables | Received by every service of the project and its previews as environment variables. |
| Same name on the service | The service's own environment variable or secret wins. |
| Secret values | Never shown again, not even to admins or the operator. Plain values are visible to members of the organization. |
| When changes apply | At the next deployment of each service. Rolling back to an earlier revision uses the current values. |
| **Redeploy all services** | Redeploys only services that are live and not already deploying. |
| Reference | `${secret:NAME}`, and the whole value must be the reference. |
| Missing reference | The deployment fails with a clear message, and the previous revision keeps serving. |
| Previews | Receive project variables; service secrets are copied when the preview is created (see [Preview environments](/docs/previews)). |

### API

| Method | Path | Role | Purpose |
| --- | --- | --- | --- |
| GET | `/api/v1/projects/PROJECT/variables` | viewer | List project variables (`name`, `secret`, `value` for plain ones only, `updated_at`). |
| PUT | `/api/v1/projects/PROJECT/variables/NAME` | developer | Create or change a variable. |
| DELETE | `/api/v1/projects/PROJECT/variables/NAME` | developer | Delete a variable (`204`). |
| POST | `/api/v1/projects/PROJECT/redeploy` | developer | Redeploy every running service of the project (`202`, `{"redeploying": N}`). |
| GET | `/api/v1/secrets` | viewer | List managed secrets (no values). |
| POST | `/api/v1/secrets` | developer | Create a managed secret (`201`). |
| GET | `/api/v1/secrets/NAME` | developer | Read a secret (`?version=N` for an older version). The read is audited. |
| PUT | `/api/v1/secrets/NAME` | developer | Save a new version or change the description. |
| DELETE | `/api/v1/secrets/NAME` | developer | Delete a secret that is not in use (`204`). |
| PUT | `/api/v1/projects/PROJECT/services/SERVICE/secrets/NAME` | developer | Set a service secret (see [Applications](/docs/applications#add-secrets-to-a-service)). |

Project variable (`PUT …/variables/NAME`):

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `value` | string | yes | The value, 1 to 8192 characters. |
| `secret` | bool | no | Secret unless you send `false`: omitted or `true` hides the value after saving. |

`NAME` uses capitals, digits and underscores (up to 64). The call answers `{"name", "secret", "status"}`; a refused name or value returns `422`.

Managed secret (`POST /api/v1/secrets`):

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | string | yes | Secret name (see the limits above). A name already used returns `409`. |
| `description` | string | no | Description, up to 200 characters. |
| `value` | string | yes | The value, 1 byte to 64 KB. |

Saving a new version (`PUT /api/v1/secrets/NAME`) takes `value` (optional: empty keeps the current version) and `description` (optional: empty keeps the current one), and answers with the secret and `services_redeployed`. The secret's JSON has `name`, `description`, `version`, `reference` (`${secret:NAME}`), `used_by`, `created_at` and `updated_at`; a read adds `value` and `value_version`. Deleting a secret that is still referenced returns `409` and names the services and functions that use it. Past your plan's limit, creating returns `422` ("your plan allows N secrets").

### Command-line tool

| Command | Purpose |
| --- | --- |
| `platformctl var list PROJECT` | Shared project variables (secret values hidden). |
| `platformctl var set PROJECT NAME [--plain]` | Set a shared variable; value from standard input; secret unless `--plain`. |
| `platformctl var unset PROJECT NAME` | Delete a shared variable. |
| `platformctl redeploy PROJECT` | Redeploy every running service of the project. |
| `platformctl secret set PROJECT/SERVICE NAME` | Set a service secret; value from standard input. |
| `platformctl vault list` | List managed secrets. |
| `platformctl vault create NAME VALUE\|-` | Create a managed secret (`-` reads standard input). |
| `platformctl vault get NAME [--version N]` | Read a secret. |
| `platformctl vault put NAME VALUE\|-` | Save a new version. |
| `platformctl vault delete NAME` | Delete a secret. |

### Environment variables

| Variable | Value |
| --- | --- |
| `PORT` | Set by the platform to the service's port. Reserved. |
| `PLATFORM_*` | Reserved names. |
| Your own names | Project variables and resolved `${secret:NAME}` references, passed to each service. |

### Behind the scenes

- Values are encrypted at rest.
- Rotating a managed secret redeploys every live service that uses it, with no downtime; functions pick it up at their next start.
- Each **reveal** of a secret value is recorded in the audit log.
- The list of managed secrets never shows values: only name, description, current version, how many resources use it and when it changed.

## Troubleshooting

### The deployment fails with a missing secret

A variable references `${secret:NAME}` and no secret has that name. Create it or fix the name. The previous revision keeps serving.

### A new value is not used

Changes apply at the next deployment. Tick **Redeploy the running services now to apply it**, click **Redeploy all services**, or redeploy the service.

### I cannot delete a secret

It is in use. The page lists the services and functions under **Used by**: change their variables first.

### The service ignores my project variable

A variable or secret with the same name on the service wins over the project variable. Remove it from the service.

### I cannot see a variable's value

Secret values are never shown again. Save the variable again with a new value, or untick **Secret** for values you want to read back.
