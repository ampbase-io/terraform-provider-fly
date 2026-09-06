# Gates the apply on the cert actually being live. Pass the IDs of the DNS
# records it depends on so validation waits for them.
resource "fly_cert_validation" "web" {
  cert_id                 = fly_cert.web.id
  validation_dependencies = [cloudflare_dns_record.web.id]
}
