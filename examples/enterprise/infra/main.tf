locals {
  # whether a second token was given is not a secret (the token itself is)
  one_organization = nonsensitive(var.production_token == "")
}

# Two environments built from one module, so production is a copy of what was tested:
#   staging     small, scales 1-2, lean data tier, always on the latest image
#   production  scales 2-N, full data tier (MongoDB, Kafka), backups, custom domain,
#               runs the image you promote (production_image)

module "staging" {
  source         = "./modules/environment"
  name           = "staging"
  project_prefix = var.project_prefix
  image          = var.image
  internal_token = var.internal_token
  payment_secret = var.payment_secret
  email_from     = var.email_from

  web    = { cpu_millis = 100, memory_mb = 128, min = 1, max = 2 }
  api    = { cpu_millis = 150, memory_mb = 192, min = 1, max = 2 }
  worker = { cpu_millis = 100, memory_mb = 128, min = 1, max = 1 }

  postgres        = { cpu_millis = 250, memory_mb = 256, backup_hour = 2 }
  cache_memory_mb = 128
  photos_quota_mb = 256
  edge_cache      = false # see changes at once while testing

  # a name in the zone only when both environments share the organization that hosts it
  domain     = var.dns_zone != "" && local.one_organization ? "staging.${var.dns_zone}" : ""
  depends_on = [staysa_dns_zone.main]
}

module "production" {
  count          = var.production_image == "" ? 0 : 1
  source         = "./modules/environment"
  providers      = { staysa = staysa.production }
  name           = "production"
  project_prefix = var.project_prefix
  image          = var.production_image
  internal_token = var.internal_token
  payment_secret = var.payment_secret
  email_from     = var.email_from
  report_to      = var.report_to

  # Two instances at least for each public path: one can be replaced or fail while the other serves.
  web    = { cpu_millis = 250, memory_mb = 256, min = 2, max = 6 }
  api    = { cpu_millis = 500, memory_mb = 384, min = 2, max = 8 }
  worker = { cpu_millis = 250, memory_mb = 256, min = 1, max = 4 }

  postgres         = { cpu_millis = 1000, memory_mb = 1024, backup_hour = 3 }
  cache_memory_mb  = 256
  enable_documents = true
  enable_events    = true
  photos_quota_mb  = 4096

  domain     = var.dns_zone == "" ? "" : "shop.${var.dns_zone}"
  depends_on = [staysa_dns_zone.main]
}

# ---------------- DNS hosted on the platform ----------------

# The zone belongs to the production organization; staging's domain is a name inside it.
resource "staysa_dns_zone" "main" {
  provider = staysa.production
  count    = var.dns_zone == "" ? 0 : 1
  domain   = var.dns_zone
}

# Mail and ownership records live with the zone; the storefront records are written by the
# platform when the custom domains are added (staysa_domain in the module).
resource "staysa_dns_record" "spf" {
  provider = staysa.production
  count    = var.dns_zone == "" ? 0 : 1
  zone     = staysa_dns_zone.main[0].domain
  name     = "@"
  type     = "TXT"
  value    = "v=spf1 -all"
}

resource "staysa_dns_record" "caa" {
  provider = staysa.production
  count    = var.dns_zone == "" ? 0 : 1
  zone     = staysa_dns_zone.main[0].domain
  name     = "@"
  type     = "CAA"
  value    = "0 issue \"letsencrypt.org\""
}
