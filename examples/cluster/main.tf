# A ClickHouse cluster on Fly: a Keeper Raft quorum and N ClickHouse replicas,
# each machine pinned to its own volume, with an OTel Collector sidecar
# scraping every replica. This is the shape the provider's file, mount,
# check, container and metrics blocks exist for.

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
  keeper_app     = "${var.name}-keeper"
  clickhouse_app = var.name

  # Each Keeper is reachable at <process-group>.process.<app>.internal, which
  # is what fly_process_group in the machine metadata below gives it.
  keeper_peers = [
    for i in range(var.keeper_replicas) : {
      id   = i + 1
      host = "keeper-${i + 1}.process.${local.keeper_app}.internal"
    }
  ]
  replicas = [
    for i in range(var.clickhouse_replicas) :
    "replica-${i + 1}.process.${local.clickhouse_app}.internal"
  ]
}

# --- Apps, on their own private network ---

resource "fly_app" "keeper" {
  name    = local.keeper_app
  network = "${var.name}-net"
}

resource "fly_app" "clickhouse" {
  name    = local.clickhouse_app
  network = "${var.name}-net"
}

# --- Images ---
#
# Both images are built through Fly's remote builder inside this apply,
# into each app's own registry namespace, at a label that is a content
# hash of the files under images/; a change to any of them rolls the
# machines. See ../modules/fly-image. The collector sidecar runs the stock
# upstream image, since nothing about it is specific to this cluster.

module "keeper_image" {
  source = "../modules/fly-image"

  app     = fly_app.keeper.name
  context = "${path.module}/images/keeper"
  trigger_files = [
    "${path.module}/images/keeper/Dockerfile",
    "${path.module}/images/keeper/fly.toml",
    "${path.module}/images/keeper/docker-entrypoint-fly.sh",
  ]
}

module "clickhouse_image" {
  source = "../modules/fly-image"

  app     = fly_app.clickhouse.name
  context = "${path.module}/images/server"
  trigger_files = [
    "${path.module}/images/server/Dockerfile",
    "${path.module}/images/server/fly.toml",
    "${path.module}/images/server/storage.xml",
    "${path.module}/images/server/prometheus.xml",
    "${path.module}/images/server/logging.xml",
  ]
}

# --- Keeper quorum ---
#
# Volumes are host-pinned. Distinct per-machine names plus
# unique_zone_app_wide spread the quorum across hardware zones; the compute
# block lets Fly place each volume on a host that can also run the machine.

resource "fly_volume" "keeper_data" {
  count   = var.keeper_replicas
  app     = fly_app.keeper.name
  name    = "keeper_data_${count.index + 1}"
  region  = var.region
  size_gb = var.keeper_volume_gb

  unique_zone_app_wide = true
  auto_backup_enabled  = true
  snapshot_retention   = 7

  # If Fly migrates the machine to a new host it forks the volume; this
  # re-anchors the resource to the fork on the next refresh, paired with
  # the same flag on the mount below.
  readopt_on_host_migration = true

  compute {
    cpu_kind  = "shared"
    cpus      = 1
    memory_mb = 1024
  }
}

resource "fly_machine" "keeper" {
  count  = var.keeper_replicas
  app    = fly_app.keeper.name
  region = var.region
  name   = "keeper-${count.index + 1}"
  image  = module.keeper_image.ref

  metadata = {
    fly_process_group = "keeper-${count.index + 1}"
  }

  guest {
    cpu_kind  = "shared"
    cpus      = 1
    memory_mb = 1024
  }

  # Per-machine config without a per-machine image: this node's server_id
  # and the full peer list, rendered into the guest at boot.
  file {
    guest_path = "/etc/clickhouse-keeper/keeper_config.xml"
    raw_value = base64encode(templatefile("${path.module}/templates/keeper_config.xml.tmpl", {
      server_id = count.index + 1
      peers     = local.keeper_peers
    }))
  }

  mount {
    volume                    = fly_volume.keeper_data[count.index].id
    path                      = "/var/lib/clickhouse-keeper"
    readopt_on_host_migration = true
  }

  restart {
    policy = "always"
  }

  # Keeper publishes no service (it is reached over .internal DNS), so a
  # machine-level check is what gates the apply: the next machine is not
  # touched until this one answers on its client port.
  check {
    name         = "keeper-client-port"
    type         = "tcp"
    port         = 9181
    interval     = "10s"
    timeout      = "5s"
    grace_period = "20s"
  }
}

# --- ClickHouse replicas ---

