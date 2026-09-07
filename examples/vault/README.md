# A Vault HA cluster: adoption, a one-shot registrar, and a sidecar

Derived from a production Vault deployment. Every machine runs three
containers, and the example carries the reasoning that was measured against
Fly rather than assumed:

- **The image is built in the apply**, through Fly's remote builder, by
  [`../modules/fly-image`](../modules/fly-image/): the label is a content
  hash of the Dockerfile, the scripts and every plugin source (enumerated
  with `fileset`, so a new file cannot be forgotten), and the machines take
  the module's `ref`, so they wait for the push and roll when it changes.
- **Adoption.** `import` blocks take the app and the machines `fly deploy`
  created; names Fly assigned stay, since the API ignores a rename.
- **Containers.** Once `container` blocks exist, Fly ignores the machine's
  top-level image, env and files; everything Vault needs moves into the
  `app` container. Secrets do not propagate into sidecars; each lists its
  own.
- **A readiness `healthcheck` with `exec`**, because no `/v1/sys/health`
  answer expresses "a write from here reaches a node that finished
  post-unseal setup"; that is what the registrar's `depends_on { condition
  = "healthy" }` waits on. A machine-level check does not satisfy it: it
  reports passing while the gated container never starts.
- **A one-shot container.** A clean exit stays exited and does not stop
  the machine; `restart { policy = "on-failure" max_retries = 5 }` gives a
  failed registration six runs, and the waits that must outlive a Vault
  phase live in the script, not in Fly's sub-second restart.
- **The same image for the registrar as for Vault**, since containers do
  not share a filesystem and the registrar hashes the plugin binaries Vault
  will exec.
- **`metrics`** on the machine so Fly's Prometheus attributes the sidecar's
  series to the right replica.

The scripts the containers run live under `image/scripts/`; `image/` is
the build context. `flyctl` on `PATH` and `FLY_API_TOKEN` are what the
build needs.

```sh
tofu init
tofu plan -var org_slug=my-org -var storage_bucket=example-vault-storage \
  -var 'adopt_machine_ids=["2872174b330338","e784e9eb9d1e18","d891b1a9c62e38"]'
```
