# Bringing machines that flyctl created under Terraform without recreating
# them. Fly assigns names like `weathered-frog-7982` to machines it creates;
# the provider keeps the live name in state and ignores the configured one,
# so importing into a resource that says `vault-1` does not plan a replace.

terraform {
  required_providers {
    fly = {
      source  = "registry.terraform.io/ampbase-io/fly"
      version = "~> 0.2"
    }
  }
}

provider "fly" {
  org_slug = var.org_slug
}

# The app itself is adopted by name.
import {
  to = fly_app.vault
  id = var.app
}

resource "fly_app" "vault" {
  name = var.app
}

# One import per existing machine, matched to the resource index by list
# position. The composite ID is <app>/<machine_id>, because machine IDs
# are unique only within an app.
import {
  for_each = { for i, id in var.machine_ids : tostring(i) => id }
  to       = fly_machine.vault[tonumber(each.key)]
  id       = "${var.app}/${each.value}"
}

resource "fly_machine" "vault" {
  count  = length(var.machine_ids)
  app    = fly_app.vault.name
  region = var.region
  name   = "vault-${count.index + 1}"
  image  = var.image

  guest {
    cpu_kind  = "shared"
    cpus      = 1
    memory_mb = 512
  }

  service {
    internal_port = 8200
    protocol      = "tcp"
    autostart     = false
    autostop      = "off"

    port {
      port     = 8200
      handlers = ["http"]
    }

    # Standby and sealed nodes are healthy for the purpose of a rollout;
    # the query keeps a rolling update from waiting on a node that will
    # never report 200.
    check {
      type     = "http"
      port     = 8200
      method   = "GET"
      path     = "/v1/sys/health?standbyok=true&uninitok=true&sealedok=true"
      interval = "10s"
      timeout  = "5s"
    }
  }

  restart {
    policy = "on-failure"
  }
}
