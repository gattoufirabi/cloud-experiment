# Store, enterprise edition: the whole platform as code

A production-grade setup of the example store (`../store/app`): two environments in two
organizations, every resource described with OpenTofu/Terraform, one image built once and
promoted, a delivery pipeline, and the operational pieces (alarms, budgets, backups, runbook).
It uses nearly everything the platform offers.

```
                       ┌──────────────────────── organization: acme (staging) ─────────────────────────┐
 git push ─▶ CI ─build─▶ registry/acme/store:<sha> ─▶ project store-staging   (small, scales 1-2)       │
                       └────────────────────────────────────────────────────────────────────────────────┘
                 manual promotion (reviewer) — same digest copied to the production namespace
                       ┌──────────────────────── organization: acme-prod (production) ─────────────────┐
                       │ DNS zone example.com (hosted)    shop.example.com ─▶ HTTPS (automatic)        │
                       │                                                                               │
 internet ─────────────┼─▶ web  storefront, public, edge cache, autoscaled 2-6 ─/api─┐                   │
                       │                                                            ▼                  │
                       │   api  private (api.store-production.internal), autoscaled 2-8, /metrics      │
                       │    ├─ PostgreSQL main   orders, products   1 core / 1 GB, daily backup 03:00  │
                       │    ├─ Redis      cache  catalogue cache                                       │
                       │    ├─ MongoDB    docs   reviews, statistics, daily backup 04:00               │
                       │    ├─ Kafka      events "orders" topic ─────────┐                             │
                       │    ├─ bucket     photos (signed uploads) ─trigger─▶ function photo-uploaded   │
                       │    └─ queue      emails ────────────────────────┤                             │
                       │   worker  autoscaled 1-4 ◀─────────────────────┘ ─▶ email service           │
                       │   payment provider ─webhook─▶ function payment-webhook ─▶ api               │
                       │   job daily-report 07:00 UTC · alarms (memory, CPU) · budget 200 €/month     │
                       └────────────────────────────────────────────────────────────────────────────────┘
```

## What makes it enterprise grade

| Concern | How it is handled |
|---|---|
| **Reproducibility** | Every project, service, database, bucket, secret, variable, domain and DNS record is in `infra/`. `tofu plan` shows any drift from the console. Both environments come from one module, so production is what staging tested. |
| **Isolation** | Staging and production are two organizations: separate quotas, members, tokens and invoices. A staging token cannot touch production. The API service is private; only the storefront's shopper routes are forwarded. Databases live on each project's private network. |
| **Immutable releases** | The image is built once per commit (`build-and-push.sh`), scanned by the registry, then copied to production with its digest checked (`promote-image.sh`). Rolling back is promoting the previous tag. |
| **Zero-downtime changes** | Every change is a blue/green deployment: new instances start, pass the health check, take the traffic, and old ones drain before they stop. A failed deployment leaves the previous version serving and fails the pipeline. |
| **High availability** | At least two instances of the storefront and the API in production, load balanced with passive health checks. Autoscaling adds instances on CPU (65-75 % targets). |
| **Data protection** | Daily backups of PostgreSQL and MongoDB at off-peak hours, restorable from the console. Connection URLs are encrypted secrets, never in files. The state file is encrypted (OpenTofu) and stored in a private bucket. |
| **Observability** | The API exposes Prometheus metrics, scraped by the platform; container CPU, memory and network are recorded for every service. Alarms on memory and CPU go to the organization's alert channels. Logs are searchable and exportable. |
| **Cost control** | Monthly budgets with 50/80/100 % thresholds and a forecast alert, per environment. Staging is sized small and has no MongoDB or Kafka. |
| **Delivery** | `ci/github-actions.yml`: plan on pull requests, apply and smoke test staging on every push, reviewed manual promotion to production. One apply at a time. |
| **Least privilege** | Admin tokens only in CI secrets. Give developers the developer role in staging, viewer in production. Project tokens can limit a deploy key to one project. |

## Sizing: plans needed

The platform reserves each service's autoscaling **maximum** against the plan, so a sudden peak
can never be refused for quota.

| Environment | Services | Databases | CPU reserved | Memory reserved | Plan |
|---|---|---|---|---|---|
| staging | 3 + 2 functions | 2 | 1.1 cores | 1.2 GB | Pro |
| production | 3 + 2 functions | 4 | 8.3 cores | 7.8 GB | custom (an operator creates it in the admin panel: 10 cores, 10 GB, 5 databases, 5 domains) |

Lower the `max` of each tier in `infra/main.tf` to fit a smaller plan.

## Set it up

