# Security scanning

Find out when a package in your container image has a known security hole, and which version fixes it.

## Overview

The platform checks your container images against a public database of known vulnerabilities (CVEs). Scans run automatically: you do not start them.

Each finding has a severity, a vulnerability ID, the affected package, the installed version and the version that fixes it. Your organization's scan policy decides whether findings are only shown, or whether a critical one blocks a deployment.

The platform operator sets the policy (see [the admin guide](/docs/admin)).

## Use cases

- **Know before you ship**: see the findings of the image of your latest deployment on the service's **Security** tab.
- **Catch new vulnerabilities in running images**: the nightly scan alerts you with `critical_vulns` when a running image gets a critical finding.
- **Stop risky images**: ask your operator for the `block-critical` policy so a critical vulnerability stops a deployment.
- **Review all images**: open **Container registry**, then **Recent image scans**, for the latest scans.
- **Fix findings fast**: rebuild on the latest patch of the base image.

## How to use it

### See the results of a service

1. Open the service's **Security** tab. Under **Vulnerability scan** you see the image of the latest deployment and the number of findings per severity.
2. Below, up to 25 findings are listed, most severe first, with the vulnerability ID, the package, the installed version and the version that fixes it ("no fix yet" when there is none).
3. For a quick count, open the service's **Overview** tab, under **At a glance**.

### See the scans of all your images

1. Open **Container registry** in the product menu.
2. Go to **Recent image scans**. It lists the latest scans: the image with where the scan came from, the findings per severity, and when it ran. Hover an error to see why a scan failed.

### Check the scan policy

The **Container registry** page shows which policy applies to your organization. Your platform operator changes it; see [the admin guide](/docs/admin).

### Fix findings

Most findings come from the base image.

1. Rebuild your image on the latest patch version of its base image (for example `node:22-alpine` pulled again).
2. Push it to your [registry](/docs/registry) and redeploy.
3. Check the **Security** tab again.

### Get alerted about critical vulnerabilities

A `critical_vulns` alert is sent (see [Alerts](/docs/alerts)) when the nightly scan finds critical vulnerabilities in a running image.

## Technical reference

### When images are scanned

| Moment | Detail |
| --- | --- |
| On push | Every tag pushed to your private registry. |
| On deployment | Before the application starts. A result from the last 24 hours for the same image is reused. |
| Every night | Images that are running, because new vulnerabilities are published every day. Critical findings send a `critical_vulns` alert. |

### Where results appear

| Place | What it shows |
| --- | --- |
| Service **Security** tab, **Vulnerability scan** | The latest deployment's image, the findings per severity, and up to 25 findings, most severe first. |
| Service **Overview** tab, **At a glance** | The counts per severity. |
| **Container registry**, **Recent image scans** | The latest scans of all your images. |

### Scan policy

| Policy | Effect |
| --- | --- |
| warn (default) | Findings are shown; deployments are never blocked. |
| block-critical | An image with a critical vulnerability is refused, and the deployment shows which ones ("blocked by security policy"). If the scanner is unavailable, deployments fail rather than go out unscanned. |

### Alerts

| Alert | When |
| --- | --- |
| `critical_vulns` | The nightly scan finds critical vulnerabilities in a running image. |

## Troubleshooting

### A deployment is blocked by security policy

Your organization uses `block-critical` and the image has a critical vulnerability. The deployment shows which ones. Rebuild on a patched base image and push again, or ask your operator about the policy.

### Deployments fail and the scanner is unavailable

With `block-critical`, deployments fail rather than go out unscanned when the scanner cannot run. Try again later or contact your platform operator.

### A finding has no fix yet

The listing shows "no fix yet" when no patched version exists. Check again after the next base image update.
