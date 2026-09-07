variable "org_slug" {
  type = string
}

variable "app" {
  description = "Name of the existing app that owns the machines."
  type        = string
  default     = "example-vault"
}

variable "region" {
  type    = string
  default = "iad"
}

variable "replicas" {
  type    = number
  default = 3
}

variable "adopt_machine_ids" {
  description = "IDs of the existing machines, from `fly machines list -a <app>`, in index order. Empty creates fresh machines."
  type        = list(string)
  default     = []
}

variable "otelcol_image" {
  type    = string
  default = "otel/opentelemetry-collector-contrib:latest"
}

variable "storage_bucket" {
  description = "Object storage bucket Vault's storage backend uses."
  type        = string
}

variable "otlp_api_key" {
  description = "API key the collector exports with. Empty creates no secret and the sidecar references none."
  type        = string
  sensitive   = true
  default     = ""
}
