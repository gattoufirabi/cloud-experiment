variable "name" {
  description = "Environment name (staging, production). The project ID is <prefix>-<name>."
  type        = string
}

variable "project_prefix" {
  description = "Prefix of the project IDs (2 to 10 lowercase letters)."
  type        = string
}

variable "image" {
  description = "Image of the store, built once and promoted from staging to production (registry/org/store:tag)."
  type        = string
}

variable "internal_token" {
  description = "Shared secret of calls between the store's functions and its API."
  type        = string
  sensitive   = true
}

variable "payment_secret" {
  description = "HMAC secret of the payment provider's webhook."
  type        = string
  sensitive   = true
}

variable "email_from" {
  description = "Sender of the order emails (a verified sending domain), or empty to only log them."
  type        = string
  default     = ""
}

variable "report_to" {
  description = "Address of the daily sales report, or empty."
  type        = string
  default     = ""
}

# ---- sizing: one object per tier, so staging stays small and production gets headroom ----

variable "web" {
  description = "Storefront: CPU (millicores) and memory (MB) per instance, autoscaling range."
  type = object({
    cpu_millis = number
    memory_mb  = number
    min        = number
    max        = number
  })
}

variable "api" {
  description = "API: CPU and memory per instance, autoscaling range."
  type = object({
    cpu_millis = number
    memory_mb  = number
    min        = number
    max        = number
  })
}

variable "worker" {
  description = "Background worker: CPU and memory per instance, autoscaling range."
  type = object({
    cpu_millis = number
    memory_mb  = number
    min        = number
    max        = number
  })
}

variable "postgres" {
  description = "Main database (orders, products): resources and daily backup hour (UTC)."
  type = object({
    cpu_millis  = number
    memory_mb   = number
    backup_hour = number
  })
}

variable "cache_memory_mb" {
  description = "Redis cache memory (MB)."
  type        = number
  default     = 128
}

variable "enable_documents" {
  description = "MongoDB for reviews and sales statistics (needs 512 MB)."
  type        = bool
  default     = false
}

variable "enable_events" {
  description = "Kafka for order events (needs 512 MB). Without it the API hands orders to the worker through the queue only."
  type        = bool
  default     = false
}

variable "photos_quota_mb" {
  description = "Space of the product photo bucket (MB)."
  type        = number
  default     = 512
}

variable "domain" {
  description = "Public host name of the storefront (in a zone hosted on the platform), or empty to use the platform address."
  type        = string
  default     = ""
}

variable "edge_cache" {
  description = "Cache the storefront's cacheable responses at the edge."
  type        = bool
  default     = true
}
