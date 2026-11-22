output "staging" {
  description = "Staging: project, addresses, bucket."
  value = {
    project    = module.staging.project
    storefront = module.staging.storefront_url
    api        = module.staging.api_internal_host
    photos     = module.staging.photos_bucket
    revisions  = module.staging.revisions
  }
}

output "production" {
  description = "Production (null until production_image is set)."
  value = var.production_image == "" ? null : {
    project    = module.production[0].project
    storefront = module.production[0].storefront_url
    api        = module.production[0].api_internal_host
    photos     = module.production[0].photos_bucket
    revisions  = module.production[0].revisions
  }
}

output "nameservers" {
  description = "Name servers to set at your registrar for dns_zone."
  value       = var.dns_zone == "" ? [] : staysa_dns_zone.main[0].nameservers
}
