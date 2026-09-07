# Public surface: addresses, certificate, DNS, and a validation gate

Derived from an environment module that stamps out an app's public face in
one apply.

- **`fly_ip`** for the three address types a public app needs, plus a
  Flycast address for private callers.
- **`fly_cert`** returns immediately and exposes what DNS needs (`cname`,
  `a_records`, `acme_challenge_target`) as attributes, so the record that
  validates it is created in the same apply.
- **`fly_cert_validation`** is the AWS-ACM-style gate: it waits for the
  certificate to be configured and records that in state, so downstream
  resources depend on a hostname that actually serves TLS.
- A **wildcard** needs DNS-01 through the ACME challenge CNAME.

```sh
export CLOUDFLARE_API_TOKEN=...
tofu init
tofu apply -var org_slug=my-org -var hostname=www.example.com -var cloudflare_zone_id=...
```
