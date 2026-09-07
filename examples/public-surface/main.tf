# Everything an app needs to be reachable from the internet under its own
# hostname, in one apply: addresses, an ACME certificate, the DNS record it
# validates through, and a gate that holds the apply until the certificate
# is live.

terraform {
  required_providers {
    fly = {
      source  = "registry.terraform.io/ampbase-io/fly"
      version = "~> 0.2"
    }
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 5.0"
    }
  }
}

provider "fly" {
  org_slug = var.org_slug
}

provider "cloudflare" {}

resource "fly_app" "web" {
  name = var.app
}

# Public addresses. A shared IPv4 is free and enough for most apps; a
# dedicated one (type = "public_v4") is billed. Both are auto-allocated by
# `fly deploy` but not by the Machines API, so declare them here.
resource "fly_ip" "v4" {
  app  = fly_app.web.name
  type = "shared_v4"
}

resource "fly_ip" "v6" {
  app  = fly_app.web.name
  type = "public_v6"
}

# A Flycast address as well, so apps inside the org reach this one
# privately at <app>.flycast rather than going out and back in.
resource "fly_ip" "flycast" {
  app  = fly_app.web.name
  type = "private_v6"
}

# The certificate returns as soon as Fly accepts the request. Its computed
# attributes say which DNS records validation needs: a CNAME for a
# subdomain, A/AAAA for an apex, and the ACME challenge CNAME for a
# wildcard.
resource "fly_cert" "web" {
  app      = fly_app.web.name
  hostname = var.hostname
}

resource "cloudflare_dns_record" "web" {
  zone_id = var.cloudflare_zone_id
  name    = var.hostname
  type    = "CNAME"
  content = fly_cert.web.cname
  ttl     = 60
  proxied = false
}

# Holds the apply until the certificate is configured, and records that in
# state. Anything that must not exist before the hostname serves TLS
# depends on this rather than on fly_cert.
resource "fly_cert_validation" "web" {
  cert_id                 = fly_cert.web.id
  validation_dependencies = [cloudflare_dns_record.web.id]
  validation_timeout      = "15m"
}

# A wildcard certificate can only validate over DNS-01, through the ACME
# challenge CNAME the cert reports.
resource "fly_cert" "wildcard" {
  count    = var.wildcard_hostname == "" ? 0 : 1
  app      = fly_app.web.name
  hostname = var.wildcard_hostname
}

resource "cloudflare_dns_record" "wildcard_acme" {
  count   = var.wildcard_hostname == "" ? 0 : 1
  zone_id = var.cloudflare_zone_id
  name    = fly_cert.wildcard[0].acme_challenge_name
  type    = "CNAME"
  content = fly_cert.wildcard[0].acme_challenge_target
  ttl     = 60
  proxied = false
}

resource "fly_cert_validation" "wildcard" {
  count                   = var.wildcard_hostname == "" ? 0 : 1
  cert_id                 = fly_cert.wildcard[0].id
  validation_dependencies = [cloudflare_dns_record.wildcard_acme[0].id]
}

output "addresses" {
  value = {
    v4      = fly_ip.v4.address
    v6      = fly_ip.v6.address
    flycast = fly_ip.flycast.address
  }
}
