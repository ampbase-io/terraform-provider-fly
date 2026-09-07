# A Vault HA cluster on Fly, adopted from machines that flyctl created.
# Each machine runs three containers: Vault itself, a one-shot registrar
# that installs Vault's plugins once the server can take a write, and an
# OpenTelemetry Collector that Fly's Prometheus scrapes. Derived from a
# production deployment; the comments carry what was measured against Fly
# rather than assumed.

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

locals {
  # Empty adopt_machine_ids is greenfield; index N adopts adopt_machine_ids[N]
  # into fly_machine.vault[N]. Greenfield is destructive for a live cluster
  # (new machines, a new storage namespace, a fresh unseal), so adopt.
  adopting = length(var.adopt_machine_ids) > 0

  # The registrar authenticates to Vault with the machine's OIDC token,
  # against a JWT auth role bound to this app; the address is the token
  # audience, not a dial target.
  vault_addr = "http://${var.app}.flycast:8200"
}

# --- Image ---
#
# Built through Fly's remote builder inside this apply, into the app's own
# registry namespace, at a label that is a content hash of everything the
# image is made from. A change to a script, the Dockerfile, or a plugin
# source produces a new label and an in-place update that rolls the
# machines onto it; nothing changed is a no-op.
#
# The plugins are enumerated with fileset rather than listed: a
# hand-written list would silently stop rebuilding the image the day a
# plugin is added, and the symptom would be machines running a plugin
# binary nobody shipped.

locals {
  image_context = "${path.module}/image"
  plugins = sort([
    for f in fileset("${local.image_context}/plugins", "**") :
    "${local.image_context}/plugins/${f}"
  ])
}

module "vault_image" {
  source = "../modules/fly-image"

  app     = fly_app.vault.name
  context = local.image_context
  trigger_files = concat([
    "${local.image_context}/fly.toml",
    "${local.image_context}/Dockerfile",
    "${local.image_context}/scripts/register-vault-plugins",
    "${local.image_context}/scripts/vault-ready",
    "${local.image_context}/scripts/fetch-oidc-token",
  ], local.plugins)
}

# --- Adoption ---
#
# The import blocks take the machines `fly deploy` created. The first apply
# after adoption plans an in-place update to whatever differs below (the
# sidecars, the metrics block); the image and env must match the live
# machines or the plan shows that diff too. Fly's machine names
# (`weathered-frog-7982`) stay: the API ignores a rename, so the provider
# keeps the live name in state and warns instead of planning a replace.

import {
  to = fly_app.vault
  id = var.app
}

resource "fly_app" "vault" {
  name = var.app
}

import {
  for_each = local.adopting ? { for i, id in var.adopt_machine_ids : tostring(i) => id } : {}
  to       = fly_machine.vault[tonumber(each.key)]
  id       = "${var.app}/${each.value}"
}

# --- Collector's exporter credential ---
#
# One key per app: every container on every machine of the app shares
# ownership of the upstream registration, which is the trust boundary Fly
# already draws. Gated on the value so a bootstrap with no collector
# backend yet creates no secret, and the sidecar below references none.

resource "fly_secret" "otlp_api_key" {
  count            = var.otlp_api_key == "" ? 0 : 1
  app              = fly_app.vault.name
  name             = "OTLP_API_KEY"
  value_wo         = var.otlp_api_key
  value_wo_version = sha256(var.otlp_api_key)
}

# --- Machines ---

