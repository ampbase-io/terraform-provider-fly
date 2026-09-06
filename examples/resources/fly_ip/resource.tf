resource "fly_ip" "flycast" {
  app  = fly_app.web.name
  type = "private_v6"
}

resource "fly_ip" "public_v4" {
  app  = fly_app.web.name
  type = "shared_v4"
}
