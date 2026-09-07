variable "org_slug" {
  type = string
}

variable "app" {
  type    = string
  default = "example-web"
}

variable "hostname" {
  description = "Public hostname, e.g. www.example.com."
  type        = string
}

variable "wildcard_hostname" {
  description = "Optional wildcard, e.g. *.example.com; needs DNS-01 validation."
  type        = string
  default     = ""
}

variable "cloudflare_zone_id" {
  description = "Zone the hostname lives in. CLOUDFLARE_API_TOKEN authenticates the provider."
  type        = string
}
