resource "fly_cert" "web" {
  app      = fly_app.web.name
  hostname = "www.example.com"
}

# Create the DNS record the cert needs from the computed attributes, e.g.
# a CNAME to fly_cert.web.cname for a subdomain.
