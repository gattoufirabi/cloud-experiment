# Your own servers

Run your applications on your own machines, such as a VM at AWS, Azure or any other provider, or hardware you own, while the platform keeps deploying, routing and watching them.

## Overview

You attach a server with a one-line command. A small agent then runs on it and connects out to the platform over HTTPS. Each project you place on that server has its applications deployed there, in Docker, instead of on the platform's servers.

Visitors notice no difference. They reach the applications at the same addresses, through the platform's proxy, with HTTPS, [custom domains](/docs/domains), [traffic protection](/docs/traffic-protection), the [edge cache](/docs/cdn) and [analytics](/docs/analytics). The proxy forwards their requests to your server through connections the server itself opened. Your server needs no open port and no public address.

The agent on your server is not trusted with anything beyond your organization:

| It gets | It never gets |
| --- | --- |
| A token for this server only, which you can revoke at any time | Access to the platform's database |
| The applications of the projects you place on it, with their variables and secrets | The platform's encryption key, or anything of another organization |
| A pull-only token for your organization's images in the [registry](/docs/registry) | Push access, or other organizations' images |

## Use cases

- **Data that must stay on your hardware**: keep an application next to data you cannot move, while it stays published like the others.
- **Hardware you already pay for**: use reserved instances, credits or spare servers at AWS or Azure for applications that need a lot of CPU or memory.
- **Special hardware**: run on machines with GPUs, lots of memory or local disks.
- **One console for everything**: deploy, roll back, read logs and manage domains in the same way, wherever an application runs.

## How to use it

### Attach a server

You need a Linux server (x86-64) with Docker running and outgoing HTTPS. Admins of the organization can attach servers.

1. Open **Your servers** in the console and enter a name under **Add a server**, for example `aws-eu-1`.
2. Copy the command shown. It contains a join code that works once, within one hour, and is shown only now.
3. Run it on the server as root:

```
curl -fsSL {{console}}/node/install.sh | sudo sh -s -- plj_...
```

The script downloads the agent, joins the server to your organization and starts it as the `platform-node` system service. The server appears as **online** within a minute.

On AWS, use any EC2 instance with Docker, for example Amazon Linux or Ubuntu. On Azure, use any Linux VM. The security group or network security group only needs outgoing HTTPS (port 443); no incoming rule is needed.

### Run a project on your server

1. Under **Where each project runs**, choose the project and the server, then **Move project**.
2. Its running applications are deployed again on your server. New deployments of the project's applications then go there, whether they come from the console, a git push, the [API](/docs/api-and-cli) or [Terraform](/docs/terraform).

To bring a project back, move it to **the platform**. A move deploys the applications again and causes a short interruption, so do it at a quiet time.

### Run builds on your servers

Your servers can also run your builds from git and zip archives, like self-hosted agents in Azure Pipelines or GitHub Actions. Builds then use your hardware, are not billed as build minutes, and can reach git servers on your own network, such as an on-premises GitLab.

1. In the **Servers** table, tick **runs builds** for the server and select **Save**. Optionally give it labels, such as `gpu`, `arm64` or `large`, separated by commas.
2. On a service's **Builds** tab, under **Source**, set **Build on** to **your build servers**. Under **Server labels**, list the labels a server must have to take this service's builds; leave it empty for any build server.

A server takes one build at a time. To run several builds at once, attach more servers. A server does not need to run any project to run builds.

### Remove a server

Move its projects elsewhere first, then select **Remove**. Its token and its registry token stop working at once. Then uninstall the agent from the machine:

```
sudo systemctl disable --now platform-node
sudo rm -rf /usr/local/bin/platform-node /etc/systemd/system/platform-node.service /var/lib/platform-node
```

## Technical reference

### What runs on your server

| Feature | On your own server |
| --- | --- |
| Web services and workers | Yes, with several instances, health checks and blue/green deployments |
| Variables, secrets and secrets-manager references | Yes, decrypted by the platform and handed to your server over HTTPS |
| Persistent folder | Yes, a Docker volume on your server (one instance) |
| Logs | Yes, in the console's log search, as for the platform |
| CPU and memory measured | Yes, on the application's page |
| Custom domains, HTTPS, traffic protection, edge cache, analytics | Yes, at the platform's proxy |
| Private names between the project's services | Yes, `<service>.<project>.internal` on the server's project network |
| Managed databases, functions, scheduled jobs | No: they run on the platform. Keep them in another project and connect to them with their public endpoint |
| Public TCP/UDP ports | Not yet |
| Metric charts (Prometheus scraping, container charts) | Not yet |