resource "fly_machine" "vault" {
  count  = var.replicas
  app    = fly_app.vault.name
  region = var.region
  name   = "vault-${count.index + 1}"
  image  = module.vault_image.ref

  guest {
    cpu_kind  = "shared"
    cpus      = 2
    memory_mb = 1024
  }

  # Once `container` blocks are declared, Fly ignores the machine-level
  # image, env and file fields; the top-level image stays because the
  # schema requires one. Everything Vault needs lives in the "app"
  # container below. mount, service, metrics, guest, metadata and restart
  # stay machine-level: they describe the microVM every container shares.

  min_secrets_version = var.otlp_api_key == "" ? 0 : fly_secret.otlp_api_key[0].version

  service {
    internal_port = 8200
    protocol      = "tcp"
    autostart     = false
    autostop      = "off"

    port {
      port     = 8200
      handlers = ["http"]
    }

    # Standby, uninitialized and sealed nodes all count as healthy here:
    # this check gates a rolling apply, and a node that will never report
    # 200 would otherwise hold it forever.
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

  # --- Vault server ---
  #
  # Declared explicitly so the registrar can name it in depends_on. Vault
  # authenticates to its storage with the machine's identity, so no
  # app-level secret is injected here; a secret it did need would go in a
  # `secret` block on this container, since app secrets do not propagate
  # into containers on their own.
  container {
    name  = "app"
    image = module.vault_image.ref

    env = {
      VAULT_LOG_LEVEL = "info"
    }

    file {
      guest_path = "/vault/config/vault.hcl"
      raw_value = base64encode(templatefile("${path.module}/templates/vault.hcl.tmpl", {
        api_addr = local.vault_addr
        bucket   = var.storage_bucket
      }))
    }

    # The credential config Vault's storage auth reads; not baked into the
    # image because it differs per environment. mode is 0o644 spelled in
    # decimal: HCL has no octal literals.
    file {
      guest_path = "/vault/config/storage-credential.json"
      raw_value  = base64encode(jsonencode({ audience = local.vault_addr }))
      mode       = 420
    }

    # Readiness for the registrar below, and nothing else: Fly Proxy
    # routing is the service block's business, and the machine's own
    # checks are what serialise a rolling apply.
    #
    # exec rather than http, because no single /v1/sys/health response
    # expresses the condition. Health reports a standby identically whether
    # or not a leader exists, so it cannot exclude a cluster mid-election;
    # requiring an active node instead would fail forever on every standby,
    # one warning per interval for the life of the cluster. The script
    # reads sys/leader and asks the actual question: can a write issued
    # here reach a node that has finished post-unseal setup.
    healthcheck {
      name = "vault-write-ready"
      kind = "readiness"

      exec = {
        command = ["/usr/local/bin/vault-ready"]
      }

      # One interval is the window between a leader going away and this
      # noticing. One success, because sys/leader is a state read rather
      # than a flaky probe; asking twice only widens the window.
      interval_seconds  = 5
      timeout_seconds   = 2
      success_threshold = 1

      # Two intervals of grace so a Vault still opening its listener is
      # not failing, and three failures to go unhealthy so one dropped
      # poll does not tear down a healthy dependent.
      grace_period_seconds = 10
      failure_threshold    = 3
    }

    restart {
      policy = "on-failure"
    }
  }

  # --- Plugin registrar ---
  #
  # Registers the plugins baked into the image in Vault's catalog and pins
  # the cluster to them, then exits. A container rather than a line in the
  # Vault entrypoint so Vault's own process is left alone: this cannot
  # wedge the boot, its failures are a container state rather than a line
  # in Vault's log, and Fly owns the retry.
  #
  # The same image as the app container, deliberately: the registrar
  # hashes the plugin binaries and Vault execs them, and containers do not
  # share a filesystem, so one image digest is what makes those the same
  # bytes. Identity is unchanged by the split — Fly's OIDC token is
  # per-machine, so this container presents the same app name the JWT role
  # binds; a sidecar is never an auth boundary.
  #
  # A container exiting 0 does not stop the machine, and a clean exit is
  # not restarted. Measured on Fly rather than inferred, because the older
  # process-group model documents the opposite and getting it wrong here
  # stops Vault.
  container {
    name       = "plugin-registrar"
    image      = module.vault_image.ref
    entrypoint = ["/usr/local/bin/register-vault-plugins"]

    # Prefixed, because Fly strips VAULT_* from a machine's environment.
    # The registrar reaches Vault on loopback: containers share the
    # machine's network namespace.
    env = {
      REGISTRAR_VAULT_ADDR      = local.vault_addr
      REGISTRAR_VAULT_ROLE      = "vault-plugin-registrar"
      REGISTRAR_VAULT_AUTH_PATH = "jwt"
    }

    # "healthy" waits on the app container's own readiness check above —
    # not the machine-level check, which reports passing while leaving a
    # container gated on it unstarted, with no error and no event.
    #
    # Every machine's registrar runs, standbys included: a standby forwards
    # the write to the active node, which is why the readiness script
    # accepts one. Gating on "this node is active" would narrow the
    # rolling-deploy window at the price of a check that fails forever on
    # every standby. Constant cost, rare benefit.
    depends_on {
      name      = "app"
      condition = "healthy"
    }

    # A clean exit is success and stays exited; a failure is worth retrying
    # because the two that matter — Vault not yet unsealed, the auth role
    # not yet configured — both resolve on their own. Fly restarts a failed
    # container in a fraction of a second, so these restarts cover a fresh
    # login and a new attempt, not a wait; every wait that must outlive a
    # Vault phase lives in the script. max_retries counts restarts, so this
    # is six runs, which bounds the expensive failure of an attempt that
    # waits out the script's full unseal timeout.
    restart {
      policy      = "on-failure"
      max_retries = 5
    }
  }

  # --- OpenTelemetry Collector sidecar ---
  container {
    name  = "otelcol"
    image = var.otelcol_image

    file {
      guest_path = "/etc/otelcol/config.yaml"
      raw_value  = base64encode(file("${path.module}/templates/otelcol.yaml"))
    }

    # App secrets do not propagate into sidecars; each container lists what
    # it needs. Gated on the key existing so a bootstrap with none does not
    # reference a missing secret and fail the machine create.
    dynamic "secret" {
      for_each = var.otlp_api_key == "" ? [] : [1]
      content {
        env_var = "OTLP_API_KEY"
      }
    }

    # "started" is enough here: the scrape target on loopback only needs
    # the server process to exist, not a leader.
    depends_on {
      name      = "app"
      condition = "started"
    }

    restart {
      policy = "always"
    }
  }

  # Fly's Prometheus scrapes the sidecar and labels the series with this
  # machine's app, instance and region, so each replica's metrics are
  # attributed to it with no relabeling.
  metrics {
    port = 9464
    path = "/metrics"
  }
}
