variable "org_slug" {
  description = "Fly organization slug."
  type        = string
}

variable "name" {
  description = "Base name for the apps (the ClickHouse app, and <name>-keeper)."
  type        = string
  default     = "example-clickhouse"
}

variable "region" {
  description = "Fly region for every machine and volume."
  type        = string
  default     = "iad"
}

variable "keeper_replicas" {
  description = "Keeper quorum size; odd, 3 in practice."
  type        = number
  default     = 3
}

variable "clickhouse_replicas" {
  type    = number
  default = 2
}

variable "keeper_image" {
  type    = string
  default = "clickhouse/clickhouse-keeper:latest"
}

variable "clickhouse_image" {
  type    = string
  default = "clickhouse/clickhouse-server:latest"
}

variable "otelcol_image" {
  type    = string
  default = "otel/opentelemetry-collector-contrib:latest"
}

variable "keeper_volume_gb" {
  type    = number
  default = 10
}

variable "clickhouse_volume_gb" {
  type    = number
  default = 50
}

variable "s3_endpoint" {
  description = "S3 endpoint, no trailing slash and no bucket."
  type        = string
  default     = "https://fly.storage.tigris.dev"
}

variable "s3_bucket" {
  description = "Cold-tier bucket, provisioned out of band; see the README."
  type        = string
}

variable "s3_access_key_id" {
  description = "Access key ID for the cold-storage bucket. Its own rotation marker."
  type        = string
}

variable "s3_secret_access_key" {
  description = "Secret half of the key above; rotates with it."
  type        = string
  sensitive   = true
}