### How a request reaches your server

1. The visitor reaches the platform's proxy, which applies the application's rules.
2. The proxy hands the request to the tunnel service, with the deployment it is for.
3. The tunnel service takes one of the connections your server keeps open (four at all times), and asks the agent to open a stream to an instance of that deployment.
4. The agent connects to the instance on the project's Docker network, and the request and the response flow through.

Each connection carries one request. The agent opens a new connection as soon as one is used, and replaces idle ones every few minutes.

### Deployments

The agent asks every 5 seconds for what it must run. For a new deployment, it pulls the image, starts the instances, waits until each answers its health path (any status below 500, within 90 seconds), then reports it live. The proxy then sends traffic to the new version. The previous version keeps running for 20 seconds, then stops. A failed deployment shows its reason and the last log lines, as on the platform, and the previous version keeps serving.

Containers are created with the application's CPU and memory limits, without swap. All capabilities are dropped except the few common images need, and `no-new-privileges` is set. The containers use Docker's default runtime, since gVisor is usually not installed on your servers.

### How a build runs on your server

1. The agent asks every 5 seconds for a waiting build of your organization that it may run: the service builds on your servers, and the server has every label the service asks for.
2. It gets the source settings, the repository's read token if there is one, and a push token. The push token only works for that service's repository in your registry space, and only while the build runs.
3. It runs the same steps as the platform: a helper container clones the branch (or unpacks the archive), then rootless BuildKit builds the Dockerfile, or uses Railpack when there is none. The helper image is built on your server at the first build.
4. It pushes the image, sends the log as it goes, and reports the result. The platform records the image and starts the deployment.

Build containers drop all capabilities except the few rootless BuildKit needs, and run on their own Docker network. On your server, BuildKit runs without the default seccomp and AppArmor profiles, which block the mounts it needs; it still runs as a non-root user. If a server stops reporting for 3 minutes, its build fails and can be started again.

### Billing and limits

Applications on your own servers use your hardware. They do not count against your plan's CPU, memory, service or instance limits, and are not billed as platform compute. Builds on your servers are not billed as build minutes. An organization can attach up to 20 servers.

### Security

- The join code works once and expires after one hour. The server then holds its own token, stored readable by root only in `/var/lib/platform-node`.
- Everything the server reports, such as status, logs and measurements, is checked against the projects placed on it, and filed under your organization by the platform, never by the server.
- The tunnel only goes from your server to the platform. The platform never opens a connection to your server, and your server cannot reach the platform's internal services.
- A suspended organization's servers are told to stop everything.
- Joining, moves, build settings, builds a server took and removals are recorded in the [audit log](/docs/team).

## Troubleshooting

### The server stays offline

Check the agent with `sudo journalctl -u platform-node -n 50`. The server must reach the console over HTTPS (port 443). Make sure no firewall or proxy blocks outgoing connections. The message `unknown or removed server` means the server was removed in the console: join it again with a new code.

### Visitors get 502 "not reachable right now"

The server is offline, or its tunnel is reconnecting. **Your servers** shows it as offline, or online with the tunnel reconnecting. The application comes back as soon as the server reconnects.

### A deployment fails with "could not pull image"

The server must reach the registry over HTTPS. Images from Docker Hub or other public registries need outgoing access to them too.

### Builds stay "waiting for a build server"

No server can take them. Check that a server is online, has **runs builds** ticked, and has every label listed under the service's **Server labels**. A build no server takes within an hour fails.

### A build on my server fails at "build helper image" or with "operation not permitted"

The server must reach Alpine's package mirror and GitHub (to download Railpack) at its first build. BuildKit needs user namespaces: on hosts where they are turned off (`kernel.unprivileged_userns_clone=0`, or `user.max_user_namespaces=0`), turn them on.

### A project cannot move to a server

The project has a managed database, a function, a scheduled job or a public TCP/UDP port. Those run on the platform. Move them to another project, or keep the project on the platform.
