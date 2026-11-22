variable "project_prefix" {
  description = "Prefix of the project IDs: <prefix>-staging and <prefix>-production."
  type        = string
  default     = "store"

  validation {
    condition     = can(regex("^[a-z][a-z0-9]{1,9}$", var.project_prefix))
    error_message = "2 to 10 lowercase letters and digits, starting with a letter."
  }
}

variable "image" {
  description = "Image of the store for staging (registry/org/store:tag), from scripts/build-and-push.sh."
  type        = string
}

variable "production_image" {
  description = "Image running in production. Empty: production is not created. Promote by setting it to an image already tested in staging."
  type        = string
  default     = ""
}

variable "production_token" {
  description = "Admin API token of the production organization (TF_VAR_production_token). Empty: production goes to the same organization as staging."
  type        = string
  sensitive   = true
  default     = ""
}

variable "internal_token" {
  description = "Secret of internal calls (functions to API). Generate with: openssl rand -hex 24"
  type        = string
  sensitive   = true
}

variable "payment_secret" {
  description = "HMAC secret shared with the payment provider. Generate with: openssl rand -hex 24"
  type        = string
  sensitive   = true
}

variable "email_from" {
  description = "Sender of order emails, such as \"Store <shop@example.com>\" (a verified sending domain). Empty: emails are logged only."
  type        = string
  default     = ""
}

variable "report_to" {
  description = "Recipient of the daily sales report (production)."
  type        = string
  default     = ""
}

variable "dns_zone" {
  description = "Domain hosted on the platform, such as example.com. Empty: no DNS, the platform addresses are used."
  type        = string
  default     = ""
}

variable "state_passphrase" {
  description = "Passphrase of the encrypted state (OpenTofu encryption block in versions.tf)."
  type        = string
  sensitive   = true
  default     = ""
}
