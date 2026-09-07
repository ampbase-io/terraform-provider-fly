# A ClickHouse cluster with a Keeper quorum

Three Keeper machines and two ClickHouse replicas, each on its own volume,
with an OpenTelemetry Collector sidecar on every replica. Derived from a
production cluster; the shape is what the provider's less obvious blocks are
for:

- **`file`** renders per-machine config (a Keeper's `server_id`, the peer
  list) into the guest, so one image serves every node.
- **`fly_volume` + `mount` with `readopt_on_host_migration`** survive Fly
  moving a machine to a new host, which forks the volume under a new ID.
- **A machine-level `check`** gates the apply on a quorum member that
  publishes no service, so a rolling change waits for each node to answer.
- **`container` + `depends_on` + `metrics`** run the collector beside the
  server and give Fly's Prometheus per-replica attribution for free.
- **`fly_secret` with `value_wo_version`** feeds `min_secrets_version`, so a
  rotated key restarts the replicas into the new value.

```sh
tofu init
tofu apply -var org_slug=my-org -var s3_access_key_id=... -var s3_secret_access_key=...
```
