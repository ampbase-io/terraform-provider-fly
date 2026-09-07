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
- **A cold tier in object storage.** `templates/storage.xml` adds an S3 disk
  and a `hot_then_cold` policy; each replica writes under its own prefix in
  the bucket, and a table opts in with `SETTINGS storage_policy =
  'hot_then_cold'`.

## The bucket

The bucket is deliberately not in this configuration: its lifecycle is not
the cluster's, and a `destroy` of the machines must not take the data with
them. Create it once, on Fly's Tigris, and hand the keys to the variables:

```sh
fly storage create --org my-org --name example-clickhouse-cold
# prints AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY for the bucket
```

Then:

```sh
tofu init
tofu apply -var org_slug=my-org \
  -var s3_bucket=example-clickhouse-cold \
  -var s3_access_key_id=tid_... -var s3_secret_access_key=tsec_...
```
