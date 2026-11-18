# Corner Shop: an example store on the platform

A small online shop that uses most of what the platform offers, so you can see how the pieces
fit together (open the project's **Map** tab once it is deployed).

```
internet ──▶ web (storefront, public, edge cache) ──/api──▶ api (private: api.shop.internal)
                                                          │   ├─ PostgreSQL  main    products, orders
                                                          │   ├─ Redis       cache   catalogue cache
                                                          │   ├─ MongoDB     docs    reviews, sales statistics
                                                          │   ├─ bucket      photos  product photos (signed upload URLs)
                                                          │   └─ /metrics    scraped; alarm on memory
                                                          ├─▶ Kafka   events  "orders" topic ──▶ worker ──▶ MongoDB statistics
                                                          └─▶ queue   emails  confirmation jobs ──▶ worker ──▶ email service
bucket upload (products/*.jpg) ──trigger──▶ function photo-uploaded ──▶ api (sets the product photo)
payment provider ──webhook──▶ function payment-webhook ──▶ api (marks the order paid)
every morning 07:00 UTC ──job──▶ node src/report.js ──▶ email with yesterday's sales
```

| Part | Platform feature |
|---|---|
| `app/` built once from its Dockerfile (zip upload), run as three services by `ROLE` | Builds, zip deployment, services and workers |
| `web` forwards `/api/*` to `http://api.shop.internal` | Private network, private services |
| `api` uses `DATABASE_URL`, `REDIS_URL`, `MONGODB_URI`, `KAFKA_URL` | Managed PostgreSQL, Redis, MongoDB, Kafka ("Add secret") |
| product photos | Object storage, signed URLs, bucket trigger |
| order confirmations | Queues, email (`EMAIL_FROM` must use a verified sending domain) |
| `functions/` | Functions with bucket and webhook triggers, project variables |
| daily report | Scheduled jobs |
| `/metrics`, alarm, budget | Prometheus metrics, metric alarms, budgets |

## Deploy it

Create an API token with the **admin** role (console › **Access** › API tokens), then:

```
cd examples/store
PLATFORM_URL=https://console.example.com PLATFORM_TOKEN=pla_... \
  EMAIL_FROM="Corner Shop <shop@yourdomain.com>" REPORT_TO=you@yourdomain.com ./setup.sh
```

The script only uses the platform's API, and can be run again safely (what exists is kept).
It needs `curl`, and `platformctl` or Go to run it from this repository. The project needs a
plan with 4 databases and about 2.5 GB of memory (Kafka and MongoDB need 512 MB each).

Without `EMAIL_FROM` the store works and logs the emails it would send. To try the payment
webhook, sign a body with the `PAYMENT_SECRET` project variable:

```
BODY='{"order": 1, "status": "paid"}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$PAYMENT_SECRET" -hex | cut -d' ' -f2)
curl -X POST -H "X-Signature: $SIG" -d "$BODY" "$(cat .payment-webhook)"
```

## Run the code elsewhere

The app is plain Node.js 22 (`npm install`, then `ROLE=web|api|worker node src/main.js`); every
service is optional and read from the environment, so it also runs against your own databases.
