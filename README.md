# terraform-provider-fly

A Terraform and OpenTofu provider for the [Fly.io Machines API](https://fly.io/docs/machines/api/).

It manages the pieces of a Fly app that the Machines API exposes, with the
waits and retries a real apply needs: a machine is not "created" until it is
started and its checks pass, a volume's `id` is safe to mount in the same
apply, a cert can gate downstream resources on actually being live, and
transient API failures are retried on a bounded schedule.

## Resources

| Resource | Manages |
|---|---|
| `fly_app` | An app, optionally on its own private network |
| `fly_machine` | A machine: guest size, env, services and checks, files, mounts, restart policy, sidecar containers, metrics |
| `fly_ip` | Flycast (`private_v6`) and public (`shared_v4`, `public_v4`, `public_v6`) addresses |
| `fly_secret` | An app secret, write-only with a rotation trigger |
| `fly_volume` | A volume, with in-place extend and host-migration recovery |
| `fly_cert` | An ACME certificate, with the DNS records it needs as computed attributes |
| `fly_cert_validation` | A state gate that waits for a cert to be configured |

Full attribute reference is under [`docs/`](docs/) and on the registry.

## Usage

```hcl
terraform {
  required_providers {
    fly = {
      source  = "ampbase-io/fly"
      version = "~> 0.1"
    }
  }
}

provider "fly" {
  org_slug = "my-org"
  # api_token defaults to FLY_API_TOKEN. Use an org token from
  # `fly tokens create org my-org`, not a personal `fly auth token`:
  # the provider holds a static token and cannot re-issue one whose
  # discharges expire mid-run.
}

resource "fly_app" "web" {
  name = "my-app"
}

resource "fly_machine" "web" {
  app    = fly_app.web.name
  name   = "web-1"
  region = "iad"
  image  = "registry.fly.io/my-app:latest"

  service {
    internal_port = 8080
    port {
      port     = 443
      handlers = ["tls", "http"]
    }
  }
}
```

More in [`examples/`](examples/).

### Secrets

Fly never returns a secret's value, only a digest, so the provider cannot
tell from the API whether the value it last sent is current. `fly_secret`
therefore keeps the value write-only — it is never held in plan or state —
and rotates on `value_wo_version`, which is: change it whenever the value
changes. Pair it with `fly_machine.min_secrets_version` so a rotation
restarts the machines that reference it.

### Health and readiness

`fly_machine` blocks on create and update until the machine reports
`started` and every configured check passes, then optionally polls
`health_check_url`. That URL is fetched from the host running Terraform, so
a Flycast address needs WireGuard or another route into the org's network.

### Known limitations

- `fly_machine` reads back identity fields only (`id`, `state`, `region`,
  `private_ip`, `image_digest`). Out-of-band changes to the image, env,
  guest or services are not detected as drift, and an imported machine
  needs its full config declared.
- Wait timeouts are fixed (120s to start, 60s to settle, 5m for a volume,
  10m default for cert validation).

## The `flyio` client

The [`flyio`](flyio/) package is the client the provider is built on and is
importable on its own: a thin wrapper over the generated Machines API client
with the retry policy, wait loops and error classification the resources
use. `flyio/machines` is generated from Fly's OpenAPI specification with
`go generate`.

## Development

```sh
go build ./...
go test ./...
go generate ./...   # regenerates docs/ from the schemas and examples/
```

Run the provider with `-debug` to attach a CLI via the `TF_REATTACH_PROVIDERS`
line it prints.

## Releasing

Push a `v*` tag. The release workflow runs GoReleaser, which builds every
platform, signs the checksums with the repository's GPG key, and attaches
the registry manifest.
