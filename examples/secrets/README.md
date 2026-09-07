# Secret rotation

Fly never returns a secret's value, so `fly_secret` keeps it write-only and
rotates on `value_wo_version`. That attribute is held in plain state, which
is why choosing it follows a rule rather than a habit. This example shows
the three cases and the `min_secrets_version` wiring that turns a rotation
into a machine restart.

```sh
tofu init
tofu apply -var org_slug=my-org -var access_key_id=... -var secret_access_key=... -var api_token=...
```