You need OpenTofu (or Terraform 1.6+), Docker, curl, python3, and `platformctl` (or Go).

1. **Organizations and tokens.** Create the organization `acme` (staging) and `acme-prod`
   (production). In each, create an API token with the **admin** role (Access › API tokens) and a
   registry token with push rights (Registry › Tokens).
2. **Secrets for the run** (never in files):

   ```
   export STAYSA_ENDPOINT=https://console.example.com
   export STAYSA_TOKEN=pla_...               # staging organization
   export TF_VAR_production_token=pla_...    # production organization
   export TF_VAR_internal_token=$(openssl rand -hex 24)
   export TF_VAR_payment_secret=$(openssl rand -hex 24)
   ```

   Keep the two generated values in your password manager: they must stay the same across runs.
3. **State.** Create a bucket `tfstate` in the production organization and an S3 key for it,
   then uncomment the `backend "s3"` and `encryption` blocks in `infra/versions.tf`.
4. **Build and deploy staging.**

   ```
   IMAGE=$(REGISTRY=registry.example.com ORG=acme REGISTRY_TOKEN=plr_... scripts/build-and-push.sh)
   cd infra && tofu init && tofu apply -var "image=$IMAGE" && cd ..
   scripts/platform-extras.sh staging
   scripts/smoke-test.sh "$(cd infra && tofu output -json staging | python3 -c 'import json,sys; print(json.load(sys.stdin)["storefront"])')"
   ```

5. **Promote to production.**

   ```
   PROD=$(REGISTRY=registry.example.com ORG=acme REGISTRY_TOKEN=plr_... \
          PRODUCTION_ORG=acme-prod PRODUCTION_REGISTRY_TOKEN=plr_... scripts/promote-image.sh "$IMAGE")
   cd infra && tofu apply -var "image=$IMAGE" -var "production_image=$PROD" -var dns_zone=example.com && cd ..
   STAYSA_TOKEN=$TF_VAR_production_token scripts/platform-extras.sh production
   ```

   Set the name servers from `tofu output nameservers` at your registrar. `shop.example.com`
   is verified and served over HTTPS as soon as the delegation is seen.
6. **Automate.** Copy `ci/github-actions.yml` to `.github/workflows/` and add the secrets it lists.

To try everything in a single organization, leave `TF_VAR_production_token` and `PRODUCTION_ORG`
empty: production then goes next to staging (the plan must hold both).

## Operate it

| Task | How |
|---|---|
| **Release** | Merge to main (staging), then run the workflow manually with the tested image. |
| **Roll back** | Run the workflow with the previous image tag; the platform switches blue/green. In an emergency, the service's **Deployments** tab rolls back in one click (then align `production_image`). |
| **Scale** | Change `min`/`max` in `infra/main.tf`. For a known peak (sale), raise `min` before it starts. |
| **Restore data** | Database page › Backups › Restore, or restore into a new database to compare first. Backups run at 03:00 (PostgreSQL) and 04:00 (MongoDB) UTC. |
| **Rotate a secret** | Change `TF_VAR_internal_token` or `TF_VAR_payment_secret` and apply: the variables change and the services redeploy. For the payment secret, update the provider at the same time. |
| **Rotate a database password** | Recreate the database from a backup (`tofu apply -replace=module.production[0].staysa_database.main` after a fresh backup), or rotate in the database and update the secret. |
| **Investigate** | Service › Logs (search, download), Metrics, the project **Map** for connections, the audit log for who changed what (tokens are named). |
| **Drift** | `tofu plan` daily in CI (schedule) to catch console changes; apply to put things back. |
| **Tear down staging** | `tofu destroy -target=module.staging`. |

## What is not managed by the provider yet

Queues, connections of buckets/queues/email, functions and their triggers, scheduled jobs,
alarms and budgets are created by `scripts/platform-extras.sh` through the REST API (safe to
run again). They will move into `infra/` as the provider gains these resources.

## Files

```
infra/
  versions.tf                   providers (one per organization), S3 state, state encryption
  variables.tf                  inputs (images, tokens, secrets, DNS zone)
  main.tf                       staging and production from one module, DNS zone and records
  outputs.tf                    addresses, projects, revisions, name servers
  terraform.tfvars.example
  modules/environment/          one environment: project, data tier, bucket, variables,
                                services, connections, custom domain
scripts/
  build-and-push.sh             build once, push to the staging registry namespace
  promote-image.sh              copy to production's namespace, digest checked
  platform-extras.sh            queue, connections, functions, job, alarms, budget
  smoke-test.sh                 shopper checks and p95 latency
ci/github-actions.yml           plan, staging, reviewed production promotion
```
