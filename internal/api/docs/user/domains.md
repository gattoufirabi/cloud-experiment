# Custom domains and HTTPS

Serve your web services under your own domain, such as `shop.example.com`. HTTPS certificates are issued and renewed for you once you prove you own the domain.

## Overview

Every public web service has a platform address under `{{apps}}`. A custom domain lets your customers see your own name instead. You add the domain to a service, prove you own it with a DNS record, and point it at the platform. After that, the platform serves your application on that domain and gets an HTTPS certificate on the first visit.

A domain can belong to only one organization. Workers have no public address, so they cannot have a domain.

If your DNS is [hosted on the platform](/docs/dns), the records are written and verified for you, with nothing to copy.

One domain can also front several services, by routing paths (for example `/api`) to other services. You can also turn off the platform address, so that the service answers only on your own domains.

Plan limits: Starter plans allow 3 custom domains, Pro plans 20; Trial plans allow none.

## Use cases

- **Brand your site**: serve your shop on `shop.example.com` instead of the platform address. Add it on the service's **Domains** tab.
- **Website and API on one domain**: send `shop.example.com` to the website and `shop.example.com/api` to the API service. This avoids cross-site browser issues. Use **Route path**.
- **Only your own address is public**: once a custom domain works, click **Turn off the platform address** so old links stop working.
- **Root domain**: serve `example.com` itself. Most providers allow only an A record there.
- **Zero-touch setup**: host the zone on the platform and add the domain; the records and the verification happen automatically.
- **Clean-up**: use the **Domains** page to find and remove several domains at once.

## How to use it

### Add a domain

Attach a custom domain to a web service and prove you own it.

1. Open the service's **Domains** tab, enter the domain under **Custom domains** (for example `shop.example.com`) and click **Add domain**. You can also open the **Domains** page in the menu, enter the domain, choose the service and click **Add domain**.
2. At your DNS provider, create a **TXT** record named `_platform-challenge.shop.example.com` with the value `platform-verify=<token>` shown on screen.
3. Create a **CNAME** for `shop.example.com` pointing to the platform, or an A record to the platform's address. For the domain itself (`example.com`), most providers allow only an A record.
4. Click **Verify**. DNS changes can take a few minutes: click again if the record is not found yet.

Once verified, the domain serves the application within seconds, and an HTTPS certificate is issued on the first visit.

If the domain's DNS is hosted on the platform, steps 2 to 4 happen by themselves. The domain's panel then says the platform handles its DNS, and warns you if the zone's delegation is incomplete. See [DNS hosting](/docs/dns#automatic-records).

API:

```
POST /api/v1/projects/PROJECT/services/SERVICE/domains
{"hostname": "shop.example.com"}
```

### The Domains page

The **Domains** page (in the menu) lists every custom domain of the organization with its service and status (**verified** or **waiting for DNS**), out of your plan's limit.

1. Type in **Find domains** to filter the list.
2. To remove several domains, select them, choose **Remove**, type `delete` and click **Apply**.
3. On a service's **Domains** tab, click **remove** next to a domain and confirm to delete just that one.

### Turn the platform address off or on

The **Platform address** section of the service's **Domains** tab shows the service's address under `{{apps}}` and its private name. Once a custom domain is verified, you can make customers use only your domain.

1. Click **Turn off the platform address** and confirm.
2. The service now answers only on its own domains. Links to the old address stop working. Extra public ports keep their addresses.
3. Click **Turn the platform address back on** to undo it.

If the service has no verified domain left, the platform address answers again on its own. You cannot turn it off before a custom domain is verified.

A private service has no public address at all. Untick **Private** in its settings to publish it (see [Applications](/docs/applications)).

API:

```
POST /api/v1/projects/PROJECT/services/SERVICE/default-address
{"on": false}
```

Send `{"on": true}` to turn it back on.

### Route paths to other services

Let one domain serve several services, for example `shop.example.com` for the website and `shop.example.com/api` for the API.

1. Under a verified custom domain on the **Domains** tab, enter a path (such as `/api`).
2. Choose the service that serves it.
3. Tick **remove the prefix** if the target expects `/orders` rather than `/api/orders`.
4. Click **Route path**.
5. Click **remove** next to a path to delete it.

Requests for `/api` and everything below go to that service, and the rest to the domain's own service. The target can be any web service of the organization, private ones included. The longest matching path wins.

## Technical reference

### Limits and plans

| Item | Limit | Notes |
| --- | --- | --- |
| Custom domains | 3 on Starter, 20 on Pro, none on Trial | Counted across the organization. |
| Paths per domain | 20 | The longest matching path wins. |
| Verification attempts | 10 per 10 minutes per domain | Afterwards you must wait a few minutes. |
| Owner of a domain | One organization | Adding an already registered domain is refused. |

### DNS records for a domain

| Record | Name | Value | Purpose |
| --- | --- | --- | --- |
| TXT | `_platform-challenge.<domain>` | `platform-verify=<token>` | Proves you own the domain. The token is shown on screen. |
| CNAME | the domain | the platform address | Sends visitors to the platform. |
| A | the domain itself | the platform's address | Use it when a CNAME is not allowed (the domain itself). |

### Rules for domains and paths

| Item | Rule |
| --- | --- |
| Domain name | A valid host name such as `shop.example.com`. Case and a trailing dot are ignored. |
| Platform names | Names under `{{apps}}` and the console host are refused: they belong to the platform. |
| Path | Starts with `/`, does not end with `/`, no `//` (for example `/api`). |
| Path target | A web service of the organization, other than the domain's own service. |

### API

| Method | Path | Purpose |
| --- | --- | --- |
| POST | `/api/v1/projects/PROJECT/services/SERVICE/domains` | Add a custom domain. |
| POST | `/api/v1/projects/PROJECT/services/SERVICE/default-address` | Turn the platform address off or on. |

Body of the first call: `hostname` (string, required), the domain to add. Body of the second: `on` (boolean), `true` turns the address on and anything else (`false`, or no value) turns it off. Both calls need the developer role and are refused for a worker. Verifying, removing a domain and routing paths are done in the console.

### Behind the scenes

- Adding, verifying, removing a domain and changing paths or the platform address are recorded in the audit log.
- Changes reach the proxy within seconds to a minute.
- The platform looks up the TXT record `_platform-challenge.<domain>` and compares it with `platform-verify=<token>`.
- In a zone hosted on the platform, the CNAME (or A record) and the verification record are written for you, and the domain is verified at once when the zone is active, or as soon as its delegation is confirmed.

## Troubleshooting

### The TXT record was not found yet

DNS changes can take a few minutes to spread. Check the record name (`_platform-challenge.` followed by the full domain) and the exact value, then click **Verify** again. After 10 attempts in 10 minutes you must wait a few minutes.

### Your plan allows N custom domains

Remove a domain you no longer use, or ask for a larger plan. See [Billing](/docs/billing).

### This domain is already registered on the platform

A domain belongs to one organization. If you own it, remove it from the other organization first.

### I cannot turn the platform address off

Verify a custom domain first: without one, the service would have no address.
