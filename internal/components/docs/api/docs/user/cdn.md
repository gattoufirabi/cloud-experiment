# Edge cache (CDN)

Let the platform's proxy answer repeat requests for the same page or file itself, without reaching your application. Your application does less work and visitors get faster answers.

## Overview

The edge cache sits in the platform's proxy, in front of your service. It stores responses that your application marks as shareable, and serves them again to the next visitors until they expire. It suits public content that many visitors load: images, scripts, style sheets, public pages and public API answers.

The cache follows your application's `Cache-Control` header. Only responses that explicitly allow shared caching are kept, so turning the cache on never changes how other pages behave. Entries are kept per host name, so nothing is ever shared between two services.

It is a setting of each service, off by default.

## Use cases

- **Static assets**: send `Cache-Control: public, max-age=31536000` on versioned files such as `app.3f2a.js`.
- **Public pages**: cache a landing page or blog for an hour with `public, max-age=3600`.
- **Public API answers**: cache a read-only list with `s-maxage=60` so the API is hit once a minute.
- **Traffic peaks**: absorb a burst of visitors on the same page without scaling the application.
- **Different lifetimes**: use `s-maxage` to give the shared cache a different lifetime from the visitor's browser.

## How to use it

### Turn on the edge cache

Mark shareable responses in your application, then enable the cache on the service.

1. In your application, add a `Cache-Control` header to the responses that may be shared between visitors, for example `Cache-Control: public, max-age=3600` (one hour), or `s-maxage=…` to set a separate lifetime for shared caches.
2. Open the service, then the **Settings** tab, and tick **Edge cache (CDN)**.
3. Click **Save and deploy** to apply it with a deployment, or **Save only** and deploy later.

With the API, the settings call replaces the whole settings form: read the current values with `GET /api/v1/projects/PROJECT/services/SERVICE`, then send them all back with `"edge_cache": true` (a call with only that field is refused, because the image and port are missing):

```
PUT /api/v1/projects/PROJECT/services/SERVICE/settings
{"image": "registry.example.com/shop/web:1.4", "port": 8080, "cpu_millis": 250, "memory_mb": 256, "health_path": "/", "edge_cache": true}
```

See [API and command-line tool](/docs/api-and-cli) for the other settings fields.

### Check that it works

1. Load the same address twice.
2. Look at the `Cache-Status` response header. It says whether the answer was a hit (served from the cache) or a miss (served by your application).

### Change cached content

There is no purge button. To change cached content before it expires, use versioned file names (`app.3f2a.js`) or short `max-age` values.

## Technical reference

### Settings

| Setting | Values | Default | Effect |
| --- | --- | --- | --- |
| **Edge cache (CDN)** (`edge_cache`) | on or off | off | Turns the shared cache on for the service. |

### What is cached

| Response | Kept? |
| --- | --- |
| `Cache-Control` allows shared caching (`public`, `s-maxage`) | Yes |
| `Cache-Control: private` or `no-store` | Never |
| No `Cache-Control` header | No: treated as `private` |
| Larger than 10 MB | No |

### Response header

| Header | Meaning |
| --- | --- |
| `Cache-Status` | Tells you whether the answer was a hit or a miss. |

### Behind the scenes

- Entries are kept per host name, so nothing is shared between two services.
- Never mark pages that show personal data (account pages, carts) as `public`.

## Troubleshooting

### Every response is a miss

Your application probably does not send a `Cache-Control` header that allows shared caching. Add `public, max-age=…`, and make sure it is not `private` or `no-store`.

### Visitors still see old content

The entry has not expired. Wait for `max-age`, or switch to versioned file names.
