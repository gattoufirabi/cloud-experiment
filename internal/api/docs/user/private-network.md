# Private network

Let your services call each other over a private network, with no public address needed. Each web service answers on a private name that only your own projects can reach.

## Overview

Services often need to call each other: a website calling its API, a worker calling an internal service. Every web service answers on a private name, `<service>.<project>.internal`, over plain HTTP on port 80. From your code, call `http://api.shop.internal/orders` instead of the public address. The traffic stays on the server's private networks, it is faster, and the service does not even need to be public.

Within a project, services reach each other by these names at once. Between projects, an admin connects them.

Requests from other projects, other organizations and the internet are refused, even when they send the right host name.

## Use cases

- **Website to API**: the website calls `http://api.shop.internal` while only the website is public.
- **Worker to internal service**: a worker calls a private service without opening it to the internet.
- **Two projects, one product**: connect the `shop` and `billing` projects so their services call each other.
- **Functions calling services**: [functions](/docs/functions) reach the services of their project by these names too.
- **Faster calls**: skip the public proxy and TLS for traffic between your own services.

## How to use it

### Find the private names

1. Open the project's **Settings** tab. **Private network** lists this project's names.
2. Open a service's **Domains** tab to see its own private name.
3. In your code, call `http://<service>.<project>.internal/...`.

### Connect two projects

Connecting needs the admin role.

1. Open one project's **Settings** tab.
2. Under **Private network**, choose the other project and click **Connect**.
3. Services of both projects can now call each other: connections go both ways. The table lists each connected project and its private names.

API:

```
GET  /api/v1/projects/PROJECT/connections
POST /api/v1/projects/PROJECT/connections
{"project": "other"}
```

### Disconnect projects

1. Under **Private network**, click **disconnect** next to the project.
2. Confirm.

API: `DELETE /api/v1/projects/PROJECT/connections/OTHER`.

## Technical reference

### Private names

| Item | Value |
| --- | --- |
| Name | `<service>.<project>.internal` |
| Protocol and port | Plain HTTP, port 80 |
| Example | `http://api.shop.internal/orders` |
| Reachable from | Services of the same project, and of connected projects |

### API

| Method | Path | Role | Purpose |
| --- | --- | --- | --- |
| GET | `/api/v1/projects/PROJECT/connections` | viewer | List the connected projects. |
| POST | `/api/v1/projects/PROJECT/connections` | admin | Connect another project. |
| DELETE | `/api/v1/projects/PROJECT/connections/OTHER` | admin | Disconnect a project. |

Body of the POST: `project` (string, required), the slug of the other project. It answers `201` with `{"redeploying": N}`, the number of services redeployed. Connecting a project to itself is refused (`422`), an unknown project gives `404`, and projects already connected give `409`. Disconnecting answers `204`.

### Behind the scenes

- Connecting or disconnecting redeploys both projects' services, with no downtime, so their containers learn the names. A service created later is known to the other services after their next deployment.
- Databases stay reachable from their own project only.
- On an installation with several servers, private names work for projects on the main server.

## Troubleshooting

### A new service is not found by its neighbours

Services learn the names when they are deployed. Redeploy the services that call it.

### The call is refused

The caller must be in the same project or in a connected project of your organization. Other projects, other organizations and the internet are refused, even with the right host name.
