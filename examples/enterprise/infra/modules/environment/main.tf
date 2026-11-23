# One environment of the store: a project with its databases, storage, three services built
# from the same image, their secrets and shared variables, and optionally a custom domain.
#
#   internet ─▶ web (public, autoscaled, edge cache) ─/api─▶ api (private, autoscaled, /metrics)
#                                                              ├─ PostgreSQL  orders, products (daily backups)
#                                                              ├─ Redis       catalogue cache
#                                                              ├─ MongoDB     reviews, statistics   (optional)
#                                                              ├─ Kafka       order events          (optional)
#                                                              └─ bucket      product photos
#   worker (autoscaled) ◀─ queue / Kafka ─ api                 emails, statistics

terraform {
  required_providers {
    staysa = {
      source = "registry.opentofu.org/gattoufirabi/staysa"
    }
  }
}

locals {
  project = "${var.project_prefix}-${var.name}"
  # The three services run the same image; ROLE picks the part (see examples/store/app).
  common_env = {
    NODE_ENV = "production"
    APP_ENV  = var.name
    CURRENCY = "EUR"
  }
}

resource "staysa_project" "this" {
  id   = local.project
  name = "Store (${var.name})"
}

# ---------------- data ----------------

resource "staysa_database" "main" {
  project        = staysa_project.this.id
  id             = "main"
  engine         = "postgres:17"
  cpu_millis     = var.postgres.cpu_millis
  memory_mb      = var.postgres.memory_mb
  backup_enabled = true
  backup_hour    = var.postgres.backup_hour
}

resource "staysa_database" "cache" {
  project   = staysa_project.this.id
  id        = "cache"
  engine    = "redis:7"
  memory_mb = var.cache_memory_mb
}

resource "staysa_database" "docs" {
  count          = var.enable_documents ? 1 : 0
  project        = staysa_project.this.id
  id             = "docs"
  engine         = "mongodb:8.0"
  memory_mb      = 512
  backup_enabled = true
  backup_hour    = (var.postgres.backup_hour + 1) % 24
}

resource "staysa_database" "events" {
  count     = var.enable_events ? 1 : 0
  project   = staysa_project.this.id
  id        = "events"
  engine    = "kafka:3.9"
  memory_mb = 512
}

# Buckets belong to the organization, so their names carry the environment.
resource "staysa_bucket" "photos" {
  name     = "${local.project}-photos"
  quota_mb = var.photos_quota_mb
}

# ---------------- shared variables (every service of the project) ----------------

resource "staysa_project_variable" "api_url" {
  project = staysa_project.this.id
  name    = "API_URL"
  value   = "http://api.${staysa_project.this.id}.internal"
  secret  = false
}

resource "staysa_project_variable" "internal_token" {
  project = staysa_project.this.id
  name    = "INTERNAL_TOKEN"
  value   = var.internal_token
}

resource "staysa_project_variable" "payment_secret" {
  project = staysa_project.this.id
  name    = "PAYMENT_SECRET"
  value   = var.payment_secret
}

resource "staysa_project_variable" "email_from" {
  count   = var.email_from == "" ? 0 : 1
  project = staysa_project.this.id
  name    = "EMAIL_FROM"
  value   = var.email_from
  secret  = false
}

resource "staysa_project_variable" "report_to" {
  count   = var.report_to == "" ? 0 : 1
  project = staysa_project.this.id
  name    = "REPORT_TO"
  value   = var.report_to
  secret  = false
}

# ---------------- services ----------------

# The API: private (only the storefront, the worker and the functions reach it), autoscaled,
# its Prometheus metrics scraped by the platform.
resource "staysa_service" "api" {
  project      = staysa_project.this.id
  id           = "api"
  image        = var.image
  port         = 8080
  health_path  = "/"
  cpu_millis   = var.api.cpu_millis
  memory_mb    = var.api.memory_mb
  public       = false
  metrics_path = "/metrics"
  env          = merge(local.common_env, { ROLE = "api" })
  autoscaling = {
    min         = var.api.min
    max         = var.api.max
    cpu_percent = 65
  }
  depends_on = [staysa_project_variable.api_url, staysa_project_variable.internal_token]
}

# The storefront: public, autoscaled, cacheable pages served from the edge cache.
resource "staysa_service" "web" {
  project     = staysa_project.this.id
  id          = "web"
  image       = var.image
  port        = 8080
  health_path = "/"
  cpu_millis  = var.web.cpu_millis
  memory_mb   = var.web.memory_mb
  edge_cache  = var.edge_cache
  env         = merge(local.common_env, { ROLE = "web" })
  autoscaling = {
    min         = var.web.min
    max         = var.web.max
    cpu_percent = 70
  }
  depends_on = [staysa_service.api]
}

# The worker: no address; sends the emails and computes the statistics.
resource "staysa_service" "worker" {
  project    = staysa_project.this.id
  id         = "worker"
  image      = var.image
  worker     = true
  cpu_millis = var.worker.cpu_millis
  memory_mb  = var.worker.memory_mb
  env        = merge(local.common_env, { ROLE = "worker" })
  autoscaling = {
    min         = var.worker.min
    max         = var.worker.max
    cpu_percent = 75
  }
  depends_on = [staysa_project_variable.api_url]
}

# ---------------- connections: database URLs as encrypted secrets ----------------
# Each secret redeploys its service once, so it starts with every connection.

locals {
  # "<service>/<secret name>" -> database; the keys never depend on sensitive values
  connections = merge(
    { "api/DATABASE_URL" = "main", "api/REDIS_URL" = "cache" },
    var.enable_documents ? { "api/MONGODB_URI" = "docs", "worker/MONGODB_URI" = "docs" } : {},
    var.enable_events ? { "api/KAFKA_URL" = "events", "worker/KAFKA_URL" = "events" } : {},
  )
  database_urls = merge(
    { main = staysa_database.main.url, cache = staysa_database.cache.url },
    var.enable_documents ? { docs = staysa_database.docs[0].url } : {},
    var.enable_events ? { events = staysa_database.events[0].url } : {},
  )
  services = { api = staysa_service.api.id, worker = staysa_service.worker.id }
}

resource "staysa_service_secret" "connection" {
  for_each = local.connections
  project  = staysa_project.this.id
  service  = local.services[split("/", each.key)[0]]
  name     = split("/", each.key)[1]
  value    = local.database_urls[each.value]
}

# ---------------- custom domain ----------------

resource "staysa_domain" "web" {
  count    = var.domain == "" ? 0 : 1
  project  = staysa_project.this.id
  service  = staysa_service.web.id
  hostname = var.domain
}
