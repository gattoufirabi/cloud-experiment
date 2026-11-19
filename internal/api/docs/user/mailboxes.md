# Mail hosting

Host real mailboxes on your own domains, such as `anna@example.com`, and use them from any mail app or in the browser. Mailboxes come with aliases, a catch-all and spam filtering.

## Overview

Mail hosting is for your team's everyday email: each person gets a mailbox with its own address and password, and reads mail in Outlook, Apple Mail, Thunderbird, Gmail's app, on a phone, or in the built-in webmail. For messages your application sends, use [Transactional email](/docs/email) instead.

Open **Mailboxes** in the Services menu. The page shows how many mailboxes and how much storage you use out of your plan.

- A **mail domain** is a domain you have verified and turned mail on for. Mail for it is then delivered to the platform.
- A **mailbox** has an address, an optional display name, a storage size and a password.
- **Aliases** are extra addresses delivered to the same mailbox. A **catch-all** receives mail for addresses that do not exist.
- Users reach their mail over IMAP, POP3 and SMTP, or in the webmail, without a platform account.
- Your plan sets the number of mailboxes and the total storage you can give them.

## Use cases

- **Team email**: one mailbox per person at `example.com`, used from their phones and computers.
- **Role addresses**: add an alias such as `sales@example.com` to a mailbox.
- **Never miss a typo**: set a catch-all so mail to unknown addresses lands in one mailbox.
- **Branded mail server**: use `mail.example.com` as the server name and webmail address.
- **Move from another provider**: turn mail on, check the MX records and remove the old ones.
- **Mail in the browser**: share the webmail address with users who do not want to install anything.

## How to use it

### Add a mail domain

Declare the domain that will receive mail.

1. On the **Mailboxes** page, click **Add a domain**, enter it (`example.com`) and click **Add domain** (developers).
2. The domain is verified like a sending domain. If its DNS is hosted on the platform (**DNS**, see [DNS hosting](/docs/dns)), it is verified at once and every record is written for you. Otherwise create the verification record shown on its sending domain page (see [Transactional email](/docs/email)); the domain is checked every few minutes.
3. Once it is verified, open the domain and click **Turn on mail**. Its mail is then delivered here.

The domain list shows each domain's status: **waiting for DNS verification**, **verified: ready to turn on**, or **receiving mail**.

```
POST /api/v1/mail/domains
{"domain": "example.com"}
```

```
POST /api/v1/mail/domains/DOMAIN/activate
```

### Create the mail DNS records

The domain's **DNS records** tab lists what mail needs: MX, DKIM, SPF, DMARC and SRV (see [Mail DNS records](#mail-dns-records)).

1. In a hosted zone they are written automatically, and your own SPF or DMARC record is kept.
2. Otherwise create them at your DNS provider, and merge the SPF value into an existing SPF record.
3. Remove MX records that point elsewhere (for example your previous provider): mail goes to one place. The page warns you while they are there.
4. In a hosted zone, remove them, then click **Write them again**.

### Create mailboxes

1. On the domain's **Mailboxes** tab, under **Create a mailbox**, enter the **Address** (the part before `@`, such as `anna`).
2. Optionally enter a **Name** shown to recipients (`Anna Martin`).
3. Set a **Storage** size in MB (at least 50; 1024 by default).
4. Enter a **Password** of at least 12 characters, or leave it empty to get a strong one, shown once.
5. Click **Create mailbox**, and give the address and password to its owner.

Type in **Find a mailbox** to filter the list.

```
POST /api/v1/mail/domains/DOMAIN/mailboxes
{"name": "anna", "display_name": "Anna Martin", "quota_mb": 1024}
```

### Manage a mailbox

Each mailbox shows its aliases and its storage used. Its **manage** menu lets you:

