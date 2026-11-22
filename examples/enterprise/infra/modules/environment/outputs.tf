output "project" {
  description = "Project ID."
  value       = staysa_project.this.id
}

output "storefront_url" {
  description = "Public address of the storefront."
  value       = var.domain == "" ? staysa_service.web.url : "https://${var.domain}"
}

output "api_internal_host" {
  description = "Private name of the API on the project's network."
  value       = staysa_service.api.internal_host
}

output "photos_bucket" {
  description = "Bucket of the product photos (name used by the console and the API)."
  value       = staysa_bucket.photos.name
}

output "postgres_host" {
  description = "Host of the main database on the private network."
  value       = staysa_database.main.host
}

output "domain_dns_records" {
  description = "Records to create when the domain's zone is not hosted on the platform."
  value       = var.domain == "" ? [] : staysa_domain.web[0].dns_records
}

output "revisions" {
  description = "Deployed revision of each service."
  value = {
    api    = staysa_service.api.revision
    web    = staysa_service.web.revision
    worker = staysa_service.worker.revision
  }
}
