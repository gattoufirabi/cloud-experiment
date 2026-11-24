# DNS hosting

Host your domains' DNS on the platform and manage every record in one place. The platform also writes the records for your services, email and mailboxes by itself.

## Overview

DNS is the address book of the internet: it says which server answers for `example.com`, `www.example.com` and your mail. When you host a domain here, you set its name servers at your registrar (where you bought the domain) to the platform's two name server names, and the platform answers for it.

Once a zone is hosted here, the platform adds the records that your [custom domains](/docs/domains), [email sending domains](/docs/email) and [mailbox domains](/docs/mailboxes) need, and verifies them at once. Records you write yourself are never replaced.

Until the platform sees the delegation, nothing is served, so nobody can take over a domain they do not control. You can prepare your records meanwhile. Your plan sets the number of zones.

Open **DNS** in the Services menu.

## Use cases

- **One place for everything**: keep websites, mail and verification records together on the **DNS** page.
- **No manual records for custom domains**: add a custom domain in a hosted zone and it is verified without a TXT record to copy.
- **Email domains set up for you**: ownership, DKIM, SPF and DMARC records are created automatically.
- **Move from another provider**: use **Import records** with a BIND zone file so websites and mail keep working during the move.
- **Point a name at a service**: use **Point a name at a service** to create a CNAME to a service in one step.
- **Delegate a sub-domain**: add NS records to hand a sub-domain to other name servers.
- **Restrict certificate issuers**: add a CAA record.

## How to use it

### Add a zone

Start hosting a domain's DNS here.

1. On the **DNS** page, click **Add a domain**, enter it (`example.com`) and click **Add domain** (developers and above).
2. At your registrar, set the domain's name servers to the ones shown: two names for the same server.
3. The platform answers as soon as it sees the delegation, checked every 2 minutes. Click **Check now** on the zone's page to check straight away. Registrars can take up to a day to publish the change.

The **DNS** page lists your zones with their status (**active** or **waiting for the name servers**) and number of records. A zone not delegated within 14 days is removed.

API:

```
POST /api/v1/dns/zones
{"domain": "example.com"}
```

The answer lists the `nameservers` and a `status` of `pending`.

### Add and delete records

Add any record to a zone.

1. Open the zone's page.
2. Under **Add a record**, fill in **Name**, **Type**, **Value**, **Priority** (MX and SRV) and **TTL**.
3. Click **Add record**.
4. To remove a record, click **delete** next to it.

Changes apply within seconds. Resolvers may keep earlier answers for their TTL.

API:

```
GET  /api/v1/dns/zones/DOMAIN/records
POST /api/v1/dns/zones/DOMAIN/records
{"name": "www", "type": "CNAME", "value": "shop.example.net", "priority": 0, "ttl": 300}
DELETE /api/v1/dns/zones/DOMAIN/records/ID
```

### Point a name at a service

Create the record that sends a name to one of your services.

1. On the zone's page, under **Point a name at a service**, enter a name (`www`, or leave it empty for the domain itself).
2. Choose the service.
3. Click **Add record**. It adds a CNAME to the service's address, or an A record to the platform's address for the domain itself.
4. Add the domain on the service's **Domains** tab so it is served with HTTPS. In a hosted zone it is verified at once.

### Import a zone from another provider

Copy the records of a domain before you change its name servers, so websites, mail (MX, DKIM, autodiscovery SRV) and verification records keep answering.

1. Add the zone here, but do not change the name servers yet.
2. Export the zone file from your current provider, in the text "BIND" format most providers offer (for example OVH: *DNS zone*, *Change in text format*; Cloudflare: *Export*).
3. Paste it in **Import records** on the zone's page and click **Import**. The provider's SOA and name servers are left out, and records already in the zone are kept.
4. Check the records. Then at the registrar, list **only** the platform's name servers. A mix with the old provider's makes some visitors get the old answers.

API: `POST /api/v1/dns/zones/ZONE/import` with the zone file as the body.

### Check the delegation

After the zone is active, the platform checks the delegation every hour. If the registrar lists other name servers too, or not all of the platform's, the zone's page shows **The delegation is incomplete** with the exact list the registrar must have. The **DNS** page marks the zone **delegation incomplete**, and custom domains in the zone show the same warning. Fix the list at your registrar. Resolvers may keep the old list for a day.