1. Change the **Name** and **Storage**, then click **Save**.
2. Click **Add alias** to add another address delivered to the same mailbox, such as `sales@example.com`. Click **x** next to an alias to remove it.
3. Click **Set new password** (leave it empty to generate one). Mail apps must sign in again.
4. Click **Delete with its mail** and type the address to confirm. This cannot be undone.

```
PATCH /api/v1/mail/domains/DOMAIN/mailboxes/NAME
POST /api/v1/mail/domains/DOMAIN/mailboxes/NAME/password
POST /api/v1/mail/domains/DOMAIN/mailboxes/NAME/aliases
DELETE /api/v1/mail/domains/DOMAIN/aliases/ALIAS
DELETE /api/v1/mail/domains/DOMAIN/mailboxes/NAME
```

### Set a catch-all

A catch-all receives mail sent to addresses of the domain that do not exist (a typo, an old address), instead of refusing it.

1. On the domain's **Settings** tab, under **Catch-all**, choose a mailbox.
2. Click **Save**. To stop, choose **Off: refuse unknown addresses**.

Catch-alls attract spam: use one only if you need it.

```
PUT /api/v1/mail/domains/DOMAIN/catch-all
{"mailbox": "anna"}
```

### Use your own server name

By default, mail apps and the webmail use the platform's mail server name. You can use a name of your own domain instead, such as `mail.example.com`.

1. On the domain's **Settings** tab, under **Your own server name**, enter the first part (`mail`) and click **Use this name**.
2. Point that name to the platform with an A record (the IP is shown; in a hosted zone the record is written for you).
3. Its HTTPS certificate is issued within minutes. The webmail then opens at `https://mail.example.com` with your domain on its sign-in page, and IMAP, POP3 and SMTP answer under that name.

Click **Back to the platform's name** to undo it.

```
PUT /api/v1/mail/domains/DOMAIN/server-name
{"name": "mail"}
```

An empty name goes back to the platform's name.

### Use the webmail

Anyone with a mailbox can read and send mail in the browser, without installing anything and without a platform account. The webmail address is shown on the **Mailboxes** page and on the **Mail app settings** tab: share it with your users. They sign in with the full address and the mailbox's password.

In the webmail they can:

- open folders and **search** their mail;
- read messages (remote images and scripts in messages stay blocked) and download attachments;
- write, **reply**, **Reply all** and **Forward**, with Cc, Bcc and attached files;
- star messages, mark them **Unread**, and move them to **Archive**, **Spam** or **Delete** (the trash), one by one or several at once;
- in the trash, **Restore** messages or **Delete for good**;
- switch between **Light**, **Dark** and **Auto** appearance.

### Set up a mail app

Most mail apps find the settings by themselves from the address. If yours asks, open the domain's **Mail app settings** tab and copy the values (see [Mail app settings](#mail-app-settings)).

### Remove a domain

1. On the domain's **Settings** tab, click **Remove the domain**.
2. Type the domain to confirm.

Every mailbox of the domain is deleted with its mail, and mail for it is no longer received. The domain stays a sending domain for [Transactional email](/docs/email).

```
DELETE /api/v1/mail/domains/DOMAIN
```

## Technical reference

### Limits and plans

| Item | Limit/Default | Notes |
| --- | --- | --- |
| Mailboxes | set by your plan | default 10; trial 2; pro 100 |
| Total mailbox storage | set by your plan | default 10240 MB; trial 2048 MB; pro 102400 MB |
| Storage per mailbox | 50 MB to 1048576 MB | 1024 MB by default |
| Display name | 100 characters | |
| Address (the part before `@`) | letters, digits, dots, dashes, underscores | |
| Password | 12 to 200 characters | leave empty to generate one (20 characters) |

The page shows what you use.

### Mail DNS records

| Record | Purpose |
| --- | --- |
| MX | where mail for the domain is delivered |
| DKIM | keys that sign outgoing mail (they appear once mail is turned on) |
| SPF | tells other servers which senders to trust (recommended) |
| DMARC | tells receivers what to do with mail that fails the checks (recommended) |
| SRV | lets mail apps find the servers by themselves from an address |

