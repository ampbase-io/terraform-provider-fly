resource "fly_volume" "data" {
  app     = fly_app.web.name
  name    = "data"
  region  = "iad"
  size_gb = 10

  # Match the guest of the machine that will mount it so Fly places the
  # volume on a host with capacity for that machine shape.
  compute {
    cpu_kind  = "shared"
    cpus      = 1
    memory_mb = 512
  }
}
