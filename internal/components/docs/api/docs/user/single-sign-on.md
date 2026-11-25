# Single sign-on

Single sign-on (SSO) lets your team sign in with your company's identity provider instead of a separate password. It supports Google Workspace, Microsoft Entra ID, Okta, Keycloak, Authentik or any OpenID Connect provider.

## Overview

With SSO, people who leave the company lose access as soon as you disable them at your provider. Roles can follow your provider's groups, so you manage who is an admin or a developer in one place.

SSO uses OpenID Connect. You register the platform as a "web" application at your provider, then enter the provider's issuer URL, client ID and client secret on the platform. When single sign-on is turned on, the platform checks that it can reach your provider each time you save.

- People are matched to your organization by the **email domain** (for example `example.com`).
- Accounts are created at the first sign-in, with a default role or the role of their groups.
- You can require SSO so that nobody signs in with a password for your domains.
- Admins set it up in **Access management** › **Single sign-on**.

## Use cases

- **One login for the company**: enter your domains and let everyone use **Sign in with SSO**.
- **Automatic offboarding**: disable the person at your provider; they cannot sign in again.
- **Roles from groups**: fill **Admin groups**, **Developer groups** and **Viewer groups** so roles follow your provider's groups at each sign-in.
- **Enforce your provider's two-step checks**: tick **Require it** so password sign-in is refused for your domains.
- **Safe default role**: set **Role of new members** to viewer so new people can only look until you promote them.
- **Audit sign-ins**: every SSO sign-in and role change is recorded in the audit log.

## How to use it

### Set up single sign-on

Connect your identity provider to your organization.

1. **At your provider**, create an OpenID Connect application of the "web" type. Register the redirect URI shown on the tab (`{{console}}/sso/callback`) and the scopes `openid email profile`. Note the issuer URL, the client ID and the client secret.
2. **On the platform**, open **Access management** › **Single sign-on** (admins). Enter the **Issuer URL** (for example `https://accounts.google.com` or `https://login.microsoftonline.com/<tenant>/v2.0`), the **Client ID** and the **Client secret**.
3. Enter your **Email domains**, separated by commas (for example `example.com`). People with an email of these domains sign in to this organization.
4. Choose the **Role of new members**: viewer, developer or admin.
5. Optionally, map provider groups to roles (see below).
6. Tick **Turn on single sign-on** and click **Save single sign-on**. With the box ticked, the platform checks that it can reach your provider when you save.

To change the client secret later, type the new one. Leave the box empty to keep the saved one.

### Groups and roles

Let your provider decide each member's role.

1. Enter the **Groups claim**: the claim of the ID token that lists groups (`groups` by default).
2. Fill **Admin groups**, **Developer groups** and **Viewer groups** with group names, separated by commas.
3. Click **Save single sign-on**.

When you fill in group lists, a member's role follows the provider at each sign-in. Owners keep their role.

### Require single sign-on

Make everyone go through your provider.

1. Tick **Require it: refuse password sign-in for emails of these domains**.
2. Click **Save single sign-on**.

Everyone then signs in through your provider and its own two-step checks. Owners keep their password, so a broken provider cannot lock you out.

### Sign in with SSO

This is what your members do.

1. On the login page, enter your work email (or the organization ID) in the single sign-on box and click **Sign in with SSO**.
2. Sign in at your provider and come back to the console.
3. Your account is created at your first sign-in, with the role of new members (or the role of your groups).

The tab's **status** line shows whether SSO is on and required.

## Technical reference

### Settings

| Setting | Values | Default | Effect |
| --- | --- | --- | --- |
| Issuer URL | OpenID Connect issuer URL | none | The provider the platform talks to. Must be an `https://` address on a public host. Checked when you save with single sign-on turned on. |
| Client ID | Text | none | Identifies the platform at your provider. |
| Client secret | Text | none | Leave empty to keep the saved secret. |
| Email domains | Comma-separated domains | none | Emails of these domains sign in to this organization. |
| Role of new members | viewer, developer, admin | developer | Role given at first sign-in when no group matches. |
| Groups claim | ID token claim name | `groups` | Where the platform reads the member's groups. |
| Admin groups, Developer groups, Viewer groups | Comma-separated group names | empty | Role follows the groups at each sign-in; owners keep theirs. |
| Turn on single sign-on | On or off | off | Enables **Sign in with SSO** for your domains. |
| Require it | On or off | off | Refuses password sign-in for emails of your domains (owners excepted). Needs single sign-on turned on and at least one domain. |

### Provider configuration

| Item | Value |
| --- | --- |
| Application type | Web (OpenID Connect) |
| Redirect URI | `{{console}}/sso/callback` |
| Scopes | `openid email profile` |

### Rules

- A domain belongs to one organization only, and each domain must be a valid domain name with a dot.
- Sign-in attempts are limited to 30 per 15 minutes per address.
- An email outside your domains is refused, and so is an email the provider has not verified.
- Sign-in links are single-use and expire after 10 minutes.
- Every SSO sign-in and every role change it makes is recorded in the audit log.

### Console endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| POST | `/o/<org>/sso` | Save the SSO settings (form fields: `issuer`, `client_id`, `client_secret`, `domains`, `default_role`, `groups_claim`, `admin_groups`, `developer_groups`, `viewer_groups`, `enabled`, `enforce`). |
| POST | `/sso/start` | Start a sign-in (field `who`: the work email). |
| GET | `/sso/callback` | Return address used by your provider. |

## Troubleshooting

### The provider could not be reached

The platform cannot contact your provider when you save with single sign-on turned on. Check the issuer URL, and that the provider is reachable from the internet.

### Your email is not in this organization's domains

The email is outside the **Email domains** list. Sign in with your work email, or ask an admin to add your domain in **Single sign-on**. An email the provider has not verified is also refused.

### A sign-in link stopped working

Sign-in links are single-use and expire after 10 minutes. Start again with **Sign in with SSO**.
