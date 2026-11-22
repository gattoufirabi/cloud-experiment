# Terraform and OpenTofu

Describe your projects, applications, databases, domains and DNS in configuration files, keep them in git, and create or change everything with one command.

## Overview

The platform has a provider for [Terraform](https://developer.hashicorp.com/terraform) and its open-source fork [OpenTofu](https://opentofu.org). Both tools read `.tf` files that describe what you want. They compare it with what exists, show the plan, and make only the changes needed.

The provider uses the [REST API](/docs/api-and-cli) with an API token. Every change goes through the same checks, plan limits and audit trail as the console, and the audit log names the token. An application created or changed with the provider is deployed blue/green, as in the console, and the provider waits until the new version is live.

The provider can manage:

| Resource | What it is |
| --- | --- |
| `staysa_project` | A project. |
| `staysa_service` | An application (web service or worker) with all its settings, autoscaling included. |
| `staysa_service_secret` | An encrypted secret of an application. |
| `staysa_project_variable` | A variable shared by a project's applications. |
| `staysa_database` | A managed database, with its connection URL as an output. |
| `staysa_domain` | A custom domain of an application, with the DNS records to create. |
| `staysa_dns_zone`, `staysa_dns_record` | A zone hosted on the platform and its records. |
| `staysa_bucket` | An object storage bucket. |

Data sources `staysa_project`, `staysa_service` and `staysa_database` read existing objects without managing them.

## Use cases

- **Same setup in every environment**: describe staging once and create production from the same files, with other variables.
- **Review infrastructure changes**: open a pull request with the `.tf` change; the plan shows exactly what will change before anyone applies it.
- **Wire resources together**: give an application its database URL as a secret, without copying passwords by hand.
- **Rebuild after a mistake**: the files are the reference; `tofu apply` puts back what was changed or deleted in the console.
- **Point your DNS at your applications**: create the zone, the records and the custom domain in one plan.

## How to use it

### Install the tools

1. Install [OpenTofu](https://opentofu.org) or [Terraform](https://developer.hashicorp.com/terraform/install) from its website.
2. Go to **Access**, then **API tokens**, in the console. Create a token with the **admin** role (creating and deleting projects needs it; **developer** is enough to manage applications in existing projects).
3. Set the console address and the token as environment variables, so they stay out of your files:

```
export STAYSA_ENDPOINT={{console}}
export STAYSA_TOKEN=pla_...
```

### Write a first configuration

Create a folder with a `main.tf` file:

```hcl
terraform {
  required_providers {
    staysa = {
      source = "gattoufirabi/staysa"
    }
  }
}

provider "staysa" {}

resource "staysa_project" "shop" {
  id   = "shop"
  name = "Shop"
}

resource "staysa_database" "main" {
  project   = staysa_project.shop.id
  id        = "main"
  engine    = "postgres"
  memory_mb = 512
}

resource "staysa_service" "web" {
  project    = staysa_project.shop.id
  id         = "web"
  image      = "nginx:1.27-alpine"
  port       = 80
  cpu_millis = 250
  memory_mb  = 256
  env = {
    MODE = "production"
  }
  autoscaling = {
    min = 1
    max = 4
  }
}

resource "staysa_service_secret" "db" {
  project = staysa_project.shop.id
  service = staysa_service.web.id
  name    = "DATABASE_URL"
  value   = staysa_database.main.url
}

output "address" {
  value = staysa_service.web.url
}
```

Then run:

```
tofu init
tofu plan
tofu apply
```

With Terraform, use `terraform` instead of `tofu`. The first `apply` creates the project, the database and the application, sets the secret and deploys it again so it gets the value.

### Change something

Edit the file, for example `memory_mb = 512`, and run `tofu apply` again. The provider changes only that setting and deploys it. To save settings without deploying, set `deploy = false` on the application; they apply at its next deployment.

Changing an `id`, a `project`, a database `engine` or a domain `hostname` replaces the object: the plan shows it as destroyed and created again. For a database, that deletes its data, so read the plan before you confirm it.

### Add a custom domain

```hcl
resource "staysa_dns_zone" "main" {
  domain = "example.com"
}

resource "staysa_domain" "shop" {
  project  = staysa_project.shop.id
  service  = staysa_service.web.id
  hostname = "shop.example.com"
  depends_on = [staysa_dns_zone.main]
}

output "nameservers" {
  value = staysa_dns_zone.main.nameservers
}
```

When the zone is hosted on the platform, the domain's records are written and the domain is verified for you. If your DNS is elsewhere, the `dns_records` attribute of `staysa_domain` lists the CNAME and TXT records to create there. Then verify the domain in the console. See [Custom domains](/docs/domains).

### Import what already exists

Bring objects created in the console under Terraform without recreating them:

```
tofu import staysa_project.shop shop
tofu import staysa_service.web shop/web
tofu import staysa_database.main shop/main
tofu import staysa_domain.shop shop/web/shop.example.com
tofu import staysa_project_variable.region shop/REGION
tofu import staysa_dns_zone.main example.com
tofu import staysa_dns_record.www example.com/RECORD-ID
tofu import staysa_bucket.uploads uploads
```

Write the matching `resource` block first, then run `tofu plan` to see what still differs.

### Delete everything

`tofu destroy` deletes what the configuration created. A project still holding applications or databases that Terraform does not manage is not deleted, unless you set `force_delete = true` on it.

## Technical reference

### Provider settings

| Setting | Environment variable | Description |
| --- | --- | --- |
| `endpoint` | `STAYSA_ENDPOINT` | Console address, such as `{{console}}`. |
| `token` | `STAYSA_TOKEN` | API token (`pla_…`). Prefer the environment variable. |

A token limited to one project manages only that project's applications, databases, secrets and variables.

### staysa_service

| Attribute | Default | Description |
| --- | --- | --- |
| `project`, `id` | required | Where the application lives and its ID. Changing them replaces it. |
| `image` | required | Container image. |
| `port`, `health_path` | the platform's | Port and health check path of a web service. |
| `cpu_millis`, `memory_mb` | the platform's | Resources per instance. |
| `instances` | 1 | Number of instances. Ignored while `autoscaling` is set. |
| `autoscaling` | off | `{ min, max, cpu_percent }`; see [Applications](/docs/applications). |
| `worker` | `false` | A background worker without an address. |
| `env` | `{}` | Plain environment variables. Use `staysa_service_secret` for secrets. |
| `public_ports` | `[]` | Raw TCP/UDP ports, such as `"25565/tcp"`. |
| `volume_path` | none | Persistent folder; the application then runs one instance. |
| `public` | `true` | `false` keeps the application on the private network. |
| `edge_cache` | `false` | Cache responses at the proxy. |
| `metrics_path`, `metrics_port` | none | Prometheus endpoint scraped by the platform. |
| `deploy` | `true` | Deploy each change. |
| `wait_for_live` | `true` | Wait for the deployment and fail if it fails (up to 15 minutes). |
| `url`, `internal_host`, `status`, `revision` | computed | Public address, private name, latest deployment. |

While autoscaling is on, the autoscaler changes the number of instances. The provider does not report that as a difference.

### staysa_database

| Attribute | Default | Description |
| --- | --- | --- |
| `project`, `id` | required | Changing them replaces the database. |
| `engine` | required | `postgres`, `mysql`, `mariadb`, `mongodb`, `redis` or `kafka`, optionally with a version (`postgres:16`). Without one, the newest. |
| `cpu_millis`, `memory_mb` | the platform's | New values restart the database. |
| `backup_enabled`, `backup_hour` | on, 3 | Daily backups and their hour (UTC). |
| `wait_for_running` | `true` | Wait until it runs after creation. |
| `host`, `port`, `database`, `username` | computed | Connection details on the private network. |
| `password`, `url`, `secret_name` | computed | `password` and `url` are sensitive; `secret_name` is the usual variable name, such as `DATABASE_URL`. |

Deleting a database needs an admin token.

### Other resources

| Resource | Attributes |
| --- | --- |
| `staysa_project` | `id`, `name`, `force_delete` |
| `staysa_service_secret` | `project`, `service`, `name`, `value` (sensitive), `redeploy` (default `true`: deploys the application so it gets the new value) |
| `staysa_project_variable` | `project`, `name`, `value` (sensitive), `secret` (default `true`) |
| `staysa_domain` | `project`, `service`, `hostname`; computed `verified`, `dns_records` |
| `staysa_dns_zone` | `domain`; computed `status`, `nameservers` |
| `staysa_dns_record` | `zone`, `name`, `type`, `value`, `priority`, `ttl`; any change replaces the record |
| `staysa_bucket` | `name`, `quota_mb`; computed `s3_name`, `endpoint`, `region` |

### What the provider cannot see

- Secret values are never shown again by the platform. If someone changes a secret in the console, the plan does not show it; set it again from Terraform to be sure of the value.
- A bucket's quota is changed in the console; then set the same `quota_mb` in the file.
- Settings not listed here (traffic protection, path routing, scheduled jobs, alerts, mailboxes) stay in the console or the [API](/docs/api-and-cli) for now. The provider keeps them as they are.

### State and secrets

Terraform keeps a state file with the IDs and the values it knows, sensitive ones included: database passwords and secret values. Store it somewhere private and encrypted, for example in an S3 [bucket](/docs/object-storage) used as a state backend. Never commit it to git. OpenTofu can also encrypt the state itself.

### Where the provider comes from

The provider is open source and lives in the platform's repository under `terraform-provider-staysa`. It is built with HashiCorp's plugin framework and works with Terraform 1.0 or later and every OpenTofu version.

## Troubleshooting

### Missing API token

Set `STAYSA_TOKEN` (or `token` in the provider block) to a token that starts with `pla_`, and `STAYSA_ENDPOINT` to the console address.

### 403 this token's role cannot do this

Creating or deleting projects, and deleting databases, DNS zones or buckets, need an admin token. Applications, secrets and domains need at least a developer token.

### The deployment failed

The provider shows the deployment's message, the same as the application's **Deployments** tab. Fix the setting (often the port or the health path), then run `tofu apply` again. The application keeps serving its previous version in the meantime.

### A project is not deleted (409)

The project still holds applications or databases that this configuration does not manage. Delete them, import them, or set `force_delete = true` on the project.
