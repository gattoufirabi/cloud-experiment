# Transactional email

Send transactional email (receipts, password resets, sign-up confirmations, notifications) from your own domain, through an API or SMTP. Messages are signed with your domain's DKIM key so mail providers trust them.

## Overview

Email lets your applications send messages from addresses at domains you have proven you own. The platform delivers each message itself, straight to the recipient's mail server, signed with your domain's DKIM key. With the SPF and DMARC records in place, mail providers know the platform may send for your domain, so your messages do not land in spam.

Open **Email** in the Services menu. The top of the page shows how many emails you sent in the last 24 hours, out of your plan's daily limit. The page has five tabs: **Sending log**, **Domains**, **Use from code**, **Suppressions** and **Webhook**.

- You send through an HTTP API or through SMTP.
- Addresses that bounce for good go on a suppression list automatically.
- A signed delivery webhook tells you the result for each recipient.
- Your plan sets the number of emails per 24 hours (each recipient counts) and of sending domains. Emails are billed per delivered recipient, per thousand.
- To receive mail at your domain, see [Mail hosting](/docs/mailboxes).

## Use cases

- **Receipts and order confirmations**: send them from `orders@example.com` with the API and an HTML body.
- **Password resets and sign-up confirmations**: connect your service with **Connect** and send with the injected token.
- **Invoices as attachments**: add a PDF in `attachments`.
- **WordPress, Django or Rails mail**: point the software to the SMTP settings on the **Use from code** tab.
- **Flag bad addresses**: use the delivery webhook to mark a customer's address as invalid in your database.
- **Check a new domain**: use **Send a test email** before you write any code.
- **Stop mail to someone**: add the address on the **Suppressions** tab.

## How to use it

### Add a sending domain

You can send only from domains you have proven you own.

