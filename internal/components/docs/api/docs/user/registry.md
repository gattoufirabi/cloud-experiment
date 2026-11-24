# Image registry

Keep your container images in a private space at `{{registry}}/<organization>/`. Push what your CI builds, then deploy it. Nobody outside your organization can read or write it.

## Overview

Your organization has its own private space for container images. Your tools (`docker`, CI pipelines) log in with a registry token, push images under your organization's name, and a service can then be deployed from them. The platform pulls your private images by itself: you never enter registry credentials in a service.

Every image pushed there is scanned for known vulnerabilities (see [Security scanning](/docs/security-scanning)). Images built by [Builds](/docs/builds) are pushed to the same space automatically. To keep the space tidy, old tags are deleted once a day.

Open it from the product menu: **Container registry**. The page has three parts: **Image registry** (the address, the login commands and your scan policy), **Tokens** and **Recent image scans**.

## Use cases

- **CI pushes images**: create a push token, store it in your CI's secret store, and push every build.
- **Deploy a specific version**: set a service's **Image** to `{{registry}}/<organization>/myapp:1.4.2`.
- **Pull-only access**: give another tool a token without **can push**.
- **Rollbacks that work**: pin versions (`myapp:1.4.2`) so previous deployments always find their image.
- **See what is vulnerable**: open **Recent image scans** to review the latest scans of your images.
- **Rotate access**: revoke a token nobody uses, or leaked, and create a new one.

## How to use it

### Create a registry token

A registry token is the password your tools use to log in to the registry. Only admins can create and revoke tokens.

1. On **Container registry**, go to **Tokens**.
2. Enter a name (for example `ci`, up to 60 characters), and keep **can push** ticked if the token must upload images. Untick it for a pull-only token.
3. Click **Create token**. Copy the token right away: it is shown only once.
4. Store it in your CI's secret store.

Tokens expire after one year. The **Last used** column shows whether a token is still in use. Use a push token in CI only, and pull-only tokens elsewhere. Viewers and developers can see the page and the token list, but not create or revoke tokens.

### Revoke a token

1. On **Container registry**, in **Tokens**, click **revoke** next to the token (admins).
2. The token stops working immediately.

### Push and pull images

1. Log in, with your organization's ID as the user name and the token as the password:

```
docker login {{registry}} -u <organization>
```

2. Tag your image under your organization's space and push it:

```
docker tag myapp:1.4.2 {{registry}}/<organization>/myapp:1.4.2
docker push {{registry}}/<organization>/myapp:1.4.2
```

3. Deploy it: set the service's **Image** to `{{registry}}/<organization>/myapp:1.4.2`, or run:

```
platformctl deploy shop/web --image {{registry}}/<organization>/myapp:1.4.2 --wait
```

An image from another organization's space is always refused, with the message "images from the platform registry must be under …".

### Read the image scans

**Recent image scans** lists the latest scans of your organization's images: the image with where the scan came from (a push, a deployment or the nightly check), the findings per severity, and when it ran. Hover an error to see why a scan failed. See [Security scanning](/docs/security-scanning) for what the results mean.

## Technical reference

### Registry and tokens

| Item | Value |
| --- | --- |
| Registry address | `{{registry}}` |
| Your space | `{{registry}}/<organization>/` |
| User name | Your organization's ID |
| Password | A registry token |
| Token lifetime | One year |
| Token name | Up to 60 characters |
| Token rights | **can push** (push and pull) or pull only |
| Who manages tokens | Admins create and revoke; viewers and developers see the list |

### Roles

| Action | Role |
| --- | --- |
| See the page, the token list and scans | viewer and above |
| Create or revoke a token | admin |

### Automatic clean-up

The platform deletes old tags once a day. A tag is deleted only when all of these are true:

- no service is currently set to it;
- none of the last 20 deployments of any service used it;
- it was not pushed or built in the last 30 days (the default; your operator can change this number, see [the admin guide](/docs/admin)).

A tag that points to the same image as a tag that is kept is never deleted.

### Behind the scenes

- Creating and revoking tokens is recorded in the audit log.
- The registry gives each login a short-lived signed token (5 minutes) limited to your organization's space.
- Every push of a tag queues a vulnerability scan.

### Command-line tool

| Command | Purpose |
| --- | --- |
| `platformctl deploy PROJECT/SERVICE --image IMAGE --wait` | Deploy an image from the registry and wait for the result. |

## Troubleshooting

### images from the platform registry must be under …

The image is in another organization's space. Tag and push it under `{{registry}}/<organization>/`.

### docker login fails

Use your organization's ID as the user name and a valid token as the password. Tokens expire after one year and stop working when revoked. A pull-only token cannot push.

### My old tag disappeared

Tags are cleaned up when no service uses them, none of the last 20 deployments used them, and they are older than 30 days. Pin the versions you need to keep as the image of a service.