### Mail app settings

| Service | Port | Security |
| --- | --- | --- |
| Incoming (IMAP) | 993 | SSL/TLS |
| Incoming (POP3) | 995 | SSL/TLS |
| Outgoing (SMTP) | 587 | STARTTLS, with sign-in |

All on the server shown on the tab (or your own server name). The user name is the full address, such as `anna@example.com`; the password is the mailbox's password.

### Domain statuses

| Status | Meaning |
| --- | --- |
| waiting for DNS verification | the verification record is not visible yet |
| verified: ready to turn on | click **Turn on mail** |
| receiving mail | mail is delivered here |

### API

`DOMAIN` is a domain such as `example.com`. Listing calls need the viewer role; the other calls need the developer role. A mailbox can only be created on a domain that is receiving mail (turn it on first). Deletions answer `204`. A domain shows `pending` (verify the sending domain) or `active` in the API.

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/v1/mail/domains` | List mail domains |
| POST | `/api/v1/mail/domains` | Add a domain (`{"domain": "example.com"}`) |
| GET | `/api/v1/mail/domains/DOMAIN` | Show a domain |
| DELETE | `/api/v1/mail/domains/DOMAIN` | Remove a domain |
| POST | `/api/v1/mail/domains/DOMAIN/activate` | Turn on mail |
| GET | `/api/v1/mail/domains/DOMAIN/mailboxes` | List mailboxes |
| POST | `/api/v1/mail/domains/DOMAIN/mailboxes` | Create a mailbox |
| PATCH | `/api/v1/mail/domains/DOMAIN/mailboxes/NAME` | Change name or storage |
| DELETE | `/api/v1/mail/domains/DOMAIN/mailboxes/NAME` | Delete a mailbox with its mail |
| POST | `/api/v1/mail/domains/DOMAIN/mailboxes/NAME/password` | Set a new password |
| POST | `/api/v1/mail/domains/DOMAIN/mailboxes/NAME/aliases` | Add an alias |
| DELETE | `/api/v1/mail/domains/DOMAIN/aliases/ALIAS` | Remove an alias |
| PUT | `/api/v1/mail/domains/DOMAIN/catch-all` | Set or turn off the catch-all |
| PUT | `/api/v1/mail/domains/DOMAIN/server-name` | Set your own server name |

#### Create a mailbox

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | string | yes | the part before `@` |
| `display_name` | string | no | name shown to recipients |
| `quota_mb` | number | no | storage in MB (50 to 1048576; 1024 by default) |
| `password` | string | no | choose it yourself; otherwise the response carries the generated password once |

#### Other bodies

| Call | Field | Description |
| --- | --- | --- |
| `PATCH .../mailboxes/NAME` | `display_name`, `quota_mb` | change name or storage (both optional; what you leave out is kept) |
| `POST .../mailboxes/NAME/password` | `password` | optional: empty or no body generates one, returned once |
| `POST .../mailboxes/NAME/aliases` | `alias` | the alias, such as `sales` (answer `201`) |
| `PUT .../catch-all` | `mailbox` | the mailbox name; empty turns it off |
| `PUT .../server-name` | `name` | first part of the name, such as `mail`; empty goes back to the platform's name |

## Troubleshooting

### Mail does not arrive

Check the MX records on the **DNS records** tab. Remove MX records that point to a previous provider: mail goes to one place. In a hosted zone, remove them, then click **Write them again**.

### The domain is not ready to turn on

The verification record is not visible yet. In a hosted zone it is written for you; otherwise create it at your DNS provider and wait a few minutes.

### My own server name does not work

Check the A record points to the IP shown on the **Settings** tab. The HTTPS certificate is issued within minutes after that.

### A mail app cannot sign in

Use the full address as user name and the mailbox's password. After **Set new password**, mail apps must sign in again.