### Delete a zone

1. At the bottom of the zone's page, under **Delete the zone**, type the domain.
2. Click **Delete zone**.

The platform stops answering for it. Change the name servers at your registrar first, or the domain stops resolving.

## Technical reference

### Record types

| Type | Use | Value example |
| --- | --- | --- |
| A | a name's IPv4 address | `203.0.113.10` |
| AAAA | a name's IPv6 address | `2001:db8::10` |
| CNAME | an alias for another name | `shop.example.net` |
| TXT | text, for verification, SPF, DMARC | `v=spf1 ~all` |
| MX | the mail server for the name, with a priority | `mail.example.com` |
| CAA | which certificate authorities may issue certificates | `0 issue "letsencrypt.org"` |
| NS | delegate a sub-domain to other name servers | `ns1.example.net` |
| SRV | where a service runs: `weight port target`, priority in its own field | `0 443 sip.example.com` |

### Limits and rules

| Item | Limit or rule | Notes |
| --- | --- | --- |
| Records per zone | 500 | The SOA and NS records of the domain are provided by the platform. |
| TTL | 60 to 86400 seconds | 300 seconds by default. How long resolvers may keep an answer. |
| Names | `@`, `www`, `api.v2`, `*.apps` | `@` is the domain itself; a wildcard is allowed. |
| CNAME | Alone at its name | Not allowed for `@` or next to other records. |
| NS | Not for `@` | NS records only delegate a sub-domain. |
| Zones | Set by your plan | default 5; trial 1; pro 50 |
| TXT value | 1000 characters | |
| Zone file to import | 256 KB | |
| Undelegated zone | Removed after 14 days | |
| Delegation check | Every 2 minutes, then every hour once active | **Check now** triggers it. |

### API

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/v1/dns/zones` | List zones. |
| POST | `/api/v1/dns/zones` | Add a zone. |
| GET | `/api/v1/dns/zones/DOMAIN/records` | List the records of a zone. |
| POST | `/api/v1/dns/zones/DOMAIN/records` | Add a record. |
| POST | `/api/v1/dns/zones/DOMAIN/import` | Import a BIND zone file. |
| DELETE | `/api/v1/dns/zones/DOMAIN/records/ID` | Delete a record. |

Body of the zone call: `domain` (string, required). Body of the record call:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | string | yes | `@`, `www`, `api.v2`, `*.apps`. |
| `type` | string | yes | One of the record types above (case ignored). |
| `value` | string | yes | The record's value. |
| `priority` | integer | no | MX and SRV priority, 0 to 65535 (default 0). |
| `ttl` | integer | no | Seconds, 60 to 86400. Default 300. |

Listing needs the viewer role, the other calls the developer role. Deleting a zone is done in the console.

The import call takes the zone file as the body, or a JSON object `{"zone_file": "..."}`. It answers `{"added": N, "skipped": [...]}`. Names are written relative to the zone: a name that ends with the zone's own domain is shortened, and an empty name means `@`.

### Automatic records

When a domain of your organization lies in a zone you host here, the platform writes its DNS records itself:

| Owner | Records |
| --- | --- |
| A **custom domain** of a service | A CNAME to the service (an A record for the zone's own name) and the verification record. It is verified at once when the zone is active, or as soon as its delegation is confirmed. |
| An **email sending domain** | The ownership, DKIM, SPF and DMARC records, verified the same way. An SPF or DMARC record you already have is kept, not duplicated. |
| A **mail domain** with mailboxes | The MX, DKIM, SPF, DMARC and SRV records once mail is turned on, and the A record of your own server name. |

These records are marked **automatic** in the zone, with what they belong to. They change and disappear with the domain. A record you wrote yourself at the same name is never replaced. Adding a zone also does this for the domains you already had in it.

## Troubleshooting

### The zone stays waiting for the name servers

The registrar has not published the change yet, or it lists other names. Check the name servers shown on the zone's page, wait (up to a day), then click **Check now**.

### Some visitors get old answers after a move

The registrar lists a mix of the old provider's and the platform's name servers. List only the platform's, and wait for resolvers to drop the old list.

### The delegation is incomplete

Set the name servers at your registrar to exactly the list shown on the zone's page.
