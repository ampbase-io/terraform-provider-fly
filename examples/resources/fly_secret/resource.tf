# The value is write-only. Fly returns only a digest, so the provider cannot
# see a changed value on its own: change value_wo_version whenever value_wo
# changes. Prefer a non-secret marker that moves with the value (a key ID
# beside its secret) over a hash of the secret itself.
resource "fly_secret" "database_url" {
  app              = fly_app.web.name
  name             = "DATABASE_URL"
  value_wo         = var.database_url
  value_wo_version = var.database_url_version
}