1. On the **Domains** tab, under **Add domain**, enter a domain such as `example.com` or `mail.example.com` and click **Add domain** (developers).
2. Open the domain and create, at your DNS provider, the TXT records its page lists (see [DNS records](#dns-records)).
3. The platform checks the records every minute, or click **Check now**. DNS changes can take a few minutes to be visible.
4. Once the domain shows **verified**, send from any address at it.

If the domain's DNS is hosted on the platform (see [DNS hosting](/docs/dns)), the records are written for you and the domain is verified at once: the page says **automatic**.

The **Domains** tab shows each domain's status (**verified**, **waiting for DNS**, or **records removed**) and when it was last checked.

```
POST /api/v1/email/domains
{"domain": "example.com"}
```

### Connect a service to Email

Give your application what it needs to send.

1. On the **Use from code** tab, under **Connect to a service**, choose the service and click **Connect**.
2. The service gets the secrets `EMAIL_API_URL` and `EMAIL_TOKEN` (and, when SMTP is offered, `SMTP_HOST`, `SMTP_PORT`, `SMTP_USERNAME`, `SMTP_PASSWORD`).
3. Redeploy the service.

The token can only send email and read the status of messages: nothing else.

```
POST /api/v1/email/attach
{"service": "project/service"}
```

### Send with the API

Send a message from your code.

```
curl -X POST -H "Authorization: Bearer $EMAIL_TOKEN" -H 'Content-Type: application/json' $EMAIL_API_URL \
  -d '{"from": "Shop <hello@example.com>", "to": ["buyer@example.org"], "reply_to": "support@example.com",
       "subject": "Your order", "text": "Thanks!", "html": "<p>Thanks!</p>"}'
```

The answer is `202` with the message's id. To follow it, call `GET /api/v1/email/messages/ID`. See [Send an email](#send-an-email) for all fields and [Attachments](#attachments).

### Send with SMTP

For software that only speaks SMTP (WordPress, Django, Rails, Nodemailer...), the **Use from code** tab shows the settings, when the platform offers SMTP.

1. Open the **Use from code** tab and copy the host.
2. In your software, use port **465** with TLS from the start ("SSL/TLS", not STARTTLS).
3. Set any user name (such as `token`) and your email token (`EMAIL_TOKEN`) as the password.

The same rules apply as with the API: the From address must use one of your verified domains, and the same limits count. Bcc headers are removed before sending. Messages sent this way show **SMTP** in the sending log.

### Send a test email

Check that a domain works before you write any code.

1. At the bottom of the **Sending log** tab (once a domain is verified), fill in **From**, **To**, **Subject** and **Text**.
2. Click **Send**.

### Read the sending log

The **Sending log** tab lists the last 100 messages: when they were queued, from, to, subject, status, and how many recipients were delivered, bounced, suppressed or failed. Type in **Find messages** to filter. A message still being retried shows why.

### Manage suppressions

On the **Suppressions** tab:

1. See each address, why it was added (**bounced** with the server's answer, or added by hand) and since when.
2. To add an address yourself (someone asked not to get mail, a known bad address), type it and click **Suppress**.
3. Click **remove** once you know an address works again.

```
POST /api/v1/email/suppressions
DELETE /api/v1/email/suppressions/ADDRESS
```

### Set up the delivery webhook

Get told when each recipient's email is delivered, bounced, failed or suppressed.

1. On the **Webhook** tab, enter your endpoint's **URL** and tick the **Events** you want: `delivered`, `bounced`, `failed`, `suppressed`.
2. Click **Save webhook**.
3. Copy the **signing secret** shown at the top: it is shown once.
4. Check the signature in your endpoint (see [Webhook calls](#webhook-calls)).

The tab shows the result of the last call. **remove the webhook** stops the calls.

```
PUT /api/v1/email/webhook
```

### Delete a sending domain

1. At the bottom of the domain's page, click **Delete domain**.
2. Type the domain to confirm.

Its messages still queued fail.

```
DELETE /api/v1/email/domains/DOMAIN
```

## Technical reference

### Limits and plans

| Item | Limit/Default | Notes |
| --- | --- | --- |
| Emails per 24 hours | set by your plan | each recipient counts; default 500; trial 50; pro 20000 |
| Sending domains | set by your plan | default 3; trial 1; pro 20 |
| Recipients per message | 1 to 50 | |
| Subject | 998 characters | one line |
| Body (`text` + `html`) | 256 KB together | |
| Attachments | 20 files | the whole message (body and files) must stay within 10 MB; your plan may set a lower size |
| Sending log | last 100 messages shown | kept 30 days |

### DNS records

| Record | Name | Why |
| --- | --- | --- |
| TXT | `_platform-verify.<domain>` | proves you own the domain (required) |
| TXT | `<selector>._domainkey.<domain>` | publishes the DKIM key receivers use to check each message's signature (required) |
| TXT (SPF) | `<domain>` | says this platform may send for your domain (recommended); if you already have a `v=spf1` record, merge the value into it instead of adding a second one |
| TXT (DMARC) | `_dmarc.<domain>` | tells receivers what to do with mail that fails the checks (recommended) |

### Send an email

`POST /api/v1/email/send` (developer role). Authenticate with `Authorization: Bearer <token>`.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `from` | string | yes | an address at a verified domain, with an optional name: `Shop <hello@example.com>` |
| `to` | list of strings | yes | 1 to 50 recipients |
| `reply_to` | string | no | where replies go |
| `subject` | string | yes | one line, up to 998 characters |
| `text`, `html` | string | one or both | the body (256 KB together) |
| `attachments` | list | no | see [Attachments](#attachments) |

The answer is `202` with `{"id": "...", "status": "queued"}`. A refused message answers `422` (for example an unverified From domain), `429` when your plan's daily limit is reached. `GET /api/v1/email/messages/ID` gives its status and each recipient's result with the receiving server's answer.

| Status | Meaning |
| --- | --- |
| `queued` | waiting to be sent or retried |
| `sent` | delivered |
| `partial` | only some recipients got it |
| `failed` | not delivered |

### Attachments

```
"attachments": [{"filename": "invoice-1042.pdf", "content_type": "application/pdf", "content": "<base64>"}]
```

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `filename` | string | yes | the file name |
| `content_type` | string | no | the media type, such as `application/pdf` (default `application/octet-stream`) |
| `content` | string | yes | the file, base64 encoded |

Up to 20 files per message; the whole message (body and files) must stay within 10 MB, or your plan's lower limit (`413` otherwise). File names are 1 to 200 characters, without slashes or quotes. Programs and scripts (`.exe`, `.js`, `.bat`...) are refused, as mail providers would refuse them.

### SMTP settings

| Setting | Value |
| --- | --- |
| host | the host shown on the **Use from code** tab |
| port | **465**, TLS from the start (not STARTTLS) |
| user name | any value, such as `token` |
| password | your email token (`EMAIL_TOKEN`) |

### Webhook calls

For each recipient's result, the platform sends a `POST` with a JSON body:

```
{"id": "...", "type": "email.bounced", "message_id": "...", "recipient": "buyer@example.org",
 "detail": "550 5.1.1 no such user", "created_at": "2026-11-18T10:00:00Z"}
```

| Header | Value |
| --- | --- |
| `X-Platform-Signature` | `sha256=<hex>`: the HMAC-SHA256 of the raw body made with your signing secret |
| `X-Platform-Event` | the event type, such as `email.bounced` |

Compute it yourself and compare before trusting the body. Answer with a 2xx status; a failed call is retried up to 8 times, with growing delays, over about four hours. The call also carries `X-Platform-Event` (such as `email.bounced`). Events are kept 7 days.

| Event | When |
| --- | --- |
| `email.delivered` | the message was delivered to the recipient |
| `email.bounced` | the address was refused for good |
| `email.failed` | the message could not be delivered |
| `email.suppressed` | the address was on the suppression list |

### API

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/v1/email/domains` | List sending domains |
| POST | `/api/v1/email/domains` | Add a domain (`{"domain": "example.com"}`) |
| DELETE | `/api/v1/email/domains/DOMAIN` | Delete a domain |
| POST | `/api/v1/email/attach` | Give a service the email secrets (`{"service": "project/service"}`) |
| POST | `/api/v1/email/send` | Send a message |
| GET | `/api/v1/email/messages/ID` | Status of a message |
| GET | `/api/v1/email/suppressions` | List suppressed addresses |
| POST | `/api/v1/email/suppressions` | Suppress an address |
| DELETE | `/api/v1/email/suppressions/ADDRESS` | Remove an address from the list |
| GET | `/api/v1/email/webhook` | Show the webhook |
| PUT | `/api/v1/email/webhook` | Set the webhook |
| DELETE | `/api/v1/email/webhook` | Remove the webhook |

Listing calls and `GET /api/v1/email/messages/ID` need the viewer role; the other calls need the developer role. An email token (made by **Connect**) can only call `POST /api/v1/email/send` and `GET /api/v1/email/messages/ID`.

Body fields of the calls that take one:

| Call | Field | Type | Required | Description |
| --- | --- | --- | --- | --- |
| `POST /api/v1/email/domains` | `domain` | string | yes | the domain to add (answer `201`) |
| `POST /api/v1/email/suppressions` | `address` | string | yes | the address to suppress (answer `201`) |
| `PUT /api/v1/email/webhook` | `url` | string | yes | your endpoint |
| `PUT /api/v1/email/webhook` | `events` | list of strings | no | any of `delivered`, `bounced`, `failed`, `suppressed` (default: all four) |
| `POST /api/v1/email/attach` | `service` | string | yes | `project/service` |

The first `PUT` of the webhook returns the signing `secret` once; later calls keep it. Deleting calls answer `204`.

### Environment variables

Set by **Connect** on the chosen service, as secrets:

| Variable | Value |
| --- | --- |
| `EMAIL_API_URL` | the send endpoint |
| `EMAIL_TOKEN` | a token that can only send email and read message status |
| `SMTP_HOST`, `SMTP_PORT`, `SMTP_USERNAME`, `SMTP_PASSWORD` | SMTP settings, when SMTP is offered |

### Behind the scenes

- The platform delivers your messages itself, straight to each recipient's mail server, signed with your domain's DKIM key. Keep the SPF and DMARC records so receivers know this platform may send for your domain.
- The platform rechecks verified domains daily: if the records disappear, sending from the domain pauses until they are back.
- Temporary refusals (a full mailbox, a busy server) are retried for about half an hour, then the recipient is marked failed with the reason.
- When a recipient's server refuses an address for good (an unknown mailbox, a domain that does not exist: a "hard bounce"), the address goes on your organization's suppression list and later messages skip it: that recipient is marked `suppressed`.
- Bodies are deleted as soon as a message is done; the log keeps 30 days.
- Billing: per delivered recipient, per thousand.

## Troubleshooting

### The domain stays on "waiting for DNS"

The TXT records are missing or not visible yet. Check the names and values on the domain's page, wait a few minutes, then click **Check now**.

### Sending from a domain stopped

The daily recheck did not find the records (**records removed**). Put them back; sending resumes.

### A recipient is marked suppressed

The address bounced for good earlier. Check it, then click **remove** on the **Suppressions** tab.

### An attachment is refused

Programs and scripts (`.exe`, `.js`, `.bat`...) are not accepted. Send a link or a document format instead.
