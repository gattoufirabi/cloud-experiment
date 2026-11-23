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

### Billing and limits

Applications on your own servers use your hardware. They do not count against your plan's CPU, memory, service or instance limits, and are not billed as platform compute. An organization can attach up to 20 servers.

### Security

- The join code works once and expires after one hour. The server then holds its own token, stored readable by root only in `/var/lib/platform-node`.
- Everything the server reports, such as status, logs and measurements, is checked against the projects placed on it, and filed under your organization by the platform, never by the server.
- The tunnel only goes from your server to the platform. The platform never opens a connection to your server, and your server cannot reach the platform's internal services.
- A suspended organization's servers are told to stop everything.
- Joining, moves and removals are recorded in the [audit log](/docs/team).

## Troubleshooting

### The server stays offline

Check the agent with `sudo journalctl -u platform-node -n 50`. The server must reach the console over HTTPS (port 443). Make sure no firewall or proxy blocks outgoing connections. The message `unknown or removed server` means the server was removed in the console: join it again with a new code.

### Visitors get 502 "not reachable right now"

The server is offline, or its tunnel is reconnecting. **Your servers** shows it as offline, or online with the tunnel reconnecting. The application comes back as soon as the server reconnects.

### A deployment fails with "could not pull image"

The server must reach the registry over HTTPS. Images from Docker Hub or other public registries need outgoing access to them too.

### A project cannot move to a server

The project has a managed database, a function, a scheduled job or a public TCP/UDP port. Those run on the platform. Move them to another project, or keep the project on the platform.
