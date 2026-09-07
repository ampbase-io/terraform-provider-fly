# Secrets are write-only: Fly returns only a digest, so the provider cannot
# see a changed value on its own. value_wo_version is the rotation trigger,
# and it is held in plain state, so what goes there follows one rule:
#
#   a non-secret value is its own version;
#   a secret beside an identifier takes the identifier;
#   a bare secret takes its hash.
#
# Then min_secrets_version on the machine turns a rotation into a restart.

terraform {
  required_providers {
    fly = {
      source  = "ampbase-io/fly"
      version = "~> 0.2"
    }
  }
}

provider "fly" {
  org_slug = var.org_slug
}

resource "fly_app" "web" {
  name = var.app
}

# An identifier is not a secret. It is its own version and appears in state
# as itself.
resource "fly_secret" "access_key_id" {
  app              = fly_app.web.name
  name             = "AWS_ACCESS_KEY_ID"
  value_wo         = var.access_key_id
  value_wo_version = var.access_key_id
}

# The secret half of a key pair is new exactly when the identifier is, so
# the identifier is its version and nothing derived from the secret is
# stored.
resource "fly_secret" "secret_access_key" {
  app              = fly_app.web.name
  name             = "AWS_SECRET_ACCESS_KEY"
  value_wo         = var.secret_access_key
  value_wo_version = var.access_key_id
}

# A secret with no marker beside it takes its hash. Fine for a
# machine-generated token; for a human-chosen password, prefer an explicit
# version you bump on rotation.
resource "fly_secret" "api_token" {
  app              = fly_app.web.name
  name             = "API_TOKEN"
  value_wo         = var.api_token
  value_wo_version = sha256(var.api_token)
}

resource "fly_machine" "web" {
  app    = fly_app.web.name
  region = var.region
  name   = "web-1"
  image  = var.image

  # Fly stages a new secret value until a machine is updated to at least the
  # version that set it. Every secret the machine reads feeds this, so
  # rotating any of them restarts the machine into the new value.
  min_secrets_version = max(
    fly_secret.access_key_id.version,
    fly_secret.secret_access_key.version,
    fly_secret.api_token.version,
  )

  service {
    internal_port = 8080
    protocol      = "tcp"
    port {
      port     = 443
      handlers = ["tls", "http"]
    }
  }
}
