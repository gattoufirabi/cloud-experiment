# Team and roles

Invite your colleagues and give each one the access they need, from read-only to full control. You can also limit a person to a few projects and review who did what in the audit log.

## Overview

Each person who belongs to your organization is a **member**, and each member has one **role**. You manage members in **Access management** › **Members**.

There are four roles. Each role includes everything the roles above it in the table can do:

| Role | Can do |
| --- | --- |
| viewer | See projects, services, deployments, logs, metrics, analytics, scan reports, backups and incidents; search and download logs. |
| developer | Create services, functions, databases, buckets and queues; delete services and functions; change settings, secrets, domains and traffic protection; deploy and roll back; start a backup; set alert thresholds; create metric alarms and Grafana. |
| admin | Create and delete projects; delete databases, buckets and queues; invite and remove members; limit members to projects; create API and registry tokens; manage alert channels, log exports and single sign-on; restore backups; see billing and budgets; read the audit log. |
| owner | Everything, including inviting or removing owners. An organization always keeps at least one owner. |

By default a member sees the whole organization. For contractors and separate teams, you can restrict a developer or a viewer to some projects only. If your team signs in through your company's identity provider, see [Single sign-on](/docs/single-sign-on).

## Use cases

- **Onboard a new developer**: invite them with the **developer** role. They get an email link to create their account.
- **Give a client read-only access**: invite them as **viewer** so they can watch deployments, logs and metrics without changing anything.
- **Work with a contractor on one project**: set their access to that project only, with **developer** or **viewer** on it and **no access** elsewhere.
- **Hand over ownership**: invite a second **owner**, then remove the first one. Only owners can do this.
- **Review a suspicious change**: open the **Audit log** to see who did what, when and from which IP address.
- **Offboard someone**: click **remove** next to their name. Their access ends at once.
- **Delegate administration**: make a trusted colleague **admin** so they can invite people and manage tokens, without owner rights.

## How to use it

### Invite someone

Invite a colleague by email and choose the role they get.

1. Open **Access management** › **Members** (admins).
2. Under **Invite someone**, enter the email address and choose the role.
3. Keep **send it by email** ticked (when the platform sends email) and click **Invite**.
4. The person receives an email with a link to create their account and join. Someone who already has an account signs in with that email to accept.
5. The link is also shown to you once, in case the email does not arrive: send it privately. It works once and expires in 7 days.

Pending invitations are listed with their role and expiry date. Only owners can invite owners. If the platform does not send email, you always get a link to send yourself.

### Remove a member or change a role

Take access away, or give someone a different role.

1. On **Members**, click **remove** next to the member and confirm (admins).
2. To change someone's role, remove them and invite them again with the new role.

You cannot remove yourself, and the last owner cannot be removed. Only owners can remove owners. With single sign-on group lists, roles follow the provider instead (see [Single sign-on](/docs/single-sign-on#groups-and-roles)).

### Limit a member to some projects

Give a contractor or a team access to one or a few projects without showing them the rest of the organization.

1. On **Members**, click **change** in the **Access** column next to a developer or viewer (admins).
2. Choose a role for each project: **no access**, **viewer** or **developer** (never more than their own role).
3. Click **Save access**.

The member then:

- sees and works only in those projects, with the role chosen for each (a viewer cannot deploy);
- gets "not found" for every other project and for organization-wide pages (services list, databases list, billing, access management, storage, queues, secrets, monitoring and so on);
- sees only their projects on the organization's home page and in the in-progress indicator.

The **Access** column shows "whole organization" or the list of projects and roles. Leaving every project on **no access** gives them back the whole organization with their role. Owners and admins always manage the whole organization. Changes are recorded in the audit log.

API tokens can be limited the same way: see [API tokens](/docs/api-and-cli#api-tokens).

### Read the audit log

See who did what, and when.

1. Open **Access management** and follow the **audit log** link at the bottom of the page (admins).
2. Read the list, newest first: when (UTC), who (the member's email), the action, the target, details and the IP address. A change made with an API token is listed under the member who created the token, with the token's name in the details.

## Technical reference

### Roles and permissions

| Role | Level | Notes |
| --- | --- | --- |
| viewer | Read-only | Can be limited to some projects. |
| developer | Build and deploy | Can be limited to some projects. |
| admin | Manage the organization | Always sees the whole organization. |
| owner | Everything | At least one per organization; only owners invite or remove owners. |

A per-project role can be **no access**, **viewer** or **developer**, and never exceeds the member's own role.

### Invitations

| Item | Value |
| --- | --- |
| Link validity | 7 days, single use |
| Who can invite | Admins; only owners can invite owners |
| Link delivery | By email when the platform sends email, and shown once to the inviter |
| Accepting | New people create an account; existing accounts sign in with the invited email |

### Audit log

| Item | Value |
| --- | --- |
| Entries shown | The last 300 actions, newest first |
| Columns | When (UTC), who, action, target, details, IP address |
| Who | The member's email; for a change made with an API token, the member who created the token, with the token's name in the details |
| Integrity | Entries are hash-chained, so they cannot be altered without detection |

### Console endpoints

Members are managed in the console; the REST API has no member endpoints. For reference, the console form endpoints are:

| Method | Path | Purpose |
| --- | --- | --- |
| POST | `/o/<org>/invites` | Create an invitation. |
| POST | `/o/<org>/members/<user>/remove` | Remove a member. |
| POST | `/o/<org>/members/<user>/access` | Save a member's project access. |
| GET | `/o/<org>/audit` | Audit log page. |

## Troubleshooting

### The invitation email did not arrive

The platform may not send email, or the message went to spam. Send the link shown to you after inviting. It works once and expires in 7 days, so invite again if it expired.

### A member sees "not found" on many pages

They are probably limited to some projects. Organization-wide pages are hidden from them. Check the **Access** column on **Members**.
