variable "org_slug" {
  type = string
}

variable "app" {
  type    = string
  default = "example-web"
}

variable "region" {
  type    = string
  default = "iad"
}

variable "image" {
  type    = string
  default = "registry.fly.io/example-web:latest"
}

variable "access_key_id" {
  description = "Identifier half of an S3 key pair; not a secret."
  type        = string
}

variable "secret_access_key" {
  type      = string
  sensitive = true
}

variable "api_token" {
  description = "A machine-generated token; hashed for its version."
  type        = string
  sensitive   = true
}
