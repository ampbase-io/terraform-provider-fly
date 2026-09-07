# Adopting flyctl-deployed machines

Takes an app and its machines, created by `fly launch` or `fly deploy`,
under Terraform without destroying them. Derived from adopting a Vault HA
cluster.

- **`import` blocks** with the `<app>/<machine_id>` form; Fly machine IDs
  are unique per app, not globally.
- **Names are immutable after create.** Fly ignores `name` on update, so the
  provider keeps the live name (`weathered-frog-7982`) in state and warns
  rather than planning a replace for the configured `vault-N`. Taint a
  machine to rename it.
- The first apply after adoption plans an in-place update to whatever the
  configuration says (image, guest, services); review that plan before
  applying, since Terraform's view of the machine starts from the config,
  not from what flyctl deployed.

```sh
tofu init
tofu plan -var org_slug=my-org -var 'machine_ids=["2872174b330338","e784e9eb9d1e18"]'
```
