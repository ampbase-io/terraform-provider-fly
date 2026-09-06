resource "fly_machine" "web" {
  app    = fly_app.web.name
  name   = "web-1"
  region = "iad"
  image  = "registry.fly.io/my-app:latest"

  guest {
    cpu_kind  = "shared"
    cpus      = 1
    memory_mb = 512
  }

  env = {
    PORT = "8080"
  }

  # Activate secrets staged by fly_secret on this machine's next boot.
  min_secrets_version = fly_secret.database_url.version

  service {
    internal_port = 8080
    protocol      = "tcp"
    autostart     = true
    autostop      = "stop"

    port {
      port     = 443
      handlers = ["tls", "http"]
    }

    check {
      type     = "http"
      port     = 8080
      path     = "/health"
      interval = "10s"
      timeout  = "2s"
    }
  }

  mount {
    volume = fly_volume.data.id
    path   = "/data"
  }
}