resource "fly_secret" "aws_access_key_id" {
  app              = fly_app.clickhouse.name
  name             = "AWS_ACCESS_KEY_ID"
  value_wo         = var.s3_access_key_id
  value_wo_version = var.s3_access_key_id
}

resource "fly_secret" "aws_secret_access_key" {
  app              = fly_app.clickhouse.name
  name             = "AWS_SECRET_ACCESS_KEY"
  value_wo         = var.s3_secret_access_key
  value_wo_version = var.s3_access_key_id
}

resource "fly_volume" "clickhouse_data" {
  count   = var.clickhouse_replicas
  app     = fly_app.clickhouse.name
  name    = "clickhouse_data_${count.index + 1}"
  region  = var.region
  size_gb = var.clickhouse_volume_gb

  unique_zone_app_wide      = true
  auto_backup_enabled       = true
  snapshot_retention        = 7
  readopt_on_host_migration = true

  compute {
    cpu_kind  = "performance"
    cpus      = 2
    memory_mb = 4096
  }
}

resource "fly_machine" "clickhouse" {
  count  = var.clickhouse_replicas
  app    = fly_app.clickhouse.name
  region = var.region
  name   = "replica-${count.index + 1}"
  image  = module.clickhouse_image.ref

  metadata = {
    fly_process_group = "replica-${count.index + 1}"
  }

  guest {
    cpu_kind  = "performance"
    cpus      = 2
    memory_mb = 4096
  }

  # Activates the secrets above on the next boot; a rotation moves the
  # version and restarts the machine into it.
  min_secrets_version = max(
    fly_secret.aws_access_key_id.version,
    fly_secret.aws_secret_access_key.version,
  )

  mount {
    volume                    = fly_volume.clickhouse_data[count.index].id
    path                      = "/var/lib/clickhouse"
    readopt_on_host_migration = true
  }

  restart {
    policy = "always"
  }

  # Native and HTTP protocols, reachable over Flycast within the org.
  service {
    internal_port = 9000
    protocol      = "tcp"
    autostart     = true
    autostop      = "off"
    port {
      port = 9000
    }
  }

  service {
    internal_port = 8123
    protocol      = "tcp"
    autostart     = true
    autostop      = "off"
    port {
      port = 8123
    }
    check {
      type     = "http"
      port     = 8123
      path     = "/ping"
      interval = "10s"
      timeout  = "5s"
    }
  }

  # Once container blocks exist, Fly ignores the machine's top-level
  # image, env and file fields; the top-level image above stays only
  # because the schema requires one. The server is declared as a container
  # like any other, and naming it "app" is what lets the sidecar name it in
  # depends_on. Every container shares the machine's network namespace and
  # its Fly identity.
  container {
    name  = "app"
    image = module.clickhouse_image.ref

    # Each replica keeps its cold parts under its own prefix in the bucket;
    # the storage.xml baked into the image reads the endpoint and the
    # credentials from env.
    env = {
      COLD_ENDPOINT = "${var.s3_endpoint}/${var.s3_bucket}/replica-${format("%02d", count.index + 1)}/"
    }

    secret {
      env_var = "AWS_ACCESS_KEY_ID"
    }
    secret {
      env_var = "AWS_SECRET_ACCESS_KEY"
    }

    file {
      guest_path = "/etc/clickhouse-server/config.d/remote_servers.xml"
      raw_value = base64encode(templatefile("${path.module}/templates/remote_servers.xml.tmpl", {
        cluster  = var.name
        replicas = local.replicas
        keepers  = local.keeper_peers
      }))
    }
    file {
      guest_path = "/etc/clickhouse-server/config.d/macros.xml"
      raw_value = base64encode(templatefile("${path.module}/templates/macros.xml.tmpl", {
        cluster = var.name
        replica = count.index + 1
      }))
    }
  }

  container {
    name  = "otelcol"
    image = var.otelcol_image

    file {
      guest_path = "/etc/otelcol/config.yaml"
      raw_value  = base64encode(file("${path.module}/templates/otelcol.yaml"))
    }

    depends_on {
      name      = "app"
      condition = "started"
    }

    restart {
      policy = "always"
    }
  }

  # Fly's Prometheus scrapes the sidecar's exporter and labels the series
  # with this machine's app/instance/region, so replica-N's metrics are
  # attributed to replica-N with no relabeling.
  metrics {
    port = 9464
    path = "/metrics"
  }
}

# Flycast address for the cluster, so other apps in the org reach it at
# <app>.flycast.
resource "fly_ip" "clickhouse" {
  app  = fly_app.clickhouse.name
  type = "private_v6"
}
