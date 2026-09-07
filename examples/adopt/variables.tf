variable "org_slug" {
  type = string
}

variable "app" {
  description = "Name of the existing app."
  type        = string
  default     = "example-vault"
}

variable "region" {
  type    = string
  default = "iad"
}

variable "image" {
  description = "Image the machines run after adoption; the first apply updates them to it."
  type        = string
  default     = "hashicorp/vault:latest"
}

variable "machine_ids" {
  description = "IDs of the machines to adopt, from `fly machines list -a <app>`."
  type        = list(string)
}
